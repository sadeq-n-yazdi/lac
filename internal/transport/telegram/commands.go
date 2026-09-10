package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/service/messaging"
)

// helpText is what /help and anything unrecognised answers with. It is deliberately short: it is
// read on a phone.
const helpText = `LAC — the agents on your machine

/agents            who is working, and where
/resources         the shared resources and how busy they are
/queue <resource>  who is waiting for it, in order
/report [question] ask every agent what it is doing
/say <agent> <text>  send a message to one agent
/broadcast <text>  tell every agent at once
/help              this`

// respond turns one message from the operator into an answer.
func (b *Bridge) respond(ctx context.Context, text string) string {
	command, rest := splitCommand(text)

	switch command {
	case "/start", "/help":
		return helpText

	case "/agents":
		return b.describeAgents(ctx)

	case "/resources":
		return b.describeResources(ctx)

	case "/queue":
		return b.describeQueue(ctx, rest)

	case "/report":
		return b.collectReports(ctx, rest)

	case "/say":
		return b.sayTo(ctx, rest)

	case "/broadcast":
		return b.broadcast(ctx, rest)

	default:
		// Anything that is not a command is treated as a broadcast question, because typing a
		// sentence at the bot most likely means "ask everyone this".
		if !strings.HasPrefix(command, "/") {
			return b.collectReports(ctx, text)
		}

		return "I do not know that command.\n\n" + helpText
	}
}

// splitCommand separates the leading word from the rest, ignoring the @botname suffix Telegram
// adds in group chats.
func splitCommand(text string) (command, rest string) {
	command, rest, _ = strings.Cut(strings.TrimSpace(text), " ")
	if at := strings.Index(command, "@"); at > 0 {
		command = command[:at]
	}

	return strings.ToLower(command), strings.TrimSpace(rest)
}

func (b *Bridge) describeAgents(ctx context.Context) string {
	agents, err := b.services.Registry.Active(ctx)
	if err != nil {
		return "Could not read the roster: " + err.Error()
	}

	others := make([]core.Agent, 0, len(agents))
	for _, agent := range agents {
		if agent.Name != AgentName {
			others = append(others, agent)
		}
	}

	if len(others) == 0 {
		return "No agents are working right now."
	}

	var report strings.Builder
	fmt.Fprintf(&report, "%d agent(s) working:\n", len(others))

	for _, agent := range others {
		fmt.Fprintf(&report, "\n• %s (%s)\n  %s", agent.Name, agent.Kind, agent.Workdir)
	}

	return report.String()
}

func (b *Bridge) describeResources(ctx context.Context) string {
	statuses, err := b.services.Leasing.List(ctx)
	if err != nil {
		return "Could not read the resources: " + err.Error()
	}
	if len(statuses) == 0 {
		return "No shared resources are defined."
	}

	var report strings.Builder
	report.WriteString("Resources:\n")

	for _, status := range statuses {
		fmt.Fprintf(&report, "\n• %s — %d/%d in use", status.Resource.Name,
			status.ActiveLeases, status.Resource.Capacity)
		if status.Waiting > 0 {
			fmt.Fprintf(&report, ", %d waiting", status.Waiting)
		}
	}

	return report.String()
}

func (b *Bridge) describeQueue(ctx context.Context, resourceName string) string {
	if resourceName == "" {
		return "Which resource? For example: /queue test"
	}

	status, err := b.services.Leasing.Status(ctx, resourceName)
	if err != nil {
		return fmt.Sprintf("Could not read %q: %v", resourceName, err)
	}

	entries, err := b.services.Leasing.Queue(ctx, resourceName)
	if err != nil {
		return fmt.Sprintf("Could not read the queue for %q: %v", resourceName, err)
	}

	var report strings.Builder
	fmt.Fprintf(&report, "%s — %d/%d in use, %d waiting",
		status.Resource.Name, status.ActiveLeases, status.Resource.Capacity, len(entries))

	for position, entry := range entries {
		name := b.senderName(ctx, entry.AgentID)
		fmt.Fprintf(&report, "\n\n%d. %s", position+1, name)
		if entry.Reason != "" {
			fmt.Fprintf(&report, "\n   %s", entry.Reason)
		}
	}

	return report.String()
}

// collectReports asks every agent what it is doing and waits for the answers, which is the whole
// reason for carrying this bridge around on a phone.
func (b *Bridge) collectReports(ctx context.Context, question string) string {
	if question == "" {
		question = "What are you working on right now?"
	}

	request, err := b.services.Reporting.Request(ctx, b.AgentID(), question, b.options.ReportDeadline)
	if err != nil {
		return "Could not ask: " + err.Error()
	}
	if len(request.AskedAgentIDs) == 0 {
		return "No agents are working right now, so there is nobody to ask."
	}

	collection, err := b.services.Reporting.Collect(ctx, request.ID)
	if err != nil {
		return "Could not collect the answers: " + err.Error()
	}

	var report strings.Builder
	fmt.Fprintf(&report, "Asked %d agent(s): %s", len(request.AskedAgentIDs), question)

	for _, item := range collection.Reports {
		fmt.Fprintf(&report, "\n\n• %s\n%s", b.senderName(ctx, item.AgentID), item.Body)
	}

	if len(collection.SilentAgentIDs) > 0 {
		names := make([]string, 0, len(collection.SilentAgentIDs))
		for _, agentID := range collection.SilentAgentIDs {
			names = append(names, b.senderName(ctx, agentID))
		}
		fmt.Fprintf(&report, "\n\nNo answer from: %s", strings.Join(names, ", "))
	}

	return report.String()
}

func (b *Bridge) sayTo(ctx context.Context, rest string) string {
	recipient, text, found := strings.Cut(rest, " ")
	if !found || strings.TrimSpace(text) == "" {
		return "Who, and what? For example: /say claude-a please stop rebasing"
	}

	if _, err := b.services.Messaging.Send(ctx, messaging.SendRequest{
		FromAgentID: b.AgentID(),
		ToAgentName: recipient,
		Kind:        "operator",
		Body:        bodyOf(text),
	}); err != nil {
		return fmt.Sprintf("Could not reach %q: %v", recipient, err)
	}

	return fmt.Sprintf("Sent to %s.", recipient)
}

func (b *Bridge) broadcast(ctx context.Context, text string) string {
	if text == "" {
		return "What should everyone hear? For example: /broadcast I am about to rebase main"
	}

	sent, err := b.services.Messaging.Send(ctx, messaging.SendRequest{
		FromAgentID: b.AgentID(),
		Topic:       core.BroadcastTopic,
		Kind:        "operator",
		Body:        bodyOf(text),
	})
	if err != nil {
		return "Could not send: " + err.Error()
	}

	return fmt.Sprintf("Told %d agent(s).", len(sent.RecipientIDs))
}

// bodyOf wraps the operator's plain text as the JSON body a message carries.
func bodyOf(text string) json.RawMessage {
	encoded, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		// A string always encodes; this cannot happen, and an empty body would be rejected anyway.
		return json.RawMessage(`{"text":""}`)
	}

	return encoded
}

// readableBody pulls the text out of a message body for display on a phone.
func readableBody(body []byte) string {
	var shaped struct {
		Text string `json:"text"`
	}

	if err := json.Unmarshal(body, &shaped); err == nil && shaped.Text != "" {
		return shaped.Text
	}

	return string(body)
}
