package config

import (
	"strings"
	"testing"
	"time"
)

// alertsYAML is validConfig with a `server.alerts` block.
func alertsYAML(block string) string {
	indented := "  alerts:\n"
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		indented += "    " + line + "\n"
	}

	return strings.Replace(validConfig, "  max_memory: 480GiB\n", "  max_memory: 480GiB\n"+indented, 1)
}

const goodAlerts = `destinations:
  - name: oncall
    kind: slack
    url_env: BILLET_ALERT_ONCALL
  - name: hook
    kind: webhook
    url_file: /etc/billet/alert-hook-url
rules:
  - name: deploys
    match:
      repository: acme/*
      workflow: deploy.yml
      job: deploy*
      step: Run migrations*
      branch: main
      tier: billet-*
    on:
      failed: true
      cancelled: true
    in_a_row: 3
    cooldown: 30m
    notify: [oncall, hook]
  - name: slow
    on:
      disrupted: true
      running_longer_than: 2h
      waited_longer_than: 15m
    notify: [hook]
`

// A BLOCK IN USE LOADS, and what it says is what a reader gets back, defaults included.
func TestAlertsLoad(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, alertsYAML(goodAlerts)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	alerts := cfg.Server.Alerts
	if len(alerts.Destinations) != 2 || len(alerts.Rules) != 2 {
		t.Fatalf("alerts = %+v", alerts)
	}
	if d := alerts.Destinations[0]; d.Kind != AlertSlack || d.URLEnv != "BILLET_ALERT_ONCALL" || d.Name != "oncall" {
		t.Errorf("destination = %+v", d)
	}

	deploys := &alerts.Rules[0]
	if deploys.InARowOrOne() != 3 || deploys.Match.Step != "Run migrations*" || !deploys.On.Failed {
		t.Errorf("rule = %+v", deploys)
	}
	if got, err := deploys.CooldownDuration(); err != nil || got != 30*time.Minute {
		t.Errorf("cooldown = %s, %v", got, err)
	}

	slow := &alerts.Rules[1]
	if slow.InARowOrOne() != 1 {
		t.Errorf("in_a_row defaulted to %d", slow.InARowOrOne())
	}
	if got, err := slow.CooldownDuration(); err != nil || got != DefaultAlertCooldown {
		t.Errorf("cooldown defaulted to %s, %v", got, err)
	}
	if got, err := slow.On.RunningOver(); err != nil || got != 2*time.Hour {
		t.Errorf("running over = %s, %v", got, err)
	}
	if got, err := slow.On.WaitedOver(); err != nil || got != 15*time.Minute {
		t.Errorf("waited over = %s, %v", got, err)
	}

	if !cfg.AlertsNeedRunRecords() {
		t.Error("a rule naming a branch and a step does not ask for run records")
	}
}

// ZERO IS NO COOLDOWN, said on purpose rather than an absent key.
func TestAZeroCooldownIsNone(t *testing.T) {
	t.Parallel()

	rule := AlertRule{Cooldown: "0s"}
	if got, err := rule.CooldownDuration(); err != nil || got != 0 {
		t.Errorf("cooldown = %s, %v", got, err)
	}
}

