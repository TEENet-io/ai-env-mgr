package winsvc

// Event is a lifecycle transition the service reports before it goes quiet, so
// the admin can tell an expected suspend or stop apart from a crash. A crash
// delivers no event at all, which is exactly what makes the distinction work:
// silence with a preceding EventSuspend/EventStop is orderly, silence with none
// is suspicious.
type Event int

const (
	// EventSuspend fires when the machine is about to sleep or hibernate.
	EventSuspend Event = iota
	// EventStop fires when the service is being stopped or the machine is
	// shutting down.
	EventStop
)

// String renders the event as the token stored in Status.LastEvent.
func (e Event) String() string {
	switch e {
	case EventSuspend:
		return "suspend"
	case EventStop:
		return "stopped"
	default:
		return "unknown"
	}
}
