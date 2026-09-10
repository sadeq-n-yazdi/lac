package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/config"
	"code.sadeq.uk/lac/internal/daemon"
)

// session drives the MCP server the way a real client does: JSON on stdin, JSON on stdout.
type session struct {
	requests  *io.PipeWriter
	responses *bufio.Scanner
	served    chan error
	nextID    int
	mutex     sync.Mutex
}

// newSession starts an MCP server against a running daemon.
func newSession(t *testing.T, socketPath string, options Options) *session {
	t.Helper()

	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()

	options.SocketPath = socketPath
	options.Logger = quietLogger()

	server := newServer(requestReader, responseWriter, options)

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)

	go func() {
		err := server.Serve(ctx)
		_ = responseWriter.Close()
		served <- err
	}()

	subject := &session{
		requests:  requestWriter,
		responses: bufio.NewScanner(responseReader),
		served:    served,
	}

	t.Cleanup(func() {
		_ = requestWriter.Close()
		stop()

		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve() = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the MCP server did not stop when its input closed")
		}
	})

	return subject
}

// call sends a request and returns the decoded reply.
func (s *session) call(t *testing.T, method string, params any) response {
	t.Helper()

	s.mutex.Lock()
	s.nextID++
	requestID := s.nextID
	s.mutex.Unlock()

	frame := map[string]any{"jsonrpc": "2.0", "id": requestID, "method": method}
	if params != nil {
		frame["params"] = params
	}

	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	if _, err := s.requests.Write(append(encoded, '\n')); err != nil {
		t.Fatalf("sending %s: %v", method, err)
	}

	if !s.responses.Scan() {
		t.Fatalf("no reply to %s: %v", method, s.responses.Err())
	}

	var reply response
	if err := json.Unmarshal(s.responses.Bytes(), &reply); err != nil {
		t.Fatalf("decoding the reply to %s: %v", method, err)
	}

	return reply
}

// callTool invokes a tool and returns its text, failing the test if the tool reported an error.
func (s *session) callTool(t *testing.T, name string, arguments map[string]any) string {
	t.Helper()

	result := s.toolResult(t, name, arguments)
	if result.IsError {
		t.Fatalf("%s failed: %s", name, textOfResult(result))
	}

	return textOfResult(result)
}

func (s *session) toolResult(t *testing.T, name string, arguments map[string]any) callToolResult {
	t.Helper()

	params := map[string]any{"name": name}
	if arguments != nil {
		params["arguments"] = arguments
	}

	reply := s.call(t, "tools/call", params)
	if reply.Error != nil {
		t.Fatalf("tools/call %s returned a protocol error: %+v", name, reply.Error)
	}

	var result callToolResult
	decodeResult(t, reply, &result)

	return result
}

func textOfResult(result callToolResult) string {
	parts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		parts = append(parts, item.Text)
	}

	return strings.Join(parts, "\n")
}

