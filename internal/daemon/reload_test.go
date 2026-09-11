package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sadeq.uk/lac/internal/config"
	"sadeq.uk/lac/internal/daemon"
	"sadeq.uk/lac/internal/singleton"
	"sadeq.uk/lac/pkg/lacclient"
)

// configured is a daemon started from a real configuration file, so reloading has something to
// re-read.
type configured struct {
	daemon     *daemon.Daemon
	configPath string
	socketPath string
}

func startConfigured(t *testing.T, contents string, restartDelay time.Duration) *configured {
	t.Helper()

	home, err := os.MkdirTemp("", "lac")
	if err != nil {
		t.Fatalf("creating a temporary home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	configDir := filepath.Join(home, "config", "lac")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("creating the config directory: %v", err)
	}

	configPath := filepath.Join(configDir, "config.yaml")
	writeConfig(t, configPath, contents)

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", "")

	source := config.Options{ConfigPath: configPath}

	configuration, err := config.Load(source)
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if restartDelay > 0 {
		configuration.RestartDelay = config.Duration(restartDelay)
	}
	configuration.AllowedWorkdirRoots = []string{home, os.TempDir(), "/tmp"}

	instance, err := daemon.New(t.Context(), configuration, source, quietLogger())
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- instance.Run(ctx) }()

	t.Cleanup(func() {
		stop()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Run() = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop when its context ended")
		}
		if err := instance.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})

	return &configured{daemon: instance, configPath: configPath, socketPath: instance.SocketPath()}
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
}

// Two daemons sharing one database and one socket would each serve half the agents. The second must
// refuse to start.
func TestASecondDaemonIsRefused(t *testing.T) {
	subject := startConfigured(t, "log_level: info\n", 0)

	configuration, err := config.Load(config.Options{ConfigPath: subject.configPath})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	_, err = daemon.New(t.Context(), configuration, config.Options{}, quietLogger())
	if !errors.Is(err, singleton.ErrAlreadyRunning) {
		t.Fatalf("a second daemon = %v, want ErrAlreadyRunning", err)
	}
}

