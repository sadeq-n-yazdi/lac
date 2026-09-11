package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"sadeq.uk/lac/internal/version"
	"sadeq.uk/lac/pkg/lacclient"
)

// maxFrameSize bounds one MCP message. Tool calls here are small.
const maxFrameSize = 4 << 20

// instructions are shown to the model once, at initialisation. They are the etiquette an agent
// needs before it does anything: queue before heavy work, say what you are doing, read your inbox.
const instructions = `LAC coordinates the AI agents working on this machine.

Use it for three things:

1. Before running anything heavy — a test suite, a build, a long script — call lac_acquire_slot for
   the matching resource, run the work, then call lac_release_slot. The machine can only do so much
   at once; the slot is how your turn is arranged. If you are told to wait, wait: call again.
2. Before starting on a file or an area, call lac_agents to see who else is working and where, and
   lac_send_message to tell them what you are taking on. Read lac_inbox when you start and between
   tasks.
3. When the operator asks what everyone is doing, lac_agents and lac_queue answer it.

Slots are held by you until released. Always release one when the work is done, even if it failed.`

// Options configure the MCP server.
type Options struct {
	// SocketPath is the daemon's socket. Empty uses the default location.
	SocketPath string
	// AgentName is how this session appears to the other agents. Empty derives one.
	AgentName string
	// AgentKind labels the tool on the other end, such as "claude" or "codex".
	AgentKind string
	// Workdir is the directory this session is working in. Empty uses the current one.
	Workdir string
	// AcquireTimeout bounds a blocking slot request, so a tool call cannot hang forever inside a
	// client that is waiting on it. On timeout the model is told to call again.
	AcquireTimeout time.Duration
	// Logger writes diagnostics. It must never write to stdout, which carries the protocol.
	Logger *slog.Logger
}

// Server speaks MCP on stdin and stdout, and LAC's own protocol to the daemon.
type Server struct {
	options Options
	logger  *slog.Logger

	input  io.Reader
	output io.Writer

	writeMutex sync.Mutex
	encoder    *json.Encoder

	// connection is established lazily, so the server still starts — and can explain itself — when
	// the daemon is not running yet.
	connectionMutex sync.Mutex
	client          *lacclient.Client
	agent           lacclient.Agent

	heartbeatOnce sync.Once
	stopHeartbeat context.CancelFunc
}

// NewServer returns a server reading from stdin and writing to stdout.
func NewServer(options Options) *Server {
	return newServer(os.Stdin, os.Stdout, options)
}

