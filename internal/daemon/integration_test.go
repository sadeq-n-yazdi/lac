package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/config"
	"code.sadeq.uk/lac/internal/daemon"
	"code.sadeq.uk/lac/pkg/lacclient"
)

// live is a running daemon with clients connected to it, exactly as an operator would have.
type live struct {
	daemon     *daemon.Daemon
	socketPath string
}

// startDaemon brings up a real daemon on a temporary socket and database.
func startDaemon(t *testing.T, resources []config.ResourceConfig) *live {
	t.Helper()

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
	// The agents in these tests work in the temporary home rather than the developer's own.
	configuration.AllowedWorkdirRoots = []string{home, os.TempDir(), "/tmp"}

	instance, err := daemon.New(t.Context(), configuration, quietLogger())
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- instance.Run(ctx) }()

	t.Cleanup(func() {
		stop()
		if err := <-served; err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
		if err := instance.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})

	subject := &live{daemon: instance, socketPath: instance.SocketPath()}
	subject.waitUntilReady(t)

	return subject
}

func (l *live) waitUntilReady(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client, err := lacclient.Dial(t.Context(), lacclient.Options{SocketPath: l.socketPath})
		if err == nil {
			_ = client.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("the daemon never started accepting connections")
}

// connect registers an agent and returns its client.
func (l *live) connect(t *testing.T, name string) *lacclient.Client {
	t.Helper()

	client, err := lacclient.Dial(t.Context(), lacclient.Options{SocketPath: l.socketPath})
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

// This is the guarantee the whole project exists to provide, driven end to end through the real
// daemon and the real client: six agents ask for a four-slot resource, and at no moment are more
// than four of them running.
func TestSixAgentsShareFourSlots(t *testing.T) {
	const (
		capacity   = 4
		contenders = 6
	)

	subject := startDaemon(t, []config.ResourceConfig{
		{Name: "test", Capacity: capacity, Description: "concurrent test runs"},
	})

	var (
		running  atomic.Int64
		peak     atomic.Int64
		finished sync.WaitGroup
	)

	start := make(chan struct{})

	for index := range contenders {
		client := subject.connect(t, "claude-"+string(rune('a'+index)))

		finished.Add(1)
		go func() {
			defer finished.Done()
			<-start

			lease, err := client.Acquire(t.Context(), lacclient.AcquireRequest{
				Resource: "test", Reason: "make test",
			})
			if err != nil {
				t.Errorf("Acquire() = %v, want nil", err)
				return
			}

			held := running.Add(1)
			for {
				seen := peak.Load()
				if held <= seen || peak.CompareAndSwap(seen, held) {
					break
				}
			}

			time.Sleep(20 * time.Millisecond) // stand in for a test run
			running.Add(-1)

			if err := client.Release(t.Context(), lease.ID); err != nil {
				t.Errorf("Release() = %v, want nil", err)
			}
		}()
	}

	close(start)
	finished.Wait()

	if observed := peak.Load(); observed > capacity {
		t.Errorf("%d agents ran at once, want at most %d", observed, capacity)
	}
	if peak.Load() == 0 {
		t.Error("no agent ever got a slot")
	}

	status, err := subject.connect(t, "operator").ResourceStatus(t.Context(), "test")
	if err != nil {
		t.Fatalf("ResourceStatus() = %v, want nil", err)
	}
	if status.Held != 0 || status.Waiting != 0 {
		t.Errorf("after everyone finished: %d held, %d waiting, want 0 and 0", status.Held, status.Waiting)
	}
}

// An agent that dies holding a slot must not keep it: its connection going away releases it, so
// the machine recovers without anyone intervening.
func TestACrashedAgentLosesItsSlot(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{{Name: "reviewer", Capacity: 1}})

	crashed := subject.connect(t, "claude-crashed")
	waiting := subject.connect(t, "claude-waiting")

	if _, err := crashed.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "reviewer"}); err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}

	granted := make(chan lacclient.Lease, 1)
	go func() {
		lease, err := waiting.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "reviewer"})
		if err != nil {
			t.Errorf("the waiting agent failed: %v", err)
			return
		}
		granted <- lease
	}()

	waitForQueue(t, waiting, "reviewer", 1)

	// The holder deregisters, which is what a well-behaved agent does on the way out.
	if _, err := crashed.Deregister(t.Context()); err != nil {
		t.Fatalf("Deregister() = %v, want nil", err)
	}

	select {
	case lease := <-granted:
		if lease.Resource != "reviewer" {
			t.Errorf("the granted lease is for %q, want reviewer", lease.Resource)
		}
	case <-time.After(10 * time.Second):
		t.Error("the waiting agent never got the slot the departing one gave back")
	}
}

