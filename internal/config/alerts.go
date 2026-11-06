package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultAlertCooldown is how long a rule stays quiet about one job after firing,
// when it says nothing. Long enough that a job failing on every push is one message
// an hour and not one a push.
const DefaultAlertCooldown = time.Hour

// MaxAlertInARow bounds in_a_row, past which a rule would never fire in practice.
const MaxAlertInARow = 100

// AlertsConfig is `server.alerts`: who to tell, and about which jobs.
type AlertsConfig struct {
	Destinations []AlertDestination `yaml:"destinations,omitempty"`
	Rules        []AlertRule        `yaml:"rules,omitempty"`
}

// AlertDestinationKind says what shape a destination's requests take.
type AlertDestinationKind string

const (
	// AlertWebhook is a POST of billet's own JSON notice, to any URL.
	AlertWebhook AlertDestinationKind = "webhook"
	// AlertSlack is a Slack incoming webhook's `{"text": ...}`.
	AlertSlack AlertDestinationKind = "slack"
)

// AlertDestination is one place a rule may notify.
//
// ITS ADDRESS IS A CREDENTIAL, so this holds where to find it and never the address:
// a Slack incoming webhook authenticates by its URL, and a secret written into this
// file ends up in a backup, a paste buffer, and eventually a support thread. It is
// the rule the PostgreSQL DSN follows.
type AlertDestination struct {
	Name string               `yaml:"name"`
	Kind AlertDestinationKind `yaml:"kind"`
	// URLEnv names the environment variable holding the address, and URLFile a file
	// holding it, mode 0600. Exactly one.
	URLEnv  string `yaml:"url_env,omitempty"`
	URLFile string `yaml:"url_file,omitempty"`
}

// AlertRule is one thing somebody wants to hear about.
type AlertRule struct {
	Name  string          `yaml:"name"`
	Match AlertMatch      `yaml:"match,omitempty"`
	On    AlertConditions `yaml:"on"`
	// InARow is how many of a job's runs in a row must hit before the rule fires;
	// absent, one. Cooldown is how long it then stays quiet about that job, a Go
	// duration; absent, DefaultAlertCooldown, and "0s" is none.
	InARow   int    `yaml:"in_a_row,omitempty"`
	Cooldown string `yaml:"cooldown,omitempty"`
	// Notify names the destinations.
	Notify []string `yaml:"notify"`
}

// AlertMatch narrows a rule, each field a pattern with `*` and `?`, and an empty one
// matching everything. Workflow is a file name, or a path with a `/` in it. Branch
// and Step are read from GitHub, and need the App's `actions: read`. Step is a step
// that ended the way the condition is about, so it takes `failed` or `cancelled`.
type AlertMatch struct {
	Repository string `yaml:"repository,omitempty"`
	Workflow   string `yaml:"workflow,omitempty"`
	Job        string `yaml:"job,omitempty"`
	Step       string `yaml:"step,omitempty"`
	Branch     string `yaml:"branch,omitempty"`
	Tier       string `yaml:"tier,omitempty"`
}

// AlertConditions is what a rule fires on, at least one. The two durations are Go
// durations, and RunningLongerThan fires while the job still runs.
type AlertConditions struct {
	Failed            bool   `yaml:"failed,omitempty"`
	Cancelled         bool   `yaml:"cancelled,omitempty"`
	Disrupted         bool   `yaml:"disrupted,omitempty"`
	RunningLongerThan string `yaml:"running_longer_than,omitempty"`
	WaitedLongerThan  string `yaml:"waited_longer_than,omitempty"`
}

// RunningOver is RunningLongerThan, zero when absent.
func (c AlertConditions) RunningOver() (time.Duration, error) {
	return optionalAlertDuration("running_longer_than", c.RunningLongerThan)
}

// WaitedOver is WaitedLongerThan, zero when absent.
func (c AlertConditions) WaitedOver() (time.Duration, error) {
	return optionalAlertDuration("waited_longer_than", c.WaitedLongerThan)
}

func optionalAlertDuration(key, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a duration like 15m or 2h", key, value)
	}

	return d, nil
}

// InARowOrOne is InARow with its default.
func (r *AlertRule) InARowOrOne() int {
	if r.InARow == 0 {
		return 1
	}

	return r.InARow
}

