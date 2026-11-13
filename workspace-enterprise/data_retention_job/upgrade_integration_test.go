package data_retention_job

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Run the actual core SQL migration in a disposable database. Its constraint
// existence checks inspect the whole database, so a separate schema inside
// mattermost_test would give a misleading result.
func TestRetentionPolicyMigrationKeepsExistingRows(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	seed := make([]byte, 8)
	_, err := rand.Read(seed)
	require.NoError(t, err)
	databaseName := "workspace_retention_test_" + hex.EncodeToString(seed)
	adminDB := th.Store.GetInternalMasterDB()
	_, err = adminDB.Exec(`CREATE DATABASE ` + databaseName)
	if err != nil {
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			t.Skip("PostgreSQL role cannot create an isolated test database")
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, dropErr := adminDB.Exec(`DROP DATABASE ` + databaseName + ` WITH (FORCE)`)
		require.NoError(t, dropErr)
	})

	connection, err := url.Parse(model.SafeDereference(th.App.Config().SqlSettings.DataSource))
	require.NoError(t, err)
	require.Equal(t, "postgres", connection.Scheme)
	connection.Path = "/" + databaseName
	query := connection.Query()
	query.Del("options") // Do not inherit testlib's schema search_path.
	connection.RawQuery = query.Encode()
	db, err := sql.Open("postgres", connection.String())
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Ping())

	// Synthetic pre-000053 tables and index. The test exercises the SQL file,
	// rather than claiming to restore a full historic server database.
	_, err = db.Exec(`CREATE TABLE retentionpolicies (id varchar(26) PRIMARY KEY, displayname varchar(64), postduration bigint);
		CREATE TABLE retentionpoliciesteams (policyid varchar(26), teamid varchar(26) PRIMARY KEY);
		CREATE TABLE retentionpolicieschannels (policyid varchar(26), channelid varchar(26) PRIMARY KEY);
		CREATE INDEX idx_retentionpolicies_displayname_id ON retentionpolicies (displayname, id)`)
	require.NoError(t, err)
	policyID, teamID, channelID := model.NewId(), model.NewId(), model.NewId()
	_, err = db.Exec(`INSERT INTO retentionpolicies VALUES ($1,$2,$3)`, policyID, "Stored before upgrade", 14)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO retentionpoliciesteams VALUES ($1,$2)`, policyID, teamID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO retentionpolicieschannels VALUES ($1,$2)`, policyID, channelID)
	require.NoError(t, err)

	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(sourceFile), "..", "..", "mattermost", "server", "channels", "db", "migrations", "postgres", "000053_create_retention_policies.up.sql")
	migration, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err, "repeating the migration must preserve existing rows")

	var displayName string
	var duration int64
	require.NoError(t, db.QueryRow(`SELECT displayname, postduration FROM retentionpolicies WHERE id=$1`, policyID).Scan(&displayName, &duration))
	require.Equal(t, "Stored before upgrade", displayName)
	require.Equal(t, int64(14), duration)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM retentionpoliciesteams WHERE policyid=$1 AND teamid=$2`, policyID, teamID).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM retentionpolicieschannels WHERE policyid=$1 AND channelid=$2`, policyID, channelID).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname='idx_retentionpolicies_displayname'`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname='idx_retentionpolicies_displayname_id'`).Scan(&count))
	require.Zero(t, count)
	_, err = db.Exec(`DELETE FROM retentionpolicies WHERE id=$1`, policyID)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM retentionpoliciesteams WHERE policyid=$1`, policyID).Scan(&count))
	require.Zero(t, count, "the migration must add a cascade foreign key for teams")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM retentionpolicieschannels WHERE policyid=$1`, policyID).Scan(&count))
	require.Zero(t, count, "the migration must add a cascade foreign key for channels")
}
