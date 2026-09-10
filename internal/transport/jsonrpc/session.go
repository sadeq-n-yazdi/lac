package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// ErrSessionClosed means the connection went away.
var ErrSessionClosed = errors.New("the session is closed")

// Session is one client connection. It carries whatever the authentication layer learned about the
// caller, and it is how the server pushes notifications back to that particular client.
//
// Every write goes through one mutex, so responses written concurrently by several in-flight
// handlers never interleave into a corrupt frame.
type Session struct {
	connection net.Conn
	encoder    *json.Encoder
	writeMutex sync.Mutex

	// context is cancelled when the connection closes, so a blocking handler stops waiting as soon
	// as the client that asked has gone away.
	context context.Context //nolint:containedctx // the connection's lifetime is the session
	cancel  context.CancelFunc

	stateMutex sync.RWMutex
	agentID    string
	values     map[string]any

	closeOnce sync.Once
}

func newSession(parent context.Context, connection net.Conn) *Session {
	ctx, cancel := context.WithCancel(parent)

	return &Session{
		connection: connection,
		encoder:    json.NewEncoder(connection),
		context:    ctx,
		cancel:     cancel,
		values:     make(map[string]any),
	}
}

// Context is cancelled when the connection closes.
func (s *Session) Context() context.Context { return s.context }

// RemoteAddr identifies the connection, for logging.
func (s *Session) RemoteAddr() string { return s.connection.RemoteAddr().String() }

// AgentID returns the authenticated agent, or an empty string before authentication.
func (s *Session) AgentID() string {
	s.stateMutex.RLock()
	defer s.stateMutex.RUnlock()

	return s.agentID
}

// SetAgentID records who this connection belongs to. The authentication layer calls it; handlers
// read it rather than trusting an agent id in the request body, which is what stops one agent
// acting as another.
func (s *Session) SetAgentID(agentID string) {
	s.stateMutex.Lock()
	defer s.stateMutex.Unlock()

	s.agentID = agentID
}

// Set attaches a value to the session, for a transport-level concern such as a subscription.
func (s *Session) Set(key string, value any) {
	s.stateMutex.Lock()
	defer s.stateMutex.Unlock()

	s.values[key] = value
}

// Get reads a value attached with Set.
func (s *Session) Get(key string) (any, bool) {
	s.stateMutex.RLock()
	defer s.stateMutex.RUnlock()

	value, found := s.values[key]

	return value, found
}

// Notify pushes a notification to this client. It is how a waiting agent hears that its slot is
// ready without polling for it.
func (s *Session) Notify(method string, params any) error {
	return s.write(Notification{JSONRPC: Version, Method: method, Params: params})
}

// write serialises one frame onto the connection.
func (s *Session) write(frame any) error {
	select {
	case <-s.context.Done():
		return ErrSessionClosed
	default:
	}

	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()

	if err := s.encoder.Encode(frame); err != nil {
		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			return ErrSessionClosed
		}
		return fmt.Errorf("writing to %s: %w", s.RemoteAddr(), err)
	}

	return nil
}

// close ends the session, cancelling anything still running on its behalf.
func (s *Session) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.connection.Close()
	})
}
