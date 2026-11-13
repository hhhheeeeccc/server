package database_migration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func publicChannelTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := licenseTestDatabase(t)
	_, err := db.Exec(`CREATE TABLE channels(id varchar(26) PRIMARY KEY,displayname varchar(64),header text);
 CREATE TABLE posts(id varchar(26) PRIMARY KEY,channelid varchar(26) REFERENCES channels(id),message text);
 CREATE TABLE channelmembers(channelid varchar(26) REFERENCES channels(id),userid varchar(26),roles text);
 CREATE TABLE publicchannels(id varchar(26),deleteat bigint,teamid varchar(26),displayname varchar(64),name varchar(64),header text,purpose text);
 INSERT INTO channels VALUES ('channel-one','Original','Old header');
 INSERT INTO posts VALUES ('post-one','channel-one','Keep this message');
 INSERT INTO channelmembers VALUES ('channel-one','user-one','channel_user');
 INSERT INTO publicchannels VALUES ('channel-one',0,'team-one','Original','channel-name','Old header','Original purpose')`)
	require.NoError(t, err)
	return db
}

func publicChannelSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'channels',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM channels c),
 'publicchannels',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM publicchannels c),
 'posts',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM posts p),
 'members',(SELECT jsonb_agg(to_jsonb(m) ORDER BY userid) FROM channelmembers m))::text`).Scan(&snapshot))
	return snapshot
}

// Mirrors the insert and conflict target in SqlChannelStore.upsertPublicChannelT.
const publicChannelUpsert = `INSERT INTO publicchannels(id,deleteat,teamid,displayname,name,header,purpose)
 VALUES ($1,0,'team-one','Updated','channel-name','New header','New purpose')
 ON CONFLICT (id) DO UPDATE SET deleteat=EXCLUDED.deleteat,teamid=EXCLUDED.teamid,
 displayname=EXCLUDED.displayname,name=EXCLUDED.name,header=EXCLUDED.header,purpose=EXCLUDED.purpose`

func patchPublicChannelForTest(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE channels SET displayname='Updated',header='New header' WHERE id='channel-one'`); err != nil {
		return err
	}
	if _, err = tx.Exec(publicChannelUpsert, "channel-one"); err != nil {
		return err
	}
	return tx.Commit()
}

func TestRepairPublicChannelIDRestoresPatch(t *testing.T) {
	for _, tc := range []struct {
		name, ddl string
		changed   bool
	}{
		{name: "missing key", changed: true},
		{name: "existing primary key", ddl: `ALTER TABLE publicchannels ADD PRIMARY KEY(id)`},
		{name: "renamed unique index with include", ddl: `CREATE UNIQUE INDEX custom_channels ON publicchannels(id) INCLUDE(header)`},
		{name: "partial unique index is insufficient", ddl: `CREATE UNIQUE INDEX partial_channels ON publicchannels(id) WHERE deleteat=0`, changed: true},
		{name: "composite unique index is insufficient", ddl: `CREATE UNIQUE INDEX composite_channels ON publicchannels(id,teamid)`, changed: true},
		{name: "another primary key", ddl: `ALTER TABLE publicchannels ADD PRIMARY KEY(name)`, changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := publicChannelTestDatabase(t)
			if tc.ddl != "" {
				_, err := db.Exec(tc.ddl)
				require.NoError(t, err)
			}
			before := publicChannelSnapshot(t, db)
			if tc.changed {
				err := patchPublicChannelForTest(db)
				var pgErr *pq.Error
				require.ErrorAs(t, err, &pgErr)
				require.Equal(t, pq.ErrorCode("42P10"), pgErr.Code)
				require.Equal(t, before, publicChannelSnapshot(t, db), "failed patch must roll back the channel update")
			}
			changed, err := RepairPublicChannelID(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			require.Equal(t, before, publicChannelSnapshot(t, db), "migration preserves channels, messages and members")
			changed, err = RepairPublicChannelID(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			require.NoError(t, patchPublicChannelForTest(db))
			var matches bool
			require.NoError(t, db.QueryRow(`SELECT c.displayname='Updated' AND c.header='New header' AND p.displayname=c.displayname AND p.header=c.header AND p.purpose='New purpose'
 FROM channels c JOIN publicchannels p ON p.id=c.id WHERE c.id='channel-one'`).Scan(&matches))
			require.True(t, matches)
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM publicchannels WHERE id='channel-one'`).Scan(&count))
			require.Equal(t, 1, count)
			var message, roles string
			require.NoError(t, db.QueryRow(`SELECT message FROM posts WHERE id='post-one'`).Scan(&message))
			require.Equal(t, "Keep this message", message)
			require.NoError(t, db.QueryRow(`SELECT roles FROM channelmembers WHERE userid='user-one'`).Scan(&roles))
			require.Equal(t, "channel_user", roles)
		})
	}
}

func TestRepairPublicChannelIDRejectsConflicts(t *testing.T) {
	for _, tc := range []struct{ name, ddl, errorText string }{
		{"duplicate IDs", `INSERT INTO publicchannels SELECT * FROM publicchannels`, "duplicates (true)"},
		{"null ID", `UPDATE publicchannels SET id=NULL`, "NULLs (true)"},
		{"deferred unique key", `ALTER TABLE publicchannels ADD UNIQUE(id) DEFERRABLE`, "deferrable"},
		{"constraint name collision", `ALTER TABLE publicchannels ADD CONSTRAINT publicchannels_pkey CHECK(deleteat>=0)`, "restore public channel ID constraint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := publicChannelTestDatabase(t)
			_, err := db.Exec(tc.ddl)
			require.NoError(t, err)
			before := publicChannelSnapshot(t, db)
			changed, err := RepairPublicChannelID(context.Background(), db)
			require.ErrorContains(t, err, tc.errorText)
			require.False(t, changed)
			require.Equal(t, before, publicChannelSnapshot(t, db))
			var unique bool
			require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_index WHERE indrelid='publicchannels'::regclass AND indisunique AND indimmediate)`).Scan(&unique))
			require.False(t, unique)
			_, err = db.Exec(`UPDATE publicchannels SET purpose=purpose`)
			require.NoError(t, err, "failed repair must release its lock")
		})
	}
}

func TestRepairPublicChannelIDMissingTable(t *testing.T) {
	db := licenseTestDatabase(t)
	changed, err := RepairPublicChannelID(context.Background(), db)
	require.ErrorContains(t, err, "resolve publicchannels table")
	require.False(t, changed)
}
