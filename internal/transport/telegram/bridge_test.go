package telegram_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/service/leasing"
	"sadeq.uk/lac/internal/service/messaging"
	"sadeq.uk/lac/internal/service/registry"
	"sadeq.uk/lac/internal/service/reporting"
	"sadeq.uk/lac/internal/store/sqlite"
	"sadeq.uk/lac/internal/transport/telegram"
)

const (
	botToken     = "123456:test-token"
	operatorChat = int64(99)
	strangerChat = int64(1234)
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeTelegram stands in for the Bot API: it hands out the updates a test queues, and records what
// the bridge sends back.
type fakeTelegram struct {
	server *httptest.Server

	mutex    sync.Mutex
	pending  []map[string]any
	sent     []sentMessage
	requests []string
	nextID   int64
	// delivered closes each time a message is sent, so a test can wait for one.
	delivered chan struct{}
}

type sentMessage struct {
	ChatID int64
	Text   string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()

	fake := &fakeTelegram{delivered: make(chan struct{}, 64)}

	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &payload)

		method := path(request.URL.Path)

		fake.mutex.Lock()
		fake.requests = append(fake.requests, method)
		fake.mutex.Unlock()

		// The token belongs in the path, and a bridge that did not send it is broken.
		if !strings.Contains(request.URL.Path, "bot"+botToken) {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))

			return
		}

		switch method {
		case "getMe":
			reply(writer, map[string]any{"id": 1, "is_bot": true, "username": "lac_test_bot"})

		case "getUpdates":
			reply(writer, fake.takeUpdates())

		case "sendMessage":
			fake.record(payload)
			reply(writer, map[string]any{"message_id": 1})

		default:
			reply(writer, map[string]any{})
		}
	}))

	t.Cleanup(fake.server.Close)

	return fake
}

func path(urlPath string) string {
	parts := strings.Split(strings.Trim(urlPath, "/"), "/")

	return parts[len(parts)-1]
}

func reply(writer http.ResponseWriter, result any) {
	writer.Header().Set("Content-Type", "application/json")

	encoded, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	_, _ = writer.Write(encoded)
}

// send queues a message from a chat, as if somebody had typed it.
func (f *fakeTelegram) send(chatID int64, text string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.nextID++
	f.pending = append(f.pending, map[string]any{
		"update_id": f.nextID,
		"message": map[string]any{
			"message_id": f.nextID,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"from":       map[string]any{"id": chatID, "is_bot": false, "first_name": "Operator"},
			"text":       text,
		},
	})
}

func (f *fakeTelegram) takeUpdates() []map[string]any {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	taken := f.pending
	f.pending = nil

	return taken
}

func (f *fakeTelegram) record(payload map[string]any) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	chatID, _ := payload["chat_id"].(float64)
	text, _ := payload["text"].(string)
	f.sent = append(f.sent, sentMessage{ChatID: int64(chatID), Text: text})

	select {
	case f.delivered <- struct{}{}:
	default:
	}
}

// waitForReply waits for the next outbound message and returns it.
func (f *fakeTelegram) waitForReply(t *testing.T) sentMessage {
	t.Helper()

	select {
	case <-f.delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge never replied")
	}

	f.mutex.Lock()
	defer f.mutex.Unlock()

	return f.sent[len(f.sent)-1]
}

func (f *fakeTelegram) replyCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return len(f.sent)
}

// harness is a bridge wired to real services, talking to the fake Telegram.
type harness struct {
	bridge    *telegram.Bridge
	telegram  *fakeTelegram
	registry  *registry.Service
	messaging *messaging.Service
	leasing   *leasing.Service
	store     *sqlite.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	fake := newFakeTelegram(t)

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	secret, err := auth.LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("creating the secret: %v", err)
	}

	authenticator, err := auth.New(store, secret, auth.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("creating the authenticator: %v", err)
	}

	registryService := registry.New(store, authenticator, registry.Options{
		TimeToLive: time.Hour,
		CapabilitiesFor: func(string) core.Capabilities {
			return core.Capabilities{
				Resources: []string{core.WildcardResource}, CanBroadcast: true,
				CanDefineResources: true, CanRequestReports: true,
			}
		},
		Logger: quietLogger(),
	})
	messagingService := messaging.New(store, messaging.Options{Logger: quietLogger()})
	leasingService := leasing.New(store, leasing.Options{Logger: quietLogger()})
	reportingService := reporting.New(store, registryService, messagingService,
		reporting.Options{Logger: quietLogger()})

	bridge, err := telegram.New(telegram.Services{
		Registry:  registryService,
		Messaging: messagingService,
		Leasing:   leasingService,
		Reporting: reportingService,
		Audit:     store.Audit(),
	}, telegram.Options{
		Token:          botToken,
		AllowedChatIDs: []int64{operatorChat},
		BaseURL:        fake.server.URL,
		PollTimeout:    50 * time.Millisecond,
		ReportDeadline: 2 * time.Second,
		Logger:         quietLogger(),
	})
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	subject := &harness{
		bridge:    bridge,
		telegram:  fake,
		registry:  registryService,
		messaging: messagingService,
		leasing:   leasingService,
		store:     store,
	}

	ctx, stop := context.WithCancel(t.Context())
	running := make(chan error, 1)
	go func() { running <- bridge.Run(ctx) }()

	t.Cleanup(func() {
		stop()
		select {
		case err := <-running:
			if err != nil {
				t.Errorf("Run() = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the bridge did not stop when its context ended")
		}
	})

	// Wait until the bridge is registered, so tests can rely on its identity.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if bridge.AgentID() != "" {
			return subject
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("the bridge never registered with the daemon")

	return nil
}