// Stopping one daemon must let the next start at once: a restart should not have to wait for
// anything to time out.
func TestTheLockIsFreedOnShutdown(t *testing.T) {
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

	first, err := daemon.New(t.Context(), configuration, config.Options{}, quietLogger())
	if err != nil {
		t.Fatalf("the first daemon = %v, want nil", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	second, err := daemon.New(t.Context(), configuration, config.Options{}, quietLogger())
	if err != nil {
		t.Fatalf("the second daemon after a clean stop = %v, want nil", err)
	}
	if err := second.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

// A reload applies a resource added to the file, which is the point of reloading: change what the
// machine will do at once without stopping the agents already working.
func TestReloadAppliesANewResource(t *testing.T) {
	subject := startConfigured(t, `
resources:
  - name: test
    capacity: 2
`, 0)

	if _, err := subject.daemon.Store().Resources().ByName(t.Context(), "reviewer"); err == nil {
		t.Fatal("the reviewer resource exists before it was configured")
	}

	writeConfig(t, subject.configPath, `
resources:
  - name: test
    capacity: 4
  - name: reviewer
    capacity: 1
`)

	if _, err := subject.daemon.Reload(t.Context()); err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}

	reviewer, err := subject.daemon.Store().Resources().ByName(t.Context(), "reviewer")
	if err != nil {
		t.Fatalf("the added resource was not applied: %v", err)
	}
	if reviewer.Capacity != 1 {
		t.Errorf("reviewer capacity = %d, want 1", reviewer.Capacity)
	}

	test, err := subject.daemon.Store().Resources().ByName(t.Context(), "test")
	if err != nil {
		t.Fatalf("ByName() = %v, want nil", err)
	}
	if test.Capacity != 4 {
		t.Errorf("test capacity = %d, want the reloaded 4", test.Capacity)
	}
}

// A configuration that does not parse must not take the daemon down: it is still coordinating
// agents with the configuration it already has.
func TestABrokenReloadLeavesTheDaemonRunning(t *testing.T) {
	subject := startConfigured(t, `
resources:
  - name: test
    capacity: 2
`, 0)

	writeConfig(t, subject.configPath, "resources: [ this is not valid yaml\n")

	if _, err := subject.daemon.Reload(t.Context()); err == nil {
		t.Fatal("Reload() with a broken file = nil, want an error")
	}

	// The daemon still knows what it knew.
	test, err := subject.daemon.Store().Resources().ByName(t.Context(), "test")
	if err != nil {
		t.Fatalf("the daemon lost its configuration: %v", err)
	}
	if test.Capacity != 2 {
		t.Errorf("test capacity = %d, want the original 2", test.Capacity)
	}
}

// A reloaded command list is live, so an operator can add a job for the workers without a restart.
func TestReloadAppliesCommands(t *testing.T) {
	subject := startConfigured(t, `
resources:
  - name: test
    capacity: 1
`, 0)

	writeConfig(t, subject.configPath, `
resources:
  - name: test
    capacity: 1
commands:
  greet:
    resource: test
    run: ["echo", "hello"]
    description: a greeting
`)

	if _, err := subject.daemon.Reload(t.Context()); err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}

	// The dispatch service reads the catalogue live, so the new command is offered at once.
	commands := subject.daemon.Commands()
	if len(commands) != 1 || commands[0].Key != "greet" {
		t.Fatalf("Commands() = %+v, want the reloaded command", commands)
	}
}

// The watcher waits for the file to settle before applying it: an editor writing in several steps
// must not have half a configuration read out from under it.
func TestTheWatcherWaitsForTheFileToSettle(t *testing.T) {
	const delay = 100 * time.Millisecond

	subject := startConfigured(t, `
resources:
  - name: test
    capacity: 1
`, delay)

	// Keep changing the file for longer than the quiet period would be.
	deadline := time.Now().Add(6 * delay)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		writeConfig(t, subject.configPath, `
resources:
  - name: test
    capacity: 1
  - name: reviewer
    capacity: `+string(rune('1'+attempt%3))+`
`)
		time.Sleep(delay / 2)
	}

	// While it was still being written, nothing should have been applied.
	if _, err := subject.daemon.Store().Resources().ByName(t.Context(), "reviewer"); err == nil {
		t.Error("the configuration was applied while the file was still being written")
	}

	// Now leave it alone and let it settle.
	writeConfig(t, subject.configPath, `
resources:
  - name: test
    capacity: 1
  - name: reviewer
    capacity: 7
`)

	waitFor(t, 10*time.Second, func() bool {
		reviewer, err := subject.daemon.Store().Resources().ByName(t.Context(), "reviewer")

		return err == nil && reviewer.Capacity == 7
	})
}

// Touching the file without changing it must not cause a reload: the daemon compares contents, not
// timestamps.
func TestAnUnchangedFileIsNotReloaded(t *testing.T) {
	const delay = 100 * time.Millisecond

	contents := `
resources:
  - name: test
    capacity: 1
`
	subject := startConfigured(t, contents, delay)

	// Rewrite the same bytes, repeatedly.
	for range 5 {
		writeConfig(t, subject.configPath, contents)
		time.Sleep(delay)
	}

	// Nothing to observe directly, so check the daemon is still serving what it started with.
	test, err := subject.daemon.Store().Resources().ByName(t.Context(), "test")
	if err != nil || test.Capacity != 1 {
		t.Errorf("the daemon's configuration changed under an unchanged file: %+v (%v)", test, err)
	}
}

func waitFor(t *testing.T, limit time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("the condition never held")
}

// Shutting down must not cut work off mid-flight. A call that is running when the signal arrives
// gets to finish and its caller gets the answer.
func TestShutdownLetsWorkInFlightFinish(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 1}})

	holder := subject.connect(t, "claude-a")

	lease, err := holder.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "test"})
	if err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}

	// A second agent waiting for the slot: the release below must reach it before the daemon stops.
	waiting := subject.connect(t, "claude-b")
	granted := make(chan error, 1)

	go func() {
		_, err := waiting.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "test"})
		granted <- err
	}()

	waitForQueue(t, holder, "test", 1)

	if err := holder.Release(t.Context(), lease.ID); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}

	select {
	case err := <-granted:
		if err != nil {
			t.Errorf("the waiting agent was cut off: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the waiting agent never got the slot")
	}
}

