package core_test

import (
	"errors"
	"testing"
	"time"

	"sadeq.uk/lac/internal/core"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "simple", value: "claude-a"},
		{name: "digits and dots", value: "runner.2"},
		{name: "underscore", value: "test_runner"},
		{name: "single character", value: "a"},
		{name: "empty", value: "", wantErr: true},
		{name: "uppercase", value: "Claude", wantErr: true},
		{name: "leading dash", value: "-claude", wantErr: true},
		{name: "space", value: "claude a", wantErr: true},
		{name: "path traversal", value: "../etc", wantErr: true},
		{name: "shell metacharacter", value: "claude;rm", wantErr: true},
		{name: "too long", value: string(make([]byte, 64)), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := core.ValidateName("agent name", test.value)

			if test.wantErr {
				if !errors.Is(err, core.ErrInvalidArgument) {
					t.Fatalf("ValidateName(%q) error = %v, want ErrInvalidArgument", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateName(%q) = %v, want nil", test.value, err)
			}
		})
	}
}

// The zero Capabilities value must grant nothing: a caller that forgets to populate it should be
// denied rather than trusted with every resource.
func TestCapabilitiesDenyByDefault(t *testing.T) {
	var zero core.Capabilities

	if zero.MayLease("test") {
		t.Error("the zero Capabilities value granted a lease")
	}
	if zero.CanBroadcast || zero.CanDefineResources || zero.CanRequestReports {
		t.Error("the zero Capabilities value granted a permission")
	}
}

func TestCapabilitiesMayLease(t *testing.T) {
	named := core.Capabilities{Resources: []string{"test", "reviewer"}}
	wildcard := core.Capabilities{Resources: []string{core.WildcardResource}}

	if !named.MayLease("test") {
		t.Error("a named resource was denied")
	}
	if named.MayLease("database") {
		t.Error("an unnamed resource was granted")
	}
	if !wildcard.MayLease("anything-at-all") {
		t.Error("the wildcard did not grant an arbitrary resource")
	}
}

func TestAgentValidate(t *testing.T) {
	valid := core.Agent{
		ID: "agent_1", Name: "claude-a", Kind: "claude", Workdir: "/tmp/work", State: core.AgentActive,
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() on a valid agent = %v, want nil", err)
	}

	tests := map[string]func(core.Agent) core.Agent{
		"missing id":      func(a core.Agent) core.Agent { a.ID = ""; return a },
		"missing name":    func(a core.Agent) core.Agent { a.Name = ""; return a },
		"missing kind":    func(a core.Agent) core.Agent { a.Kind = ""; return a },
		"missing workdir": func(a core.Agent) core.Agent { a.Workdir = ""; return a },
		"unknown state":   func(a core.Agent) core.Agent { a.State = "wandering"; return a },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if err := mutate(valid).Validate(); !errors.Is(err, core.ErrInvalidArgument) {
				t.Errorf("Validate() = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

// An agent that stopped heartbeating must stop counting as alive, because its leases are released
// on that basis; getting this wrong strands a slot forever.
func TestAgentIsAlive(t *testing.T) {
	base := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	const timeToLive = 30 * time.Second

	tests := []struct {
		name      string
		state     core.AgentState
		heartbeat time.Time
		want      bool
	}{
		{name: "recent heartbeat", state: core.AgentActive, heartbeat: base.Add(-time.Second), want: true},
		{name: "exactly at the limit", state: core.AgentActive, heartbeat: base.Add(-timeToLive), want: true},
		{name: "past the limit", state: core.AgentActive, heartbeat: base.Add(-timeToLive - time.Nanosecond)},
		{name: "already stale", state: core.AgentStale, heartbeat: base},
		{name: "deregistered", state: core.AgentDeregistered, heartbeat: base},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := core.Agent{State: test.state, LastHeartbeatAt: test.heartbeat}

			if got := agent.IsAlive(base, timeToLive); got != test.want {
				t.Errorf("IsAlive() = %v, want %v", got, test.want)
			}
		})
	}
}

// A message must be addressed to exactly one place. Allowing both a recipient and a topic would
// deliver it twice; allowing neither would deliver it nowhere.
func TestMessageValidateAddressing(t *testing.T) {
	base := core.Message{ID: "msg_1", FromAgentID: "agent_1", Kind: "status", Body: []byte(`{"a":1}`)}

	direct := base
	direct.ToAgentID = "agent_2"
	if err := direct.Validate(); err != nil {
		t.Errorf("a direct message was rejected: %v", err)
	}

	topic := base
	topic.Topic = "all"
	if err := topic.Validate(); err != nil {
		t.Errorf("a topic message was rejected: %v", err)
	}

	both := base
	both.ToAgentID, both.Topic = "agent_2", "all"
	if err := both.Validate(); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("a message with both a recipient and a topic was accepted: %v", err)
	}

	if err := base.Validate(); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("a message with no destination was accepted: %v", err)
	}
}

