package shared

// Next hands the event on. The last plugin in the chain is given one that does
// nothing.
type Next func(ev *Event)

// Plugin is one link in the chain. It holds its successor and continues the
// walk itself, so the chain runs as a call stack and a plugin may work either
// side of Next.
//
// A plugin may appear in the chain twice, as two constructions - not one value
// listed twice, which would have Setup called on it twice.
//
// Plugins trust each other: nothing here defends one against another, and a
// panic takes the process down. If a proposal only makes sense against a plugin
// behaving badly, it does not get built.
type Plugin interface {
	// Name identifies the plugin in logs, in metrics, and in the chain.
	Name() string

	// Wants declares which actions this plugin cares about, and how much of
	// each has to be read before it sees one.
	//
	// Read from every plugin before anything is wired, because the enricher
	// serves the whole chain from a single action and needs the finished union.
	Wants() []Want

	// Setup wires the plugin, once, before anything runs. An error here stops
	// the process.
	Setup(args SetupArgs) error

	// Handle runs in its parent's goroutine and must not block: it enqueues and
	// returns, and Next is called from the plugin's own goroutine once the work
	// is done. So a plugin's state belongs to that one goroutine.
	//
	// A plugin that acts on events checks the state first, because one that is
	// not StateOk is only passing through for the observers to see:
	//
	//	func (p *Plugin) Handle(ev *shared.Event) {
	//		if ev.State() != shared.StateOk {
	//			p.next(ev)
	//
	//			return
	//		}
	//		...
	//	}
	Handle(ev *Event)
}
