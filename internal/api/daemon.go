package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/transport/jsonrpc"
)

// ReloadOutcome is what a configuration reload did.
//
// It names what changed and, just as importantly, what changed in the file but could not be
// applied to a running daemon — an operator who edited a setting and saw nothing happen would
// reasonably assume it had taken effect.
type ReloadOutcome struct {
	// Source is the configuration file that was read.
	Source string `json:"source,omitempty"`
	// Applied lists what now differs in the running daemon.
	Applied []string `json:"applied"`
	// Deferred lists settings that changed in the file but need a restart.
	Deferred []string `json:"deferred"`
}

// Reloader re-reads the configuration. The daemon implements it; the API only needs to be able to
// ask, which keeps the two from depending on each other's internals.
type Reloader func(ctx context.Context) (ReloadOutcome, error)

func (a *API) handleDaemonReload(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, _ json.RawMessage,
) (any, error) {
	if a.reload == nil {
		return nil, errors.New("this daemon cannot reload its configuration")
	}

	// Reloading is gated on the same capability as defining a resource, because a reload can
	// redefine them: anyone who may reload could already have done it one resource at a time.
	// The error is rewritten, because "may not define_resource" is a puzzling answer to "reload".
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionDefineResource, "configuration"); err != nil {
		return nil, fmt.Errorf("%w: agent %q may not reload the configuration; that is an "+
			"operator's power, granted by the operators list in the daemon's own configuration",
			core.ErrUnauthorised, caller.Name)
	}

	outcome, err := a.reload(ctx)
	if err != nil {
		return nil, err
	}

	if outcome.Applied == nil {
		outcome.Applied = []string{}
	}
	if outcome.Deferred == nil {
		outcome.Deferred = []string{}
	}

	return outcome, nil
}