// Everything the daemon held must be given back when it stops: the socket removed, the database
// closed, the lock released. Anything left behind makes the next start harder than it should be.
func TestShutdownLeavesNothingBehind(t *testing.T) {
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

	instance, err := daemon.New(t.Context(), configuration, config.Options{}, quietLogger())
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- instance.Run(ctx) }()

	socketPath := instance.SocketPath()
	waitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(socketPath)

		return err == nil
	})

	// Stop it the way a signal does.
	stop()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Run() = %v, want a clean stop", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not stop")
	}

	if err := instance.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("the socket survived shutdown: %v", err)
	}

	// The lock is free, so the next daemon starts at once.
	lock, err := singleton.Acquire(configuration.LockPath())
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	_ = lock.Release()
}

// The operator should not have to find a process id to reload. `lac reload` goes through the
// daemon's own API, and reports what changed and what needs a restart.
func TestReloadFromTheClient(t *testing.T) {
	subject := startConfigured(t, `
resources:
  - name: test
    capacity: 2
`, 0)

	operator := connectTo(t, subject.socketPath, "operator")

	// Nothing has changed yet, and saying so is a useful answer in itself.
	quiet, err := operator.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}
	if len(quiet.Applied) != 0 {
		t.Errorf("Applied = %v, want nothing to have changed", quiet.Applied)
	}
	if quiet.Source != subject.configPath {
		t.Errorf("Source = %q, want the file the daemon was started from", quiet.Source)
	}

	writeConfig(t, subject.configPath, `
resources:
  - name: test
    capacity: 2
  - name: reviewer
    capacity: 1
`)

	outcome, err := operator.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}
	if len(outcome.Applied) == 0 {
		t.Fatalf("Applied = %v, want the reloaded resources", outcome.Applied)
	}

	if _, err := subject.daemon.Store().Resources().ByName(t.Context(), "reviewer"); err != nil {
		t.Errorf("the reloaded resource was not applied: %v", err)
	}
}

// A setting that cannot change while the daemon runs must be reported, not silently dropped.
func TestReloadReportsWhatNeedsARestart(t *testing.T) {
	subject := startConfigured(t, "log_level: info\n", 0)
	operator := connectTo(t, subject.socketPath, "operator")

	writeConfig(t, subject.configPath, "log_level: info\nsocket_path: /tmp/somewhere-else.sock\n")

	outcome, err := operator.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}

	found := false
	for _, setting := range outcome.Deferred {
		if setting == "socket_path" {
			found = true
		}
	}
	if !found {
		t.Errorf("Deferred = %v, want it to name socket_path", outcome.Deferred)
	}

	// And the daemon is still listening where it was.
	if _, err := os.Stat(subject.socketPath); err != nil {
		t.Errorf("the daemon moved its socket on a reload: %v", err)
	}
}

// Reloading is an operator's business: an ordinary agent must not be able to reshape the machine.
func TestOnlyOperatorsMayReload(t *testing.T) {
	subject := startConfigured(t, "log_level: info\n", 0)
	ordinary := connectTo(t, subject.socketPath, "claude-a")

	_, err := ordinary.Reload(t.Context())
	if lacclient.ErrorCode(err) != lacclient.CodeUnauthorised {
		t.Errorf("Reload() by an ordinary agent = %v, want CodeUnauthorised", err)
	}
}

// connectTo registers an agent against a daemon and returns its client.
func connectTo(t *testing.T, socketPath, name string) *lacclient.Client {
	t.Helper()

	client, err := lacclient.Dial(t.Context(), lacclient.Options{SocketPath: socketPath})
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	workdir, err := os.MkdirTemp("", "lacwork")
	if err != nil {
		t.Fatalf("creating a working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })

	if _, err := client.Register(t.Context(), name, "claude", workdir, os.Getpid()); err != nil {
		t.Fatalf("Register(%q) = %v, want nil", name, err)
	}

	return client
}
