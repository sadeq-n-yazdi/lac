package jsonrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// maxConcurrentCallsPerSession bounds how many requests one connection can have in flight. Calls
// here block on purpose — waiting for a resource slot is the point — so without a bound a single
// client could pin an unbounded number of goroutines.
const maxConcurrentCallsPerSession = 64

// maxFrameSize bounds one request. LAC's messages are small; anything larger is a mistake or an
// attempt to exhaust memory.
const maxFrameSize = 4 << 20

// defaultShutdownGrace is how long in-flight calls have to finish when nothing else is configured.
const defaultShutdownGrace = 5 * time.Second

// Options configure a Server.
type Options struct {
	// Logger records connection-level events. Defaults to slog.Default.
	Logger *slog.Logger
	// ShutdownGrace is how long in-flight calls have to finish on shutdown.
	ShutdownGrace time.Duration
	// OnConnect, when set, runs for each new connection before any request is read. Returning an
	// error rejects the connection, which is where a transport-level greeting or handshake goes.
	OnConnect func(session *Session) error
	// OnDisconnect, when set, runs after a connection ends. It is where per-connection state, such
	// as a subscription, is cleaned up.
	OnDisconnect func(session *Session)
}

// Server reads JSON-RPC requests from accepted connections and dispatches them through a Router.
type Server struct {
	router  *Router
	logger  *slog.Logger
	grace   time.Duration
	onOpen  func(*Session) error
	onClose func(*Session)

	sessions sync.Map // *Session -> struct{}
	// connections counts the goroutines serving a client, and calls counts the handlers running
	// inside them. Shutdown waits for the calls — that is what "in flight" means — and only then
	// hangs up on connections that are merely idle.
	connections sync.WaitGroup
	calls       sync.WaitGroup

	// lifecycle guards stopping, so that "is the server still accepting calls?" and "start
	// counting another call" happen as one step. Without that, a call could be registered just
	// after Shutdown decided nothing was left to wait for.
	lifecycle sync.Mutex
	stopped   bool
}

// NewServer returns a server that dispatches through the router.
func NewServer(router *Router, options Options) *Server {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	grace := options.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}

	return &Server{
		router:  router,
		logger:  logger,
		grace:   grace,
		onOpen:  options.OnConnect,
		onClose: options.OnDisconnect,
	}
}

// beginCall registers a call as in flight, or reports that the server is shutting down and the
// call must be refused.
func (s *Server) beginCall() bool {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()

	if s.stopped {
		return false
	}
	s.calls.Add(1)

	return true
}

// Serve accepts connections until the listener is closed or the context is cancelled.
//
// It returns nil for an ordinary shutdown, so a caller can treat any non-nil result as a genuine
// failure rather than having to recognise "the listener was closed on purpose".
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A cancelled context must stop the blocking Accept, and closing the listener is the only way
	// to do that.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		connection, err := listener.Accept()
		if err != nil {
			if s.stopping() || ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accepting a connection: %w", err)
		}

		s.connections.Add(1)
		go func() {
			defer s.connections.Done()
			s.serveConnection(ctx, connection)
		}()
	}
}

// serveConnection reads requests from one client until it goes away.
func (s *Server) serveConnection(ctx context.Context, connection net.Conn) {
	session := newSession(ctx, connection)
	s.sessions.Store(session, struct{}{})

	defer func() {
		s.sessions.Delete(session)
		session.close()
		if s.onClose != nil {
			s.onClose(session)
		}
	}()

	if s.onOpen != nil {
		if err := s.onOpen(session); err != nil {
			s.logger.Warn("rejected a connection", "remote", session.RemoteAddr(), "error", err)
			return
		}
	}

	// The session's context is derived from ctx and additionally ends when this connection does,
	// which is what stops a handler waiting for a client that has already gone.
	s.readLoop(session.Context(), session) //nolint:contextcheck // session.Context() descends from ctx
}

// readLoop reads one newline-delimited JSON request per line. Line framing, rather than a
// streaming decoder, is what lets each frame be size-limited on its own: a limit across the whole
// stream would quietly cut off a long-lived connection.
func (s *Server) readLoop(ctx context.Context, session *Session) {
	lines := bufio.NewScanner(session.connection)
	lines.Buffer(make([]byte, 0, 4096), maxFrameSize)

	inFlight := make(chan struct{}, maxConcurrentCallsPerSession)

	var pending sync.WaitGroup

	// Deferred calls run last-in-first-out, so the session is closed — cancelling every handler
	// still running for this client — before we wait for those handlers to finish. The other order
	// deadlocks: a handler parked on a queue would be waiting for a cancellation that only arrives
	// once it has returned.
	defer pending.Wait()
	defer session.close()

	for lines.Scan() {
		line := bytes.TrimSpace(lines.Bytes())
		if len(line) == 0 {
			continue
		}

		var request Request
		if err := json.Unmarshal(line, &request); err != nil {
			s.respondUnattributed(session, Errorf(CodeParseError, "malformed request: %v", err))
			return
		}

		if err := validateRequest(request); err != nil {
			s.respond(session, request.ID, nil, err)
			continue
		}

		select {
		case inFlight <- struct{}{}:
		case <-ctx.Done():
			return
		}

		if !s.beginCall() {
			<-inFlight
			s.respond(session, request.ID, nil,
				NewError(CodeShuttingDown, "the daemon is shutting down; retry once it is back"))

			continue
		}

		pending.Add(1)

		go func() {
			defer pending.Done()
			defer s.calls.Done()
			defer func() { <-inFlight }()

			s.dispatch(ctx, session, request)
		}()
	}

	if err := lines.Err(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		if errors.Is(err, bufio.ErrTooLong) {
			s.respondUnattributed(session, Errorf(CodeInvalidRequest,
				"the request exceeds the %d byte limit", maxFrameSize))
			return
		}
		s.logger.Debug("a connection ended", "remote", session.RemoteAddr(), "error", err)
	}
}