func decodeResult(t *testing.T, reply response, target any) {
	t.Helper()

	encoded, err := json.Marshal(reply.Result)
	if err != nil {
		t.Fatalf("re-encoding the result: %v", err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		t.Fatalf("decoding the result: %v", err)
	}
}

// startDaemon brings up a real LAC daemon for the MCP server to talk to.
func startDaemon(t *testing.T, resources []config.ResourceConfig) string {
	t.Helper()

	home, err := os.MkdirTemp("", "lac")
	if err != nil {
		t.Fatalf("creating a temporary home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", "")

	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	configuration.Resources = resources
	configuration.AllowedWorkdirRoots = []string{home, os.TempDir(), "/tmp"}

	instance, err := daemon.New(t.Context(), configuration, quietLogger())
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- instance.Run(ctx) }()

	t.Cleanup(func() {
		stop()
		if err := <-served; err != nil {
			t.Errorf("the daemon failed: %v", err)
		}
		if err := instance.Close(); err != nil {
			t.Errorf("closing the daemon: %v", err)
		}
	})

	return instance.SocketPath()
}

func workdir(t *testing.T) string {
	t.Helper()

	directory, err := os.MkdirTemp("", "lacwork")
	if err != nil {
		t.Fatalf("creating a working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	return directory
}

// A client states the protocol revision it speaks; a server that answers with something else has
// to be understood by the client anyway, so echoing a known version back is the whole handshake.
func TestInitializeNegotiatesTheProtocol(t *testing.T) {
	socketPath := startDaemon(t, nil)
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	tests := map[string]string{
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		"1999-01-01": preferredProtocolVersion,
		"":           preferredProtocolVersion,
	}

	for requested, want := range tests {
		t.Run(requested, func(t *testing.T) {
			reply := subject.call(t, "initialize", map[string]any{
				"protocolVersion": requested,
				"clientInfo":      map[string]string{"name": "test-client", "version": "1.0"},
			})
			if reply.Error != nil {
				t.Fatalf("initialize failed: %+v", reply.Error)
			}

			var result initializeResult
			decodeResult(t, reply, &result)

			if result.ProtocolVersion != want {
				t.Errorf("protocolVersion = %q, want %q", result.ProtocolVersion, want)
			}
		})
	}
}

// The instructions are the etiquette an agent needs before it does anything, so they must actually
// be sent and must name the tools they talk about.
func TestInitializeSendsInstructionsAndCapabilities(t *testing.T) {
	socketPath := startDaemon(t, nil)
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	reply := subject.call(t, "initialize", map[string]any{
		"protocolVersion": preferredProtocolVersion,
		"clientInfo":      map[string]string{"name": "claude-code", "version": "1.0"},
	})

	var result initializeResult
	decodeResult(t, reply, &result)

	if result.Capabilities.Tools == nil || result.Capabilities.Resources == nil {
		t.Errorf("capabilities = %+v, want both tools and resources", result.Capabilities)
	}
	if result.ServerInfo.Name != "lac" {
		t.Errorf("server name = %q, want lac", result.ServerInfo.Name)
	}
	for _, mentioned := range []string{"lac_acquire_slot", "lac_release_slot", "lac_agents"} {
		if !strings.Contains(result.Instructions, mentioned) {
			t.Errorf("the instructions never mention %s", mentioned)
		}
	}
}

// Every tool must be listed with a schema, or a model cannot call it correctly.
func TestToolsAreListedWithSchemas(t *testing.T) {
	socketPath := startDaemon(t, nil)
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	reply := subject.call(t, "tools/list", nil)
	if reply.Error != nil {
		t.Fatalf("tools/list failed: %+v", reply.Error)
	}

	var result toolListResult
	decodeResult(t, reply, &result)

	if len(result.Tools) == 0 {
		t.Fatal("no tools were listed")
	}

	handlers := toolHandlers()
	for _, listed := range result.Tools {
		if _, found := handlers[listed.Name]; !found {
			t.Errorf("the tool %q is listed but has no handler", listed.Name)
		}
		if listed.Description == "" {
			t.Errorf("the tool %q has no description, so a model cannot tell when to use it", listed.Name)
		}
		if listed.InputSchema["type"] != "object" {
			t.Errorf("the schema for %q is not an object: %v", listed.Name, listed.InputSchema)
		}
	}

	if len(handlers) != len(result.Tools) {
		t.Errorf("%d handlers but %d tools listed; one of them is unreachable",
			len(handlers), len(result.Tools))
	}
}

// The whole point, through MCP: take a slot, hold it, give it back.
func TestAcquireHoldAndReleaseASlot(t *testing.T) {
	socketPath := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 1}})
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	granted := subject.callTool(t, "lac_acquire_slot", map[string]any{
		"resource": "test", "reason": "running the parser tests",
	})
	if !strings.Contains(granted, "Lease id:") {
		t.Fatalf("lac_acquire_slot said %q, want it to give a lease id", granted)
	}

	held := subject.callTool(t, "lac_my_slots", nil)
	if !strings.Contains(held, "test") {
		t.Errorf("lac_my_slots said %q, want the slot that was granted", held)
	}

	leaseID := leaseIDFrom(t, granted)

	released := subject.callTool(t, "lac_release_slot", map[string]any{"lease_id": leaseID})
	if !strings.Contains(released, "released") {
		t.Errorf("lac_release_slot said %q", released)
	}

	if after := subject.callTool(t, "lac_my_slots", nil); !strings.Contains(after, "not holding") {
		t.Errorf("lac_my_slots after releasing said %q, want nothing held", after)
	}
}

