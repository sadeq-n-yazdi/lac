//go:build linux

package unixsock

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// credentialsOf reads the peer's credentials from the kernel.
//
// Linux reports them through SO_PEERCRED, which carries the process id as well as the user.
func credentialsOf(descriptor uintptr) (Peer, error) {
	credentials, err := unix.GetsockoptUcred(int(descriptor), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return Peer{}, fmt.Errorf("reading SO_PEERCRED: %w", err)
	}

	return Peer{UID: credentials.Uid, ProcessID: int(credentials.Pid)}, nil
}