func (h *harness) registerAgent(t *testing.T, name string, processID int) core.Agent {
	t.Helper()

	registration, err := h.registry.Register(t.Context(), registry.RegisterRequest{
		Name: name, Kind: "claude", Workdir: "/tmp/" + name, ProcessID: processID,
	})
	if err != nil {
		t.Fatalf("registering %q: %v", name, err)
	}

	return registration.Agent
}

// The allowlist is the whole of the bridge's access control. A stranger who finds the bot must get
// nothing back — not even an error, which would confirm the bot is worth poking at.
func TestAMessageFromAnUnknownChatIsIgnored(t *testing.T) {
	subject := newHarness(t)

	subject.telegram.send(strangerChat, "/agents")
	// Give the bridge time to poll and decide.
	time.Sleep(300 * time.Millisecond)

	if count := subject.telegram.replyCount(); count != 0 {
		t.Fatalf("the bridge sent %d replies to a stranger, want 0", count)
	}

	// And the operator must be able to find out that it happened.
	entries, err := subject.store.Audit().List(t.Context(), core.AuditFilter{})
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}

	found := false
	for _, entry := range entries {
		if strings.Contains(entry.Target, "1234") {
			found = true
		}
	}
	if !found {
		t.Error("the refused message was not recorded in the audit log")
	}
}

func TestHelp(t *testing.T) {
	subject := newHarness(t)

	subject.telegram.send(operatorChat, "/help")
	reply := subject.telegram.waitForReply(t)

	if reply.ChatID != operatorChat {
		t.Errorf("replied to chat %d, want %d", reply.ChatID, operatorChat)
	}
	for _, command := range []string{"/agents", "/queue", "/report", "/say"} {
		if !strings.Contains(reply.Text, command) {
			t.Errorf("the help does not mention %s", command)
		}
	}
}

// The question the operator carries a phone for: who is working, and where.
func TestAgentsCommand(t *testing.T) {
	subject := newHarness(t)
	subject.registerAgent(t, "claude-a", 1)
	subject.registerAgent(t, "claude-b", 2)

	subject.telegram.send(operatorChat, "/agents")
	reply := subject.telegram.waitForReply(t)

	for _, name := range []string{"claude-a", "claude-b"} {
		if !strings.Contains(reply.Text, name) {
			t.Errorf("the roster does not mention %s: %q", name, reply.Text)
		}
	}
	// The bridge itself is not an agent the operator cares about.
	if strings.Contains(reply.Text, "• telegram") {
		t.Errorf("the bridge listed itself: %q", reply.Text)
	}
}

func TestResourcesAndQueueCommands(t *testing.T) {
	subject := newHarness(t)
	agent := subject.registerAgent(t, "claude-a", 1)

	if err := subject.leasing.Define(t.Context(), "operator", core.Resource{
		Name: "test", Capacity: 1, LeaseTimeToLive: time.Hour, Description: "concurrent test runs",
	}); err != nil {
		t.Fatalf("Define() = %v, want nil", err)
	}
	if _, err := subject.leasing.Acquire(t.Context(), leasing.AcquireRequest{
		AgentID: agent.ID, ResourceName: "test", Reason: "make test",
	}); err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}

	subject.telegram.send(operatorChat, "/resources")
	resources := subject.telegram.waitForReply(t)
	if !strings.Contains(resources.Text, "test — 1/1 in use") {
		t.Errorf("/resources said %q", resources.Text)
	}

	subject.telegram.send(operatorChat, "/queue test")
	queue := subject.telegram.waitForReply(t)
	if !strings.Contains(queue.Text, "test — 1/1 in use") {
		t.Errorf("/queue said %q", queue.Text)
	}
}

func TestQueueWithoutAResourceAsksForOne(t *testing.T) {
	subject := newHarness(t)

	subject.telegram.send(operatorChat, "/queue")
	reply := subject.telegram.waitForReply(t)

	if !strings.Contains(reply.Text, "Which resource") {
		t.Errorf("/queue with no argument said %q", reply.Text)
	}
}

