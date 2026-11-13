package elasticsearch_indexing

import (
	"errors"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/stretchr/testify/require"
)

func TestIndexingBatchRetryRetainsBudgetAndOnlyUsesSuccessfulRead(t *testing.T) {
	for _, recover := range []bool{false, true} {
		name := "exhausted"
		if recover {
			name = "recovered"
		}
		t.Run(name, func(t *testing.T) {
			logger := mlog.CreateTestLogger(t)
			cause := errors.New("transient read failure")
			attempts := 0
			rows, status, err := readIndexingBatch(logger, "posts", func() string { return "" }, time.Nanosecond, func() ([]string, error) {
				attempts++
				if recover && attempts == 2 {
					return []string{"successful-row"}, nil
				}
				// Failed reads may carry a partial result. It must never be
				// transformed, indexed or confused with an exhausted phase.
				return []string{"partial-failed-row"}, cause
			})
			require.Empty(t, status)
			if recover {
				require.NoError(t, err)
				require.Equal(t, []string{"successful-row"}, rows)
				require.Equal(t, 2, attempts)
			} else {
				require.ErrorIs(t, err, cause)
				require.Nil(t, rows)
				require.Equal(t, 11, attempts)
			}
		})
	}
}
