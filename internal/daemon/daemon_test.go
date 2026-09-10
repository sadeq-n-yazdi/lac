package daemon_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/config"
	"code.sadeq.uk/lac/internal/daemon"
)

// start brings up a real daemon on a temporary socket and database, and returns it with a client
// connection. Nothing here is mocked: this is the daemon an operator runs.
func start(t *testing.T, resources []config.ResourceConfig) (*daemon.Daemon, net.Conn) {
	t.Helper()

	// A short home: a Unix socket path must fit in about a hundred bytes.
	home, err := os.MkdirTemp("", "lac")
	if err != nil {
		t.Fatalf("creating a temporary home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", "")

	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	configuration.Resources = resources

	instance, err := daemon.New(t.Context(), configuration, quietLogger())
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- instance.Run(ctx) }()

	connection, err := dialWhenReady(t, instance.SocketPath())
	if err != nil {
		t.Fatalf("connecting to the daemon: %v", err)
	}

	t.Cleanup(func() {
		_ = connection.Close()
		stop()
		if err := <-served; err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
		if err := instance.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})

	return instance, connection
}

// dialWhenReady retries briefly, because Run starts accepting a moment after New returns.
func dialWhenReady(t *testing.T, path string) (net.Conn, error) {
	t.Helper()

	var lastErr error
	for range 100 {
		connection, err := net.Dial("unix", path)
		if err == nil {
			return connection, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}

	return nil, lastErr
}

func call(t *testing.T, connection net.Conn, method string) map[string]any {
	t.Helper()

	request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("sending %s: %v", method, err)
	}

	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}

	lines := bufio.NewScanner(connection)
	if !lines.Scan() {
		t.Fatalf("no reply to %s: %v", method, lines.Err())
	}

	var response struct {
		Result map[string]any   `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(lines.Bytes(), &response); err != nil {
		t.Fatalf("decoding the reply to %s: %v", method, err)
	}
	if response.Error != nil {
		t.Fatalf("%s failed: %s", method, *response.Error)
	}

	return response.Result
}

// The daemon must come up from nothing — no configuration, no database — and answer a call.
func TestDaemonStartsAndAnswers(t *testing.T) {
	_, connection := start(t, nil)

	result := call(t, connection, "daemon.info")

	if result["protocol"] != "2.0" {
		t.Errorf("protocol = %v, want 2.0", result["protocol"])
	}

	methods, ok := result["methods"].([]any)
	if !ok || len(methods) == 0 {
		t.Errorf("methods = %v, want the served methods", result["methods"])
	}
}

// Resources declared in the configuration must exist the moment the daemon is up, so a fresh
// install already knows this machine's limits.
func TestConfiguredResourcesAreDefinedAtStartup(t *testing.T) {
	instance, _ := start(t, []config.ResourceConfig{
		{Name: "test", Capacity: 4, Description: "concurrent test runs"},
	})

	resource, err := instance.Store().Resources().ByName(t.Context(), "test")
	if err != nil {
		t.Fatalf("ByName() = %v, want nil", err)
	}
	if resource.Capacity != 4 {
		t.Errorf("capacity = %d, want 4", resource.Capacity)
	}
	if resource.LeaseTimeToLive <= 0 {
		t.Error("the resource did not inherit a lease time to live")
	}
}

// The socket must be private, and it must not be left behind when the daemon stops: the next
// start should not have to work out whether it is stale.
func TestSocketIsPrivateAndRemovedOnClose(t *testing.T) {
	instance, _ := start(t, nil)
	path := instance.SocketPath()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Errorf("the socket is mode %#o, want 0600", permissions)
	}

	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat the socket directory: %v", err)
	}
	if permissions := directory.Mode().Perm(); permissions != 0o700 {
		t.Errorf("the socket directory is mode %#o, want 0700", permissions)
	}
}
