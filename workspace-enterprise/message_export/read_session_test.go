package message_export

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestMessageExportReaderBoundsSilentStartup(t *testing.T) {
	for _, mode := range []string{"deadline", "caller-cancel", "job-cancel"} {
		t.Run(mode, func(t *testing.T) {
			dsn, entered, cleanup := silentMessageExportDatabase(t)
			job, cancelJob := context.WithCancel(context.Background())
			defer cancelJob()
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			reader := newMessageExportReader(job, model.SqlSettings{DataSource: &dsn})
			reader.timeout = 200 * time.Millisecond
			defer func() { cleanup(); reader.Close() }()
			finished := make(chan error, 1)
			go func() {
				finished <- reader.query(parent, func(ctx context.Context, db *sql.DB) error { return db.PingContext(ctx) })
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("reader did not send PostgreSQL startup bytes")
			}
			want := context.DeadlineExceeded
			if mode == "caller-cancel" {
				cancelParent()
				want = context.Canceled
			} else if mode == "job-cancel" {
				cancelJob()
				want = context.Canceled
			}
			select {
			case err := <-finished:
				require.ErrorIs(t, err, want)
			case <-time.After(time.Second):
				t.Fatal("reader remained blocked in PostgreSQL startup")
			}
			require.Nil(t, reader.db)
			cancelJob()
			called := false
			err := reader.query(context.Background(), func(context.Context, *sql.DB) error { called = true; return nil })
			require.ErrorIs(t, err, context.Canceled)
			require.False(t, called, "a canceled owning job must not open a new backend or run a later read")
		})
	}
}

func TestMessageExportReaderRejectsCanceledCallerAndRedactsConfiguration(t *testing.T) {
	for _, dsn := range []string{"postgres://probe:private-test-secret@bad%escape/db", "password='private-test-secret"} {
		reader := newMessageExportReader(context.Background(), model.SqlSettings{DataSource: &dsn})
		defer reader.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		callback := func(context.Context, *sql.DB) error { called = true; return nil }
		require.ErrorIs(t, reader.query(ctx, callback), context.Canceled)
		require.False(t, reader.hasSlot)
		require.EqualError(t, reader.query(context.Background(), callback), "invalid message export read database configuration")
		require.False(t, called)
		require.Nil(t, reader.db)
		reader.Close()
		reader.Close()
	}
}
