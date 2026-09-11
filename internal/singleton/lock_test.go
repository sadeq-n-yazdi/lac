package singleton_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"sadeq.uk/lac/internal/singleton"
)

// The whole point: a second daemon against the same state is refused.
func TestASecondLockIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lacd.lock")

	first, err := singleton.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	_, err = singleton.Acquire(path)
	if !errors.Is(err, singleton.ErrAlreadyRunning) {
		t.Fatalf("a second Acquire() = %v, want ErrAlreadyRunning", err)
	}

	// The message has to be actionable: it names who holds it and what to do about it.
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("the error does not name the holder: %v", err)
	}
	if !strings.Contains(err.Error(), "--database") {
		t.Errorf("the error does not say how to run a second daemon deliberately: %v", err)
	}
}

// Separate state directories are separate daemons, which is how the tests — and anybody running a
// throwaway instance — get to have more than one.
func TestSeparateStateDirectoriesDoNotContend(t *testing.T) {
	first, err := singleton.Acquire(filepath.Join(t.TempDir(), "lacd.lock"))
	if err != nil {
		t.Fatalf("the first Acquire() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	second, err := singleton.Acquire(filepath.Join(t.TempDir(), "lacd.lock"))
	if err != nil {
		t.Fatalf("a second state directory = %v, want nil", err)
	}
	t.Cleanup(func() { _ = second.Release() })
}

// Releasing must hand the lock straight on, so a restart does not have to wait for anything.
func TestReleaseFreesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lacd.lock")

	first, err := singleton.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}

	second, err := singleton.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() after Release() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = second.Release() })

	// Releasing twice is what a deferred cleanup does after an explicit close.
	if err := first.Release(); err != nil {
		t.Errorf("a second Release() = %v, want nil", err)
	}
}

// A daemon that was killed leaves no stale lock to clear up: the kernel drops it with the process.
// This is the property that makes a lock file better than a process-id file.
func TestAKilledHolderLeavesNoStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lacd.lock")

	// A child process takes the lock and waits to be killed.
	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestHelperHoldsTheLock")
	helper.Env = append(os.Environ(), "LAC_LOCK_HELPER="+path)

	output, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("attaching to the helper: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}

	// Wait for it to say it has the lock. Read a whole line and check what it says: the helper
	// reports a failure on the same stream, and treating any output as readiness turns "the helper
	// could not take the lock" into a baffling assertion about this process instead.
	lines := bufio.NewScanner(output)
	if !lines.Scan() {
		t.Fatalf("waiting for the helper: %v", lines.Err())
	}
	if ready := strings.TrimSpace(lines.Text()); ready != "held" {
		t.Fatalf("the helper said %q, want \"held\"", ready)
	}

	if _, err := singleton.Acquire(path); !errors.Is(err, singleton.ErrAlreadyRunning) {
		t.Fatalf("Acquire() while the helper holds it = %v, want ErrAlreadyRunning", err)
	}

	// Kill it outright: no chance to release anything, exactly like a crash.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("killing the helper: %v", err)
	}
	_ = helper.Wait()

	taken, err := singleton.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() after the holder was killed = %v, want nil", err)
	}
	_ = taken.Release()
}

// TestHelperHoldsTheLock is not a test: it is the child process the test above kills. It does
// nothing unless the environment says to.
func TestHelperHoldsTheLock(t *testing.T) {
	path := os.Getenv("LAC_LOCK_HELPER")
	if path == "" {
		t.Skip("not the helper")
	}

	lock, err := singleton.Acquire(path)
	if err != nil {
		t.Fatalf("the helper could not take the lock: %v", err)
	}
	defer func() { _ = lock.Release() }()

	if _, err := os.Stdout.WriteString("held\n"); err != nil {
		t.Fatalf("the helper could not report: %v", err)
	}

	// Wait to be killed. The parent does not let this run long.
	select {}
}

func TestARelativePathIsRefused(t *testing.T) {
	if _, err := singleton.Acquire("lacd.lock"); err == nil {
		t.Error("Acquire() accepted a relative path")
	}
}

// The lock file is the daemon's, and it says who holds it.
func TestTheLockFileRecordsTheHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lacd.lock")

	lock, err := singleton.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = lock.Release() })

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the lock file: %v", err)
	}
	if strings.TrimSpace(string(contents)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("the lock file says %q, want this process's id", strings.TrimSpace(string(contents)))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Errorf("the lock file is mode %#o, want 0600", permissions)
	}
}
