package registry_test

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/service/registry"
	"code.sadeq.uk/lac/internal/store/sqlite"
)

var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// harness is a registry wired to a real store, with a clock the test drives.
type harness struct {
	service *registry.Service
	store   *sqlite.Store
	now     time.Time
}

func newHarness(t *testing.T, options registry.Options) *harness {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	secret, err := auth.LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("creating the secret: %v", err)
	}

	subject := &harness{store: store, now: baseTime}

	authenticator, err := auth.New(store, secret, auth.Options{
		Clock:  func() time.Time { return subject.now },
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("creating the authenticator: %v", err)
	}

	options.Clock = func() time.Time { return subject.now }
	options.Logger = quietLogger()
	if options.TimeToLive == 0 {
		options.TimeToLive = 90 * time.Second
	}
	if options.CapabilitiesFor == nil {
		options.CapabilitiesFor = func(string) core.Capabilities {
			return core.Capabilities{Resources: []string{core.WildcardResource}, CanBroadcast: true}
		}
	}

	subject.service = registry.New(store, authenticator, options)

	return subject
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (h *harness) register(t *testing.T, name string, processID int) registry.Registration {
	t.Helper()

	registration, err := h.service.Register(t.Context(), registry.RegisterRequest{
		Name: name, Kind: "claude", Workdir: "/tmp/work/" + name, ProcessID: processID,
	})
	if err != nil {
		t.Fatalf("Register(%q) = %v, want nil", name, err)
	}

	return registration
}

func TestRegisterIssuesATokenAndRoster(t *testing.T) {
	subject := newHarness(t, registry.Options{})

	registration := subject.register(t, "claude-a", 1234)

	if registration.Token == "" {
		t.Fatal("Register() returned no token")
	}
	if registration.Agent.State != core.AgentActive {
		t.Errorf("state = %q, want %q", registration.Agent.State, core.AgentActive)
	}

	agents, err := subject.service.List(t.Context(), core.AgentFilter{})
	if err != nil {
		t.Fatalf("List() = %v, want nil", err)
	}
	if len(agents) != 1 || agents[0].Name != "claude-a" {
		t.Errorf("List() = %v, want the one agent that registered", agents)
	}
}

// An agent must not be able to grant itself powers by asking for them: capabilities come from the
// operator's policy and nowhere else.
func TestCapabilitiesComeFromPolicy(t *testing.T) {
	subject := newHarness(t, registry.Options{
		CapabilitiesFor: func(name string) core.Capabilities {
			if name == "operator" {
				return core.Capabilities{
					Resources: []string{core.WildcardResource}, CanDefineResources: true,
				}
			}

			return core.Capabilities{Resources: []string{"test"}}
		},
	})

	ordinary := subject.register(t, "claude-a", 1)
	if ordinary.Agent.Capabilities.CanDefineResources {
		t.Error("an ordinary agent was granted the power to define resources")
	}
	if ordinary.Agent.Capabilities.MayLease("reviewer") {
		t.Error("an ordinary agent was granted a resource the policy withheld")
	}

	operator := subject.register(t, "operator", 2)
	if !operator.Agent.Capabilities.CanDefineResources {
		t.Error("the operator was not granted the power to define resources")
	}
}

// A restarted agent must be able to reclaim its own name, keeping the identity its messages and
// leases are attached to.
func TestSameProcessReclaimsItsRegistration(t *testing.T) {
	subject := newHarness(t, registry.Options{})

	first := subject.register(t, "claude-a", 1234)
	subject.now = baseTime.Add(time.Minute)
	second := subject.register(t, "claude-a", 1234)

	if second.Agent.ID != first.Agent.ID {
		t.Errorf("the agent id changed on re-registration: %q then %q", first.Agent.ID, second.Agent.ID)
	}
	if second.Token == first.Token {
		t.Error("re-registration reused the old token; it must issue a fresh one")
	}
}

// Two live agents answering to one name would make every "tell claude-a to stop" ambiguous.
func TestADifferentProcessCannotStealAName(t *testing.T) {
	subject := newHarness(t, registry.Options{})
	subject.register(t, "claude-a", 1234)

	_, err := subject.service.Register(t.Context(), registry.RegisterRequest{
		Name: "claude-a", Kind: "codex", Workdir: "/tmp/work/other", ProcessID: 9999,
	})
	if !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("Register() = %v, want ErrAlreadyExists", err)
	}
	if err != nil && !strings.Contains(err.Error(), "1234") {
		t.Errorf("the error does not say who holds the name: %v", err)
	}
}

