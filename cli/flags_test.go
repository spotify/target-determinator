package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpaqueInputRepositoryFlag(t *testing.T) {
	var repositories OpaqueInputRepositoryFlag

	require.NoError(t, repositories.Set("@snapshot"))
	require.NoError(t, repositories.Set("@@+extension+snapshot"))

	assert.Equal(t, OpaqueInputRepositoryFlag{"snapshot", "+extension+snapshot"}, repositories)
	assert.Equal(t, "[snapshot, +extension+snapshot]", repositories.String())
}

func TestOpaqueInputRepositoryFlagRejectsLabelsAndEmptyNames(t *testing.T) {
	for _, value := range []string{"", "@", "@repo//:target"} {
		t.Run(value, func(t *testing.T) {
			var repositories OpaqueInputRepositoryFlag
			assert.Error(t, repositories.Set(value))
		})
	}
}
