//go:build darwin

package unixsock

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// credentialsOf reads the peer's credentials from the kernel.
//
// macOS reports them through LOCAL_PEERCRED as an xucred, which carries the user and group but no
// process id, so Peer.ProcessID is left zero here.
func credentialsOf(descriptor uintptr) (Peer, error) {
	credentials, err := unix.GetsockoptXucred(int(descriptor), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return Peer{}, fmt.Errorf("reading LOCAL_PEERCRED: %w", err)
	}

	return Peer{UID: credentials.Uid}, nil
}
