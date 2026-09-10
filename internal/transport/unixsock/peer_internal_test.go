package unixsock

import (
	"net"
	"os"
	"testing"
)

// The whole guarantee is that peer identity comes from the kernel rather than from anything the
// connecting process says. This checks that the lookup really reports the connecting process's
// user, which is what Accept then compares against.
func TestPeerOfReportsTheConnectingUser(t *testing.T) {
	left, right := unixConnectionPair(t)

	peer, err := peerOf(left)
	if err != nil {
		t.Fatalf("peerOf() = %v, want nil", err)
	}

	if want := uint32(os.Getuid()); peer.UID != want {
		t.Errorf("peer UID = %d, want this process's own uid %d", peer.UID, want)
	}

	_ = right.Close()
}

func unixConnectionPair(t *testing.T) (left, right *net.UnixConn) {
	t.Helper()

	// A short path: a socket address must fit in about a hundred bytes, and t.TempDir() on macOS
	// spends most of that budget before the file name.
	directory, err := os.MkdirTemp("", "lac")
	if err != nil {
		t.Fatalf("creating a temporary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	address, err := net.ResolveUnixAddr("unix", directory+"/pair.sock")
	if err != nil {
		t.Fatalf("resolving the address: %v", err)
	}

	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan *net.UnixConn, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- connection
	}()

	client, err := net.DialUnix("unix", nil, address)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	server := <-accepted
	if server == nil {
		t.Fatal("the listener did not accept the connection")
	}
	t.Cleanup(func() { _ = server.Close() })

	return server, client
}
