package database_migration

import (
	"testing"

	"github.com/mattermost/mattermost/server/v8/channels/db"
)

func TestValidate(t *testing.T) {
	expected, err := Read(db.Assets())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		history []Entry
		valid   bool
	}{
		{"fresh", nil, true},
		{"upstream", []Entry{{153, "add_translation_channel_id"}}, true},
		{"legacy fork collision", []Entry{{153, "add_download_count"}}, false},
		{"shifted fork migration", []Entry{{160, "add_translation_channel_id"}}, false},
		{"future binary", []Entry{{999, "future"}}, false},
		{"backport", []Entry{{142, "add_schemeid_to_roles"}, {143, "backfill_roles_schemeid"}, {144, "add_roles_schemeid_index"}}, true},
		{"backport target collision", []Entry{{142, "add_schemeid_to_roles"}, {156, "add_schemeid_to_roles"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.history, expected); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	all := make([]Entry, 0, len(expected))
	for version, name := range expected {
		all = append(all, Entry{version, name})
	}
	if err := Validate(all, expected); err != nil {
		t.Fatal(err)
	}
}