// EACH REFUSAL, held by the one edit of a good block that breaks it, and the same
// block with no edit loading is what holds the other direction.
func TestAlertsRefuse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"an unknown kind", "kind: slack", "kind: teams", `kind "teams" is not one of`},
		{"a destination with no address", "    url_env: BILLET_ALERT_ONCALL\n", "", "one of url_env and url_file is required"},
		{"a destination with two", "    url_env: BILLET_ALERT_ONCALL\n", "    url_env: BILLET_ALERT_ONCALL\n    url_file: /x\n", "both set"},
		{"an illegal variable", "url_env: BILLET_ALERT_ONCALL", "url_env: 9-lives", "not a legal environment variable name"},
		{"the address itself as the variable", "url_env: BILLET_ALERT_ONCALL", "url_env: https://hooks.slack.example/services/T/B/x", "looks like an address"},
		{"a relative file", "url_file: /etc/billet/alert-hook-url", "url_file: alert-hook-url", "must be absolute"},
		{"a destination named twice", "name: hook", "name: oncall", `name "oncall" is used twice`},
		{"a destination name that is no word", "name: oncall", "name: on call", "is not a letter or digit"},
		{"a rule named twice", "name: slow", "name: deploys", `name "deploys" is used twice`},
		{"a rule name that is no word", "name: slow", "name: \"sl/ow\"", "is not a letter or digit"},
		{"no conditions", "on:\n      disrupted: true\n      running_longer_than: 2h\n      waited_longer_than: 15m\n", "on: {}\n", "on names no condition"},
		{"a bad duration", "running_longer_than: 2h", "running_longer_than: soon", `running_longer_than "soon" is not a duration`},
		{"a zero duration", "running_longer_than: 2h", "running_longer_than: 0s", "must be positive"},
		{"a negative wait", "waited_longer_than: 15m", "waited_longer_than: -15m", "must be positive"},
		{"a step on a running rule", "on:\n      disrupted: true\n", "match: {step: x}\n    on:\n      disrupted: true\n", "match.step takes only"},
		{"too many in a row", "in_a_row: 3", "in_a_row: 101", "in_a_row 101 is not between"},
		{"a negative count", "in_a_row: 3", "in_a_row: -1", "in_a_row -1 is not between"},
		{"a bad cooldown", "cooldown: 30m", "cooldown: soon", `cooldown "soon" is not a duration`},
		{"a negative cooldown", "cooldown: 30m", "cooldown: -1m", "cooldown must not be negative"},
		{"nobody to tell", "notify: [hook]", "notify: []", "notify names no destination"},
		{"an undeclared destination", "notify: [hook]", "notify: [pager]", `notify names "pager", which is no destination`},
		{"a destination named twice in a rule", "notify: [oncall, hook]", "notify: [hook, hook]", `notify names "hook" twice`},
		{"a control character in a pattern", "tier: billet-*", "tier: \"billet\\n\"", "match.tier holds a control character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !strings.Contains(goodAlerts, tc.from) {
				t.Fatalf("the good block has no %q, so this edit tests nothing", tc.from)
			}

			_, err := Load(writeConfig(t, alertsYAML(strings.Replace(goodAlerts, tc.from, tc.to, 1))))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}

// AN ADDRESS WRITTEN WHERE ITS NAME BELONGS IS REFUSED WITHOUT BEING REPEATED: it is
// the credential, and this error reaches a terminal and a journal.
func TestAnAddressInTheConfigIsNotQuotedBack(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-webhook-token"

	for from, to := range map[string]string{
		"url_env: BILLET_ALERT_ONCALL":         "url_env: https://hooks.slack.example/services/T/B/" + secret,
		"url_file: /etc/billet/alert-hook-url": "url_file: https://hooks.slack.example/services/T/B/" + secret,
	} {
		_, err := Load(writeConfig(t, alertsYAML(strings.Replace(goodAlerts, from, to, 1))))
		if err == nil {
			t.Errorf("%q loaded", to)

			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal repeats the address: %v", err)
		}
	}
}

// AN UNKNOWN KEY IS REFUSED, because a misspelt condition is a rule that never fires.
func TestAMisspeltConditionIsRefused(t *testing.T) {
	t.Parallel()

	_, err := Load(writeConfig(t, alertsYAML(strings.Replace(goodAlerts, "disrupted: true", "disruptd: true", 1))))
	if err == nil || !strings.Contains(err.Error(), "disruptd") {
		t.Fatalf("Load = %v, want the misspelt key named", err)
	}
}

// NO BLOCK ASKS NOTHING, and a rule that reads only the ledger asks no more of the App.
func TestOnlyARuleReadingGitHubAsksForRunRecords(t *testing.T) {
	t.Parallel()

	if (&Config{}).AlertsNeedRunRecords() {
		t.Error("no server asked")
	}
	if (&Config{Server: &ServerConfig{}}).AlertsNeedRunRecords() {
		t.Error("no alerts asked")
	}

	plain := &Config{Server: &ServerConfig{Alerts: &AlertsConfig{Rules: []AlertRule{
		{Match: AlertMatch{Repository: "acme/*", Workflow: "x", Job: "y", Tier: "z"}},
	}}}}
	if plain.AlertsNeedRunRecords() {
		t.Error("a rule reading only the ledger asked")
	}

	for name, match := range map[string]AlertMatch{"branch": {Branch: "main"}, "step": {Step: "Deploy"}} {
		cfg := &Config{Server: &ServerConfig{Alerts: &AlertsConfig{Rules: []AlertRule{{Match: match}}}}}
		if !cfg.AlertsNeedRunRecords() {
			t.Errorf("a rule naming a %s did not ask", name)
		}
	}
}
