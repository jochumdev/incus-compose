package iutil

import (
	"encoding/json"
	"iter"
	"maps"
)

// Project is what one project read amounted to: its own configuration, as
// `incus project set` writes it, never its default profile. Read-only once
// built, so every instance event in the project shares one.
type Project struct {
	config map[string]string
}

// NewProject builds one project read. It takes ownership of the map - do not
// reuse it.
func NewProject(config map[string]string) *Project {
	return &Project{config: config}
}

// ConfigValue is one configuration key, which is what a consumer looking for
// its own namespace wants: no copy of the rest.
func (p *Project) ConfigValue(key string) (string, bool) {
	if p == nil {
		return "", false
	}

	v, ok := p.config[key]

	return v, ok
}

// Config is everything the project sets, whoever wrote it. An iterator rather
// than the map, so reading it costs nothing and writing it is not on offer.
func (p *Project) Config() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		if p == nil {
			return
		}

		for k, v := range p.config {
			if !yield(k, v) {
				return
			}
		}
	}
}

// Equal reports whether two projects say the same thing.
func (p *Project) Equal(other *Project) bool {
	if p == nil || other == nil {
		return p == other
	}

	return maps.Equal(p.config, other.config)
}

type projectJSON struct {
	Config map[string]string `json:"config,omitempty"`
}

// MarshalJSON writes the wire form, since the fields it is built from are unexported.
func (p Project) MarshalJSON() ([]byte, error) {
	return json.Marshal(projectJSON{Config: p.config})
}

// UnmarshalJSON reads the wire form back.
func (p *Project) UnmarshalJSON(b []byte) error {
	var v projectJSON

	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}

	*p = Project{config: v.Config}

	return nil
}

// Project is what the project read found, or nil where nothing read it. Its
// name, which every event carries whether or not one was read, is ProjectName.
func (e *Event) Project() *Project {
	return e.project
}

// WithProject derives an event carrying the project's own configuration.
func (e *Event) WithProject(p *Project) *Event {
	next := *e

	next.project = p
	next.enriched |= EnrichedProject

	return &next
}
