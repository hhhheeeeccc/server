// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// Package database_migration validates migration history and provides explicit,
// transactional repairs for legacy database constraints.
package database_migration

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

type Entry struct {
	Version int
	Name    string
}

func Read(assets fs.FS) (map[int]string, error) {
	files, err := fs.ReadDir(assets, "migrations/postgres")
	if err != nil {
		return nil, err
	}
	result := make(map[int]string)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".up.sql") {
			continue
		}
		version, name, ok := strings.Cut(strings.TrimSuffix(file.Name(), ".up.sql"), "_")
		n, err := strconv.Atoi(version)
		if !ok || err != nil {
			return nil, fmt.Errorf("invalid migration filename %q", file.Name())
		}
		if _, exists := result[n]; exists {
			return nil, fmt.Errorf("duplicate migration version %d", n)
		}
		result[n] = name
	}
	return result, nil
}

// Validate permits the documented upstream 10.11 backport, which preMigration
// renumbers later. Everything else must match by both number and name: Morph
// uses numbers alone, so accepting a fork's history can silently skip SQL.
func Validate(applied []Entry, expected map[int]string) error {
	backports := map[int]Entry{
		142: {156, "add_schemeid_to_roles"},
		143: {157, "backfill_roles_schemeid"},
		144: {158, "add_roles_schemeid_index"},
	}
	seen := make(map[int]bool)
	var conflicts []string
	for _, migration := range applied {
		original := migration
		if replacement, ok := backports[migration.Version]; ok && replacement.Name == migration.Name {
			migration = replacement
		}
		if name, ok := expected[migration.Version]; !ok || name != migration.Name {
			conflicts = append(conflicts, fmt.Sprintf("%d/%s (binary expects %q)", original.Version, original.Name, expected[migration.Version]))
		}
		if seen[migration.Version] {
			conflicts = append(conflicts, fmt.Sprintf("duplicate effective version %d", migration.Version))
		}
		seen[migration.Version] = true
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("incompatible database migration history; refusing schema changes: %s; review the migration audit and rehearse the upgrade on an isolated copy first", strings.Join(conflicts, "; "))
	}
	return nil
}