// validateRequest rejects anything that is not a well-formed 2.0 request.
func validateRequest(request Request) *Error {
	if request.JSONRPC != Version {
		return Errorf(CodeInvalidRequest, "jsonrpc must be %q, got %q", Version, request.JSONRPC)
	}
	if request.Method == "" {
		return NewError(CodeInvalidRequest, "a request needs a method")
	}

	return nil
}

// dispatch runs one call and replies, unless the caller asked for no reply.
func (s *Server) dispatch(ctx context.Context, session *Session, request Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// A panicking handler must not take the daemon with it: the other agents are still
			// depending on their queue positions.
			s.logger.Error("a handler panicked",
				"method", request.Method, "remote", session.RemoteAddr(), "panic", recovered)
			s.respond(session, request.ID, nil,
				Errorf(CodeInternalError, "the %s handler failed unexpectedly", request.Method))
		}
	}()

	handler, found := s.router.handlerFor(request.Method)
	if !found {
		s.respond(session, request.ID, nil, Errorf(CodeMethodNotFound, "unknown method %q", request.Method))
		return
	}

	result, err := handler(ctx, session, request.Params)
	if err != nil {
		s.logger.Debug("a call failed",
			"method", request.Method, "agent", session.AgentID(), "error", err)
		s.respond(session, request.ID, nil, errorFrom(err))
		return
	}

	s.respond(session, request.ID, result, nil)
}

// respondUnattributed reports a failure that happened before a request id could be read, such as
// unparseable JSON. The protocol asks for a null id here, and saying nothing at all would leave the
// client waiting on a reply that is never coming.
func (s *Server) respondUnattributed(session *Session, wireError *Error) {
	response := Response{JSONRPC: Version, ID: json.RawMessage("null"), Error: wireError}

	if err := session.write(response); err != nil && !errors.Is(err, ErrSessionClosed) {
		s.logger.Warn("could not report a malformed request", "remote", session.RemoteAddr(), "error", err)
	}
}

// respond writes a reply, unless the request was a notification, which by definition gets none.
func (s *Server) respond(session *Session, requestID json.RawMessage, result any, wireError *Error) {
	if len(requestID) == 0 || string(requestID) == "null" {
		return
	}

	response := Response{JSONRPC: Version, ID: requestID, Result: result, Error: wireError}

	if err := session.write(response); err != nil && !errors.Is(err, ErrSessionClosed) {
		s.logger.Warn("could not write a response", "remote", session.RemoteAddr(), "error", err)
	}
}

// Broadcast pushes a notification to every session an agent currently has open. It reports how
// many were reached, so a caller can tell "nobody was listening" from "everybody got it".
func (s *Server) Broadcast(agentID, method string, params any) int {
	reached := 0

	s.sessions.Range(func(key, _ any) bool {
		session, ok := key.(*Session)
		if !ok || session.AgentID() != agentID {
			return true
		}
		if err := session.Notify(method, params); err != nil {
			s.logger.Debug("could not notify a session",
				"agent", agentID, "method", method, "error", err)
			return true
		}
		reached++

		return true
	})

	return reached
}

// Shutdown lets in-flight calls finish within the grace period, then hangs up on every connection.
//
// The caller is expected to have stopped Serve first, by cancelling the context it was given, so
// no new connection arrives while this is running.
func (s *Server) Shutdown(ctx context.Context) error {
	s.lifecycle.Lock()
	s.stopped = true
	s.lifecycle.Unlock()

	deadline, cancel := context.WithTimeout(ctx, s.grace)
	defer cancel()

	finished := make(chan struct{})
	go func() {
		s.calls.Wait()
		close(finished)
	}()

	var shutdownErr error

	select {
	case <-finished:
	case <-deadline.Done():
		// The grace period is over. Closing the sessions cancels their contexts, which is what
		// releases a handler still parked on a queue.
		shutdownErr = fmt.Errorf("shutdown: in-flight calls did not finish: %w", context.DeadlineExceeded)
	}

	s.closeSessions()
	s.calls.Wait()
	s.connections.Wait()

	return shutdownErr
}

func (s *Server) closeSessions() {
	s.sessions.Range(func(key, _ any) bool {
		if session, ok := key.(*Session); ok {
			session.close()
		}
		return true
	})
}

// stopping reports whether Shutdown has been called.
func (s *Server) stopping() bool {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()

	return s.stopped
}
