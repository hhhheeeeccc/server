package data_retention

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetentionCutoff(t *testing.T) {
	const now int64 = 10 * 24 * 60 * 60 * 1000
	const hour int64 = 60 * 60 * 1000

	cutoff, err := retentionCutoff(now, new(48), new(3), 365)
	require.NoError(t, err)
	require.Equal(t, now-48*hour, cutoff, "hours take precedence over deprecated days")

	cutoff, err = retentionCutoff(now, new(0), new(3), 365)
	require.NoError(t, err)
	require.Equal(t, now-72*hour, cutoff)

	cutoff, err = retentionCutoff(now, nil, nil, 365)
	require.NoError(t, err)
	require.Zero(t, cutoff, "a duration longer than the age of the server must not delete data")

	_, err = retentionCutoff(now, new(math.MaxInt), nil, 365)
	require.Error(t, err)
	_, err = retentionCutoff(now, nil, new(math.MaxInt), 365)
	require.Error(t, err)
}
