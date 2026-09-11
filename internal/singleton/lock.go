// Package singleton stops a second daemon running against the same state.
//
// Two daemons sharing one database and one socket would each serve half the agents and disagree
// about who holds what — the precise failure LAC exists to prevent, produced by LAC itself.
//
// The guarantee comes from an advisory lock held on a file for the life of the process. The kernel
// releases it when the process ends, however it ends, so there is no stale lock to clear up after a
// crash and no process-id file to go out of date.
package singleton

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// fileMode keeps the lock file readable only by its owner, like everything else in the state
// directory.
const fileMode os.FileMode = 0o600

// ErrAlreadyRunning means another daemon holds the lock.
var ErrAlreadyRunning = errors.New("another lac daemon is already running")

// Lock is an exclusive claim on a state directory, held until it is released.
type Lock struct {
	file *os.File
	path string
}

// Acquire takes the lock, or reports ErrAlreadyRunning with the holder's process id.
//
// The lock is advisory, which is all that is needed: every party that might contend for it is
// another copy of this daemon, and they all take it.
func Acquire(path string) (*Lock, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the lock path %q must be absolute", path)
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode) //nolint:gosec // the daemon's own state directory
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		holder := holderOf(file)
		_ = file.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			if holder > 0 {
				return nil, fmt.Errorf("%w (pid %d). Stop it first, or point this one at a "+
					"different state directory with --database and --socket", ErrAlreadyRunning, holder)
			}

			return nil, fmt.Errorf("%w. Stop it first, or point this one at a different state "+
				"directory with --database and --socket", ErrAlreadyRunning)
		}

		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	lock := &Lock{file: file, path: path}

	// The process id is written for a person reading the error above; the lock itself is what
	// enforces anything, so a failure to record it is not worth refusing to start over.
	if err := lock.recordProcess(); err != nil {
		return nil, errors.Join(err, lock.Release())
	}

	return lock, nil
}

// Path is the file the lock is held on.
func (l *Lock) Path() string { return l.path }

// Release gives up the lock. The file is left behind: removing it would let a second daemon create
// and lock a new one while this process still holds the old, which is exactly what the lock is for.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}

	// Closing the descriptor releases the lock; doing it explicitly first makes the intent plain.
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
		_ = l.file.Close()
		return fmt.Errorf("unlocking %s: %w", l.path, err)
	}

	if err := l.file.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", l.path, err)
	}
	l.file = nil

	return nil
}

func (l *Lock) recordProcess() error {
	if err := l.file.Truncate(0); err != nil {
		return fmt.Errorf("truncating %s: %w", l.path, err)
	}
	if _, err := l.file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		return fmt.Errorf("writing %s: %w", l.path, err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("flushing %s: %w", l.path, err)
	}

	return nil
}

// holderOf reads the process id the current holder recorded, for the error message. Anything
// unreadable simply means no id is quoted.
func holderOf(file *os.File) int {
	contents := make([]byte, 32)

	read, err := file.ReadAt(contents, 0)
	if read == 0 && err != nil {
		return 0
	}

	holder, err := strconv.Atoi(strings.TrimSpace(string(contents[:read])))
	if err != nil {
		return 0
	}

	return holder
}