func newServer(input io.Reader, output io.Writer, options Options) *Server {
	logger := options.Logger
	if logger == nil {
		// stdout belongs to the protocol; diagnostics go to stderr or nowhere.
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	if options.AcquireTimeout <= 0 {
		options.AcquireTimeout = 5 * time.Minute
	}
	if options.AgentKind == "" {
		options.AgentKind = "mcp"
	}

	return &Server{
		options: options,
		logger:  logger,
		input:   input,
		output:  output,
		encoder: json.NewEncoder(output),
	}
}

// Serve reads requests until stdin closes or the context ends.
func (s *Server) Serve(ctx context.Context) error {
	// Deregistration gets its own context, because by the time it runs the caller's is usually the
	// thing that ended.
	defer s.disconnect() //nolint:contextcheck // disconnect makes a fresh, bounded context

	lines := bufio.NewScanner(s.input)
	lines.Buffer(make([]byte, 0, 4096), maxFrameSize)

	for lines.Scan() {
		if ctx.Err() != nil {
			return nil
		}

		line := lines.Bytes()
		if len(line) == 0 {
			continue
		}

		var incoming request
		if err := json.Unmarshal(line, &incoming); err != nil {
			s.reply(response{
				JSONRPC: jsonrpcVersion,
				ID:      json.RawMessage("null"),
				Error:   errorf(codeParseError, "malformed request: %v", err),
			})

			continue
		}

		s.handle(ctx, incoming)
	}

	if err := lines.Err(); err != nil {
		return fmt.Errorf("reading from the client: %w", err)
	}

	return nil
}

// handle dispatches one request.
func (s *Server) handle(ctx context.Context, incoming request) {
	if incoming.JSONRPC != jsonrpcVersion {
		s.fail(incoming, errorf(codeInvalidRequest, "jsonrpc must be %q", jsonrpcVersion))
		return
	}

	switch incoming.Method {
	case "initialize":
		s.handleInitialize(incoming)

	case "notifications/initialized", "notifications/cancelled":
		// Nothing to do, and notifications never get a reply.

	case "ping":
		s.succeed(incoming, map[string]any{})

	case "tools/list":
		s.succeed(incoming, toolListResult{Tools: toolDefinitions()})

	case "tools/call":
		s.handleCallTool(ctx, incoming)

	case "resources/list":
		s.succeed(incoming, resourceListResult{Resources: resourceDefinitions()})

	case "resources/read":
		s.handleReadResource(ctx, incoming)

	default:
		s.fail(incoming, errorf(codeMethodNotFound, "unknown method %q", incoming.Method))
	}
}

func (s *Server) handleInitialize(incoming request) {
	var arguments initializeParams
	if len(incoming.Params) > 0 {
		if err := json.Unmarshal(incoming.Params, &arguments); err != nil {
			s.fail(incoming, errorf(codeInvalidParams, "invalid parameters: %v", err))
			return
		}
	}

	// The client's name is a better label than "mcp" for the other agents to read, and it is how
	// the operator tells one session from another.
	if s.options.AgentKind == "mcp" && arguments.ClientInfo.Name != "" {
		s.options.AgentKind = sanitiseKind(arguments.ClientInfo.Name)
	}

	s.succeed(incoming, initializeResult{
		ProtocolVersion: negotiateProtocol(arguments.ProtocolVersion),
		Capabilities: serverCapabilities{
			Tools:     &toolsCapability{},
			Resources: &resourcesCapability{},
		},
		ServerInfo:   serverInfo{Name: "lac", Version: version.Current().Version},
		Instructions: instructions,
	})
}

func (s *Server) handleCallTool(ctx context.Context, incoming request) {
	var arguments callToolParams
	if err := json.Unmarshal(incoming.Params, &arguments); err != nil {
		s.fail(incoming, errorf(codeInvalidParams, "invalid parameters: %v", err))
		return
	}

	handler, found := toolHandlers()[arguments.Name]
	if !found {
		s.fail(incoming, errorf(codeMethodNotFound, "unknown tool %q", arguments.Name))
		return
	}

	client, err := s.connect(ctx)
	if err != nil {
		// A missing daemon is an ordinary situation the model can act on, so it is reported as a
		// tool failure rather than a protocol error the model never sees.
		s.succeed(incoming, errorResult("%s", err.Error()))
		return
	}

	result := handler(ctx, s, client, arguments.Arguments)
	s.succeed(incoming, result)
}

func (s *Server) handleReadResource(ctx context.Context, incoming request) {
	var arguments readResourceParams
	if err := json.Unmarshal(incoming.Params, &arguments); err != nil {
		s.fail(incoming, errorf(codeInvalidParams, "invalid parameters: %v", err))
		return
	}

	client, err := s.connect(ctx)
	if err != nil {
		s.fail(incoming, errorf(codeInternalError, "%s", err.Error()))
		return
	}

	text, err := readResource(ctx, client, arguments.URI)
	if err != nil {
		s.fail(incoming, errorf(codeInvalidParams, "%s", err.Error()))
		return
	}

	s.succeed(incoming, readResourceResult{Contents: []resourceContents{{
		URI: arguments.URI, MIMEType: "application/json", Text: text,
	}}})
}

// connect returns the daemon client, registering this session as an agent the first time.
//
// It is lazy on purpose: an MCP server that refused to start because the daemon was not running
// would leave the model with no tools and no explanation.
func (s *Server) connect(ctx context.Context) (*lacclient.Client, error) {
	s.connectionMutex.Lock()
	defer s.connectionMutex.Unlock()

	if s.client != nil {
		select {
		case <-s.client.Done():
			// The daemon restarted or went away; register again below.
			s.client = nil
		default:
			return s.client, nil
		}
	}

	client, err := lacclient.Dial(ctx, lacclient.Options{SocketPath: s.options.SocketPath})
	if err != nil {
		return nil, fmt.Errorf("the lac daemon is not reachable, so nothing can be coordinated yet. "+
			"Start it with `lacd &` and try again. (%v)", err)
	}

	name, workdir, err := s.identity()
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}

	registration, err := client.Register(ctx, name, s.options.AgentKind, workdir, os.Getpid())
	if err != nil {
		return nil, errors.Join(fmt.Errorf("registering with the lac daemon as %q: %w", name, err), client.Close())
	}

	s.client = client
	s.agent = registration.Agent

	s.heartbeatOnce.Do(func() {
		heartbeatCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		s.stopHeartbeat = stop

		go s.heartbeat(heartbeatCtx)
	})

	s.logger.Info("registered with the lac daemon", "agent", name, "kind", s.options.AgentKind)

	return client, nil
}

// heartbeat keeps this session on the roster for as long as the MCP server is running. Without it
// the daemon would decide the session had died and give away whatever slots it holds.
func (s *Server) heartbeat(ctx context.Context) {
	interval := time.Minute

	if client := s.currentClient(); client != nil {
		if timeToLive, err := client.Heartbeat(ctx); err == nil && timeToLive > 0 {
			// Beat three times per lifetime, so one lost beat is never fatal.
			interval = timeToLive / 3
		}
	}
	if interval < time.Second {
		interval = time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			client := s.currentClient()
			if client == nil {
				continue
			}
			if _, err := client.Heartbeat(ctx); err != nil {
				s.logger.Warn("could not send a heartbeat to the lac daemon", "error", err)
			}
		}
	}
}

func (s *Server) currentClient() *lacclient.Client {
	s.connectionMutex.Lock()
	defer s.connectionMutex.Unlock()

	return s.client
}

// disconnect gives back everything this session holds. A model that forgot to release a slot must
// not strand it when its session ends.
func (s *Server) disconnect() {
	s.connectionMutex.Lock()
	client := s.client
	s.client = nil
	s.connectionMutex.Unlock()

	if s.stopHeartbeat != nil {
		s.stopHeartbeat()
	}
	if client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Deregister(ctx); err != nil {
		s.logger.Warn("could not deregister from the lac daemon", "error", err)
	}
	_ = client.Close()
}

// reply writes one frame. Writes are serialised because a tool call and a notification could
// otherwise interleave into something unreadable.
func (s *Server) reply(frame any) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()

	if err := s.encoder.Encode(frame); err != nil {
		s.logger.Error("could not write to the client", "error", err)
	}
}

func (s *Server) succeed(incoming request, result any) {
	if incoming.isNotification() {
		return
	}

	s.reply(response{JSONRPC: jsonrpcVersion, ID: incoming.ID, Result: result})
}

func (s *Server) fail(incoming request, failure *rpcError) {
	if incoming.isNotification() {
		return
	}

	s.reply(response{JSONRPC: jsonrpcVersion, ID: incoming.ID, Error: failure})
}
