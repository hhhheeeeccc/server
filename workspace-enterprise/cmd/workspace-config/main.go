// Workspace distribution configuration generator.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mattermost/enterprise/plugin_ids"
	"github.com/mattermost/enterprise/site_links"
	"github.com/mattermost/mattermost/server/public/model"
)

// writeDefaults starts with upstream defaults, never a developer's live config.
func writeDefaults(w io.Writer) error {
	cfg := &model.Config{}
	cfg.SetDefaults()
	site_links.Migrate(cfg)
	plugin_ids.DisableAutomaticPluginDefaults(&cfg.PluginSettings)
	*cfg.TeamSettings.SiteName = "Workspace"
	*cfg.LogSettings.ConsoleLevel = "INFO"
	*cfg.ServiceSettings.SiteURL = ""
	*cfg.EmailSettings.SendEmailNotifications = false
	*cfg.EmailSettings.FeedbackEmail = ""
	*cfg.EmailSettings.ReplyToAddress = ""
	*cfg.EmailSettings.SMTPServer = ""
	*cfg.EmailSettings.SMTPPort = ""

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(cfg)
}

func run(output string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create config (existing files are never overwritten): %w", err)
	}
	if err := writeDefaults(file); err != nil {
		_ = file.Close()
		_ = os.Remove(output)
		return err
	}
	return file.Close()
}

func main() {
	output := flag.String("output", "dist/workspace/config/config.json", "path for a NEW distribution configuration")
	flag.Parse()
	if err := run(*output); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(*output)
}