// Messaging, end to end: one agent tells another something, and the recipient is pushed a
// notification rather than having to poll for it.
func TestAgentsTalkToEachOther(t *testing.T) {
	subject := startDaemon(t, nil)

	notified := make(chan string, 1)
	listener, err := lacclient.Dial(t.Context(), lacclient.Options{
		SocketPath: subject.socketPath,
		OnNotification: func(method string, _ json.RawMessage) {
			select {
			case notified <- method:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	workdir := t.TempDir()
	if _, err := listener.Register(t.Context(), "claude-b", "claude", workdir, os.Getpid()); err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}

	sender := subject.connect(t, "claude-a")

	sent, err := sender.SendTo(t.Context(), "claude-b", "question",
		map[string]string{"ask": "are you touching the parser?"})
	if err != nil {
		t.Fatalf("SendTo() = %v, want nil", err)
	}
	if sent.Recipients != 1 {
		t.Errorf("the message reached %d agents, want 1", sent.Recipients)
	}

	select {
	case method := <-notified:
		if method != "message.received" {
			t.Errorf("notification method = %q, want message.received", method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the recipient was never notified")
	}

	inbox, err := listener.Inbox(t.Context(), 0)
	if err != nil {
		t.Fatalf("Inbox() = %v, want nil", err)
	}
	if len(inbox) != 1 || inbox[0].FromName != "claude-a" {
		t.Fatalf("Inbox() = %+v, want one message from claude-a", inbox)
	}

	acknowledged, err := listener.Acknowledge(t.Context(), inbox[0].ID)
	if err != nil || acknowledged != 1 {
		t.Fatalf("Acknowledge() = %d (%v), want 1", acknowledged, err)
	}

	remaining, err := listener.Inbox(t.Context(), 0)
	if err != nil || len(remaining) != 0 {
		t.Errorf("the acknowledged message came back: %+v (%v)", remaining, err)
	}
}

// Ordinary agents must not be able to redefine the machine's limits; the operator must.
func TestOnlyOperatorsMayDefineResources(t *testing.T) {
	subject := startDaemon(t, nil)

	ordinary := subject.connect(t, "claude-a")
	if _, err := ordinary.DefineResource(t.Context(), "test", 99, 0, "as many as I like"); err == nil {
		t.Fatal("an ordinary agent redefined the machine's limits")
	} else if lacclient.ErrorCode(err) != lacclient.CodeUnauthorised {
		t.Errorf("error code = %d, want CodeUnauthorised", lacclient.ErrorCode(err))
	}

	operator := subject.connect(t, "operator")
	resource, err := operator.DefineResource(t.Context(), "test", 4, 0, "concurrent test runs")
	if err != nil {
		t.Fatalf("the operator could not define a resource: %v", err)
	}
	if resource.Capacity != 4 {
		t.Errorf("capacity = %d, want 4", resource.Capacity)
	}
}

// Calling anything before authenticating must be refused, or the whole identity model is decorative.
func TestUnauthenticatedCallsAreRefused(t *testing.T) {
	subject := startDaemon(t, nil)

	client, err := lacclient.Dial(t.Context(), lacclient.Options{SocketPath: subject.socketPath})
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.Agents(t.Context()); lacclient.ErrorCode(err) != lacclient.CodeUnauthorised {
		t.Errorf("agent.list without a token = %v, want CodeUnauthorised", err)
	}
	if _, err := client.Inbox(t.Context(), 0); lacclient.ErrorCode(err) != lacclient.CodeUnauthorised {
		t.Errorf("message.inbox without a token = %v, want CodeUnauthorised", err)
	}
}

// A token from one registration must work on a new connection: that is how an agent survives a
// reconnect without losing its identity.
func TestATokenAuthenticatesANewConnection(t *testing.T) {
	subject := startDaemon(t, nil)

	first, err := lacclient.Dial(t.Context(), lacclient.Options{SocketPath: subject.socketPath})
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}

	registration, err := first.Register(t.Context(), "claude-a", "claude", t.TempDir(), os.Getpid())
	if err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	second, err := lacclient.Dial(t.Context(), lacclient.Options{
		SocketPath: subject.socketPath, Token: registration.Token,
	})
	if err != nil {
		t.Fatalf("reconnecting with the token = %v, want nil", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	agents, err := second.Agents(t.Context())
	if err != nil {
		t.Fatalf("Agents() = %v, want nil", err)
	}
	if len(agents) != 1 || agents[0].Name != "claude-a" {
		t.Errorf("Agents() = %+v, want the reconnected agent", agents)
	}
}

// A rubbish token must be refused, whatever it looks like.
func TestABadTokenIsRefused(t *testing.T) {
	subject := startDaemon(t, nil)

	_, err := lacclient.Dial(t.Context(), lacclient.Options{
		SocketPath: subject.socketPath, Token: "lac_not-a-real-token",
	})
	if err == nil {
		t.Fatal("Dial() with a bad token = nil, want an error")
	}
	if lacclient.ErrorCode(err) != lacclient.CodeUnauthorised {
		t.Errorf("error code = %d, want CodeUnauthorised", lacclient.ErrorCode(err))
	}
}

// NoWait is what a client uses when it would rather do something else than queue.
func TestNoWaitReportsCapacity(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{{Name: "reviewer", Capacity: 1}})

	holder := subject.connect(t, "claude-a")
	hopeful := subject.connect(t, "claude-b")

	if _, err := holder.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "reviewer"}); err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}

	_, err := hopeful.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "reviewer", NoWait: true})
	if !lacclient.IsCapacityReached(err) {
		t.Errorf("Acquire(NoWait) = %v, want a capacity error", err)
	}
}

// Giving up on a queue must actually leave the queue, or the agents behind wait for a ghost.
func TestATimedOutRequestLeavesTheQueue(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{{Name: "reviewer", Capacity: 1}})

	holder := subject.connect(t, "claude-a")
	impatient := subject.connect(t, "claude-b")

	if _, err := holder.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "reviewer"}); err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}

	_, err := impatient.Acquire(t.Context(), lacclient.AcquireRequest{
		Resource: "reviewer", Timeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Acquire() with a timeout succeeded while the resource was full")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Acquire() = %v, want a timeout rather than a cancellation", err)
	}

	waitForQueue(t, holder, "reviewer", 0)
}

func waitForQueue(t *testing.T, client *lacclient.Client, resource string, want int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := client.Queue(t.Context(), resource)
		if err != nil {
			t.Fatalf("Queue() = %v, want nil", err)
		}
		if len(status.Waiting) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("the queue for %q never reached %d waiting", resource, want)
}