// The operator asks from a phone; the agents answer; the answers come back in one message.
func TestReportCommand(t *testing.T) {
	subject := newHarness(t)
	answering := subject.registerAgent(t, "claude-a", 1)
	subject.registerAgent(t, "claude-b", 2)

	// Answer as soon as the question arrives.
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			messages, err := subject.messaging.Inbox(context.Background(), answering.ID, 0)
			if err != nil {
				return
			}
			for _, item := range messages {
				if item.Kind != reporting.MessageKind {
					continue
				}

				var body struct {
					RequestID string `json:"request_id"`
				}
				if err := json.Unmarshal(item.Body, &body); err != nil {
					return
				}
				_ = subject.bridgeReport(body.RequestID, answering.ID)

				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	subject.telegram.send(operatorChat, "/report what are you working on?")
	reply := subject.telegram.waitForReply(t)

	if !strings.Contains(reply.Text, "claude-a") || !strings.Contains(reply.Text, "the parser") {
		t.Errorf("the report does not carry the answer: %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "No answer from: claude-b") {
		t.Errorf("the report does not name the agent that stayed silent: %q", reply.Text)
	}
}

// A plain sentence, with no command, is the operator asking everyone something.
func TestPlainTextIsTreatedAsAQuestion(t *testing.T) {
	subject := newHarness(t)

	subject.telegram.send(operatorChat, "what is everyone doing?")
	reply := subject.telegram.waitForReply(t)

	if !strings.Contains(reply.Text, "nobody to ask") {
		t.Errorf("a plain sentence said %q, want it treated as a question", reply.Text)
	}
}

func TestSayAndBroadcast(t *testing.T) {
	subject := newHarness(t)
	agent := subject.registerAgent(t, "claude-a", 1)

	subject.telegram.send(operatorChat, "/say claude-a please stop rebasing")
	said := subject.telegram.waitForReply(t)
	if !strings.Contains(said.Text, "Sent to claude-a") {
		t.Fatalf("/say said %q", said.Text)
	}

	inbox, err := subject.messaging.Inbox(t.Context(), agent.ID, 0)
	if err != nil {
		t.Fatalf("Inbox() = %v, want nil", err)
	}
	if len(inbox) != 1 || !strings.Contains(string(inbox[0].Body), "please stop rebasing") {
		t.Fatalf("the agent received %+v, want the operator's message", inbox)
	}

	subject.telegram.send(operatorChat, "/broadcast I am about to rebase main")
	broadcast := subject.telegram.waitForReply(t)
	if !strings.Contains(broadcast.Text, "Told 1 agent") {
		t.Errorf("/broadcast said %q", broadcast.Text)
	}
}

func TestSayToAnUnknownAgent(t *testing.T) {
	subject := newHarness(t)

	subject.telegram.send(operatorChat, "/say nobody hello")
	reply := subject.telegram.waitForReply(t)

	if !strings.Contains(reply.Text, "Could not reach") {
		t.Errorf("/say to an unknown agent said %q", reply.Text)
	}
}

// An agent messaging "telegram" reaches the operator's phone: that is how an agent asks for help
// when the operator is not at the machine.
func TestAMessageToTheBridgeReachesThePhone(t *testing.T) {
	subject := newHarness(t)
	agent := subject.registerAgent(t, "claude-a", 1)

	sent, err := subject.messaging.Send(t.Context(), messaging.SendRequest{
		FromAgentID: agent.ID,
		ToAgentName: telegram.AgentName,
		Kind:        "question",
		Body:        []byte(`{"text":"the migration failed, should I roll back?"}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	if len(sent.RecipientIDs) != 1 {
		t.Fatalf("the message reached %d agents, want the bridge", len(sent.RecipientIDs))
	}

	// The bridge is wired as a notifier by the daemon; here the test plays that part.
	if reached := subject.bridge.Broadcast(subject.bridge.AgentID(), "message.received", nil); reached == 0 {
		t.Fatal("the bridge did not forward the message")
	}

	reply := subject.telegram.waitForReply(t)
	if !strings.Contains(reply.Text, "should I roll back?") {
		t.Errorf("the forwarded message reads %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "claude-a") {
		t.Errorf("the forwarded message does not say who sent it: %q", reply.Text)
	}
}

// A bridge without an allowlist would let anyone who found the bot drive the machine.
func TestABridgeWithoutAnAllowlistIsRefused(t *testing.T) {
	_, err := telegram.New(telegram.Services{}, telegram.Options{Token: botToken})
	if err == nil {
		t.Fatal("New() accepted a bridge with no allowed chats")
	}
	if !strings.Contains(err.Error(), "chat id") {
		t.Errorf("the error does not explain what is missing: %v", err)
	}
}

func TestABridgeWithoutATokenIsRefused(t *testing.T) {
	_, err := telegram.New(telegram.Services{}, telegram.Options{AllowedChatIDs: []int64{operatorChat}})
	if err == nil {
		t.Error("New() accepted a bridge with no token")
	}
}

// bridgeReport answers a report request as the given agent, standing in for what an AI session
// does through MCP.
func (h *harness) bridgeReport(requestID, agentID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return h.reporting().Submit(ctx, requestID, agentID, "the parser")
}

func (h *harness) reporting() *reporting.Service {
	return reporting.New(h.store, h.registry, h.messaging, reporting.Options{Logger: quietLogger()})
}
