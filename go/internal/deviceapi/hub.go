package deviceapi

import "sync"

// Hub is how a change in the console reaches a waiting agent without the
// agent asking again. Every long poll registers for its device; a change
// wakes that device's pollers, or all of them when the fleet policy moved.
//
// It is in-process state, which is enough: one console serves the fleet.
// A wake is a hint, not a promise -- the poll re-reads the configuration
// and answers only if it actually changed, and re-reads on a timer anyway.
type Hub struct {
	mu      sync.Mutex
	devices map[string]chan struct{}
	all     chan struct{}
}

func NewHub() *Hub {
	return &Hub{devices: map[string]chan struct{}{}, all: make(chan struct{})}
}

// Wait returns two channels: the first closes when this device is woken,
// the second when every device is.
func (h *Hub) Wait(deviceID string) (device, all <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.devices[deviceID]
	if !ok {
		ch = make(chan struct{})
		h.devices[deviceID] = ch
	}
	return ch, h.all
}

// Wake closes the channel of each named device and hands out a fresh one.
func (h *Hub) Wake(deviceIDs ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range deviceIDs {
		if ch, ok := h.devices[id]; ok {
			close(ch)
			delete(h.devices, id)
		}
	}
}

// WakeAll wakes every poller: the fleet policy changed.
func (h *Hub) WakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.all)
	h.all = make(chan struct{})
}
