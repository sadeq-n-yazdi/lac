package github

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"sync"
)

// Accounts are the GitHub logins gh is signed in to.
//
// gh has one *active* account at a time, and it does not change by directory: standing in a
// repository that belongs to another account does not make gh use that account. A machine where
// one login owns the work repositories and another owns the personal ones therefore cannot see half
// of them through the active account alone — which is the situation this exists for.
func (c *Client) Accounts(ctx context.Context) ([]string, error) {
	output, err := c.run(ctx, "auth", "status")
	if err != nil {
		return nil, err
	}

	return parseAccounts(string(output)), nil
}

// parseAccounts reads the logins out of `gh auth status`, active one first.
//
// The output is meant for a person, so this reads it forgivingly: anything it does not recognise
// simply yields no account, and the caller falls back to whichever login gh considers active.
func parseAccounts(output string) []string {
	var (
		accounts []string
		active   string
		current  string
	)

	lines := bufio.NewScanner(strings.NewReader(output))
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())

		if login, found := loginFrom(line); found {
			current = login
			accounts = append(accounts, login)

			continue
		}

		if current != "" && strings.HasPrefix(line, "- Active account:") &&
			strings.Contains(line, "true") {
			active = current
		}
	}

	if active == "" {
		return accounts
	}

	// The active account first: it is the one most likely to work, and trying it first keeps the
	// usual case to a single request.
	ordered := []string{active}
	for _, account := range accounts {
		if account != active {
			ordered = append(ordered, account)
		}
	}

	return ordered
}

// loginFrom reads a line such as "✓ Logged in to github.com account sadeq-huma (keyring)".
func loginFrom(line string) (string, bool) {
	const marker = "account "

	if !strings.Contains(line, "Logged in to") {
		return "", false
	}

	index := strings.Index(line, marker)
	if index < 0 {
		return "", false
	}

	login, _, _ := strings.Cut(strings.TrimSpace(line[index+len(marker):]), " ")

	return strings.TrimSpace(login), login != ""
}

// tokens caches the token for each account for the life of the process.
//
// Fetching one is another subprocess, and a poll happens every minute per watched pull request. The
// token is held in memory only, never logged, and never written anywhere.
type tokens struct {
	mutex  sync.Mutex
	byUser map[string]string
}

func newTokens() *tokens {
	return &tokens{byUser: make(map[string]string)}
}

func (t *tokens) get(account string) (string, bool) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	token, found := t.byUser[account]

	return token, found
}

func (t *tokens) put(account, token string) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.byUser[account] = token
}

func (t *tokens) forget(account string) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	delete(t.byUser, account)
}

// tokenFor returns the token gh holds for an account, so a request can be made as that login
// without changing which account is active for the operator's own shell.
func (c *Client) tokenFor(ctx context.Context, account string) (string, error) {
	if token, cached := c.tokens.get(account); cached {
		return token, nil
	}

	output, err := c.run(ctx, "auth", "token", "--user", account)
	if err != nil {
		return "", fmt.Errorf("reading the token for %s: %w", account, err)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("%w: gh holds no token for %s", ErrNoCLI, account)
	}

	c.tokens.put(account, token)

	return token, nil
}
