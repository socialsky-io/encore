package daemon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"encr.dev/cli/daemon"
	"encr.dev/cli/daemon/dash"
	"encr.dev/cli/daemon/run"
	"encr.dev/cli/daemon/runtime"
	"encr.dev/cli/daemon/runtime/trace"
	"encr.dev/cli/daemon/secret"
	"encr.dev/cli/daemon/sqldb"
	"encr.dev/cli/internal/conf"
	"encr.dev/cli/internal/xos"
	daemonpb "encr.dev/proto/encore/daemon"
	"encr.dev/proto/encore/server/remote"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
	"google.golang.org/grpc/keepalive"
)

// Main runs the daemon.
func Main(version string) {
	if err := runMain(version); err != nil {
		log.Fatal().Err(err).Msg("daemon failed")
	}
}

func runMain(version string) (err error) {
	// xit receives signals from the different subsystems
	// that something went wrong and it's time to exit.
	// Sending nil indicates it's time to gracefully exit.
	exit := make(chan error)

	d := &Daemon{exit: exit, Version: version}
	defer handleBailout(&err)
	defer d.closeAll()

	d.init()
	d.serve()

	return <-exit
}

// Daemon orchestrates setting up the different daemon subsystems.
type Daemon struct {
	Log     zerolog.Logger
	Daemon  *net.UnixListener
	Runtime *net.TCPListener
	DBProxy *net.TCPListener
	Dash    *net.TCPListener
	Version string

	Remote     remote.RemoteClient
	Secret     *secret.Manager
	RunMgr     *run.Manager
	ClusterMgr *sqldb.ClusterManager
	Trace      *trace.Store
	DashSrv    *dash.Server
	Server     *daemon.Server

	// exit is a channel that shuts down the daemon when sent on.
	// A nil error indicates graceful exit.
	exit chan<- error

	// close are the things to close when exiting.
	close []io.Closer
}

func (d *Daemon) init() {
	d.Daemon = d.listenDaemonSocket()
	d.Runtime = d.listenTCP()
	d.DBProxy = d.listenTCP()
	d.Dash = d.listenTCP()

	d.Trace = trace.NewStore()
	d.ClusterMgr = sqldb.NewClusterManager()
	d.Remote = d.setupRemoteClient()
	d.Secret = secret.New(d.Remote)
	d.RunMgr = &run.Manager{
		RuntimePort: tcpPort(d.Runtime),
		DBProxyPort: tcpPort(d.DBProxy),
		DashPort:    tcpPort(d.Dash),
		Secret:      d.Secret,
	}
	d.DashSrv = dash.NewServer(d.RunMgr, d.Trace)
	d.Server = daemon.New(d.Version, d.RunMgr, d.ClusterMgr, d.Secret, d.Remote)
}

func (d *Daemon) serve() {
	go d.serveDaemon()
	go d.serveRuntime()
	go d.serveDBProxy()
	go d.serveDash()
}

// listenDaemonSocket listens on the encored.sock UNIX socket
// and arranges to exit when the socket is closed.
func (d *Daemon) listenDaemonSocket() *net.UnixListener {
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		fatal(err)
	}
	socketPath := filepath.Join(userCacheDir, "encore", "encored.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		fatal(err)
	}

	// If the daemon socket already exists, remove it so we can take over listening.
	if _, err := xos.SocketStat(socketPath); err == nil {
		os.Remove(socketPath)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		fatal(err)
	}
	d.closeOnExit(ln)

	// Detect when the socket is closed.
	go func() {
		d.exit <- detectSocketClose(ln, socketPath)
	}()
	return ln
}

// setupRemoteClient sets up a grpc client to Encore's backend service.
func (d *Daemon) setupRemoteClient() remote.RemoteClient {
	ts := &conf.TokenSource{}
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(nil)),
		grpc.WithPerRPCCredentials(oauth.TokenSource{TokenSource: ts}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 20 * time.Second,
		}),
	}
	conn, err := grpc.Dial("remote.encoreapis.com:443", dialOpts...)
	if err != nil {
		fatalf("failed to dial encore server: %v", err)
	}
	d.closeOnExit(conn)
	return remote.NewRemoteClient(conn)
}

func (d *Daemon) serveDaemon() {
	log.Info().Stringer("addr", d.Daemon.Addr()).Msg("serving daemon")
	srv := grpc.NewServer()
	daemonpb.RegisterDaemonServer(srv, d.Server)
	d.exit <- srv.Serve(d.Daemon)
}

func (d *Daemon) serveRuntime() {
	log.Info().Stringer("addr", d.Runtime.Addr()).Msg("serving runtime")
	srv := runtime.NewServer(d.RunMgr, d.Trace, d.Remote)
	d.exit <- http.Serve(d.Runtime, srv)
}

func (d *Daemon) serveDBProxy() {
	log.Info().Stringer("addr", d.DBProxy.Addr()).Msg("serving dbproxy")
	d.exit <- d.ClusterMgr.ServeProxy(d.DBProxy)
}

func (d *Daemon) serveDash() {
	log.Info().Stringer("addr", d.Dash.Addr()).Msg("serving dash")
	srv := dash.NewServer(d.RunMgr, d.Trace)
	d.exit <- http.Serve(d.Dash, srv)
}

// listenTCP listens for TCP connections on a random port on localhost.
func (d *Daemon) listenTCP() *net.TCPListener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal(err)
	}
	d.closeOnExit(ln)
	return ln.(*net.TCPListener)
}

func tcpPort(ln net.Listener) int {
	return ln.Addr().(*net.TCPAddr).Port
}

// detectSocketClose polls for the unix socket at socketPath to be removed
// or changed to a different underlying inode.
func detectSocketClose(ln *net.UnixListener, socketPath string) error {
	orig, err := xos.SocketStat(socketPath)
	if err != nil {
		return err
	}

	// When this function exits, the socket has been changed.
	// In that case, don't unlink the socket since it has already been changed.
	defer ln.SetUnlinkOnClose(false)

	// Sleep until the socket changes
	errs := 0
	for {
		time.Sleep(200 * time.Millisecond)
		fi, err := xos.SocketStat(socketPath)
		if os.IsNotExist(err) {
			// Socket was removed; don't remove it again
			return nil
		} else if err != nil {
			errs++
			if errs == 3 {
				return err
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if !xos.SameSocket(orig, fi) {
			return nil
		}
	}
}

func (d *Daemon) closeOnExit(c io.Closer) {
	d.close = append(d.close, c)
}

func (d *Daemon) closeAll() {
	for _, c := range d.close {
		c.Close()
	}
}

type bailout struct {
	err error
}

func fatal(err error) {
	panic(bailout{err})
}

func fatalf(format string, args ...interface{}) {
	panic(bailout{fmt.Errorf(format, args...)})
}

func handleBailout(err *error) {
	if e := recover(); e != nil {
		if b, ok := e.(bailout); ok {
			*err = b.err
		} else {
			panic(e)
		}
	}
}