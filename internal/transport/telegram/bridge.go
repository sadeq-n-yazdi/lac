package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/service/leasing"
	"sadeq.uk/lac/internal/service/messaging"
	"sadeq.uk/lac/internal/service/registry"
	"sadeq.uk/lac/internal/service/reporting"
)

// AgentName is how the bridge appears to the other agents, so an agent can address the operator
// directly with `lac send telegram …`.
const AgentName = "telegram"

// Default timings. The poll timeout is what makes this a long poll rather than a busy loop.
const (
	defaultPollTimeout = 30 * time.Second
	// retryDelay is how long to wait after a failed poll, so a daemon whose network is down does
	// not hammer Telegram.
	retryDelay = 5 * time.Second
	// sendInterval throttles outbound messages, comfortably inside Telegram's own limits.
	sendInterval = 100 * time.Millisecond
	// defaultReportDeadline is how long a /report waits for the agents to answer. It is a wait on
	// somebody's phone, so it is short: a slow agent is reported as silent rather than held for.
	defaultReportDeadline = 30 * time.Second
)

// Options configure the bridge.
type Options struct {
	// Token is the bot token. Required.
	Token string
	// AllowedChatIDs are the chats the bridge will talk to. Anything else is ignored, and there is
	// no way to turn that off: without it, anyone who finds the bot could drive this machine.
	AllowedChatIDs []int64
	// BaseURL overrides Telegram's API, for tests. Empty means the real one.
	BaseURL string
	// PollTimeout is how long each long poll waits. Zero means thirty seconds.
	PollTimeout time.Duration
	// ReportDeadline is how long /report waits for the agents to answer. Zero means thirty seconds.
	ReportDeadline time.Duration
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Services are the parts of LAC the bridge speaks for.
type Services struct {
	Registry  *registry.Service
	Messaging *messaging.Service
	Leasing   *leasing.Service
	Reporting *reporting.Service
	// Audit records what came in over the bridge, including messages from chats that were refused.
	Audit core.AuditLog
}

// Bridge relays between Telegram and the daemon.
type Bridge struct {
	client   *client
	services Services
	options  Options
	logger   *slog.Logger

	agentMutex sync.RWMutex
	agentID    string

	sendMutex sync.Mutex
	lastSend  time.Time
}

// New returns a bridge. It does not talk to Telegram until Run is called.
func New(services Services, options Options) (*Bridge, error) {
	if options.Token == "" {
		return nil, errors.New("a telegram bot token is required")
	}
	if len(options.AllowedChatIDs) == 0 {
		return nil, errors.New("at least one allowed chat id is required: without one, anyone who " +
			"finds the bot could drive this machine")
	}
	if options.PollTimeout <= 0 {
		options.PollTimeout = defaultPollTimeout
	}
	if options.ReportDeadline <= 0 {
		options.ReportDeadline = defaultReportDeadline
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	// The HTTP timeout must outlast a long poll, or every poll looks like a failure.
	return &Bridge{
		client:   newClient(options.BaseURL, options.Token, options.PollTimeout+30*time.Second),
		services: services,
		options:  options,
		logger:   options.Logger,
	}, nil
}

// Run registers the bridge as an agent and relays until the context ends.
func (b *Bridge) Run(ctx context.Context) error {
	me, err := b.client.getMe(ctx)
	if err != nil {
		return fmt.Errorf("checking the telegram bot token: %w", err)
	}

	if err := b.register(ctx); err != nil {
		return err
	}

	b.logger.Info("telegram bridge started",
		"bot", me.Username, "allowed_chats", len(b.options.AllowedChatIDs))

	b.poll(ctx)

	return nil
}

// register puts the bridge on the roster, so agents can address the operator by name and the
// bridge can ask everyone for a report.
func (b *Bridge) register(ctx context.Context) error {
	registration, err := b.services.Registry.Register(ctx, registry.RegisterRequest{
		Name:      AgentName,
		Kind:      "telegram",
		Workdir:   "/",
		ProcessID: 0,
	})
	if err != nil {
		return fmt.Errorf("registering the telegram bridge: %w", err)
	}

	b.agentMutex.Lock()
	b.agentID = registration.Agent.ID
	b.agentMutex.Unlock()

	return nil
}

// AgentID is the bridge's own agent id, so the daemon can route notifications to it.
func (b *Bridge) AgentID() string {
	b.agentMutex.RLock()
	defer b.agentMutex.RUnlock()

	return b.agentID
}

// poll reads updates until the context ends.
func (b *Bridge) poll(ctx context.Context) {
	var offset int64

	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := b.client.getUpdates(ctx, offset, b.options.PollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrUnauthorised) {
				b.logger.Error("telegram rejected the bot token; the bridge is stopping")
				return
			}

			b.logger.Warn("could not read telegram updates; retrying", "error", err)

			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}

			continue
		}

		for _, item := range updates {
			// Acknowledge every update, even one we ignore, or it is delivered again forever.
			offset = max(offset, item.UpdateID+1)

			if item.Message == nil {
				continue
			}
			b.handleMessage(ctx, *item.Message)
		}
	}
}

