package daemon

import "sadeq.uk/lac/internal/service/messaging"

// fanOutNotifier sends a notification to every front end that might be able to deliver it, and
// reports how many actually did.
//
// A message for an agent connected over the socket goes there; a message for the Telegram bridge
// goes to the operator's phone. Neither needs to know the other exists.
type fanOutNotifier struct {
	targets []messaging.Notifier
}

// compile-time proof that the contract is satisfied.
var _ messaging.Notifier = (*fanOutNotifier)(nil)

func newFanOutNotifier(targets ...messaging.Notifier) *fanOutNotifier {
	return &fanOutNotifier{targets: targets}
}

// add attaches another front end. It is called during start-up, before anything is serving.
func (n *fanOutNotifier) add(target messaging.Notifier) {
	if target != nil {
		n.targets = append(n.targets, target)
	}
}

// Broadcast delivers to every target and returns the total reached.
func (n *fanOutNotifier) Broadcast(agentID, method string, params any) int {
	reached := 0

	for _, target := range n.targets {
		if target == nil {
			continue
		}
		reached += target.Broadcast(agentID, method, params)
	}

	return reached
}
