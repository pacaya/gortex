package ledger

// EventKind enumerates ledger lifecycle events.
type EventKind int

const (
	EventAccountOpened EventKind = iota
	EventPosted
	EventPostFailed
)

// Event is a ledger lifecycle notification.
type Event struct {
	Kind    EventKind
	Subject string
}

// Listener receives ledger events.
type Listener interface {
	Notify(ev Event)
}

// ListenerFunc adapts a plain function to the Listener interface.
type ListenerFunc func(ev Event)

// Notify invokes the underlying function.
func (f ListenerFunc) Notify(ev Event) { f(ev) }

// Dispatcher fans events out to registered listeners.
type Dispatcher struct {
	listeners []Listener
}

// NewDispatcher returns an empty dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{}
}

// Subscribe adds a listener.
func (d *Dispatcher) Subscribe(l Listener) {
	d.listeners = append(d.listeners, l)
}

// Emit delivers ev to every listener in registration order.
func (d *Dispatcher) Emit(ev Event) {
	for _, l := range d.listeners {
		l.Notify(ev)
	}
}