// A working directory outside the allowed roots is refused, so a stray registration cannot
// advertise somewhere it has no business being.
func TestWorkdirMustBeInsideTheAllowedRoots(t *testing.T) {
	subject := newHarness(t, registry.Options{WorkdirRoots: []string{"/home/user/code"}})

	tests := map[string]struct {
		workdir string
		wantErr bool
	}{
		"inside a root":         {workdir: "/home/user/code/project"},
		"the root itself":       {workdir: "/home/user/code"},
		"outside":               {workdir: "/etc", wantErr: true},
		"a sibling with prefix": {workdir: "/home/user/code-evil", wantErr: true},
		"relative":              {workdir: "code/project", wantErr: true},
		"empty":                 {workdir: "", wantErr: true},
		"traversal":             {workdir: "/home/user/code/../../etc", wantErr: true},
	}

	processID := 0
	for name, test := range tests {
		processID++

		t.Run(name, func(t *testing.T) {
			_, err := subject.service.Register(t.Context(), registry.RegisterRequest{
				Name: "claude-" + sanitise(name), Kind: "claude",
				Workdir: test.workdir, ProcessID: processID,
			})

			if test.wantErr {
				if !errors.Is(err, core.ErrInvalidArgument) {
					t.Errorf("Register(%q) = %v, want ErrInvalidArgument", test.workdir, err)
				}
				return
			}
			if err != nil {
				t.Errorf("Register(%q) = %v, want nil", test.workdir, err)
			}
		})
	}
}

// An agent that stopped heartbeating must drop out of the roster, because everything it holds is
// released on that basis.
func TestReapStale(t *testing.T) {
	subject := newHarness(t, registry.Options{TimeToLive: 30 * time.Second})
	quiet := subject.register(t, "claude-quiet", 1)
	chatty := subject.register(t, "claude-chatty", 2)

	subject.now = baseTime.Add(time.Minute)
	if err := subject.service.Heartbeat(t.Context(), chatty.Agent.ID); err != nil {
		t.Fatalf("Heartbeat() = %v, want nil", err)
	}

	stale, err := subject.service.ReapStale(t.Context())
	if err != nil {
		t.Fatalf("ReapStale() = %v, want nil", err)
	}
	if len(stale) != 1 || stale[0].ID != quiet.Agent.ID {
		t.Fatalf("ReapStale() returned %d agents, want only the quiet one", len(stale))
	}

	active, err := subject.service.Active(t.Context())
	if err != nil {
		t.Fatalf("Active() = %v, want nil", err)
	}
	if len(active) != 1 || active[0].ID != chatty.Agent.ID {
		t.Errorf("Active() = %v, want only the agent that heartbeated", active)
	}
}

// A heartbeat is proof of life: it must bring a stale agent back rather than leave it excluded.
func TestHeartbeatRevivesAStaleAgent(t *testing.T) {
	subject := newHarness(t, registry.Options{TimeToLive: 30 * time.Second})
	agent := subject.register(t, "claude-a", 1)

	subject.now = baseTime.Add(time.Minute)
	if _, err := subject.service.ReapStale(t.Context()); err != nil {
		t.Fatalf("ReapStale() = %v, want nil", err)
	}
	if err := subject.service.Heartbeat(t.Context(), agent.Agent.ID); err != nil {
		t.Fatalf("Heartbeat() = %v, want nil", err)
	}

	active, err := subject.service.Active(t.Context())
	if err != nil {
		t.Fatalf("Active() = %v, want nil", err)
	}
	if len(active) != 1 {
		t.Errorf("Active() returned %d agents, want the revived one", len(active))
	}
}

// Deregistration must free the name, so the operator can restart an agent under it.
func TestDeregisterFreesTheName(t *testing.T) {
	subject := newHarness(t, registry.Options{})
	agent := subject.register(t, "claude-a", 1)

	if err := subject.service.Deregister(t.Context(), agent.Agent.ID); err != nil {
		t.Fatalf("Deregister() = %v, want nil", err)
	}

	reused, err := subject.service.Register(t.Context(), registry.RegisterRequest{
		Name: "claude-a", Kind: "codex", Workdir: "/tmp/work/new", ProcessID: 2,
	})
	if err != nil {
		t.Fatalf("re-registering the freed name = %v, want nil", err)
	}
	if reused.Agent.ID == agent.Agent.ID {
		t.Error("the new registration reused the retired agent's identity")
	}
}

func TestRegisterRejectsBadNames(t *testing.T) {
	subject := newHarness(t, registry.Options{})

	for _, name := range []string{"", "Claude A", "../etc", "claude;rm"} {
		t.Run(name, func(t *testing.T) {
			_, err := subject.service.Register(t.Context(), registry.RegisterRequest{
				Name: name, Kind: "claude", Workdir: "/tmp/work", ProcessID: 1,
			})
			if !errors.Is(err, core.ErrInvalidArgument) {
				t.Errorf("Register(%q) = %v, want ErrInvalidArgument", name, err)
			}
		})
	}
}

// sanitise turns a test's subtest name into something that passes agent-name validation.
func sanitise(name string) string {
	cleaned := make([]rune, 0, len(name))
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			cleaned = append(cleaned, character)
		default:
			cleaned = append(cleaned, '-')
		}
	}

	return string(cleaned)
}
