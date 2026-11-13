package database_migration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func propertyFieldTestSchema(t *testing.T) *sql.DB {
	t.Helper()
	db := licenseTestDatabase(t)
	_, err := db.Exec(`CREATE TABLE propertyfields (
 id text PRIMARY KEY, groupid varchar(26), targetid varchar(26) DEFAULT '',
 name varchar(255), objecttype varchar(64) DEFAULT '', targettype varchar(64) DEFAULT 'system',
 deleteat bigint DEFAULT 0, linkedfieldid text, attrs jsonb DEFAULT '{}'::jsonb);
 CREATE TABLE propertyvalues(id text PRIMARY KEY, fieldid text REFERENCES propertyfields(id), value jsonb);
 INSERT INTO propertyfields(id,groupid,name,objecttype,attrs) VALUES
 ('template','access','classification','template','{"options":[{"id":"option-one","rank":1}]}');
 INSERT INTO propertyvalues VALUES ('value-one','template','"preserved"')`)
	require.NoError(t, err)
	return db
}

func propertyFieldSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'fields',(SELECT jsonb_agg(to_jsonb(f) ORDER BY id) FROM propertyfields f),
 'values',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM propertyvalues v))::text`).Scan(&snapshot))
	return snapshot
}

func TestRepairPropertyFieldUniquenessPostgres(t *testing.T) {
	for _, variant := range []string{"constraint", "standalone index", "existing core indexes"} {
		t.Run(variant, func(t *testing.T) {
			db := propertyFieldTestSchema(t)
			ddl := `ALTER TABLE propertyfields ADD CONSTRAINT propertyfields_groupid_name_unique UNIQUE(groupid,name)`
			if variant == "standalone index" {
				ddl = `CREATE UNIQUE INDEX propertyfields_groupid_name_unique ON propertyfields(groupid,name)`
			}
			_, err := db.Exec(ddl)
			require.NoError(t, err)
			if variant == "existing core indexes" {
				_, err = db.Exec(`CREATE UNIQUE INDEX idx_propertyfields_unique_legacy ON propertyfields(groupid,targetid,name) WHERE deleteat=0 AND objecttype='';
 CREATE UNIQUE INDEX idx_propertyfields_unique_typed ON propertyfields(objecttype,groupid,targettype,targetid,name) WHERE deleteat=0 AND objecttype!=''`)
				require.NoError(t, err)
			}
			const insertSystem = `INSERT INTO propertyfields(id,groupid,name,objecttype,linkedfieldid) VALUES ('system','access','classification','system','template')`
			_, err = db.Exec(insertSystem)
			var pgErr *pq.Error
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, pq.ErrorCode("23505"), pgErr.Code)
			require.Equal(t, "propertyfields_groupid_name_unique", pgErr.Constraint)
			before := propertyFieldSnapshot(t, db)
			changed, err := RepairPropertyFieldUniqueness(context.Background(), db)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, before, propertyFieldSnapshot(t, db))
			changed, err = RepairPropertyFieldUniqueness(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			_, err = db.Exec(insertSystem)
			require.NoError(t, err, "linked system field must coexist with the template")
			_, err = db.Exec(`INSERT INTO propertyfields(id,groupid,name,objecttype) VALUES ('duplicate','access','classification','system')`)
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, "idx_propertyfields_unique_typed", pgErr.Constraint)
			_, err = db.Exec(`INSERT INTO propertyfields(id,groupid,name,objecttype,targetid) VALUES ('different-target','access','classification','system','channel-one');
 INSERT INTO propertyfields(id,groupid,name,objecttype,deleteat) VALUES ('archived','access','classification','system',1);
 INSERT INTO propertyfields(id,groupid,name) VALUES ('legacy','access','classification')`)
			require.NoError(t, err, "different scopes and archived fields remain supported")
			_, err = db.Exec(`INSERT INTO propertyfields(id,groupid,name) VALUES ('legacy-duplicate','access','classification')`)
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, "idx_propertyfields_unique_legacy", pgErr.Constraint)
			after := propertyFieldSnapshot(t, db)
			changed, err = RepairPropertyFieldUniqueness(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, after, propertyFieldSnapshot(t, db))
		})
	}
}

func TestRepairPropertyFieldUniquenessRejectsUnexpectedSchemas(t *testing.T) {
	for _, tc := range []struct{ name, ddl, errorText string }{
		{"wrong obsolete key", `CREATE UNIQUE INDEX propertyfields_groupid_name_unique ON propertyfields(name)`, "unexpected definition"},
		{"wrong typed index", `CREATE INDEX idx_propertyfields_unique_typed ON propertyfields(name)`, "unexpected definition"},
		{"wrong predicate", `CREATE UNIQUE INDEX idx_propertyfields_unique_typed ON propertyfields(objecttype,groupid,targettype,targetid,name) WHERE deleteat=1 AND objecttype<>''`, "unexpected definition"},
		{"index name collision", `CREATE TABLE idx_propertyfields_unique_typed(id text)`, "restore idx_propertyfields_unique_typed"},
		{"dependent foreign key", `CREATE TABLE dependent(groupid varchar(26),name varchar(255),FOREIGN KEY(groupid,name) REFERENCES propertyfields(groupid,name))`, "replace obsolete property field key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := propertyFieldTestSchema(t)
			if tc.name != "wrong obsolete key" {
				_, err := db.Exec(`ALTER TABLE propertyfields ADD CONSTRAINT propertyfields_groupid_name_unique UNIQUE(groupid,name)`)
				require.NoError(t, err)
			}
			_, err := db.Exec(tc.ddl)
			require.NoError(t, err)
			before := propertyFieldSnapshot(t, db)
			changed, err := RepairPropertyFieldUniqueness(context.Background(), db)
			require.ErrorContains(t, err, tc.errorText)
			require.False(t, changed)
			require.Equal(t, before, propertyFieldSnapshot(t, db))
			var oldExists, legacyExists bool
			require.NoError(t, db.QueryRow(`SELECT to_regclass('propertyfields_groupid_name_unique') IS NOT NULL,to_regclass('idx_propertyfields_unique_legacy') IS NOT NULL`).Scan(&oldExists, &legacyExists))
			require.True(t, oldExists, "old protection remains after rollback")
			require.False(t, legacyExists, "newly created replacement index rolls back")
		})
	}
}

func TestRepairPropertyFieldUniquenessNoOldKey(t *testing.T) {
	db := propertyFieldTestSchema(t)
	before := propertyFieldSnapshot(t, db)
	changed, err := RepairPropertyFieldUniqueness(context.Background(), db)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, before, propertyFieldSnapshot(t, db))
}
