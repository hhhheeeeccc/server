package cluster

import (
	"database/sql"
	"net/url"
	"testing"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// Only the child fixture's coordination pool uses this role. Native app SQL
// remains healthy, while every actual peer-discovery SELECT is denied by
// PostgreSQL. No product hook, driver mock or discovery result is substituted.
func clusterDiscoveryDeniedDataSource(t *testing.T, admin *sql.DB, dsn string) string {
	t.Helper()
	role, password := "cluster_discovery_"+model.NewId(), model.NewId()
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
	_, err = admin.Exec(`REVOKE SELECT ON ClusterDiscovery FROM ` + quoted)
	require.NoError(t, err)
	// The real heartbeat's WHERE Id=$2 needs this column privilege, even
	// though its UPDATE supplies LastPingAt as a parameter. Keep it working
	// while withholding every column needed to discover remote endpoints.
	_, err = admin.Exec(`GRANT SELECT (Id) ON ClusterDiscovery TO ` + quoted)
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
	_, err = restricted.Exec(`UPDATE ClusterDiscovery SET LastPingAt=$1 WHERE Id=$2`, model.GetMillis(), model.NewId())
	require.NoError(t, err, "the fixture must preserve the actual heartbeat statement's privileges")
	err = restricted.QueryRow(`SELECT GossipPort FROM ClusterDiscovery LIMIT 1`).Scan(&value)
	var pgError *pq.Error
	require.ErrorAs(t, err, &pgError)
	require.Equal(t, pq.ErrorCode("42501"), pgError.Code)
	t.Log("coordination fixture: PostgreSQL SELECT on ClusterDiscovery rejected with 42501; native application keeps its original pool")
	return address.String()
}
