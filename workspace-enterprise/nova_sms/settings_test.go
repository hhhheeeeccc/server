package nova_sms

import (
	"encoding/json"
	"testing"
)

func TestValidUpdate(t *testing.T) {
	valid := settingsUpdate{
		Endpoint: "http://192.168.20.139:3000", ApplicationIdentifier: "sample-app", SecretKey: "test-secret", Version: 0,
	}
	if !validUpdate(valid) {
		t.Fatal("private HTTP Nova endpoint should be accepted")
	}
	for _, endpoint := range []string{"", "ftp://example.com", "http://user:pass@example.com", "http://example.com#fragment", "not a URL"} {
		input := valid
		input.Endpoint = endpoint
		if validUpdate(input) {
			t.Errorf("accepted invalid endpoint %q", endpoint)
		}
	}
	valid.ApplicationIdentifier = "contains spaces"
	if validUpdate(valid) {
		t.Fatal("accepted whitespace in application identifier")
	}
	valid.ApplicationIdentifier = "sample-app"
	valid.Enabled = true
	if validUpdate(valid) {
		t.Fatal("accepted enabled delivery without a workflow")
	}
	valid.WorkflowIdentifier = "urgent-sms"
	if !validUpdate(valid) {
		t.Fatal("rejected enabled delivery with a workflow")
	}
}

func TestSettingsResponseNeverContainsSecret(t *testing.T) {
	raw, err := json.Marshal(Settings{Endpoint: "https://example.com", ApplicationIdentifier: "app", HasSecretKey: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exposed := fields["secret_key"]; exposed {
		t.Fatal("settings response exposed a secret-key field")
	}
}
