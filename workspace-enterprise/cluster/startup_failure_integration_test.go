package cluster

import (
	"database/sql"
	"net/url"
	"testing"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func clusterStartupDeniedStatement(failure string) (string, string) {
	if failure == "startup schema read denied" {
		return `SELECT Version FROM db_migrations ORDER BY Version DESC LIMIT 1`, "db_migrations"
	}
	return `INSERT INTO ClusterDiscovery (Id,Type,ClusterName,Hostname,GossipPort,Port,CreateAt,LastPingAt) VALUES ('startup-probe','mattermost_app','startup-probe','127.0.0.1',8074,0,1,1)`, "clusterdiscovery"
}

// Fail real coordination SQL before worker creation, either before memberlist
// creation or at the registration INSERT after it. Application SQL keeps the
// original pool and privileges. Credentials exist only in the test config.
func clusterStartupDeniedDataSource(t *testing.T, admin *sql.DB, dsn, failure string) string {
	t.Helper()
	role, password := "cluster_startup_"+model.NewId(), model.NewId()
	quoted := pq.QuoteIdentifier(role)
	_, err := admin.Exec(`CREATE ROLE ` + quoted + ` LOGIN PASSWORD ` + pq.QuoteLiteral(password))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(`DROP OWNED BY ` + quoted)
		require.NoError(t, err)
		_, err = admin.Exec(`DROP ROLE ` + quoted)
		require.NoError(t, err)
	})
	_, err = admin.Exec(`GRANT USAGE, CREATE ON SCHEMA public TO ` + quoted)
	require.NoError(t, err)
	_, err = admin.Exec(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + quoted)
	require.NoError(t, err)
	statement, table := clusterStartupDeniedStatement(failure)
	privilege := "INSERT"
	if failure == "startup schema read denied" {
		privilege = "SELECT"
	}
	_, err = admin.Exec(`REVOKE ` + privilege + ` ON ` + table + ` FROM ` + quoted)
	require.NoError(t, err)
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, address.Scheme)
	address.User = url.UserPassword(role, password)
	restricted, err := newCoordinationDatabase(address.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restricted.Close()) })
	var value int
	require.NoError(t, restricted.QueryRow(`SELECT 1`).Scan(&value))
	require.NoError(t, prepareSchema(t.Context(), restricted), "the fixture must reach the intended failing startup statement")
	_, err = restricted.Exec(statement)
	var pgError *pq.Error
	require.ErrorAs(t, err, &pgError)
	require.Equal(t, pq.ErrorCode("42501"), pgError.Code)
	t.Logf("startup fixture: actual %s on %s rejected with PostgreSQL 42501; native application pool unchanged", privilege, table)
	return address.String()
}
