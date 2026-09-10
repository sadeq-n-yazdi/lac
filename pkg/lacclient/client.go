// Package lacclient is the Go client for a LAC daemon.
//
// It is the only place request logic lives: the CLI, the MCP server and LAC's own integration
// tests all go through it, so there is one implementation of the protocol to get right.
//
// A client is safe for concurrent use. Calls that block on the daemon — waiting for a resource
// slot, above all — do not stop other calls on the same connection.
package lacclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol constants shared with the daemon.
const (
	protocolVersion = "2.0"
	maxFrameSize    = 4 << 20
)

// ErrNotConnected means the connection to the daemon has gone.
var ErrNotConnected = errors.New("not connected to the daemon")

// NotificationHandler is called for a message the daemon pushes without being asked, such as a
// message arriving for this agent. It runs on the client's reading goroutine, so it should hand
// work off rather than block.
type NotificationHandler func(method string, params json.RawMessage)

// Options configure a Client.
type Options struct {
	// SocketPath is the daemon's socket. Empty uses the default location.
	SocketPath string
	// Token authenticates an existing agent. Empty means the client must register first.
	Token string
	// OnNotification receives daemon-initiated messages. Optional.
	OnNotification NotificationHandler
	// DialTimeout bounds the initial connection. Zero means five seconds.
	DialTimeout time.Duration
}

// Client is a connection to the daemon.
type Client struct {
	connection net.Conn
	encoder    *json.Encoder
	writeMutex sync.Mutex

	nextID atomic.Int64

	pendingMutex sync.Mutex
	pending      map[int64]chan response

	onNotification NotificationHandler

	closeOnce sync.Once
	closed    chan struct{}
	readErr   atomic.Pointer[error]
}

// response is one reply from the daemon.
type response struct {
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// Error is a failure reported by the daemon. Its Code is stable and can be compared with the
// exported code constants; its Message was written for a person to read.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Dial connects to the daemon, authenticating if a token was supplied.
func Dial(ctx context.Context, options Options) (*Client, error) {
	socketPath := options.SocketPath
	if socketPath == "" {
		resolved, err := DefaultSocketPath()
		if err != nil {
			return nil, err
		}
		socketPath = resolved
	}

	timeout := options.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	dialer := net.Dialer{Timeout: timeout}

	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to the lac daemon at %s: %w. Is lacd running?", socketPath, err)
	}

	client := &Client{
		connection:     connection,
		encoder:        json.NewEncoder(connection),
		pending:        make(map[int64]chan response),
		onNotification: options.OnNotification,
		closed:         make(chan struct{}),
	}

	go client.readLoop()

	if options.Token != "" {
		if _, err := client.Authenticate(ctx, options.Token); err != nil {
			return nil, errors.Join(err, client.Close())
		}
	}

	return client, nil
}

// Call sends a request and waits for its reply.
//
// The reply is decoded into result, which may be nil for a method whose answer does not matter.
// Cancelling ctx abandons the wait; the daemon sees the connection's context end only when the
// connection itself does, so a cancelled blocking call is also released on the daemon side by
// closing the client.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if err := c.connectionError(); err != nil {
		return err
	}

	requestID := c.nextID.Add(1)
	replies := make(chan response, 1)

	c.pendingMutex.Lock()
	c.pending[requestID] = replies
	c.pendingMutex.Unlock()

	defer func() {
		c.pendingMutex.Lock()
		delete(c.pending, requestID)
		c.pendingMutex.Unlock()
	}()

	request := map[string]any{"jsonrpc": protocolVersion, "id": requestID, "method": method}
	if params != nil {
		request["params"] = params
	}

	if err := c.write(request); err != nil {
		return err
	}

	select {
	case reply := <-replies:
		if reply.Error != nil {
			return reply.Error
		}
		if result == nil || len(reply.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("decoding the result of %s: %w", method, err)
		}

		return nil

	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // callers compare with context errors

	case <-c.closed:
		return c.connectionError()
	}
}

// Notify sends a request that expects no reply.
func (c *Client) Notify(method string, params any) error {
	if err := c.connectionError(); err != nil {
		return err
	}

	request := map[string]any{"jsonrpc": protocolVersion, "method": method}
	if params != nil {
		request["params"] = params
	}

	return c.write(request)
}

func (c *Client) write(frame any) error {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()

	if err := c.encoder.Encode(frame); err != nil {
		return fmt.Errorf("sending to the daemon: %w", err)
	}

	return nil
}

// readLoop routes replies to whoever is waiting for them, and notifications to the handler.
func (c *Client) readLoop() {
	defer c.shutdown(nil)

	lines := bufio.NewScanner(c.connection)
	lines.Buffer(make([]byte, 0, 4096), maxFrameSize)

	for lines.Scan() {
		var frame struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}

		if err := json.Unmarshal(lines.Bytes(), &frame); err != nil {
			c.shutdown(fmt.Errorf("the daemon sent something unreadable: %w", err))
			return
		}

		if frame.ID == nil {
			if c.onNotification != nil && frame.Method != "" {
				c.onNotification(frame.Method, frame.Params)
			}

			continue
		}

		c.deliver(*frame.ID, response{Result: frame.Result, Error: frame.Error})
	}

	if err := lines.Err(); err != nil {
		c.shutdown(fmt.Errorf("reading from the daemon: %w", err))
	}
}

func (c *Client) deliver(requestID int64, reply response) {
	c.pendingMutex.Lock()
	replies, waiting := c.pending[requestID]
	c.pendingMutex.Unlock()

	if waiting {
		replies <- reply
	}
}

// shutdown records why the connection ended and releases everyone waiting on it.
func (c *Client) shutdown(cause error) {
	c.closeOnce.Do(func() {
		if cause == nil {
			cause = ErrNotConnected
		}
		c.readErr.Store(&cause)
		close(c.closed)
		_ = c.connection.Close()
	})
}

func (c *Client) connectionError() error {
	select {
	case <-c.closed:
		if cause := c.readErr.Load(); cause != nil {
			return *cause
		}
		return ErrNotConnected
	default:
		return nil
	}
}

// Close hangs up. Any call still waiting fails, and the daemon releases whatever that call was
// waiting for — a queue place, above all.
func (c *Client) Close() error {
	c.shutdown(ErrNotConnected)

	return nil
}

// Done is closed when the connection ends, so a caller can notice without making a call.
func (c *Client) Done() <-chan struct{} { return c.closed }

// DefaultSocketPath resolves where the daemon listens, following the same rules the daemon uses.
func DefaultSocketPath() (string, error) {
	if fromEnvironment := os.Getenv("LAC_SOCKET"); fromEnvironment != "" {
		return fromEnvironment, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving the home directory: %w", err)
	}

	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(runtimeDir) {
		return filepath.Join(runtimeDir, "lac", "lacd.sock"), nil
	}

	stateHome := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(stateHome) {
		stateHome = filepath.Join(home, ".local", "state")
	}

	return filepath.Join(stateHome, "lac", "run", "lacd.sock"), nil
}
