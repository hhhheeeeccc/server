package database_migration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepairUserEmailRestoredReports(t *testing.T) {
	for _, ddl := range []string{
		`CREATE UNIQUE INDEX idx_users_email ON users(email)`,
		`ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE(email)`,
		`CREATE UNIQUE INDEX legacy_email_name ON users(email) INCLUDE(username)`,
		`CREATE UNIQUE INDEX idx_users_email ON users(email); CREATE UNIQUE INDEX idx_users_email_unique ON users(email) WHERE email<>''`,
		`CREATE UNIQUE INDEX idx_users_email ON users(email) WHERE email<>''`,
		``,
	} {
		t.Run(ddl, func(t *testing.T) {
			db := licenseTestDatabase(t)
			_, err := db.Exec(`CREATE TABLE users(id text PRIMARY KEY,username text,email varchar(128));
    INSERT INTO users VALUES('one','الأول',NULL),('two','الثاني',NULL),('real','ثالث','real@example.com'),('empty','رابع','');` + ddl)
			require.NoError(t, err)
			var email string
			require.ErrorContains(t, db.QueryRow(`SELECT email FROM users WHERE id='one'`).Scan(&email), "converting NULL to string")
			before := userEmailSnapshot(t, db)
			require.NoError(t, RepairUserEmail(context.Background(), db))
			require.Equal(t, before, userEmailSnapshot(t, db), "only NULL emails may change")
			rows, err := db.Query(`SELECT email FROM users ORDER BY id`)
			require.NoError(t, err)
			for rows.Next() {
				require.NoError(t, rows.Scan(&email))
			}
			require.NoError(t, rows.Err())
			rows.Close()
			require.NoError(t, RepairUserEmail(context.Background(), db), "repeat must succeed")
			require.Equal(t, before, userEmailSnapshot(t, db))
			_, err = db.Exec(`INSERT INTO users(id) VALUES('default'); INSERT INTO users(id,email) VALUES('another','')`)
			require.NoError(t, err)
			require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id='default'`).Scan(&email))
			require.Empty(t, email)
			_, err = db.Exec(`INSERT INTO users(id,email) VALUES('duplicate','real@example.com')`)
			require.Error(t, err, "real emails stay unique")
			_, err = db.Exec(`UPDATE users SET email=NULL WHERE id='one'`)
			require.Error(t, err, "future NULLs are rejected")
		})
	}
}

func userEmailSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var value string
	require.NoError(t, db.QueryRow(`SELECT jsonb_agg(jsonb_build_array(id,username,COALESCE(email,'')) ORDER BY id)::text FROM users`).Scan(&value))
	return value
}

func TestRepairUserEmailRefusesConflictsAtomically(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"duplicate real emails", `INSERT INTO users VALUES('duplicate','keep','real@example.com')`},
		{"dependent foreign key", `ALTER TABLE users ADD CONSTRAINT email_key UNIQUE(email); CREATE TABLE email_refs(email text REFERENCES users(email))`},
		{"deferrable uniqueness", `ALTER TABLE users ADD UNIQUE(email) DEFERRABLE`},
		{"index name collision", `CREATE INDEX idx_users_email_unique ON users(username)`},
		{"incompatible empty-value check", `ALTER TABLE users ADD CHECK(email IS NULL OR email<>'')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := licenseTestDatabase(t)
			_, err := db.Exec(`CREATE TABLE users(id text PRIMARY KEY,username text,email varchar(128)); INSERT INTO users VALUES('one','preserve',NULL),('real','keep','real@example.com');` + tc.extra)
			require.NoError(t, err)
			snapshot := func() string {
				var s string
				require.NoError(t, db.QueryRow(`SELECT jsonb_build_object('rows',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM users u),'indexes',(SELECT jsonb_agg(indexdef ORDER BY indexname) FROM pg_indexes WHERE schemaname=current_schema() AND tablename='users'))::text`).Scan(&s))
				return s
			}
			before := snapshot()
			require.Error(t, RepairUserEmail(context.Background(), db))
			require.Equal(t, before, snapshot(), "data and DDL must roll back together")
		})
	}
}
