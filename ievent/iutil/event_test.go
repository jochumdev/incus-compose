package iutil

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestEqual pins what counts as the same thing said twice, which is what decides
// whether an event the enricher invented is worth walking.
func TestEqual(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	base := func() *Event {
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
		return NewEvent(at, "instance-updated", "p", "web", "").
			WithInstance(inst, true)
	}

	cases := []struct {
		name  string
		other func() *Event
		equal bool
	}{
		{
			name:  "the same read twice",
			other: base,
			equal: true,
		},
		{
			// The read is what the event is about; when it was decoded is not.
			name: "decoded at another moment",
			other: func() *Event {
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
				return NewEvent(at.Add(time.Hour), "instance-updated", "p", "web", "").WithInstance(inst, true)
			},
			equal: true,
		},
		{
			// What the whole chain was doing is not about this subject, and the
			// brackets are what carry it.
			name:  "created while the chain was elsewhere",
			other: func() *Event { return base().WithChainState(ChainWarm) },
			equal: true,
		},
		{
			// Included on purpose: a stop whose transition an update already
			// absorbed still has to reach whoever is counting stops.
			name: "another action about the same instance",
			other: func() *Event {
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
				return NewEvent(at, "instance-stopped", "p", "web", "").WithInstance(inst, true)
			},
		},
		{
			name: "the same name in another project",
			other: func() *Event {
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
				return NewEvent(at, "instance-updated", "other", "web", "").WithInstance(inst, true)
			},
		},
		{
			name: "an address that moved",
			other: func() *Event {
				myInst := &Instance{
					running: true,
					config:  map[string]string{"user.label.a": "1"},
					interfaces: []InstanceInterface{
						NewInstanceInterface("p", "p/net0", true, []string{"10.0.0.3/24"}, nil),
					},
					networks: map[string]*Network{
						NetworkKey("p", "p/net0"): NewNetwork("p/net0", "p", true, "10.0.0.0/24", ""),
					},
				}
				return NewEvent(at, "instance-updated", "p", "web", "").WithInstance(myInst, true)
			},
		},
		{
			name: "no longer running",
			other: func() *Event {
				myInst := &Instance{
					running:    false,
					config:     map[string]string{"user.label.a": "2"},
					interfaces: []InstanceInterface{},
				}
				return NewEvent(at, "instance-updated", "p", "web", "").WithInstance(myInst, true)
			},
		},
		{
			name: "a label that changed",
			other: func() *Event {
				myInst := &Instance{
					running: true,
					config:  map[string]string{"user.label.a": "2"},
					interfaces: []InstanceInterface{
						NewInstanceInterface("p", "p/net0", true, []string{"10.0.0.2/24"}, nil),
					},
					networks: map[string]*Network{
						NetworkKey("p", "p/net0"): NewNetwork("p/net0", "p", true, "10.0.0.0/24", ""),
					},
				}
				return NewEvent(at, "instance-updated", "p", "web", "").WithInstance(myInst, true)
			},
		},
		{
			// A failure is never the same as the answer it failed to get.
			name:  "the same read, failed",
			other: func() *Event { return base().WithFailed(errors.New("source/read")) },
		},
		{
			name:  "the same read, dropped",
			other: func() *Event { return base().WithDropped("debounce") },
		},
		{
			// Reads that landed is part of what an event says: "no networks" and
			// "nobody asked for networks" are different answers.
			name:  "read for less",
			other: func() *Event { return NewEvent(at, "instance-updated", "p", "web", "") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.equal, base().Equal(tc.other()))
			assert.Equal(t, tc.equal, tc.other().Equal(base()), "the answer depends on which side is asked")
		})
	}
}

// TestEqualWithoutNets pins the difference between the two answers: a consumer
// keying on addresses takes it as the instance having moved and nothing else.
func TestEqualWithoutNets(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

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
	base := func() *Event {
		return NewEvent(at, "instance-updated", "p", "web", "").
			WithInstance(inst, true)
	}

	cases := []struct {
		name  string
		other func() *Event

		equal       bool
		withoutNets bool
	}{
		{
			name:        "the same read twice",
			other:       base,
			equal:       true,
			withoutNets: true,
		},
		{
			name: "the same instance on another address",
			other: func() *Event {
				myInst := &Instance{
					running: true,
					config:  map[string]string{"user.label.a": "1"},
					interfaces: []InstanceInterface{
						NewInstanceInterface("p", "p/net0", true, []string{"10.0.0.3/24"}, nil),
					},
					networks: map[string]*Network{
						NetworkKey("p", "p/net0"): NewNetwork("p/net0", "p", true, "10.0.0.0/24", ""),
					},
				}
				return NewEvent(at, "instance-updated", "p", "web", "").WithInstance(myInst, true)
			},
			withoutNets: true,
		},
		{
			name: "a label changed as well",
			other: func() *Event {
				myInst := &Instance{
					running: true,
					config:  map[string]string{"user.label.a": "2"},
					interfaces: []InstanceInterface{
						NewInstanceInterface("p", "p/net0", true, []string{"10.0.0.3/24"}, nil),
					},
					networks: map[string]*Network{
						NetworkKey("p", "p/net0"): NewNetwork("p/net0", "p", true, "10.0.0.0/24", ""),
					},
				}
				return NewEvent(at, "instance-updated", "p", "web", "").WithInstance(myInst, true)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.equal, base().Equal(tc.other()))
			assert.Equal(t, tc.withoutNets, base().EqualWithoutNets(tc.other()))
			assert.Equal(t, tc.withoutNets, tc.other().EqualWithoutNets(base()),
				"the answer depends on which side is asked")
		})
	}
}

// TestEqualNil pins the one comparison with nothing to compare: the first event
// about a subject has no predecessor, and it is news.
func TestEqualNil(t *testing.T) {
	t.Parallel()

	ev := NewEvent(time.Now(), "instance-updated", "p", "web", "")

	assert.False(t, ev.Equal(nil))
	assert.True(t, (*Event)(nil).Equal(nil))
}
