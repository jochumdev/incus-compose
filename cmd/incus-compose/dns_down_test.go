package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDNSDownConfirmRefusesWithoutATerminal pins the gate that keeps a
// script from stopping a daemon other projects rely on.
func TestDNSDownConfirmRefusesWithoutATerminal(t *testing.T) {
	t.Parallel()

	out := &bytes.Buffer{}

	ok, err := dnsDownConfirm(out, []string{"blog", "shop"})

	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "--force")

	assert.Contains(t, out.String(), "blog, shop")
	assert.Contains(t, out.String(), "2 other project(s)")
}

func TestDNSDownCommandFlags(t *testing.T) {
	t.Parallel()

	cmd := newDNSDownCommand()
	require.NotNil(t, cmd)
	assert.Equal(t, "down", cmd.Name)

	var hasForce, hasTimeout, hasVolumes bool
	for _, f := range cmd.Flags {
		for _, name := range f.Names() {
			switch name {
			case "force":
				hasForce = true
			case "timeout":
				hasTimeout = true
			case "volumes":
				hasVolumes = true
			}
		}
	}
	assert.True(t, hasForce, "down should have --force flag")
	assert.True(t, hasTimeout, "down should have --timeout flag")
	assert.True(t, hasVolumes, "down should have --volumes flag")
}
