package iutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEnrichmentString(t *testing.T) {
	cases := []struct {
		name string
		in   Enrichment
		want string
	}{
		{
			// What every event before the enricher reports.
			name: "nothing landed",
			in:   0,
			want: "none",
		},
		{
			name: "one kind",
			in:   EnrichedProject,
			want: "project",
		},
		{
			// In the order the kinds are declared, not the order they were set.
			name: "what an instance read fills",
			in:   EnrichedNetwork | EnrichedInstanceWithInterfaces | EnrichedInstance,
			want: "instance,instance-with-interfaces,network",
		},
		{
			name: "everything there is",
			in:   EnrichedInstance | EnrichedInstanceWithInterfaces | EnrichedNetwork | EnrichedProject,
			want: "instance,instance-with-interfaces,network,project",
		},
		{
			// A kind added without a name here shows up rather than vanishing.
			name: "a bit with no name is still reported",
			in:   EnrichedInstance | 1<<7,
			want: "instance,0x80",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.in.String())
		})
	}
}

// TestEnrichmentsIsWhatLanded pins that the set comes off the event whole,
// which is what log reports.
func TestEnrichmentsIsWhatLanded(t *testing.T) {
	ev := NewEvent(time.Now(), "instance-started", "shop", "web", "")
	assert.Equal(t, Enrichment(0), ev.Enrichments())

	inst := &Instance{
		running: true,
		config:  map[string]string{"user.label.a": "1"},
		interfaces: []InstanceInterface{
			NewInstanceInterface("p", "p/net0", true, []string{"10.0.0.2/24"}, nil),
		},
		networks: map[string]*Network{
			NetworkKey("p", "p/net0"): NewNetwork("p/net0", "p", true, "10.0.0.0/24", ""),
		},
	}
	ev = ev.WithInstance(inst, true)
	assert.Equal(t, EnrichedInstance|EnrichedInstanceWithInterfaces, ev.Enrichments())

	ev = ev.WithProject(NewProject(map[string]string{}))
	assert.Equal(t, EnrichedInstance|EnrichedInstanceWithInterfaces|EnrichedProject, ev.Enrichments())
}
