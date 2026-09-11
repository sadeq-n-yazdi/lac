package jsonrpc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/transport/jsonrpc"
)

// client is a minimal JSON-RPC client, enough to drive the server from a test.
type client struct {
	connection net.Conn
	lines      *bufio.Scanner
	encoder    *json.Encoder
}

func (c *client) send(t *testing.T, frame any) {
	t.Helper()

	if err := c.encoder.Encode(frame); err != nil {
		t.Fatalf("writing a request: %v", err)
	}
}

// receive reads one frame, failing the test if nothing arrives in time.
func (c *client) receive(t *testing.T) jsonrpc.Response {
	t.Helper()

	if err := c.connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}
	if !c.lines.Scan() {
		t.Fatalf("no reply arrived: %v", c.lines.Err())
	}

	var response jsonrpc.Response
	if err := json.Unmarshal(c.lines.Bytes(), &response); err != nil {
		t.Fatalf("decoding the reply %q: %v", c.lines.Text(), err)
	}

	return response
}

// receiveRaw reads one frame without interpreting it as a response.
func (c *client) receiveRaw(t *testing.T) map[string]any {
	t.Helper()

	if err := c.connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}
	if !c.lines.Scan() {
		t.Fatalf("no frame arrived: %v", c.lines.Err())
	}

	var frame map[string]any
	if err := json.Unmarshal(c.lines.Bytes(), &frame); err != nil {
		t.Fatalf("decoding the frame %q: %v", c.lines.Text(), err)
	}

	return frame
}

func (c *client) sendRaw(t *testing.T, text string) {
	t.Helper()

	if _, err := io.WriteString(c.connection, text); err != nil {
		t.Fatalf("writing raw bytes: %v", err)
	}
}

// serve starts a server on an in-process listener and returns a connected client.
func serve(t *testing.T, router *jsonrpc.Router, options jsonrpc.Options) (*jsonrpc.Server, *client) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	server := jsonrpc.NewServer(router, options)
	ctx, cancel := context.WithCancel(t.Context())

	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()

	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}

	t.Cleanup(func() {
		_ = connection.Close()
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve() = %v, want nil", err)
		}
	})

	return server, &client{
		connection: connection,
		lines:      bufio.NewScanner(connection),
		encoder:    json.NewEncoder(connection),
	}
}

func request(id int, method string, params any) map[string]any {
	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}

	return frame
}

func echoRouter(t *testing.T) *jsonrpc.Router {
	t.Helper()

	router := jsonrpc.NewRouter()
	router.Register("echo", func(_ context.Context, _ *jsonrpc.Session, params json.RawMessage) (any, error) {
		var arguments struct {
			Text string `json:"text"`
		}
		if err := jsonrpc.ParseParams(params, &arguments); err != nil {
			return nil, err
		}

		return map[string]string{"text": arguments.Text}, nil
	})

	return router
}

func TestCallReturnsAResult(t *testing.T) {
	_, connection := serve(t, echoRouter(t), jsonrpc.Options{})

	connection.send(t, request(1, "echo", map[string]string{"text": "hello"}))
	response := connection.receive(t)

	if response.Error != nil {
		t.Fatalf("the call failed: %+v", response.Error)
	}
	if string(response.ID) != "1" {
		t.Errorf("response id = %s, want 1", response.ID)
	}

	result, ok := response.Result.(map[string]any)
	if !ok || result["text"] != "hello" {
		t.Errorf("result = %v, want the echoed text", response.Result)
	}
}