// handleMessage acts on one message from Telegram.
func (b *Bridge) handleMessage(ctx context.Context, incoming message) {
	if !b.allowed(incoming.Chat.ID) {
		// Somebody who is not the operator found the bot. Say nothing to them — a reply confirms
		// the bot exists and is worth poking — and record it where the operator will see it.
		b.audit(ctx, fmt.Sprintf("telegram chat %d", incoming.Chat.ID),
			"a message arrived from a chat that is not on the allowlist and was ignored")
		b.logger.Warn("ignored a telegram message from a chat that is not on the allowlist",
			"chat", incoming.Chat.ID)

		return
	}

	text := strings.TrimSpace(incoming.Text)
	if text == "" {
		return
	}

	reply := b.respond(ctx, text)
	if reply == "" {
		return
	}

	if err := b.send(ctx, incoming.Chat.ID, reply); err != nil {
		b.logger.Warn("could not reply on telegram", "error", err)
	}
}

// allowed reports whether a chat is one the operator said to talk to.
func (b *Bridge) allowed(chatID int64) bool {
	return slices.Contains(b.options.AllowedChatIDs, chatID)
}

// send writes to a chat, throttled so a burst of notifications stays inside Telegram's limits.
func (b *Bridge) send(ctx context.Context, chatID int64, text string) error {
	b.sendMutex.Lock()
	wait := sendInterval - time.Since(b.lastSend)
	b.lastSend = time.Now()
	b.sendMutex.Unlock()

	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // the caller compares with context errors
		case <-time.After(wait):
		}
	}

	return b.client.sendMessage(ctx, chatID, text)
}

// Notify is the messaging service's Notifier: a message addressed to the bridge is forwarded to
// every allowed chat, which is how an agent reaches the operator's phone.
func (b *Bridge) Broadcast(agentID, method string, _ any) int {
	if agentID != b.AgentID() || method != messaging.NotificationMethod {
		return 0
	}

	// The notification says only that something arrived; the message itself comes from the inbox.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	messages, err := b.services.Messaging.Inbox(ctx, b.AgentID(), 0)
	if err != nil {
		b.logger.Warn("could not read the bridge inbox", "error", err)
		return 0
	}

	delivered := 0
	for _, item := range messages {
		sender := b.senderName(ctx, item.FromAgentID)
		text := fmt.Sprintf("%s (%s):\n%s", sender, item.Kind, readableBody(item.Body))

		for _, chatID := range b.options.AllowedChatIDs {
			if err := b.send(ctx, chatID, text); err != nil {
				b.logger.Warn("could not forward a message to telegram", "error", err)
				continue
			}
			delivered++
		}

		if _, err := b.services.Messaging.Acknowledge(ctx, b.AgentID(), []string{item.ID}); err != nil {
			b.logger.Warn("could not acknowledge a forwarded message", "error", err)
		}
	}

	return delivered
}

func (b *Bridge) senderName(ctx context.Context, agentID string) string {
	agent, err := b.services.Registry.ByID(ctx, agentID)
	if err != nil {
		return agentID
	}

	return agent.Name
}

// audit records something that happened on the bridge. A message from a chat that is not on the
// allowlist is exactly the sort of thing an operator wants to find later.
func (b *Bridge) audit(ctx context.Context, target, detail string) {
	if b.services.Audit == nil {
		return
	}

	entry := core.AuditEntry{
		Actor:  AgentName,
		Action: core.AuditAuthDenied,
		Target: target,
		Detail: detail,
		At:     time.Now().UTC(),
	}

	if err := b.services.Audit.Append(ctx, entry); err != nil {
		b.logger.Error("could not record a telegram event", "detail", detail, "error", err)
	}
}
