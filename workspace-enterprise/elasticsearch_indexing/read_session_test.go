package elasticsearch_indexing

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestIndexingReaderBoundsSilentStartup(t *testing.T) {
	for _, stopEarly := range []bool{false, true} {
		name := "deadline"
		if stopEarly {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			dsn, entered, _, cleanup := indexingSilentDatabase(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 100 * time.Millisecond
			if stopEarly {
				timeout = 5 * time.Second
			}
			reader := newIndexingReader(ctx, []string{dsn}, timeout)
			defer func() { cleanup(); reader.Close() }()
			completed := make(chan error, 1)
			go func() { var n int; completed <- reader.get(&n, `SELECT 1`) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("read session did not reach the silent startup fixture")
			}
			if stopEarly {
				cancel()
			}
			select {
			case err := <-completed:
				require.Error(t, err)
			case <-time.After(700 * time.Millisecond):
				t.Fatal("read session did not close its silent PostgreSQL startup socket")
			}
			require.Nil(t, reader.db)
		})
	}
}

func TestIndexingReadSourcesPreferSearchThenReplicaThenPrimary(t *testing.T) {
	sources := indexingReadSources(model.SqlSettings{
		DataSourceSearchReplicas: []string{"search-a", "search-b"},
		DataSourceReplicas:       []string{"replica", "search-a"}, DataSource: new("primary"),
	})
	require.ElementsMatch(t, []string{"search-a", "search-b"}, sources[:2])
	require.Equal(t, []string{"replica", "primary"}, sources[2:])
	require.Equal(t, []string{"primary"}, indexingReadSources(model.SqlSettings{DataSource: new("primary")}))
}
