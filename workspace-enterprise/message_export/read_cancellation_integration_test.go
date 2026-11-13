package message_export

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestMessageExportStopInterruptsNativeMetadataRead(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = model.NewPointer(true)
		cfg.MessageExportSettings.ExportFormat = model.NewPointer(model.ComplianceExportTypeCsv)
		cfg.SqlSettings.QueryTimeout = model.NewPointer(30)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeMessageExport, nil)
	require.Nil(t, appErr)
	db := th.Store.GetInternalMasterDB()
	blocker, err := db.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`LOCK TABLE Posts IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	worker := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	go worker.Run()
	sent := make(chan struct{})
	go func() { worker.JobChannel() <- *job; close(sent) }()
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("message export worker did not accept its job")
	}
	stopped := make(chan struct{})
	t.Cleanup(func() { _ = blocker.Rollback(); worker.Stop() })
	require.Eventually(t, func() bool {
		var waiting bool
		return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
            AND wait_event_type='Lock' AND query ILIKE '%COUNT(*)%' AND query ILIKE '%Posts%')`).Scan(&waiting) == nil && waiting
	}, 3*time.Second, 10*time.Millisecond)
	claimed, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, claimed.Status)
	go func() { worker.Stop(); close(stopped) }()
	interruptedBeforeUnlock := false
	select {
	case <-stopped:
		interruptedBeforeUnlock = true
	case <-time.After(800 * time.Millisecond):
	}
	// Drain the original implementation safely even when the assertion fails.
	require.NoError(t, blocker.Rollback())
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("message export did not drain after the test released its SQL lock")
	}
	t.Logf("metadata read interrupted before SQL lock release: %t", interruptedBeforeUnlock)
	require.True(t, interruptedBeforeUnlock, "Stop waited for native AnalyticsPostCount despite cancellation of the owning context")
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, stored.Status)
	require.Equal(t, claimed.Data[shared.JobDataExportDir], stored.Data[shared.JobDataExportDir])
	require.Equal(t, claimed.Data[shared.JobDataJobEndTime], stored.Data[shared.JobDataJobEndTime])
	require.NotEqual(t, "true", stored.Data[shared.JobDataIsDownloadable])
	require.NoError(t, db.PingContext(context.Background()))
}

func TestMessageExportReaderCancelsEverySQLPhase(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	for _, spec := range []struct {
		name, table string
		read        func(*exportReadStore) error
	}{
		{"count", "Posts", func(s *exportReadStore) error {
			_, err := s.ExportPostCount(th.Context, &model.PostCountOptions{})
			return err
		}},
		{"active-channels", "ChannelMemberHistory", func(s *exportReadStore) error {
			_, err := s.ExportActiveChannels(th.Context, 1, model.GetMillis())
			return err
		}},
		{"channels", "Channels", func(s *exportReadStore) error {
			_, err := s.ExportChannels(th.Context, []string{th.BasicChannel.Id})
			return err
		}},
		{"membership", "ChannelMemberHistory", func(s *exportReadStore) error {
			_, err := s.ExportMembershipHistory(th.Context, 1, model.GetMillis(), []string{th.BasicChannel.Id})
			return err
		}},
		{"file-metadata", "FileInfo", func(s *exportReadStore) error {
			_, err := s.FileInfo().GetForPost(model.NewId(), true, true, false)
			return err
		}},
		{"posts", "Posts", func(s *exportReadStore) error {
			_, _, err := s.Compliance().MessageExport(th.Context, model.MessageExportCursor{}, 10)
			return err
		}},
		{"independent-file-deletions", "FileInfo", func(s *exportReadStore) error {
			_, err := getFileDeletions(th.Context, s, 1, "", model.GetMillis(), 10)
			return err
		}},
		{"previous-job", "Jobs", func(s *exportReadStore) error {
			worker := &MessageExportWorker{reader: s.reader}
			_, err := worker.getPreviousNonCliJob(th.Context)
			return err
		}},
	} {
		t.Run(spec.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := newMessageExportReader(ctx, th.App.Config().SqlSettings)
			defer reader.Close()
			bounded := &exportReadStore{MessageExportStore: shared.NewMessageExportStore(th.Store), reader: reader}
			db := th.Store.GetInternalMasterDB()
			blocker, err := db.Begin()
			require.NoError(t, err)
			defer blocker.Rollback()
			// Table names are fixed test cases above, never external input.
			_, err = blocker.Exec(`LOCK TABLE ` + spec.table + ` IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			finished := make(chan error, 1)
			go func() { finished <- spec.read(bounded) }()
			var pid int
			require.Eventually(t, func() bool {
				return db.QueryRow(`SELECT pid FROM pg_stat_activity WHERE datname=current_database()
                    AND application_name='workspace_message_export_reader' AND wait_event_type='Lock'
                    AND query ILIKE '%'||$1||'%' LIMIT 1`, spec.table).Scan(&pid) == nil
			}, 3*time.Second, 10*time.Millisecond)
			cancel()
			select {
			case err := <-finished:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(800 * time.Millisecond):
				// Avoid leaving the test goroutine behind if cancellation regresses.
				_ = blocker.Rollback()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
				}
				t.Fatal("export read ignored owning-job cancellation until SQL lock release")
			}
			require.Eventually(t, func() bool {
				var waiting bool
				return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&waiting) == nil && !waiting
			}, time.Second, 10*time.Millisecond)
			require.Nil(t, reader.db)
			require.NoError(t, db.PingContext(context.Background()))
		})
	}
}
