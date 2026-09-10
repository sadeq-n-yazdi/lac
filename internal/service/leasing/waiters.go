package leasing

import "sync"

// waiters lets every agent queued for a resource be woken at once when a slot might have freed.
//
// Each resource has a channel that is closed — never sent on — to signal "something changed, look
// again". Closing broadcasts to every waiter at the same time, and a fresh channel takes its
// place. Waiters therefore never poll, and a waiter that arrives between two signals still sees
// the next one.
type waiters struct {
	mutex    sync.Mutex
	channels map[string]chan struct{}
}

func newWaiters() *waiters {
	return &waiters{channels: make(map[string]chan struct{})}
}

// watch returns a channel that closes the next time the resource changes. Callers must take it
// before looking at the resource's state, so a change that happens while they are looking is not
// missed.
func (w *waiters) watch(resource string) <-chan struct{} {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	channel, found := w.channels[resource]
	if !found {
		channel = make(chan struct{})
		w.channels[resource] = channel
	}

	return channel
}

// signal wakes everyone waiting on the resource.
func (w *waiters) signal(resource string) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if channel, found := w.channels[resource]; found {
		close(channel)
		delete(w.channels, resource)
	}
}

// signalAll wakes everyone waiting on anything. The reaper uses it after releasing whatever it
// found, rather than tracking which resources were touched.
func (w *waiters) signalAll() {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	for resource, channel := range w.channels {
		close(channel)
		delete(w.channels, resource)
	}
}
