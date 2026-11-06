package fleetops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alerts"
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

// alertReportRows bounds each table `billet alerts list` prints.
const alertReportRows = 20

// Alerts is `billet alerts`: what the rules have done, and an operator's mute.
func Alerts(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet alerts list | billet alerts mute <rule> --for <duration> | " +
			"billet alerts unmute <rule> | billet alerts test --destination <name>")
	}

	switch args[0] {
	case "list":
		return cmdAlertsList(ctx, env, args[1:])
	case "mute":
		return cmdAlertsMute(ctx, env, args[1:])
	case "unmute":
		return cmdAlertsUnmute(ctx, env, args[1:])
	case "test":
		return cmdAlertsTest(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown alerts command %q; try list, mute, unmute, or test", args[0])
}

// alertsConfig loads the config, and refuses one that names no alerts: every one of
// these commands is about a block that is not there.
func alertsConfig(cfgPath string) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if cfg.Server == nil || cfg.Server.Alerts == nil {
		return nil, fmt.Errorf("%s has no server.alerts block", cfgPath)
	}

	return cfg, nil
}

// declaredRule says whether the config names a rule, so a typo in a mute is refused
// rather than recording a mute of nothing while the rule goes on firing.
func declaredRule(cfg *config.Config, name string) bool {
	for i := range cfg.Server.Alerts.Rules {
		if cfg.Server.Alerts.Rules[i].Name == name {
			return true
		}
	}

	return false
}

func cmdAlertsList(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet alerts list", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := alertsConfig(*cfgPath)
	if err != nil {
		return err
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	report, err := a.AlertReport(ctx, alertReportRows)
	if err != nil {
		return err
	}

	printAlertReport(env, cfg, &report, time.Now())

	return nil
}

// printAlertReport prints each table an operator asks about, from the one snapshot.
func printAlertReport(env cli.Env, cfg *config.Config, report *alloc.AlertReport, now time.Time) {
	states := make(map[string]alloc.AlertRuleState, len(report.Rules))
	for _, state := range report.Rules {
		states[state.Name] = state
	}

	w := tabwriter.NewWriter(env.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "RULE\tCONDITIONS\tNOTIFY\tMUTED")

	for i := range cfg.Server.Alerts.Rules {
		rule := &cfg.Server.Alerts.Rules[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", rule.Name, alertConditionWords(rule), strings.Join(rule.Notify, ","),
			alertMuteWords(states[rule.Name], now))
	}

	_ = w.Flush()

	// A MUTE OF A RULE THE CONFIG NO LONGER NAMES IS NOT SHOWN: nothing can fire under
	// it, and its row is what makes a rule brought back keep its history.

	if len(report.Streaks) > 0 {
		fmt.Fprintf(env.Stdout, "\nJobs each rule is counting, most recent first:\n")

		w = tabwriter.NewWriter(env.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "RULE\tJOB\tIN A ROW\tLAST FIRED\tQUIET UNTIL")

		for i := range report.Streaks {
			s := &report.Streaks[i]
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", s.Rule, alertJobWords(s.JobKey), s.Streak, alertTime(s.LastFired),
				alertQuietWords(s.CooldownUntil, now))
		}

		_ = w.Flush()
	}

	if len(report.Notices) > 0 {
		fmt.Fprintf(env.Stdout, "\nNotices by destination:\n")

		w = tabwriter.NewWriter(env.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "DESTINATION\tSTATE\tNOTICES")

		for _, n := range report.Notices {
			fmt.Fprintf(w, "%s\t%s\t%d\n", n.Destination, n.State, n.Notices)
		}

		_ = w.Flush()
	}

	if len(report.Failures) > 0 {
		fmt.Fprintf(env.Stdout, "\nNotices given up on, most recent first:\n")

		w = tabwriter.NewWriter(env.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "AT\tDESTINATION\tRULE\tLEASE\tWHY")

		for i := range report.Failures {
			f := &report.Failures[i]
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", alertTime(f.At), f.Destination, f.Rule, f.LeaseID, strconv.Quote(f.Failure))
		}

		_ = w.Flush()
	}
}

// alertConditionWords is what a rule fires on, as the config says it.
func alertConditionWords(r *config.AlertRule) string {
	var words []string
	if r.On.Failed {
		words = append(words, "failed")
	}
	if r.On.Cancelled {
		words = append(words, "cancelled")
	}
	if r.On.Disrupted {
		words = append(words, "disrupted")
	}
	if r.On.RunningLongerThan != "" {
		words = append(words, "running>"+r.On.RunningLongerThan)
	}
	if r.On.WaitedLongerThan != "" {
		words = append(words, "waited>"+r.On.WaitedLongerThan)
	}

	return strings.Join(words, ",")
}

