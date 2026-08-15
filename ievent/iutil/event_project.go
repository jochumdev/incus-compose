package iutil

import "maps"

// ProjectLabels returns a clone of the project's own labels, apart from
// whatever an instance's own configuration set. See WithProject.
func (e *Event) ProjectLabels() map[string]string {
	return maps.Clone(e.projectLabels)
}

// WithProject derives an event carrying the project's own labels. Nil meta is
// not an error.
func (e *Event) WithProject(meta map[string]string) *Event {
	next := *e
	next.projectLabels = maps.Clone(meta)

	next.enriched |= EnrichedProject

	return &next
}
