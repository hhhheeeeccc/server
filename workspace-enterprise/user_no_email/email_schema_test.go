package user_no_email

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
)

func TestEmailSchemaRepairsLegacyNullAndPreventsNewNull(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	ctx := context.Background()
	user, _, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
		Email:    "legacy-email-" + model.NewId() + "@example.com",
		Username: "legacy-email-" + model.NewId(),
		Password: "Test-password1",
	})
	require.NoError(t, err)

	db := th.Store.GetInternalMasterDB()
	_, err = db.ExecContext(ctx, `ALTER TABLE users ALTER COLUMN email DROP NOT NULL`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE users SET email = NULL WHERE id = $1`, user.Id)
	require.NoError(t, err)

	require.NoError(t, (&Service{app: th.App}).ensureEmailSchema(ctx))
	var email string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, user.Id).Scan(&email))
	require.Empty(t, email)

	_, err = db.ExecContext(ctx, `UPDATE users SET email = NULL WHERE id = $1`, user.Id)
	require.Error(t, err)

	var notNull bool
	var defaultExpr string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT a.attnotnull, COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = 'users'::regclass AND a.attname = 'email'
	`).Scan(&notNull, &defaultExpr))
	require.True(t, notNull)
	require.Contains(t, defaultExpr, "''")
}
