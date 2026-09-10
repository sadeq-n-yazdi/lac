// Package telegram bridges LAC to a Telegram bot, so the operator can ask what the agents are
// doing from a phone.
//
// It is the only part of LAC that talks to the outside world, and it does so in one direction: the
// bridge calls Telegram, Telegram never calls in. There is no listener, no port, and no inbound
// connection to firewall. Everything else about LAC stays on the machine.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIBaseURL is Telegram's Bot API. Tests point the client somewhere else; nothing else should.
const APIBaseURL = "https://api.telegram.org"

// maxResponseSize bounds what we read from Telegram. Updates are small; anything larger is a
// mistake or an attempt to exhaust memory.
const maxResponseSize = 8 << 20

// maxMessageLength is Telegram's limit for one message. Longer text is split rather than dropped.
const maxMessageLength = 4096

// ErrUnauthorised means Telegram rejected the bot token.
var ErrUnauthorised = errors.New("telegram rejected the bot token")

// client is a minimal Telegram Bot API client: long-poll for updates, send text back.
type client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func newClient(baseURL, token string, timeout time.Duration) *client {
	if baseURL == "" {
		baseURL = APIBaseURL
	}

	return &client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		// The timeout must outlast a long poll, or every poll looks like a failure.
		httpClient: &http.Client{Timeout: timeout},
	}
}

// update is one thing that happened in Telegram. Only messages are of interest here.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	From      *user  `json:"from"`
	Chat      chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
}

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// apiResponse is the envelope every Bot API method returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

// getUpdates long-polls for new messages, starting after the given offset.
func (c *client) getUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]update, error) {
	payload := map[string]any{
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message"},
	}
	if offset > 0 {
		payload["offset"] = offset
	}

	raw, err := c.call(ctx, "getUpdates", payload)
	if err != nil {
		return nil, err
	}

	var updates []update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, fmt.Errorf("decoding updates: %w", err)
	}

	return updates, nil
}

// sendMessage delivers text to a chat, splitting anything over Telegram's length limit.
func (c *client) sendMessage(ctx context.Context, chatID int64, text string) error {
	for _, part := range split(text, maxMessageLength) {
		payload := map[string]any{
			"chat_id":                  chatID,
			"text":                     part,
			"disable_web_page_preview": true,
		}

		if _, err := c.call(ctx, "sendMessage", payload); err != nil {
			return err
		}
	}

	return nil
}

// getMe checks the token works, so a misconfigured bridge complains at start-up rather than
// looking silently broken.
func (c *client) getMe(ctx context.Context) (user, error) {
	raw, err := c.call(ctx, "getMe", nil)
	if err != nil {
		return user{}, err
	}

	var me user
	if err := json.Unmarshal(raw, &me); err != nil {
		return user{}, fmt.Errorf("decoding the bot's own details: %w", err)
	}

	return me, nil
}

// call invokes one Bot API method.
//
// The token is in the URL path, which is how the Bot API works, so no error message from here ever
// includes the URL: it would put the credential in the log.
func (c *client) call(ctx context.Context, method string, payload map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the %s request: %w", method, err)
	}

	endpoint, err := url.JoinPath(c.baseURL, "bot"+c.token, method)
	if err != nil {
		return nil, fmt.Errorf("building the %s request: %w", method, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building the %s request: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		// The URL carries the token, and Go puts the URL in this error, so it is replaced rather
		// than wrapped.
		return nil, fmt.Errorf("calling telegram %s: %s", method, withoutToken(err.Error(), c.token))
	}
	defer func() { _ = response.Body.Close() }()

	contents, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("reading the %s response: %w", method, err)
	}

	var envelope apiResponse
	if err := json.Unmarshal(contents, &envelope); err != nil {
		return nil, fmt.Errorf("decoding the %s response (http %d)", method, response.StatusCode)
	}

	if !envelope.OK {
		if envelope.ErrorCode == http.StatusUnauthorized {
			return nil, ErrUnauthorised
		}

		return nil, fmt.Errorf("telegram refused %s: %s", method, envelope.Description)
	}

	return envelope.Result, nil
}

// withoutToken removes the bot token from text that is about to be logged.
func withoutToken(text, token string) string {
	if token == "" {
		return text
	}

	return strings.ReplaceAll(text, token, "…")
}

// split breaks text into pieces no longer than limit, preferring to break at a line ending so a
// table or a list stays readable.
func split(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}

	parts := make([]string, 0, len(text)/limit+1)

	for len(text) > limit {
		cut := strings.LastIndex(text[:limit], "\n")
		if cut <= 0 {
			cut = limit
		}

		parts = append(parts, strings.TrimRight(text[:cut], "\n"))
		text = strings.TrimLeft(text[cut:], "\n")
	}

	if text != "" {
		parts = append(parts, text)
	}

	return parts
}
