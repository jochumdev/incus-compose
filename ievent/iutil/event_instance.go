package iutil

import (
	"maps"
	"strings"
)

// Running returns if the instance is running.
func (e *Event) Running() bool { return e.running }

// Label returns a single label, the instance's own if it set one, else the
// project's.
func (e *Event) Label(key string) (string, bool) {
	if v, ok := e.instanceLabels[key]; ok {
		return v, true
	}

	v, ok := e.projectLabels[key]

	return v, ok
}

// Labels returns the effective view: the instance's own labels, with the
// project's own filled in under whatever the instance left unset.
func (e *Event) Labels() map[string]string {
	var out map[string]string

	for k, v := range e.projectLabels {
		if out == nil {
			out = map[string]string{}
		}

		out[k] = v
	}

	for k, v := range e.instanceLabels {
		if out == nil {
			out = map[string]string{}
		}

		out[k] = v
	}

	return out
}

// InstanceLabels returns a clone of the labels the instance's own
// configuration set, apart from whatever a project filled in under them.
func (e *Event) InstanceLabels() map[string]string {
	return maps.Clone(e.instanceLabels)
}

func (e *Event) Config(key string) (string, bool) {
	v, ok := e.config[key]

	return v, ok
}

func (e *Event) Configs() map[string]string {
	return maps.Clone(e.config)
}

// WithInstance derives an event carrying what one instance read found.
func (e *Event) WithInstance(running bool, config map[string]string, nets map[string]*Network) *Event {
	next := *e
	next.running = running
	next.config = config

	own := map[string]string{}
	for k, v := range config {
		if !strings.HasPrefix(k, "user.label.") {
			continue
		}

		own[k[len("user.label."):]] = v
	}
	next.instanceLabels = own

	next.nets = nets
	next.enriched |= EnrichedInstance | EnrichedNetworks

	return &next
}
