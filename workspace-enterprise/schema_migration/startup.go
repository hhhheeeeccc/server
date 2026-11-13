package schema_migration

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/channels/store/sqlstore"
)

// Register uses the existing store-option extension point. The Enterprise
// license constructor runs before the platform creates its SQL store, so the
// repair can run before Morph without modifying the core migration engine.
func Register(p *platform.PlatformService) {
	// StoreOption only appends an option and always returns nil.
	_ = platform.StoreOption(StartupOption(p.Config().SqlSettings, p.Log()))(p)
}

// StartupOption reconciles only the explicitly supported legacy history. It
// uses a short-lived connection because SqlStore options run before the core
// connection exists. Errors stop store creation before any core migrations.
func StartupOption(settings model.SqlSettings, logger mlog.LoggerIFace) sqlstore.Option {
	return func(*sqlstore.SqlStore) error {
		if settings.DriverName == nil || *settings.DriverName != model.DatabaseDriverPostgres || settings.DataSource == nil {
			return fmt.Errorf("legacy migration history repair requires PostgreSQL settings")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		db, err := sql.Open("postgres", *settings.DataSource)
		if err != nil {
			return fmt.Errorf("open legacy migration history connection: %w", err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("connect for legacy migration history repair: %w", err)
		}
		changed, err := ReconcileLegacyVersions(ctx, db)
		if err != nil {
			return fmt.Errorf("reconcile legacy migration history before core migrations: %w", err)
		}
		if changed {
			logger.Info("Reconciled legacy migration history before core migrations; branch-only records preserved in db_migrations_legacy_versions")
		}
		return nil
	}
}
