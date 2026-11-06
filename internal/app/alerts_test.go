package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// secretAddress is what a Slack incoming webhook looks like: the path is the
// credential, so no error may carry any of it.
const secretAddress = "https://hooks.slack.com/services/T000/B000/s3cretS3cretS3cret"

// AN ADDRESS IS READ FROM WHERE THE CONFIG SAYS AND NOWHERE ELSE, and every refusal
// names where it looked and nothing of what it found.
func TestAnAlertAddressIsReadFromWhereTheConfigSays(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}

		return path
	}

	t.Setenv("BILLET_TEST_ALERT_SET", secretAddress)
	t.Setenv("BILLET_TEST_ALERT_EMPTY", "")
	t.Setenv("BILLET_TEST_ALERT_CLEAR", "http://hooks.example.com/services/s3cretS3cretS3cret")

	for _, tc := range []struct {
		name        string
		destination config.AlertDestination
		refused     string
	}{
		{"from the environment", config.AlertDestination{URLEnv: "BILLET_TEST_ALERT_SET"}, ""},
		{"from a file", config.AlertDestination{URLFile: write("ok", secretAddress+"\n", 0o600)}, ""},
		{"an empty variable", config.AlertDestination{URLEnv: "BILLET_TEST_ALERT_EMPTY"}, "is empty"},
		{"a file others can read", config.AlertDestination{URLFile: write("open", secretAddress, 0o644)}, "mode 0644"},
		{"a missing file", config.AlertDestination{URLFile: filepath.Join(dir, "absent")}, "does not exist"},
		{"plain http off loopback", config.AlertDestination{URLEnv: "BILLET_TEST_ALERT_CLEAR"}, "not usable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, err := AlertAddress(&tc.destination)

			if tc.refused == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if string(address) != secretAddress {
					t.Errorf("read %d bytes, not the address", len(address))
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("err = %v, want one saying %q", err, tc.refused)
			}
			for _, secret := range []string{"s3cretS3cretS3cret", "hooks."} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal quotes the address: %v", err)
				}
			}
		})
	}
}

// A DESTINATION WHOSE ADDRESS CANNOT BE HAD STOPS NOTHING: the set is built, the error
// is logged without the address, and the destination's sender refuses every notice
// with the reason, so the rules naming it record why nothing arrived.
func TestAnUnreadableDestinationStopsNothingAndSaysWhy(t *testing.T) {
	t.Setenv("BILLET_TEST_ALERT_GOOD", secretAddress)

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	cfg := &config.Config{Server: &config.ServerConfig{Alerts: &config.AlertsConfig{
		Destinations: []config.AlertDestination{
			{Name: "good", Kind: config.AlertSlack, URLEnv: "BILLET_TEST_ALERT_GOOD"},
			{Name: "unset", Kind: config.AlertWebhook, URLEnv: "BILLET_TEST_ALERT_NEVER_SET"},
		},
		Rules: []config.AlertRule{{Name: "r", On: config.AlertConditions{Failed: true}, Notify: []string{"good", "unset"}}},
	}}}

	set, err := BuildAlerts(cfg, log)
	if err != nil {
		t.Fatalf("BuildAlerts: %v", err)
	}
	if len(set.Rules) != 1 || len(set.Destinations) != 2 {
		t.Fatalf("set = %d rules, %d destinations", len(set.Rules), len(set.Destinations))
	}

	err = set.Destinations["unset"].Sender.Send(t.Context(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "BILLET_TEST_ALERT_NEVER_SET") {
		t.Errorf("the unreadable destination's send = %v, want why", err)
	}
	if !strings.Contains(logged.String(), "unset") || strings.Contains(logged.String(), "s3cret") {
		t.Errorf("logged %q", logged.String())
	}
	if set.Destinations["good"].Kind != config.AlertSlack {
		t.Errorf("the readable destination = %+v", set.Destinations["good"])
	}

	// AND A CONFIG WITH NO RULE BUILDS A SET WITH NONE, which starts no loop.
	empty, err := BuildAlerts(&config.Config{Server: &config.ServerConfig{}}, log)
	if err != nil || empty == nil || len(empty.Rules) != 0 {
		t.Errorf("no rules = %+v, %v", empty, err)
	}
}
