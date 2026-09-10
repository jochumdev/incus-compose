package ecs_view

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lxc/incus-compose/shared"
)

// TestLevelTraceMatchesShared pins the one value this package carries a copy
// of: the engine may not import ours, so drift would leave its trace lines unfiltered.
func TestLevelTraceMatchesShared(t *testing.T) {
	t.Parallel()

	assert.Equal(t, shared.LevelTrace, levelTrace)
}
