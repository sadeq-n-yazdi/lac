package unixsock

import (
	"fmt"
	"net"
)

// Peer identifies the process on the other end of a Unix socket, as reported by the kernel. The
// values cannot be forged by the connecting process, which is what makes them worth checking.
type Peer struct {
	// UID is the peer's effective user id.
	UID uint32
	// ProcessID is the peer's process id, or zero on systems that do not report it.
	ProcessID int
}

// peerOf reads the credentials of the process at the other end of the connection.
func peerOf(connection *net.UnixConn) (Peer, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return Peer{}, fmt.Errorf("reaching the connection's file descriptor: %w", err)
	}

	var (
		peer      Peer
		lookupErr error
	)

	if err := raw.Control(func(descriptor uintptr) {
		peer, lookupErr = credentialsOf(descriptor)
	}); err != nil {
		return Peer{}, fmt.Errorf("reading peer credentials: %w", err)
	}
	if lookupErr != nil {
		return Peer{}, lookupErr
	}

	return peer, nil
}
