// Package unixsock listens on a Unix domain socket that only the daemon's own user can reach.
//
// Two things keep other users out. The socket lives in a directory only the owner may enter, and
// every accepted connection's peer credentials are read from the kernel and checked against the
// daemon's own user id. The second check is what makes the guarantee independent of however the
// filesystem happens to be configured.
package unixsock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// directoryMode and socketMode keep the socket private to its owner.
const (
	directoryMode os.FileMode = 0o700
	socketMode    os.FileMode = 0o600
)

// probeTimeout is how long we wait when checking whether a socket already has a live daemon behind
// it. A local connect either succeeds immediately or fails immediately.
const probeTimeout = 500 * time.Millisecond

// ErrDaemonRunning means a live daemon is already listening on that socket.
var ErrDaemonRunning = errors.New("a daemon is already listening on this socket")

// Options configure a Listener.
type Options struct {
	// Path is the socket to create. Its parent directory is created if necessary.
	Path string
	// Logger records rejected connections. Defaults to slog.Default.
	Logger *slog.Logger
}

// Listener accepts local connections, rejecting any peer that is not the daemon's own user.
type Listener struct {
	inner  *net.UnixListener
	path   string
	ownUID uint32
	logger *slog.Logger
}

// Listen creates the socket and starts accepting.
//
// A socket left behind by a crashed daemon is replaced; one with a live daemon behind it is
// reported as ErrDaemonRunning rather than silently stolen.
func Listen(ctx context.Context, options Options) (*Listener, error) {
	if !filepath.IsAbs(options.Path) {
		return nil, fmt.Errorf("the socket path %q must be absolute", options.Path)
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := prepareDirectory(filepath.Dir(options.Path)); err != nil {
		return nil, err
	}
	if err := clearStaleSocket(ctx, options.Path); err != nil {
		return nil, err
	}

	address, err := net.ResolveUnixAddr("unix", options.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", options.Path, err)
	}

	inner, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", options.Path, err)
	}

	// The socket is only reachable through a directory nobody else may enter, so there is no
	// window here another user could exploit; this makes the intent explicit anyway.
	if err := os.Chmod(options.Path, socketMode); err != nil {
		_ = inner.Close()
		return nil, fmt.Errorf("tightening permissions on %s: %w", options.Path, err)
	}

	// Go removes the socket file on Close; we do it ourselves so the behaviour is explicit and
	// survives a listener that is closed indirectly.
	inner.SetUnlinkOnClose(false)

	return &Listener{inner: inner, path: options.Path, ownUID: uint32(os.Getuid()), logger: logger}, nil //nolint:gosec // a uid always fits
}

// Accept returns the next connection from the daemon's own user. Connections from anywhere else
// are closed and audited without ever reaching the caller.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		connection, err := l.inner.AcceptUnix()
		if err != nil {
			return nil, err //nolint:wrapcheck // callers compare with net.ErrClosed
		}

		peer, err := peerOf(connection)
		if err != nil {
			l.logger.Warn("closing a connection whose peer could not be identified", "error", err)
			_ = connection.Close()
			continue
		}

		if peer.UID != l.ownUID {
			l.logger.Warn("rejected a connection from another user",
				"peer_uid", peer.UID, "peer_pid", peer.ProcessID, "own_uid", l.ownUID)
			_ = connection.Close()
			continue
		}

		return connection, nil
	}
}

// Addr returns the listener's address, completing the net.Listener contract.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Path returns the socket path.
func (l *Listener) Path() string { return l.path }

// Close stops accepting and removes the socket file, so the next start does not have to decide
// whether it is stale.
func (l *Listener) Close() error {
	err := l.inner.Close()

	if removeErr := os.Remove(l.path); removeErr != nil && !os.IsNotExist(removeErr) {
		err = errors.Join(err, fmt.Errorf("removing %s: %w", l.path, removeErr))
	}
	if err != nil {
		return fmt.Errorf("closing the listener: %w", err)
	}

	return nil
}

// prepareDirectory creates the socket's directory if needed and makes sure no other user can enter
// it. This is the outer half of the access control.
func prepareDirectory(directory string) error {
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return fmt.Errorf("creating %s: %w", directory, err)
	}
	if err := os.Chmod(directory, directoryMode); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", directory, err)
	}

	return nil
}

// clearStaleSocket removes a socket file left behind by a crashed daemon, and refuses to touch one
// that still has a daemon behind it.
func clearStaleSocket(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to remove it", path)
	}

	dialer := net.Dialer{Timeout: probeTimeout}

	connection, err := dialer.DialContext(ctx, "unix", path)
	if err == nil {
		_ = connection.Close()
		return fmt.Errorf("%w: %s", ErrDaemonRunning, path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing the stale socket %s: %w", path, err)
	}

	return nil
}