// Capacity is the guarantee the whole project rests on, so a resource that cannot express a real
// limit must never reach the store.
func TestResourceValidate(t *testing.T) {
	valid := core.Resource{Name: "test", Capacity: 4, LeaseTimeToLive: 10 * time.Minute}

	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() on a valid resource = %v, want nil", err)
	}

	tests := map[string]core.Resource{
		"zero capacity":     {Name: "test", Capacity: 0, LeaseTimeToLive: time.Minute},
		"negative capacity": {Name: "test", Capacity: -1, LeaseTimeToLive: time.Minute},
		"absurd capacity":   {Name: "test", Capacity: core.MaxCapacity + 1, LeaseTimeToLive: time.Minute},
		"no time to live":   {Name: "test", Capacity: 1},
		"bad name":          {Name: "Test Runner", Capacity: 1, LeaseTimeToLive: time.Minute},
	}

	for name, resource := range tests {
		t.Run(name, func(t *testing.T) {
			if err := resource.Validate(); !errors.Is(err, core.ErrInvalidArgument) {
				t.Errorf("Validate() = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestResourceStatusFree(t *testing.T) {
	resource := core.Resource{Name: "test", Capacity: 4, LeaseTimeToLive: time.Minute}

	tests := []struct {
		name   string
		active int
		want   int
	}{
		{name: "idle", active: 0, want: 4},
		{name: "partly used", active: 3, want: 1},
		{name: "full", active: 4, want: 0},
		{name: "over capacity is clamped", active: 5, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := core.ResourceStatus{Resource: resource, ActiveLeases: test.active}

			if got := status.Free(); got != test.want {
				t.Errorf("Free() = %d, want %d", got, test.want)
			}
		})
	}
}

// A lease holds a slot until it is released or it expires. Both conditions gate whether another
// agent may be granted that slot, so the boundary matters.
func TestLeaseActiveAndExpired(t *testing.T) {
	base := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	lease := core.Lease{AcquiredAt: base, ExpiresAt: base.Add(time.Minute)}

	tests := []struct {
		name        string
		at          time.Time
		released    time.Time
		wantActive  bool
		wantExpired bool
	}{
		{name: "held", at: base.Add(time.Second), wantActive: true},
		{name: "at expiry", at: base.Add(time.Minute), wantExpired: true},
		{name: "after expiry", at: base.Add(time.Hour), wantExpired: true},
		{name: "released early", at: base.Add(time.Second), released: base.Add(time.Second)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			subject := lease
			subject.ReleasedAt = test.released

			if got := subject.Active(test.at); got != test.wantActive {
				t.Errorf("Active() = %v, want %v", got, test.wantActive)
			}
			if got := subject.Expired(test.at); got != test.wantExpired {
				t.Errorf("Expired() = %v, want %v", got, test.wantExpired)
			}
		})
	}
}

func TestQueueEntryStateTerminal(t *testing.T) {
	if core.QueueWaiting.Terminal() {
		t.Error("a waiting entry was reported as terminal")
	}
	for _, state := range []core.QueueEntryState{core.QueueGranted, core.QueueCancelled, core.QueueExpired} {
		if !state.Terminal() {
			t.Errorf("%q was not reported as terminal", state)
		}
	}
}
