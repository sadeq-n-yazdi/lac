//go:build !darwin && !linux

package unixsock

import "errors"

// credentialsOf refuses to guess on a platform whose peer-credential interface LAC has not been
// taught. Returning an error closes the connection, which is the safe direction: better to refuse
// a legitimate local agent than to accept an unverified one.
func credentialsOf(uintptr) (Peer, error) {
	return Peer{}, errors.New("peer credentials are not supported on this platform; " +
		"LAC refuses connections it cannot verify")
}
