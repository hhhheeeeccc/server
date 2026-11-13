package database_migration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func propertyValueTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := licenseTestDatabase(t)
	_, err := db.Exec(`CREATE TABLE propertyvalues (
 id varchar(26) PRIMARY KEY, targetid varchar(255) NOT NULL, targettype varchar(255) NOT NULL,
 groupid varchar(26) NOT NULL, fieldid varchar(26) NOT NULL, value jsonb NOT NULL,
 createat bigint NOT NULL, updateat bigint NOT NULL, deleteat bigint NOT NULL,
 createdby varchar(26), updatedby varchar(26));
 INSERT INTO propertyvalues VALUES
 ('original','system','system','access','classification','["secret"]',10,20,0,'creator','editor'),
 ('archived','system','system','access','other','["archived"]',1,2,3,NULL,NULL)`)
	require.NoError(t, err)
	return db
}

func propertyValueSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT jsonb_agg(to_jsonb(v) ORDER BY id)::text FROM propertyvalues v`).Scan(&snapshot))
	return snapshot
}

// Keep the conflict target, partial predicate, updates and returning columns
// aligned with SqlPropertyValueStore.Upsert, rather than testing index names only.
const propertyValueUpsert = `INSERT INTO propertyvalues
 (id,targetid,targettype,groupid,fieldid,value,createat,updateat,deleteat,createdby,updatedby)
 VALUES ($1,$2,'system','access',$3,$4,100,100,0,'new-creator','new-editor')
 ON CONFLICT (groupid,targetid,fieldid) WHERE deleteat=0
 DO UPDATE SET value=EXCLUDED.value,updateat=EXCLUDED.updateat,deleteat=0,updatedby=EXCLUDED.updatedby
 RETURNING id,targetid,targettype,groupid,fieldid,value,createat,updateat,deleteat,
 COALESCE(createdby,''),COALESCE(updatedby,'')`

func TestRepairPropertyValueUniquenessPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, ddl string
		changed   bool
	}{
		{name: "missing index", changed: true},
		{name: "existing canonical index", ddl: `CREATE UNIQUE INDEX idx_propertyvalues_unique ON propertyvalues(groupid,targetid,fieldid) WHERE deleteat=0`},
		{name: "renamed reordered index with include", ddl: `CREATE UNIQUE INDEX custom_values ON propertyvalues(fieldid,groupid,targetid) INCLUDE(value) WHERE deleteat=0`},
		{name: "full unique index", ddl: `CREATE UNIQUE INDEX custom_values ON propertyvalues(groupid,targetid,fieldid)`},
		{name: "nonunique is insufficient", ddl: `CREATE INDEX custom_values ON propertyvalues(groupid,targetid,fieldid) WHERE deleteat=0`, changed: true},
		{name: "wrong predicate is insufficient", ddl: `CREATE UNIQUE INDEX custom_values ON propertyvalues(groupid,targetid,fieldid) WHERE deleteat>0`, changed: true},
		{name: "extra key is insufficient", ddl: `CREATE UNIQUE INDEX custom_values ON propertyvalues(groupid,targetid,fieldid,targettype) WHERE deleteat=0`, changed: true},
		{name: "group and field index is insufficient", ddl: `CREATE INDEX idx_propertyvalues_groupid_fieldid ON propertyvalues(groupid,fieldid) WHERE deleteat=0`, changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := propertyValueTestDatabase(t)
			if tc.ddl != "" {
				_, err := db.Exec(tc.ddl)
				require.NoError(t, err)
			}
			if tc.changed {
				_, err := db.Exec(propertyValueUpsert, "attempt", "system", "classification", `["changed"]`)
				var pgErr *pq.Error
				require.ErrorAs(t, err, &pgErr)
				require.Equal(t, pq.ErrorCode("42P10"), pgErr.Code)
			}
			before := propertyValueSnapshot(t, db)
			changed, err := RepairPropertyValueUniqueness(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			require.Equal(t, before, propertyValueSnapshot(t, db), "migration must not change values or audit metadata")
			changed, err = RepairPropertyValueUniqueness(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			rows, err := db.Query(propertyValueUpsert, "attempt", "system", "classification", `["changed"]`)
			require.NoError(t, err)
			require.True(t, rows.Next())
			var id, target, targetType, group, field, value, creator, editor string
			var created, updated, deleted int64
			require.NoError(t, rows.Scan(&id, &target, &targetType, &group, &field, &value, &created, &updated, &deleted, &creator, &editor))
			require.False(t, rows.Next())
			require.NoError(t, rows.Err())
			rows.Close()
			require.Equal(t, "original", id, "upsert must update, not duplicate the existing scope")
			require.JSONEq(t, `["changed"]`, value)
			require.Equal(t, int64(10), created)
			require.Equal(t, int64(100), updated)
			require.Zero(t, deleted)
			require.Equal(t, "creator", creator)
			require.Equal(t, "new-editor", editor)
			_, err = db.Exec(propertyValueUpsert, "new-target", "channel-one", "classification", `["new"]`)
			require.NoError(t, err, "a different target must have its own value")
			newField := "other"
			if tc.name == "full unique index" {
				// A pre-existing full index also matches archived records. Leave
				// that schema's original behavior unchanged; do not revive one here.
				newField = "third"
			}
			_, err = db.Exec(propertyValueUpsert, "new-active", "system", newField, `["active"]`)
			require.NoError(t, err)
			var archived string
			require.NoError(t, db.QueryRow(`SELECT value FROM propertyvalues WHERE id='archived'`).Scan(&archived))
			require.JSONEq(t, `["archived"]`, archived)
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM propertyvalues WHERE groupid='access' AND targetid='system' AND fieldid='classification' AND deleteat=0`).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

