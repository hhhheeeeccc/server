package database_migration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// Same two grouped subqueries and ordering used by SqlRetentionPolicyStore.GetAll.
const retentionPoliciesQuery = `SELECT p.id,p.displayname,p.postduration,c.n,t.n FROM retentionpolicies p
 JOIN (SELECT retentionpolicies.id,count(channelid) n FROM retentionpolicies
 LEFT JOIN retentionpolicieschannels ON retentionpolicies.id=policyid
 GROUP BY retentionpolicies.id ORDER BY retentionpolicies.displayname,retentionpolicies.id LIMIT 10 OFFSET 0) c ON c.id=p.id
 JOIN (SELECT retentionpolicies.id,count(teamid) n FROM retentionpolicies
 LEFT JOIN retentionpoliciesteams ON retentionpolicies.id=policyid
 GROUP BY retentionpolicies.id ORDER BY retentionpolicies.displayname,retentionpolicies.id LIMIT 10 OFFSET 0) t ON t.id=p.id
 ORDER BY p.displayname,p.id`

const retentionPolicyFixture = `CREATE TABLE retentionpolicies(id varchar(26),displayname varchar(64),postduration bigint);
 CREATE TABLE retentionpolicieschannels(policyid varchar(26),channelid varchar(26));
 CREATE TABLE retentionpoliciesteams(policyid varchar(26),teamid varchar(26));
 INSERT INTO retentionpolicies VALUES('policy1','سياسة الاحتفاظ',30),('policy2','ثانية',60);
 INSERT INTO retentionpolicieschannels VALUES('policy1','channel1'),('policy1','channel2');
 INSERT INTO retentionpoliciesteams VALUES('policy1','team1');`

func retentionPolicySnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object('policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id,displayname) FROM retentionpolicies p),'channels',(SELECT jsonb_agg(to_jsonb(c) ORDER BY channelid) FROM retentionpolicieschannels c),'teams',(SELECT jsonb_agg(to_jsonb(t) ORDER BY teamid) FROM retentionpoliciesteams t))::text`).Scan(&s))
	return s
}

func TestRepairRetentionPolicyIDRestoredQuery(t *testing.T) {
	for _, tc := range []struct {
		name, ddl string
		changed   bool
	}{
		{"missing primary key", "", true},
		{"unique index alone is insufficient", `CREATE UNIQUE INDEX legacy_policy_unique ON retentionpolicies(id)`, true},
		{"existing primary key", `ALTER TABLE retentionpolicies ADD PRIMARY KEY(id)`, false},
		{"renamed primary key", `ALTER TABLE retentionpolicies ADD CONSTRAINT legacy_policy_pk PRIMARY KEY(id)`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := licenseTestDatabase(t)
			_, err := db.Exec(retentionPolicyFixture + tc.ddl)
			require.NoError(t, err)
			before := retentionPolicySnapshot(t, db)
			if tc.changed {
				rows, err := db.Query(retentionPoliciesQuery)
				if rows != nil {
					rows.Close()
				}
				require.ErrorContains(t, err, `must appear in the GROUP BY clause`)
			}
			changed, err := RepairRetentionPolicyID(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			rows, err := db.Query(retentionPoliciesQuery)
			require.NoError(t, err)
			count := 0
			for rows.Next() {
				var id, name string
				var days, channels, teams int64
				require.NoError(t, rows.Scan(&id, &name, &days, &channels, &teams))
				if id == "policy1" {
					require.EqualValues(t, 2, channels)
					require.EqualValues(t, 1, teams)
					require.EqualValues(t, 30, days)
				}
				count++
			}
			require.NoError(t, rows.Err())
			rows.Close()
			require.Equal(t, 2, count)
			changed, err = RepairRetentionPolicyID(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, before, retentionPolicySnapshot(t, db), "policies and associations must be unchanged")
		})
	}
}

func TestRepairRetentionPolicyIDConflicts(t *testing.T) {
	for _, tc := range []struct{ name, ddl string }{
		{"duplicate IDs", `INSERT INTO retentionpolicies VALUES('policy1','duplicate',90)`},
		{"null IDs", `INSERT INTO retentionpolicies VALUES(NULL,'missing',90)`},
		{"different primary key", `ALTER TABLE retentionpolicies ADD PRIMARY KEY(displayname)`},
		{"deferrable primary key", `ALTER TABLE retentionpolicies ADD PRIMARY KEY(id) DEFERRABLE`},
		{"constraint name collision", `ALTER TABLE retentionpolicies ADD CONSTRAINT retentionpolicies_pkey CHECK(postduration>0)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := licenseTestDatabase(t)
			_, err := db.Exec(retentionPolicyFixture + tc.ddl)
			require.NoError(t, err)
			before := retentionPolicySnapshot(t, db)
			changed, err := RepairRetentionPolicyID(context.Background(), db)
			require.Error(t, err)
			require.False(t, changed)
			require.Equal(t, before, retentionPolicySnapshot(t, db))
			_, err = db.Exec(`ALTER TABLE retentionpolicies ADD COLUMN after_failed_migration text`)
			require.NoError(t, err, "failed transaction must release its locks")
		})
	}
}
