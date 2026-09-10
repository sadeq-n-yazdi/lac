package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/service/leasing"
	"code.sadeq.uk/lac/internal/service/messaging"
	"code.sadeq.uk/lac/internal/service/registry"
	"code.sadeq.uk/lac/internal/transport/jsonrpc"
)

// API binds the services to the JSON-RPC router.
type API struct {
	registry      *registry.Service
	messaging     *messaging.Service
	leasing       *leasing.Service
	authenticator *auth.Authenticator
	logger        *slog.Logger
}

// Options configure the API.
type Options struct {
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// New returns an API over the given services.
func New(
	registryService *registry.Service,
	messagingService *messaging.Service,
	leasingService *leasing.Service,
	authenticator *auth.Authenticator,
	options Options,
) *API {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &API{
		registry:      registryService,
		messaging:     messagingService,
		leasing:       leasingService,
		authenticator: authenticator,
		logger:        logger,
	}
}

// Register adds every method to the router.
//
// Only two methods are reachable without a token: registering, and authenticating an existing one.
// Everything else goes through authenticated, which is what makes "who is calling?" a question the
// handlers never have to ask.
func (a *API) Register(router *jsonrpc.Router) {
	router.Register("agent.register", a.handleRegister)
	router.Register("agent.authenticate", a.handleAuthenticate)

	router.Register("agent.heartbeat", a.authenticated(a.handleHeartbeat))
	router.Register("agent.list", a.authenticated(a.handleAgentList))
	router.Register("agent.deregister", a.authenticated(a.handleDeregister))

	router.Register("message.send", a.authenticated(a.handleSend))
	router.Register("message.inbox", a.authenticated(a.handleInbox))
	router.Register("message.ack", a.authenticated(a.handleAcknowledge))
	router.Register("message.subscribe", a.authenticated(a.handleSubscribe))
	router.Register("message.unsubscribe", a.authenticated(a.handleUnsubscribe))

	router.Register("resource.define", a.authenticated(a.handleDefineResource))
	router.Register("resource.list", a.authenticated(a.handleResourceList))
	router.Register("resource.status", a.authenticated(a.handleResourceStatus))

	router.Register("lease.acquire", a.authenticated(a.handleAcquire))
	router.Register("lease.renew", a.authenticated(a.handleRenew))
	router.Register("lease.release", a.authenticated(a.handleRelease))
	router.Register("lease.held", a.authenticated(a.handleHeld))

	router.Register("queue.status", a.authenticated(a.handleQueueStatus))
	router.Register("queue.cancel", a.authenticated(a.handleQueueCancel))
}

// handler is a method that already knows who is calling.
type handler func(ctx context.Context, caller core.Agent, session *jsonrpc.Session, params json.RawMessage) (any, error)

// authenticated wraps a handler so it only ever runs for a caller whose token has been checked.
//
// The agent is read from the connection, never from the request body: an agent id in a payload is
// just a claim, and trusting it would let any agent act as any other.
func (a *API) authenticated(next handler) jsonrpc.Handler {
	return func(ctx context.Context, session *jsonrpc.Session, params json.RawMessage) (any, error) {
		agentID := session.AgentID()
		if agentID == "" {
			return nil, fmt.Errorf("%w: authenticate first with agent.register or agent.authenticate",
				core.ErrUnauthorised)
		}

		caller, err := a.registry.ByID(ctx, agentID)
		if err != nil {
			return nil, err
		}
		if caller.State == core.AgentDeregistered {
			return nil, fmt.Errorf("%w: this agent has deregistered", core.ErrUnauthorised)
		}

		return next(ctx, caller, session, params)
	}
}