func TestRepairPropertyValueUniquenessRejectsConflicts(t *testing.T) {
	for _, tc := range []struct{ name, ddl, errorText string }{
		{"active duplicates", `INSERT INTO propertyvalues SELECT 'duplicate',targetid,targettype,groupid,fieldid,value,createat,updateat,deleteat,createdby,updatedby FROM propertyvalues WHERE id='original'`, "duplicate scopes (true)"},
		{"null active key", `ALTER TABLE propertyvalues ALTER COLUMN fieldid DROP NOT NULL; UPDATE propertyvalues SET fieldid=NULL WHERE id='original'`, "NULL keys (true)"},
		{"wrong canonical index", `CREATE INDEX idx_propertyvalues_unique ON propertyvalues(groupid)`, "restore property value conflict index"},
		{"name collision", `CREATE TABLE idx_propertyvalues_unique(id text)`, "restore property value conflict index"},
		{"deferred constraint", `ALTER TABLE propertyvalues ADD CONSTRAINT deferred_values UNIQUE(groupid,targetid,fieldid) DEFERRABLE`, "deferrable"},
		{"deferred despite valid partial index", `ALTER TABLE propertyvalues ADD CONSTRAINT deferred_values UNIQUE(groupid,targetid,fieldid) DEFERRABLE; CREATE UNIQUE INDEX valid_values ON propertyvalues(groupid,targetid,fieldid) WHERE deleteat=0`, "deferrable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := propertyValueTestDatabase(t)
			_, err := db.Exec(tc.ddl)
			require.NoError(t, err)
			before := propertyValueSnapshot(t, db)
			changed, err := RepairPropertyValueUniqueness(context.Background(), db)
			require.ErrorContains(t, err, tc.errorText)
			require.False(t, changed)
			require.Equal(t, before, propertyValueSnapshot(t, db))
			var validCanonical bool
			require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_index WHERE indexrelid=to_regclass('idx_propertyvalues_unique') AND indisunique)`).Scan(&validCanonical))
			require.False(t, validCanonical, "failed repairs must not leave a new index")
			_, err = db.Exec(`INSERT INTO propertyvalues VALUES ('after-rollback','other','system','access','other','[]',1,1,0,NULL,NULL)`)
			require.NoError(t, err, "the transaction must release its lock after failure")
		})
	}
}

func TestRepairPropertyValueUniquenessArchivedDuplicates(t *testing.T) {
	db := propertyValueTestDatabase(t)
	_, err := db.Exec(`INSERT INTO propertyvalues SELECT 'older',targetid,targettype,groupid,fieldid,value,createat,updateat,99,createdby,updatedby FROM propertyvalues WHERE id='original'`)
	require.NoError(t, err)
	before := propertyValueSnapshot(t, db)
	changed, err := RepairPropertyValueUniqueness(context.Background(), db)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, before, propertyValueSnapshot(t, db))
	_, err = db.Exec(propertyValueUpsert, "attempt", "system", "classification", `["new"]`)
	require.NoError(t, err)
	var archived string
	require.NoError(t, db.QueryRow(`SELECT value FROM propertyvalues WHERE id='older'`).Scan(&archived))
	require.JSONEq(t, `["secret"]`, archived)
}

func TestRepairPropertyValueUniquenessMissingTable(t *testing.T) {
	db := licenseTestDatabase(t)
	changed, err := RepairPropertyValueUniqueness(context.Background(), db)
	require.ErrorContains(t, err, "resolve propertyvalues table")
	require.False(t, changed)
}
