package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
	"github.com/mattermost/enterprise/file_metadata_repair"
)

func run(configPath, database string, apply bool) error {
	if configPath == "" || database == "" {
		return fmt.Errorf("-config and -database are required")
	}
	// Read precisely this JSON file; do not initialize the application or load
	// configuration defaults, environment overrides, plugins, or migrations.
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var config struct {
		SqlSettings struct {
			DriverName string
			DataSource string
		}
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if config.SqlSettings.DriverName != "postgres" || config.SqlSettings.DataSource == "" {
		return fmt.Errorf("config must contain an explicit PostgreSQL SqlSettings.DataSource")
	}
	db, err := sql.Open("postgres", config.SqlSettings.DataSource)
	if err != nil {
		return fmt.Errorf("invalid PostgreSQL connection settings")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	report, err := file_metadata_repair.Run(ctx, db, database, apply)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func main() {
	config := flag.String("config", "", "explicit config.json path (read only)")
	database := flag.String("database", "", "expected PostgreSQL database name")
	apply := flag.Bool("apply", false, "archive original metadata and normalize NULL optional strings; default only inspects")
	flag.Parse()
	if err := run(*config, *database, *apply); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
