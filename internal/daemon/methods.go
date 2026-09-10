package daemon

import (
	"context"
	"encoding/json"
	"time"

	"code.sadeq.uk/lac/internal/transport/jsonrpc"
	"code.sadeq.uk/lac/internal/version"
)

// InfoResult describes the running daemon. A client calls daemon.info before anything else, to
// check it is talking to a version it understands and to discover what this build can do.
type InfoResult struct {
	// Version is the daemon's build version.
	Version string `json:"version"`
	// Commit is the git revision it was built from.
	Commit string `json:"commit"`
	// Protocol is the JSON-RPC version spoken on this socket.
	Protocol string `json:"protocol"`
	// StartedAt is when the daemon started, in RFC 3339.
	StartedAt string `json:"started_at"`
	// UptimeSeconds is how long it has been running.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// Methods are the methods this daemon serves.
	Methods []string `json:"methods"`
}

// registerMethods adds the methods every build serves, whatever services are attached.
func (d *Daemon) registerMethods() {
	d.router.Register("daemon.ping", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		return map[string]string{"pong": time.Now().UTC().Format(time.RFC3339Nano)}, nil
	})

	d.router.Register("daemon.info", func(context.Context, *jsonrpc.Session, json.RawMessage) (any, error) {
		return InfoResult{
			Version:       version.Version,
			Commit:        version.Commit,
			Protocol:      jsonrpc.Version,
			StartedAt:     d.startedAt.Format(time.RFC3339),
			UptimeSeconds: int64(time.Since(d.startedAt).Seconds()),
			Methods:       d.router.Methods(),
		}, nil
	})
}