// CooldownDuration is Cooldown with its default.
func (r *AlertRule) CooldownDuration() (time.Duration, error) {
	if r.Cooldown == "" {
		return DefaultAlertCooldown, nil
	}

	d, err := time.ParseDuration(r.Cooldown)
	if err != nil {
		return 0, fmt.Errorf("cooldown %q is not a duration like 30m or 1h", r.Cooldown)
	}

	return d, nil
}

// NeedsRunRecords says whether a rule reads GitHub's record of a run or a job, which
// is what asks the App for `actions: read`.
func (r *AlertRule) NeedsRunRecords() bool { return r.Match.Branch != "" || r.Match.Step != "" }

// alertName is what a rule or a destination is called: it is a ledger key, a metric
// label, and a word in a message.
var alertName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// quoteAlertName is a name as a refusal may show it. ONLY A NAME IS QUOTED: the likeliest
// thing in a name's place is the address pasted there, a credential, and a refusal is
// printed, logged and pasted into tickets.
func quoteAlertName(name string) string {
	if alertName.MatchString(name) {
		return fmt.Sprintf("%q", name)
	}

	return "a value that is not a name (not repeated here, in case it is an address)"
}

// validateAlerts checks `server.alerts`.
func (c *Config) validateAlerts() []error {
	if c.Server == nil || c.Server.Alerts == nil {
		return nil
	}

	alerts := c.Server.Alerts

	var errs []error

	kinds := make(map[string]bool, len(alerts.Destinations))
	for i := range alerts.Destinations {
		d := &alerts.Destinations[i]
		where := fmt.Sprintf("server.alerts.destinations[%d]", i)

		errs = append(errs, d.errors(where)...)

		if kinds[d.Name] && alertName.MatchString(d.Name) {
			errs = append(errs, fmt.Errorf("%s: name %s is used twice", where, quoteAlertName(d.Name)))
		}
		kinds[d.Name] = true
	}

	names := make(map[string]bool, len(alerts.Rules))
	for i := range alerts.Rules {
		r := &alerts.Rules[i]
		where := fmt.Sprintf("server.alerts.rules[%d]", i)

		errs = append(errs, r.errors(where, kinds)...)

		if names[r.Name] && alertName.MatchString(r.Name) {
			errs = append(errs, fmt.Errorf("%s: name %s is used twice", where, quoteAlertName(r.Name)))
		}
		names[r.Name] = true
	}

	return errs
}

func (d *AlertDestination) errors(where string) []error {
	var errs []error

	if !alertName.MatchString(d.Name) {
		errs = append(errs, fmt.Errorf("%s: name is not a letter or digit followed by at most 63 of "+
			"letters, digits, `.`, `_` and `-`; it is not repeated here, in case it is an address", where))
	}

	switch d.Kind {
	case AlertWebhook, AlertSlack:
	default:
		errs = append(errs, fmt.Errorf("%s: kind %q is not one of [webhook slack]", where, d.Kind))
	}

	switch {
	case d.URLEnv == "" && d.URLFile == "":
		errs = append(errs, fmt.Errorf("%s: one of url_env and url_file is required; the address is a credential, "+
			"so it is read from the environment or a file and never written here", where))
	case d.URLEnv != "" && d.URLFile != "":
		errs = append(errs, fmt.Errorf("%s: url_env and url_file are both set; name one", where))
	case d.URLEnv != "":
		if err := checkAlertURLEnv(d.URLEnv); err != nil {
			errs = append(errs, fmt.Errorf("%s: url_env: %w", where, err))
		}
	// THE ADDRESS FIRST, because the next refusal quotes the path.
	case strings.Contains(d.URLFile, "://"):
		errs = append(errs, fmt.Errorf("%s: url_file looks like an address rather than the path of a file "+
			"holding one; write the path, and keep the address out of the config", where))
	case !filepath.IsAbs(d.URLFile):
		// NOT QUOTED: an address written without its scheme passes the check above.
		errs = append(errs, fmt.Errorf("%s: url_file must be an absolute path: the service's working directory "+
			"is not what an operator is looking at", where))
	}

	return errs
}

