package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectorSettings_UpsertKeepsOneRowPerType(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	require.NoError(t, s.SetDetectorEnabled(ctx, "PERSON", true))
	require.NoError(t, s.SetDetectorEnabled(ctx, "PERSON", false))
	require.NoError(t, s.SetDetectorEnabled(ctx, "EMAIL_ADDRESS", false))

	rows, err := s.ListDetectorSettings(ctx)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Type] = r.Enabled
	}
	assert.Equal(t, map[string]bool{"PERSON": false, "EMAIL_ADDRESS": false}, got)
}