func TestProtocolErrors(t *testing.T) {
	tests := []struct {
		name     string
		frame    map[string]any
		wantCode int
	}{
		{
			name:     "unknown method",
			frame:    request(1, "nonexistent", nil),
			wantCode: jsonrpc.CodeMethodNotFound,
		},
		{
			name:     "wrong version",
			frame:    map[string]any{"jsonrpc": "1.0", "id": 1, "method": "echo"},
			wantCode: jsonrpc.CodeInvalidRequest,
		},
		{
			name:     "no method",
			frame:    map[string]any{"jsonrpc": "2.0", "id": 1},
			wantCode: jsonrpc.CodeInvalidRequest,
		},
		{
			name:     "unknown parameter",
			frame:    request(1, "echo", map[string]string{"txet": "typo"}),
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "parameters of the wrong type",
			frame:    request(1, "echo", map[string]int{"text": 42}),
			wantCode: jsonrpc.CodeInvalidParams,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, connection := serve(t, echoRouter(t), jsonrpc.Options{})

			connection.send(t, test.frame)
			response := connection.receive(t)

			if response.Error == nil {
				t.Fatalf("the call succeeded; want error code %d", test.wantCode)
			}
			if response.Error.Code != test.wantCode {
				t.Errorf("error code = %d (%s), want %d",
					response.Error.Code, response.Error.Message, test.wantCode)
			}
		})
	}
}

// Unparseable input must still produce a reply, or the client waits forever for something that is
// never coming.
func TestMalformedJSONIsReported(t *testing.T) {
	_, connection := serve(t, echoRouter(t), jsonrpc.Options{})

	connection.sendRaw(t, "{this is not json}\n")
	response := connection.receive(t)

	if response.Error == nil || response.Error.Code != jsonrpc.CodeParseError {
		t.Fatalf("response = %+v, want a parse error", response)
	}
	if string(response.ID) != "null" {
		t.Errorf("response id = %s, want null", response.ID)
	}
}

// Domain errors must reach the client as codes it can act on, not as prose it has to parse.
func TestDomainErrorsMapToCodes(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "unauthorised", err: core.ErrUnauthorised, wantCode: jsonrpc.CodeUnauthorised},
		{name: "not found", err: core.ErrNotFound, wantCode: jsonrpc.CodeNotFound},
		{name: "already exists", err: core.ErrAlreadyExists, wantCode: jsonrpc.CodeAlreadyExists},
		{name: "conflict", err: core.ErrConflict, wantCode: jsonrpc.CodeConflict},
		{name: "capacity", err: core.ErrCapacityReached, wantCode: jsonrpc.CodeCapacityReached},
		{name: "invalid argument", err: core.ErrInvalidArgument, wantCode: jsonrpc.CodeInvalidParams},
		{name: "anything else", err: errors.New("the disk caught fire"), wantCode: jsonrpc.CodeInternalError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := jsonrpc.NewRouter()
			router.Register("fail", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
				return nil, test.err
			})
			_, connection := serve(t, router, jsonrpc.Options{})

			connection.send(t, request(1, "fail", nil))
			response := connection.receive(t)

			if response.Error == nil {
				t.Fatalf("the call succeeded; want error code %d", test.wantCode)
			}
			if response.Error.Code != test.wantCode {
				t.Errorf("error code = %d, want %d", response.Error.Code, test.wantCode)
			}
		})
	}
}

// A notification is fire-and-forget: replying to one would leave an unexpected frame in the
// stream and confuse the next read.
func TestNotificationsGetNoReply(t *testing.T) {
	handled := make(chan struct{}, 1)

	router := jsonrpc.NewRouter()
	router.Register("note", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		handled <- struct{}{}
		return "ignored", nil
	})
	router.Register("ping", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		return "pong", nil
	})

	_, connection := serve(t, router, jsonrpc.Options{})

	connection.send(t, map[string]any{"jsonrpc": "2.0", "method": "note"})
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the notification was never handled")
	}

	// The next reply must be for the ping, proving nothing was written for the notification.
	connection.send(t, request(7, "ping", nil))
	response := connection.receive(t)

	if string(response.ID) != "7" {
		t.Errorf("response id = %s, want 7: something was written for the notification", response.ID)
	}
}