// checkAlertURLEnv is what may name the environment variable holding an address.
//
// THE MOST LIKELY MISTAKE IS PUTTING THE ADDRESS ITSELF HERE, which os.Getenv would
// answer as empty, and it is refused without quoting the value, since it is then the
// credential this indirection exists to keep out of output.
func checkAlertURLEnv(name string) error {
	if strings.ContainsAny(name, ":/@ ") {
		return errors.New("this looks like an address rather than the NAME of an environment variable " +
			"holding one; write the name, and keep the address out of the config")
	}
	if !envVarName.MatchString(name) {
		return fmt.Errorf("%q is not a legal environment variable name; nothing could export it", name)
	}

	return nil
}

func (r *AlertRule) errors(where string, destinations map[string]bool) []error {
	var errs []error

	if !alertName.MatchString(r.Name) {
		errs = append(errs, fmt.Errorf("%s: name is not a letter or digit followed by at most 63 of "+
			"letters, digits, `.`, `_` and `-`; it is not repeated here, in case it is an address", where))
	}

	running, err := r.On.RunningOver()
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: on.%w", where, err))
	} else if r.On.RunningLongerThan != "" && running <= 0 {
		errs = append(errs, fmt.Errorf("%s: on.running_longer_than must be positive", where))
	}

	waited, err := r.On.WaitedOver()
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: on.%w", where, err))
	} else if r.On.WaitedLongerThan != "" && waited <= 0 {
		errs = append(errs, fmt.Errorf("%s: on.waited_longer_than must be positive", where))
	}

	if !r.On.Failed && !r.On.Cancelled && !r.On.Disrupted && r.On.RunningLongerThan == "" && r.On.WaitedLongerThan == "" {
		errs = append(errs, fmt.Errorf("%s: on names no condition, so the rule could never fire; "+
			"name at least one of failed, cancelled, disrupted, running_longer_than and waited_longer_than", where))
	}

	// A STEP ENDS ONE WAY OR ANOTHER, and only those two conditions are about how: a
	// rule about a job running long, narrowed by a step, could never be judged.
	if r.Match.Step != "" && (r.On.Disrupted || r.On.RunningLongerThan != "" || r.On.WaitedLongerThan != "" ||
		(!r.On.Failed && !r.On.Cancelled)) {
		errs = append(errs, fmt.Errorf("%s: match.step takes only on.failed and on.cancelled: a step says how a "+
			"job ended, and the other conditions are not about that", where))
	}

	for field, pattern := range map[string]string{
		"repository": r.Match.Repository, "workflow": r.Match.Workflow, "job": r.Match.Job,
		"step": r.Match.Step, "branch": r.Match.Branch, "tier": r.Match.Tier,
	} {
		if strings.ContainsAny(pattern, "\x00\r\n") {
			errs = append(errs, fmt.Errorf("%s: match.%s holds a control character, which no name has", where, field))
		}
	}

	if r.InARow < 0 || r.InARow > MaxAlertInARow {
		errs = append(errs, fmt.Errorf("%s: in_a_row %d is not between 1 and %d", where, r.InARow, MaxAlertInARow))
	}

	if cooldown, err := r.CooldownDuration(); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", where, err))
	} else if cooldown < 0 {
		errs = append(errs, fmt.Errorf("%s: cooldown must not be negative", where))
	}

	if len(r.Notify) == 0 {
		errs = append(errs, fmt.Errorf("%s: notify names no destination, so the rule could tell nobody", where))
	}

	seen := make(map[string]bool, len(r.Notify))
	for _, name := range r.Notify {
		if !destinations[name] {
			errs = append(errs, fmt.Errorf("%s: notify names %s, which is no destination", where, quoteAlertName(name)))
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("%s: notify names %s twice", where, quoteAlertName(name)))
		}
		seen[name] = true
	}

	return errs
}

// AlertsNeedRunRecords says whether a rule reads GitHub's record of a run or a job.
// Every target's App is asked for `actions: read` then: a rule is not scoped to a
// target, and could be about a job of any of them.
func (c *Config) AlertsNeedRunRecords() bool {
	if c.Server == nil || c.Server.Alerts == nil {
		return false
	}

	for i := range c.Server.Alerts.Rules {
		if c.Server.Alerts.Rules[i].NeedsRunRecords() {
			return true
		}
	}

	return false
}
