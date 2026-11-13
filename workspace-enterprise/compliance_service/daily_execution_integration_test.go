package compliance_service

import (
	"context"
	"database/sql"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func TestDailyExecutionMetadataRequiresOriginalLockSession(t *testing.T) {
	for _, loss := range []string{"unlock", "terminate"} {
		t.Run(loss, func(t *testing.T) {
			th, service := dailyScheduleFixture(t)
			job := savedReadReport(t, th)
			job.Type = model.ComplianceTypeDaily
			_, err := th.Store.Compliance().Update(job)
			require.NoError(t, err)
			execution, acquired, err := service.acquireDailyExecution(context.Background())
			require.NoError(t, err)
			require.True(t, acquired)
			defer execution.Close()
			job.Status = model.ComplianceStatusRunning
			require.NoError(t, execution.update(job))
			var pid int
			require.NoError(t, execution.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
				return db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
			}))
			if loss == "unlock" {
				require.NoError(t, execution.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
					_, err := db.ExecContext(ctx, `SELECT pg_advisory_unlock(11283544,3)`)
					return err
				}))
			} else {
				var terminated bool
				require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
				require.True(t, terminated)
			}
			job.Status, job.Count = model.ComplianceStatusFinished, 123
			require.ErrorIs(t, execution.update(job), errComplianceOwnershipLost)
			require.ErrorIs(t, execution.check(), errComplianceOwnershipLost)
			stored, err := th.Store.Compliance().Get(job.Id)
			require.NoError(t, err)
			require.Equal(t, model.ComplianceStatusRunning, stored.Status)
			require.Zero(t, stored.Count)
			execution.Close()
			replacement, acquired, err := service.acquireDailyExecution(context.Background())
			require.NoError(t, err)
			require.True(t, acquired)
			defer replacement.Close()
			require.NoError(t, replacement.update(job))
			stored, err = th.Store.Compliance().Get(job.Id)
			require.NoError(t, err)
			require.Equal(t, model.ComplianceStatusFinished, stored.Status)
			require.Equal(t, 123, stored.Count)
		})
	}
}

func TestDailyExecutionBoundsSilentStartupAndReleasesFile(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	primary := *th.App.Config().SqlSettings.DataSource
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			accepted, drained := make(chan net.Conn, 1), make(chan struct{})
			go func() {
				defer close(drained)
				conn, err := listener.Accept()
				if err == nil {
					accepted <- conn
					_, _ = io.Copy(io.Discard, conn)
					_ = conn.Close()
				}
			}()
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.SqlSettings.DataSource = model.NewPointer("postgres://probe@" + listener.Addr().String() + "/probe?sslmode=disable&connect_timeout=0")
				cfg.SqlSettings.QueryTimeout = model.NewPointer(1)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				e, acquired, err := service.acquireDailyExecution(ctx)
				if e != nil {
					e.Close()
				}
				if acquired {
					err = nil
				}
				done <- err
			}()
			select {
			case conn := <-accepted:
				defer conn.Close()
			case <-time.After(time.Second):
				t.Fatal("ownership startup did not reach silent fixture")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				want := context.DeadlineExceeded
				if mode == "cancel" {
					want = context.Canceled
				}
				require.ErrorIs(t, err, want)
			case <-time.After(2 * time.Second):
				t.Fatal("ownership startup ignored its SQL budget")
			}
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("ownership startup socket was not closed")
			}
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.SqlSettings.DataSource = model.NewPointer(primary) })
			e, acquired, err := service.acquireDailyExecution(context.Background())
			require.NoError(t, err)
			require.True(t, acquired, "the failed startup must release both the local permit and shared file lock")
			e.Close()
		})
	}
}

func TestDailyExecutionCutsSilentSQLAndCancellationSocket(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	primary := *th.App.Config().SqlSettings.DataSource
	for _, mode := range []string{"deadline", "silent-cancel-link"} {
		t.Run(mode, func(t *testing.T) {
			proxy, source := newComplianceSQLProxy(t, primary)
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.SqlSettings.DataSource = model.NewPointer(source)
				cfg.SqlSettings.QueryTimeout = model.NewPointer(2)
			})
			execution, acquired, err := service.acquireDailyExecution(context.Background())
			require.NoError(t, err)
			require.True(t, acquired)
			defer func() { proxy.Close(); execution.Close() }()
			statementCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			proxy.blocked.Store(true)
			done := make(chan error, 1)
			go func() {
				done <- execution.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
					if mode == "silent-cancel-link" {
						ctx = statementCtx
					}
					_, err := db.ExecContext(ctx, `SELECT 1`)
					return err
				})
			}()
			require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, time.Second, time.Millisecond)
			before := proxy.dropped.Load()
			if mode == "silent-cancel-link" {
				cancel()
				require.Eventually(t, func() bool { return proxy.dropped.Load() > before }, time.Second, time.Millisecond,
					"the driver's cancel request must actually reach the silent proxy")
			}
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.ErrorIs(t, err, errComplianceOwnershipLost)
			case <-time.After(3 * time.Second):
				t.Fatal("the private ownership query or cancellation link remained blocked")
			}
			require.ErrorIs(t, execution.check(), errComplianceOwnershipLost)
			require.NoError(t, th.Store.GetInternalMasterDB().Ping())
		})
	}
}

func TestDailyExecutionDoesNotStartWithoutLicenseOrDailySetting(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	th.Server.SetLicense(nil)
	e, acquired, err := service.acquireDailyExecution(context.Background())
	require.NoError(t, err)
	require.False(t, acquired)
	require.Nil(t, e)
	entries, err := os.ReadDir(*th.App.Config().ComplianceSettings.Directory)
	require.NoError(t, err)
	require.Empty(t, entries)
	license := model.NewTestLicense()
	license.Features.Compliance = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.EnableDaily = model.NewPointer(false) })
	e, acquired, err = service.acquireDailyExecution(context.Background())
	require.NoError(t, err)
	require.False(t, acquired)
	require.Nil(t, e)
	_, err = os.Stat(filepath.Join(*th.App.Config().ComplianceSettings.Directory, "compliance"))
	require.True(t, os.IsNotExist(err))
}

func TestDailyExecutionKeepsOutputInsideLockedDirectory(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	job.Type = model.ComplianceTypeDaily
	_, err := th.Store.Compliance().Update(job)
	require.NoError(t, err)
	e, acquired, err := service.acquireDailyExecution(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)
	defer e.Close()
	unlocked := t.TempDir()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.Directory = model.NewPointer(unlocked) })
	appErr := service.runComplianceJobWithExecution(e.ctx, request.EmptyContext(th.App.Log()), job, e)
	require.NotNil(t, appErr)
	require.Contains(t, appErr.Error(), "directory changed")
	entries, err := os.ReadDir(unlocked)
	require.NoError(t, err)
	require.Empty(t, entries, "a configuration update must not redirect output outside the guarded directory")
	stored, err := th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusFailed, stored.Status)
}
