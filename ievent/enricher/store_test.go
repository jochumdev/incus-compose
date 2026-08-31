package enricher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreSendNeverBlocks is the whole reason a clone crosses a channel: the
// fold may not wait on a disk, whatever the disk is doing. A clone the slot
// already held is dropped rather than queued, since the one behind it says
// everything it said.
func TestStoreSendNeverBlocks(t *testing.T) {
	t.Parallel()

	in := make(chan *state, 1)

	first := newState("")
	first.setProject("first", map[string]string{})

	last := newState("")
	last.setProject("last", map[string]string{})

	// Nothing is reading, so the slot fills and then has to be displaced.
	storeSend(in, first)
	storeSend(in, newState(""))
	storeSend(in, last)

	require.Len(t, in, 1, "the slot grew")

	got := <-in
	assert.NotNil(t, got.projectConfig("last"), "the newest clone was the one dropped")
}
