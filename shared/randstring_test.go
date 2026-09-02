package shared

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRandString(t *testing.T) {
	t.Parallel()

	const n = 12

	got := RandString(n)
	require.Len(t, got, n)

	for _, r := range got {
		require.True(t, strings.ContainsRune(letterBytes, r), "unexpected character %q", r)
	}

	require.NotEqual(t, got, RandString(n), "two ids in one process must differ")
}
