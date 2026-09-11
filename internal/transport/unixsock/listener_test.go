package unixsock_test

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sadeq.uk/lac/internal/transport/unixsock"
)

// socketPath returns a short path, because a Unix socket path must fit in about a hundred bytes
// and the default temporary directory on macOS is long enough to matter.
func socketPath(t *testing.T) string {
	t.Helper()

	directory, err := os.MkdirTemp("", "lac")
	if err != nil {
		t.Fatalf("creating a temporary directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("removing %s: %v", directory, err)
		}
	})

	return filepath.Join(directory, "run", "lacd.sock")
}

func listen(t *testing.T, path string) *unixsock.Listener {
	t.Helper()

	listener, err := unixsock.Listen(t.Context(), unixsock.Options{Path: path})
	if err != nil {
		t.Fatalf("Listen(%q) = %v, want nil", path, err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Close() = %v", err)
		}
	})

	return listener
}

// The daemon's own user must be able to connect and exchange bytes; everything else in LAC rests
// on this working.
func TestAcceptsTheOwnersConnection(t *testing.T) {
	listener := listen(t, socketPath(t))

	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer func() { _ = connection.Close() }()

		_, err = connection.Write([]byte("hello"))
		accepted <- err
	}()

	client, err := net.Dial("unix", listener.Path())
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	defer func() { _ = client.Close() }()

	greeting := make([]byte, 5)
	if _, err := io.ReadFull(client, greeting); err != nil {
		t.Fatalf("reading the greeting: %v", err)
	}
	if string(greeting) != "hello" {
		t.Errorf("read %q, want %q", greeting, "hello")
	}

	if err := <-accepted; err != nil {
		t.Errorf("the server side failed: %v", err)
	}
}

// The socket and its directory are the first half of the access control, so their modes are part
// of the contract rather than an implementation detail.
func TestPermissionsAreOwnerOnly(t *testing.T) {
	listener := listen(t, socketPath(t))

	socket, err := os.Stat(listener.Path())
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if permissions := socket.Mode().Perm(); permissions != 0o600 {
		t.Errorf("the socket is mode %#o, want 0600", permissions)
	}

	directory, err := os.Stat(filepath.Dir(listener.Path()))
	if err != nil {
		t.Fatalf("stat the directory: %v", err)
	}
	if permissions := directory.Mode().Perm(); permissions != 0o700 {
		t.Errorf("the socket directory is mode %#o, want 0700", permissions)
	}
}

// A crashed daemon leaves its socket file behind. Refusing to start until somebody deletes it by
// hand would be useless, so it is replaced.
func TestReplacesAStaleSocket(t *testing.T) {
	path := socketPath(t)

	first := listen(t, path)
	// Simulate a crash: the process is gone but the file remains.
	if err := first.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("creating a leftover file: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the leftover file: %v", err)
	}

	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("creating a stale socket: %v", err)
	}
	if unixListener, ok := stale.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := stale.Close(); err != nil {
		t.Fatalf("closing the stale socket: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket file is not there to begin with: %v", err)
	}

	second := listen(t, path)
	if _, err := net.Dial("unix", second.Path()); err != nil {
		t.Errorf("the replacement listener is not reachable: %v", err)
	}
}

// Two daemons on one socket would each serve half the agents. The second must refuse to start.
func TestRefusesToStealALiveSocket(t *testing.T) {
	listener := listen(t, socketPath(t))

	_, err := unixsock.Listen(t.Context(), unixsock.Options{Path: listener.Path()})
	if !errors.Is(err, unixsock.ErrDaemonRunning) {
		t.Errorf("a second Listen() = %v, want ErrDaemonRunning", err)
	}
}

// Removing a regular file that happens to sit at the socket path would destroy somebody's data.
func TestRefusesToRemoveANonSocket(t *testing.T) {
	path := socketPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	if _, err := unixsock.Listen(t.Context(), unixsock.Options{Path: path}); err == nil {
		t.Fatal("Listen() over a regular file = nil, want an error")
	}

	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("the regular file was destroyed: %v", err)
	}
}

func TestRejectsRelativePaths(t *testing.T) {
	if _, err := unixsock.Listen(t.Context(), unixsock.Options{Path: "lacd.sock"}); err == nil {
		t.Error("Listen() with a relative path = nil, want an error")
	}
}

// Close must leave nothing behind, so the next start does not have to decide whether a leftover
// socket is stale.
func TestCloseRemovesTheSocket(t *testing.T) {
	path := socketPath(t)
	listener := listen(t, path)

	if err := listener.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket file survived Close(): %v", err)
	}
}

// Accept must return promptly once the listener is closed, so the daemon can shut down.
func TestAcceptUnblocksOnClose(t *testing.T) {
	listener := listen(t, socketPath(t))

	failed := make(chan error, 1)
	go func() {
		_, err := listener.Accept()
		failed <- err
	}()

	if err := listener.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	select {
	case err := <-failed:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept() after Close() = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Accept() did not return after Close()")
	}
}
