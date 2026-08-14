package shared

import "maps"

// ProjectMetadata returns one of the project's configuration keys, by its whole
// name. The project's own configuration, which is what `incus project set`
// writes - never its default profile, whose keys every instance already carries
// expanded.
func (e *Event) ProjectMetadata(key string) (string, bool) {
	v, ok := e.projectMeta[key]

	return v, ok
}

// ProjectMetadatas returns the project's whole configuration, as a clone.
func (e *Event) ProjectMetadatas() map[string]string {
	return maps.Clone(e.projectMeta)
}

// WithProject derives an event carrying the project's own labels. Nil meta is
// not an error.
func (e *Event) WithProject(meta map[string]string) *Event {
	next := *e
	next.projectMeta = meta
	next.enriched |= EnrichedProject

	return &next
}
