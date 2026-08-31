package iutil

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInstanceRoundTrip is what the cold store rests on: what one run wrote is
// what the next one holds, down to comparing equal to the read it came from.
func TestInstanceRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *Instance
	}{
		{
			name: "a running instance on one network",
			in: NewInstance(true, map[string]string{"user.label.a": "1"}, []InstanceInterface{
				NewInstanceInterface("p", "net0", true, []string{"10.0.0.2"}, []string{"fd42::2"}),
			}, map[string]*Network{
				NetworkKey("p", "net0"): NewNetwork("net0", "p", true, "10.0.0.1/24", "fd42::1/64"),
			}),
		},
		{
			// The three empties are different answers, and a round trip that
			// turned any of them into another would be a lie about the fleet.
			name: "a stopped instance sits nowhere",
			in:   NewInstance(false, map[string]string{}, nil, nil),
		},
		{
			name: "a NIC up before its lease holds no address",
			in: NewInstance(true, nil, []InstanceInterface{
				NewInstanceInterface("p", "net0", true, nil, nil),
			}, map[string]*Network{
				NetworkKey("p", "net0"): NewNetwork("net0", "p", true, "10.0.0.1/24", ""),
			}),
		},
		{
			name: "an unmanaged bridge still keys records",
			in: NewInstance(true, nil, []InstanceInterface{
				NewInstanceInterface("default", "br0", false, []string{"10.9.0.2"}, nil),
			}, map[string]*Network{
				NetworkKey("default", "br0"): NewNetwork("br0", "default", false, "", ""),
			}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := json.Marshal(tc.in)
			require.NoError(t, err)

			var got *Instance

			require.NoError(t, json.Unmarshal(b, &got))
			assert.True(t, tc.in.Equal(got), "what was written back is not what was read")
		})
	}
}

// TestNetworkAndProjectRoundTrip covers the other two the store holds.
func TestNetworkAndProjectRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("network", func(t *testing.T) {
		t.Parallel()

		in := NewNetwork("net0", "p", true, "10.0.0.1/24", "fd42::1/64")

		b, err := json.Marshal(in)
		require.NoError(t, err)

		var got *Network

		require.NoError(t, json.Unmarshal(b, &got))
		assert.True(t, in.equal(got))
	})

	t.Run("project", func(t *testing.T) {
		t.Parallel()

		in := NewProject(map[string]string{"features.networks": "true"})

		b, err := json.Marshal(in)
		require.NoError(t, err)

		var got *Project

		require.NoError(t, json.Unmarshal(b, &got))
		assert.True(t, in.Equal(got))
	})
}

// TestNilMarshalsAsNull: nothing read is a hole in the file rather than an
// empty record, so a reload cannot mistake it for something that was read.
func TestNilMarshalsAsNull(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   any
	}{
		{name: "instance", in: (*Instance)(nil)},
		{name: "network", in: (*Network)(nil)},
		{name: "project", in: (*Project)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := json.Marshal(tc.in)
			require.NoError(t, err)
			assert.JSONEq(t, "null", string(b))
		})
	}
}

// TestNothingIsWritableThroughAnAccessor is the promise Event makes about what
// it hands back, and the one the old exported fields broke: a plugin holding an
// event may not move what the enricher is serving from.
func TestNothingIsWritableThroughAnAccessor(t *testing.T) {
	t.Parallel()

	held := NewInstance(true, map[string]string{"a": "1"}, []InstanceInterface{
		NewInstanceInterface("p", "net0", true, []string{"10.0.0.2"}, nil),
	}, map[string]*Network{
		NetworkKey("p", "net0"): NewNetwork("net0", "p", true, "10.0.0.1/24", ""),
	})

	ev := NewEvent(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), "instance-updated", "p", "web", "").WithInstance(held, true)

	// Everything a consumer can reach, written to as hard as it can be.
	for iface := range ev.Instance().Interfaces() {
		addrs := iface.IPv4()
		addrs[0] = "10.9.9.9"
	}

	config := maps.Collect(ev.Instance().Config())
	config["a"] = "moved"

	assert.Equal(t, []string{"10.0.0.2"},
		slices.Collect(func(yield func(string) bool) {
			for iface := range held.Interfaces() {
				for _, a := range iface.IPv4() {
					if !yield(a) {
						return
					}
				}
			}
		}), "an address moved under the state that holds it")

	got, ok := held.ConfigValue("a")
	require.True(t, ok)
	assert.Equal(t, "1", got, "a configuration key moved under the state that holds it")
}
