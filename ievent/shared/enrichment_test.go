package shared

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
			// What every event in front of the enricher reports.
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
			in:   EnrichedNetworks | EnrichedInstance,
			want: "instance,networks",
		},
		{
			name: "everything there is",
			in:   EnrichedInstance | EnrichedNetworks | EnrichedProject,
			want: "instance,networks,project",
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

	ev = ev.WithInstance(true, map[string]string{}, map[string]*Network{})
	assert.Equal(t, EnrichedInstance|EnrichedNetworks, ev.Enrichments())

	ev = ev.WithProject(map[string]string{})
	assert.Equal(t, EnrichedInstance|EnrichedNetworks|EnrichedProject, ev.Enrichments())
}