// alertMuteWords says whether a rule is muted now. A mute past its end is not one.
func alertMuteWords(state alloc.AlertRuleState, now time.Time) string {
	if state.MutedUntil.IsZero() || !now.Before(state.MutedUntil) {
		return "-"
	}

	words := "until " + alertTime(state.MutedUntil)
	if state.MutedBy != "" {
		words += " by " + state.MutedBy
	}

	return words
}

// alertJobWords renders a streak's key, which the ledger keeps joined. A job's name is
// a workflow author's to choose, so it is quoted where it reaches an operator.
func alertJobWords(key string) string {
	parts := strings.Split(key, "\x00")
	if len(parts) != 3 {
		return strconv.Quote(key)
	}

	return strconv.Quote(parts[0] + " " + parts[1] + " " + parts[2])
}

// alertTime is an instant, or a dash for none.
func alertTime(at time.Time) string {
	if at.IsZero() {
		return "-"
	}

	return at.UTC().Format(time.RFC3339)
}

// alertQuietWords is when a cooldown ends, or a dash where it has.
func alertQuietWords(until, now time.Time) string {
	if until.IsZero() || !now.Before(until) {
		return "-"
	}

	return alertTime(until)
}

func cmdAlertsMute(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet alerts mute", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	duration := fs.Duration("for", 0, "how long to mute the rule, like 2h; required")

	name, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}
	if *duration <= 0 {
		return errors.New("--for must be positive: a mute that never ends is a rule to take out of the config")
	}

	cfg, err := alertsConfig(*cfgPath)
	if err != nil {
		return err
	}
	if !declaredRule(cfg, name) {
		return fmt.Errorf("%s names no rule %q", *cfgPath, name)
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	until := time.Now().Add(*duration)
	if err := a.MuteAlertRule(ctx, name, until, alertMuter()); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "Muted %s until %s. Its jobs go on being counted, and nothing is sent.\n",
		name, alertTime(until))

	return nil
}

// alertMuter is who ran the command, for the next operator who wonders.
func alertMuter() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}

	return ""
}

func cmdAlertsUnmute(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet alerts unmute", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)

	name, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := alertsConfig(*cfgPath)
	if err != nil {
		return err
	}
	if !declaredRule(cfg, name) {
		return fmt.Errorf("%s names no rule %q", *cfgPath, name)
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	known, err := a.UnmuteAlertRule(ctx, name)
	if err != nil {
		return err
	}
	if !known {
		fmt.Fprintf(env.Stdout, "%s was not muted.\n", name)

		return nil
	}

	fmt.Fprintf(env.Stdout, "%s is no longer muted.\n", name)

	return nil
}

// cmdAlertsTest sends one made-up notice to a destination, with this shell's
// environment and files: what proves an address is usable before a job goes red.
//
// IT TOUCHES NO LEDGER, so it can be run anywhere the address can be read, and says
// which environment it used: a service's is not an operator's shell's.
func cmdAlertsTest(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet alerts test", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	name := fs.String("destination", "", "the destination to send to; required")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return flagRequired(fs, "destination")
	}

	cfg, err := alertsConfig(*cfgPath)
	if err != nil {
		return err
	}

	var destination *config.AlertDestination
	for i := range cfg.Server.Alerts.Destinations {
		if cfg.Server.Alerts.Destinations[i].Name == *name {
			destination = &cfg.Server.Alerts.Destinations[i]
		}
	}
	if destination == nil {
		return fmt.Errorf("%s names no destination %q", *cfgPath, *name)
	}

	sender, err := app.AlertSender(destination)
	if err != nil {
		return fmt.Errorf("destination %s, read with this shell's environment and files: %w", *name, err)
	}

	notice := alerts.NewNotice(&alerts.Rule{Name: "billet-alerts-test"}, alerts.Failed, &alerts.Job{
		JobName: "a test notice from billet alerts test", Tier: "no tier", Result: "none",
	}, "", time.Now())

	body, err := notice.Render(destination.Kind)
	if err != nil {
		return err
	}

	if err := sender.Send(ctx, body); err != nil {
		return fmt.Errorf("destination %s: %w", *name, err)
	}

	fmt.Fprintf(env.Stdout, "%s accepted a test notice.\n", *name)

	return nil
}

// flagRequired names a required flag that was not given.
func flagRequired(fs *flag.FlagSet, name string) error {
	return fmt.Errorf("%s: --%s is required", fs.Name(), name)
}