// A blocking call is the normal case here — waiting for a resource slot — so other calls on the
// same connection must not be stuck behind it.
func TestCallsOnOneConnectionRunConcurrently(t *testing.T) {
	release := make(chan struct{})

	router := jsonrpc.NewRouter()
	router.Register("block", func(ctx context.Context, _ *jsonrpc.Session, _ json.RawMessage) (any, error) {
		select {
		case <-release:
			return "released", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	router.Register("quick", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		return "immediate", nil
	})

	_, connection := serve(t, router, jsonrpc.Options{})

	connection.send(t, request(1, "block", nil))
	connection.send(t, request(2, "quick", nil))

	// The quick call must answer while the blocking one is still parked.
	first := connection.receive(t)
	if string(first.ID) != "2" {
		t.Fatalf("the first reply was for id %s, want the quick call to overtake the blocked one", first.ID)
	}

	close(release)
	second := connection.receive(t)
	if string(second.ID) != "1" {
		t.Errorf("the second reply was for id %s, want 1", second.ID)
	}
}

// A handler waiting on a queue must stop waiting when the client that asked disconnects,
// otherwise a crashed agent would keep its place in line forever.
func TestHandlerContextEndsWithTheConnection(t *testing.T) {
	cancelled := make(chan error, 1)

	router := jsonrpc.NewRouter()
	router.Register("block", func(ctx context.Context, _ *jsonrpc.Session, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		cancelled <- ctx.Err()

		return nil, ctx.Err()
	})

	_, connection := serve(t, router, jsonrpc.Options{})
	connection.send(t, request(1, "block", nil))

	// Give the handler a moment to start, then hang up on it.
	time.Sleep(50 * time.Millisecond)
	if err := connection.connection.Close(); err != nil {
		t.Fatalf("closing the connection: %v", err)
	}

	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("handler context error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the handler was never cancelled when its client disconnected")
	}
}

// A panicking handler must not take the daemon with it: the other agents still hold queue places.
func TestPanicIsContained(t *testing.T) {
	router := jsonrpc.NewRouter()
	router.Register("panic", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		panic("something went badly wrong")
	})
	router.Register("ping", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		return "pong", nil
	})

	_, connection := serve(t, router, jsonrpc.Options{Logger: quietLogger()})

	connection.send(t, request(1, "panic", nil))
	response := connection.receive(t)

	if response.Error == nil || response.Error.Code != jsonrpc.CodeInternalError {
		t.Fatalf("response = %+v, want an internal error", response)
	}

	connection.send(t, request(2, "ping", nil))
	if follow := connection.receive(t); follow.Error != nil {
		t.Errorf("the server stopped working after a panic: %+v", follow.Error)
	}
}

// Notifications are how an agent learns its slot is ready without polling for it.
func TestServerPushesNotifications(t *testing.T) {
	router := jsonrpc.NewRouter()
	router.Register("subscribe", func(_ context.Context, session *jsonrpc.Session, _ json.RawMessage) (any, error) {
		session.SetAgentID("agent_1")
		return "subscribed", nil
	})

	server, connection := serve(t, router, jsonrpc.Options{})

	connection.send(t, request(1, "subscribe", nil))
	if response := connection.receive(t); response.Error != nil {
		t.Fatalf("subscribe failed: %+v", response.Error)
	}

	if reached := server.Broadcast("agent_1", "lease.granted", map[string]string{"resource": "test"}); reached != 1 {
		t.Fatalf("Broadcast() reached %d sessions, want 1", reached)
	}

	frame := connection.receiveRaw(t)
	if frame["method"] != "lease.granted" {
		t.Errorf("notification = %v, want the lease.granted method", frame)
	}
	if _, hasID := frame["id"]; hasID {
		t.Error("a notification must not carry an id")
	}
}

// Broadcasting to an agent nobody is holding a session for must be a quiet no-op, not an error.
func TestBroadcastToAnAbsentAgent(t *testing.T) {
	server, _ := serve(t, echoRouter(t), jsonrpc.Options{})

	if reached := server.Broadcast("agent_nobody", "lease.granted", nil); reached != 0 {
		t.Errorf("Broadcast() reached %d sessions, want 0", reached)
	}
}