// A full resource with no_wait must come back as advice the model can act on, not as a failure it
// has to interpret.
func TestNoWaitOnAFullResourceExplainsItself(t *testing.T) {
	socketPath := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 1}})

	holder := newSession(t, socketPath, Options{AgentName: "holder", Workdir: workdir(t)})
	hopeful := newSession(t, socketPath, Options{AgentName: "hopeful", Workdir: workdir(t)})

	holder.callTool(t, "lac_acquire_slot", map[string]any{"resource": "test"})

	answer := hopeful.callTool(t, "lac_acquire_slot", map[string]any{
		"resource": "test", "no_wait": true,
	})
	if !strings.Contains(answer, "fully in use") {
		t.Errorf("lac_acquire_slot said %q, want it to explain the resource is busy", answer)
	}
}

// Waiting too long is normal, not an error: the model must be told to call again rather than to
// give up or to start work without a slot.
func TestATimedOutSlotRequestTellsTheModelToRetry(t *testing.T) {
	socketPath := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 1}})

	holder := newSession(t, socketPath, Options{AgentName: "holder", Workdir: workdir(t)})
	waiting := newSession(t, socketPath, Options{
		AgentName: "waiting", Workdir: workdir(t), AcquireTimeout: 200 * time.Millisecond,
	})

	holder.callTool(t, "lac_acquire_slot", map[string]any{"resource": "test"})

	answer := waiting.callTool(t, "lac_acquire_slot", map[string]any{"resource": "test"})
	if !strings.Contains(answer, "again") {
		t.Errorf("a timed out request said %q, want it to tell the model to call again", answer)
	}
}

// Two sessions must see each other, and be able to talk.
func TestAgentsSeeEachOtherAndExchangeMessages(t *testing.T) {
	socketPath := startDaemon(t, nil)

	first := newSession(t, socketPath, Options{AgentName: "claude-a", Workdir: workdir(t)})
	second := newSession(t, socketPath, Options{AgentName: "claude-b", Workdir: workdir(t)})

	// The roster is only populated once a session has made a call.
	second.callTool(t, "lac_agents", nil)

	roster := first.callTool(t, "lac_agents", nil)
	if !strings.Contains(roster, "claude-b") {
		t.Fatalf("lac_agents said %q, want it to list the other session", roster)
	}

	sent := first.callTool(t, "lac_send_message", map[string]any{
		"to": "claude-b", "kind": "question", "text": "are you touching the parser?",
	})
	if !strings.Contains(sent, "Sent") {
		t.Fatalf("lac_send_message said %q", sent)
	}

	inbox := second.callTool(t, "lac_inbox", nil)
	if !strings.Contains(inbox, "are you touching the parser?") {
		t.Fatalf("lac_inbox said %q, want the message that was sent", inbox)
	}
	if !strings.Contains(inbox, "claude-a") {
		t.Errorf("lac_inbox does not say who sent it: %q", inbox)
	}

	// Reading acknowledges by default, so the same message must not come back.
	if again := second.callTool(t, "lac_inbox", nil); !strings.Contains(again, "Nothing waiting") {
		t.Errorf("lac_inbox returned the same message twice: %q", again)
	}
}

func TestInboxCanLeaveMessagesUnread(t *testing.T) {
	socketPath := startDaemon(t, nil)

	sender := newSession(t, socketPath, Options{AgentName: "claude-a", Workdir: workdir(t)})
	recipient := newSession(t, socketPath, Options{AgentName: "claude-b", Workdir: workdir(t)})
	recipient.callTool(t, "lac_agents", nil)

	sender.callTool(t, "lac_send_message", map[string]any{
		"to": "claude-b", "kind": "status", "text": "rebasing now",
	})

	first := recipient.callTool(t, "lac_inbox", map[string]any{"keep_unread": true})
	if !strings.Contains(first, "rebasing now") {
		t.Fatalf("lac_inbox said %q", first)
	}

	second := recipient.callTool(t, "lac_inbox", nil)
	if !strings.Contains(second, "rebasing now") {
		t.Errorf("a message left unread did not come back: %q", second)
	}
}

