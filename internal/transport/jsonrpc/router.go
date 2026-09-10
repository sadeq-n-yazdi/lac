package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
)

// Handler serves one method. It receives the calling session, so it can learn who is calling and
// push notifications back to them, and the raw parameters, which it decodes with ParseParams.
//
// Returning a domain error is enough: the server maps it onto the right protocol code.
type Handler func(ctx context.Context, session *Session, params json.RawMessage) (any, error)

// Router maps method names to handlers. It is safe for concurrent use once serving has started;
// registration is expected to happen during start-up.
type Router struct {
	mutex    sync.RWMutex
	handlers map[string]Handler
}

// NewRouter returns an empty router.
func NewRouter() *Router {
	return &Router{handlers: make(map[string]Handler)}
}

// Register adds a handler, panicking if the method is already taken. A duplicate registration is a
// programming mistake at start-up, not a condition to recover from at run time.
func (r *Router) Register(method string, handler Handler) {
	if method == "" || handler == nil {
		panic("jsonrpc: a method name and a handler are both required")
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()

	if _, taken := r.handlers[method]; taken {
		panic(fmt.Sprintf("jsonrpc: the method %q is already registered", method))
	}
	r.handlers[method] = handler
}

// Methods returns the registered method names, sorted. It backs the introspection a client uses to
// discover what this daemon can do.
func (r *Router) Methods() []string {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	methods := make([]string, 0, len(r.handlers))
	for method := range r.handlers {
		methods = append(methods, method)
	}
	slices.Sort(methods)

	return methods
}

// handlerFor returns the handler for a method, if there is one.
func (r *Router) handlerFor(method string) (Handler, bool) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	handler, found := r.handlers[method]

	return handler, found
}