// OnConnect is where authentication will live, so refusing there must actually keep the client out.
func TestOnConnectCanRejectAConnection(t *testing.T) {
	router := echoRouter(t)
	options := jsonrpc.Options{
		Logger:    quietLogger(),
		OnConnect: func(*jsonrpc.Session) error { return errors.New("not welcome") },
	}

	_, connection := serve(t, router, options)
	connection.send(t, request(1, "echo", map[string]string{"text": "hello"}))

	if err := connection.connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}
	if connection.lines.Scan() {
		t.Errorf("the rejected connection was served: %q", connection.lines.Text())
	}
}

func TestOnDisconnectRuns(t *testing.T) {
	disconnected := make(chan string, 1)
	options := jsonrpc.Options{
		OnDisconnect: func(session *jsonrpc.Session) { disconnected <- session.AgentID() },
	}

	router := jsonrpc.NewRouter()
	router.Register("identify", func(_ context.Context, session *jsonrpc.Session, _ json.RawMessage) (any, error) {
		session.SetAgentID("agent_1")
		return nil, nil //nolint:nilnil // a method with nothing to return
	})

	_, connection := serve(t, router, options)
	connection.send(t, request(1, "identify", nil))
	connection.receive(t)

	if err := connection.connection.Close(); err != nil {
		t.Fatalf("closing the connection: %v", err)
	}

	select {
	case agentID := <-disconnected:
		if agentID != "agent_1" {
			t.Errorf("OnDisconnect saw agent %q, want agent_1", agentID)
		}
	case <-time.After(5 * time.Second):
		t.Error("OnDisconnect never ran")
	}
}

// Shutdown must let work in progress finish: an agent mid-test-run should not lose its answer.
func TestShutdownWaitsForInFlightCalls(t *testing.T) {
	started := make(chan struct{})
	router := jsonrpc.NewRouter()
	router.Register("slow", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		close(started)
		time.Sleep(200 * time.Millisecond)

		return "finished", nil
	})

	server, connection := serve(t, router, jsonrpc.Options{ShutdownGrace: 5 * time.Second})

	connection.send(t, request(1, "slow", nil))
	<-started

	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}

	response := connection.receive(t)
	if response.Result != "finished" {
		t.Errorf("result = %v, want the call to have finished", response.Result)
	}
}

// A handler that will not stop must not hold the daemon open forever; the grace period is the
// limit, and exceeding it is reported rather than hidden.
func TestShutdownGiveUpAfterTheGracePeriod(t *testing.T) {
	router := jsonrpc.NewRouter()
	router.Register("stuck", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		time.Sleep(2 * time.Second)
		return nil, nil //nolint:nilnil // a method with nothing to return
	})

	server, connection := serve(t, router, jsonrpc.Options{
		Logger:        quietLogger(),
		ShutdownGrace: 50 * time.Millisecond,
	})

	connection.send(t, request(1, "stuck", nil))
	time.Sleep(50 * time.Millisecond)

	err := server.Shutdown(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown() = %v, want a deadline exceeded error", err)
	}
}

func TestRouterMethods(t *testing.T) {
	router := jsonrpc.NewRouter()
	for _, method := range []string{"lease.acquire", "agent.register", "message.send"} {
		router.Register(method, func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
			return nil, nil //nolint:nilnil // a stub
		})
	}

	got := strings.Join(router.Methods(), ",")
	if want := "agent.register,lease.acquire,message.send"; got != want {
		t.Errorf("Methods() = %q, want %q sorted", got, want)
	}
}

// Registering the same method twice is a start-up mistake, and a silent overwrite would mean the
// daemon quietly serves the wrong handler.
func TestRegisteringTwicePanics(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Error("registering a duplicate method did not panic")
		}
	}()

	router := jsonrpc.NewRouter()
	handler := func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		return nil, nil //nolint:nilnil // a stub
	}
	router.Register("echo", handler)
	router.Register("echo", handler)
}