// A tool called with something missing must say what is missing, because that is the only thing
// the model has to work with.
func TestToolsExplainMissingArguments(t *testing.T) {
	socketPath := startDaemon(t, nil)
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	tests := map[string]struct {
		arguments map[string]any
		wantHint  string
	}{
		"lac_acquire_slot": {arguments: map[string]any{}, wantHint: "resource"},
		"lac_release_slot": {arguments: map[string]any{}, wantHint: "lease_id"},
		"lac_queue":        {arguments: map[string]any{}, wantHint: "resource"},
		"lac_send_message": {arguments: map[string]any{"text": "hello"}, wantHint: "lac_agents"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := subject.toolResult(t, name, test.arguments)

			if !result.IsError {
				t.Fatalf("%s succeeded with missing arguments", name)
			}
			if !strings.Contains(textOfResult(result), test.wantHint) {
				t.Errorf("%s said %q, want it to mention %q", name, textOfResult(result), test.wantHint)
			}
		})
	}
}

// An unknown tool is a protocol-level mistake by the client, not something the model should be
// asked to reason about.
func TestUnknownToolIsAProtocolError(t *testing.T) {
	socketPath := startDaemon(t, nil)
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	reply := subject.call(t, "tools/call", map[string]any{"name": "lac_do_something_odd"})
	if reply.Error == nil {
		t.Fatal("an unknown tool did not produce an error")
	}
	if reply.Error.Code != codeMethodNotFound {
		t.Errorf("error code = %d, want %d", reply.Error.Code, codeMethodNotFound)
	}
}

// The resources are the read-only views a client can fetch without spending a tool call.
func TestResourcesCanBeListedAndRead(t *testing.T) {
	socketPath := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 4}})
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	listed := subject.call(t, "resources/list", nil)
	if listed.Error != nil {
		t.Fatalf("resources/list failed: %+v", listed.Error)
	}

	var catalogue resourceListResult
	decodeResult(t, listed, &catalogue)
	if len(catalogue.Resources) == 0 {
		t.Fatal("no resources were listed")
	}

	read := subject.call(t, "resources/read", map[string]any{"uri": "lac://resources"})
	if read.Error != nil {
		t.Fatalf("resources/read failed: %+v", read.Error)
	}

	var contents readResourceResult
	decodeResult(t, read, &contents)
	if len(contents.Contents) != 1 || !strings.Contains(contents.Contents[0].Text, "test") {
		t.Errorf("resources/read returned %+v, want the test resource", contents.Contents)
	}

	unknown := subject.call(t, "resources/read", map[string]any{"uri": "lac://nothing"})
	if unknown.Error == nil {
		t.Error("reading an unknown resource did not fail")
	}
}

// The MCP server is started before the daemon as often as not. It must still come up and explain
// itself, rather than leaving the model with no tools and no reason.
func TestToolsExplainAMissingDaemon(t *testing.T) {
	subject := newSession(t, filepath.Join(t.TempDir(), "absent.sock"), Options{Workdir: workdir(t)})

	result := subject.toolResult(t, "lac_agents", nil)
	if !result.IsError {
		t.Fatal("a tool succeeded with no daemon running")
	}

	text := textOfResult(result)
	if !strings.Contains(text, "lacd") {
		t.Errorf("the failure does not say how to fix it: %q", text)
	}
}

// Malformed input must not take the server down: an AI client that sends rubbish should get an
// answer and be able to carry on.
func TestMalformedInputIsReported(t *testing.T) {
	socketPath := startDaemon(t, nil)
	subject := newSession(t, socketPath, Options{Workdir: workdir(t)})

	if _, err := subject.requests.Write([]byte("{not json}\n")); err != nil {
		t.Fatalf("writing rubbish: %v", err)
	}

	if !subject.responses.Scan() {
		t.Fatalf("no reply to malformed input: %v", subject.responses.Err())
	}

	var reply response
	if err := json.Unmarshal(subject.responses.Bytes(), &reply); err != nil {
		t.Fatalf("decoding the reply: %v", err)
	}
	if reply.Error == nil || reply.Error.Code != codeParseError {
		t.Fatalf("reply = %+v, want a parse error", reply)
	}

	// And the session must still work afterwards.
	if follow := subject.call(t, "ping", nil); follow.Error != nil {
		t.Errorf("the server stopped working after malformed input: %+v", follow.Error)
	}
}

// leaseIDFrom pulls the lease id out of the sentence the tool returns, the same way a model would.
func leaseIDFrom(t *testing.T, text string) string {
	t.Helper()

	const marker = "Lease id: "

	index := strings.Index(text, marker)
	if index < 0 {
		t.Fatalf("no lease id in %q", text)
	}

	rest := text[index+len(marker):]
	if end := strings.IndexAny(rest, " \n"); end > 0 {
		return rest[:end]
	}

	return rest
}
