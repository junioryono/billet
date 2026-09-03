package lifeops

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
)

// answers is a fake systemd: it records the argv billet built and replies with
// the KEY=VALUE text a real `systemctl show` produces. Every default below was
// copied from a measured run on systemd 255, so a parser that only works
// against an invented shape cannot pass.
type answers struct {
	calls [][]string
	reply map[string]string
	fail  error
	// block holds the call inside the runner until released, so a timeout is
	// exercised against a runner that is genuinely slow rather than one that
	// returns an error instantly.
	block chan struct{}
}

func (a *answers) run(ctx context.Context, _ string, args []string) ([]byte, error) {
	a.calls = append(a.calls, args)

	if a.block != nil {
		select {
		case <-a.block:
		case <-ctx.Done():
			// A FAKE THAT IGNORES CONTEXT cannot tell a cancelled call from a
			// fresh one, which is the whole subject of a timeout test.
			return nil, ctx.Err()
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.fail != nil {
		return nil, a.fail
	}

	// A UNIT THE FIXTURE NEVER STAGED STILL GETS AN ANSWER, because systemd
	// gives one: measured on 255, `systemctl show` reports every requested
	// property — empty — for a unit that does not exist, alongside
	// LoadState=not-found. A fake that answered nothing at all would make every
	// ordinary dependency look like a truncated reply.
	unit := args[len(args)-1]
	reply, ok := a.reply[unit]
	if !ok {
		reply = absentUnit()
	}

	// ONLY WHAT WAS ASKED FOR. `systemctl show --property=X` answers X and
	// nothing else, and a fake that handed back its whole prepared reply
	// regardless would make deleting a property from the query invisible — the
	// production code would stop asking, the fixture would keep answering, and
	// every test would still pass.
	return []byte(filterProperties(reply, args)), nil
}

// absentUnit is systemd's answer about a unit that does not exist: every
// property requested, empty, and a LoadState saying so.
func absentUnit() string {
	var b strings.Builder

	b.WriteString("LoadState=not-found\n")
	for _, name := range []string{
		"Conflicts", "Triggers", "Requires", "Requisite", "BindsTo", "Wants",
		"Upholds", "PartOf", "OnFailure", "OnSuccess", "JoinsNamespaceOf",
	} {
		b.WriteString(name)
		b.WriteString("=\n")
	}

	return b.String()
}

// filterProperties keeps the lines whose key was actually requested.
func filterProperties(reply string, args []string) string {
	asked := map[string]bool{}
	for _, arg := range args {
		if name, ok := strings.CutPrefix(arg, "--property="); ok {
			asked[name] = true
		}
	}

	if len(asked) == 0 {
		return reply
	}

	var kept []string
	for _, line := range strings.Split(reply, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if ok && asked[key] {
			kept = append(kept, line)
		}
	}

	return strings.Join(kept, "\n")
}

// measured is the reply shape systemd 255 gives for a loaded, enabled, running
// unit. Callers override individual fields; anything left alone is what a real
// host answered. A field set to REMOVE is omitted entirely.
func measured(over map[string]string) string {
	fields := map[string]string{
		"LoadState":        "loaded",
		"ActiveState":      "active",
		"SubState":         "running",
		"UnitFileState":    "enabled",
		"Result":           "success",
		"Type":             "notify",
		"User":             "billet",
		"Group":            "billet",
		"FragmentPath":     "/usr/lib/systemd/system/billet-server.service",
		"DropInPaths":      "",
		"MainPID":          "1643",
		"NRestarts":        "0",
		"NeedDaemonReload": "no",
		// The ordinary answers, measured on the packaged units: each of these
		// has a harmless VALUE rather than being empty, so a fixture that omitted
		// them would look like a host systemd could not be asked about.
		"OnFailureJobMode":                "replace",
		"FailureAction":                   "none",
		"SuccessAction":                   "none",
		"StartLimitAction":                "none",
		"JobTimeoutAction":                "none",
		"ExecMainStartTimestampMonotonic": "918273645",
		"ExecStart": "{ path=/usr/bin/billet ; argv[]=/usr/bin/billet server --config " +
			"/etc/billet/billet.yaml ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; " +
			"pid=0 ; code=(null) ; status=0/0 }",
	}
	maps.Copy(fields, over)

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		if fields[k] == "REMOVE" {
			continue
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(fields[k])
		b.WriteString("\n")
	}

	return b.String()
}

// absent is what systemd answers for a unit that does not exist: exit 0,
// LoadState=not-found, an EMPTY UnitFileState. Measured, and the reason absence
// is a value in this package rather than an error.
func absent() string {
	return measured(map[string]string{
		"LoadState": "not-found", "ActiveState": "inactive", "SubState": "dead",
		"UnitFileState": "", "Type": "", "User": "", "Group": "",
		"FragmentPath": "", "MainPID": "0", "ExecStart": "REMOVE",
	})
}

// host is a fixture with REAL files, so the inode comparisons under test are
// exercised rather than stubbed past.
type host struct {
	dir      string
	selfPath string
	procExe  map[int]string
	procErr  map[int]error
}

func newHost(t *testing.T) *host {
	t.Helper()

	dir := t.TempDir()
	self := filepath.Join(dir, "billet-running")
	if err := os.WriteFile(self, []byte("the running billet"), 0o600); err != nil {
		t.Fatalf("write the running binary: %v", err)
	}

	return &host{dir: dir, selfPath: self, procExe: map[int]string{}, procErr: map[int]error{}}
}

// file writes a fixture file and returns its path.
func (h *host) file(t *testing.T, name, body string) string {
	t.Helper()

	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	return path
}

func (h *host) inspector(a *answers, opts ...Option) *Inspector {
	base := []Option{
		withRunner(a.run),
		withExecutables(
			func() (string, error) { return h.selfPath, nil },
			func() (fs.FileInfo, error) { return os.Stat(h.selfPath) },
			func(pid int) (fs.FileInfo, error) {
				if err := h.procErr[pid]; err != nil {
					return nil, err
				}
				path, ok := h.procExe[pid]
				if !ok {
					return nil, os.ErrNotExist
				}

				return os.Stat(path)
			},
		),
		withIdentityLookup(
			func(string) (string, error) { return "", os.ErrNotExist },
			func(string) (string, error) { return "", os.ErrNotExist },
		),
	}

	return NewInspector(append(base, opts...)...)
}

// WHAT SYSTEMD SAID, parsed into values a caller can act on — INCLUDING the
// judgments, against real files, because a fixture pointing at paths that do
// not exist can only ever produce uncertainty and would pass with the
// judgments deleted.
func TestInspectReadsWhatSystemdSaysAndJudgesIt(t *testing.T) {
	h := newHost(t)
	unitFile := h.file(t, "billet-server.service", deploy.ServerUnit)
	h.procExe[1643] = h.selfPath

	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: measured(map[string]string{
			"FragmentPath": unitFile,
			"ExecStart":    "{ path=" + h.selfPath + " ; argv[]=" + h.selfPath + " server ; ignore_errors=no }",
		}),
		deploy.NodeUnitName: absent(),
	}}

	report, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", "")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	server := report.Server
	for _, tc := range []struct{ got, want, field string }{
		{server.LoadState, "loaded", "LoadState"},
		{server.ActiveState, "active", "ActiveState"},
		{server.UnitFileState, "enabled", "UnitFileState"},
		{server.Type, "notify", "Type"},
		{server.User, "billet", "User"},
		{server.Result, "success", "Result"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	if server.MainPID != 1643 {
		t.Errorf("MainPID = %d, want 1643", server.MainPID)
	}

	// THE JUDGMENTS, which are the reason this package exists.
	if server.ExecStartIsThisBuild != Yes {
		t.Errorf("the unit names the running binary but reports %s (%s)",
			server.ExecStartIsThisBuild, server.ExecStartWhy)
	}
	if server.RunningIsThisBuild != Yes {
		t.Errorf("the running process IS this build but reports %s (%s)",
			server.RunningIsThisBuild, server.RunningWhy)
	}
	if server.MatchesPackagedUnit != Yes {
		t.Errorf("the packaged unit does not compare equal to itself: %s", server.MatchesPackagedUnit)
	}

	// AN ABSENT UNIT IS A VALUE, NOT AN ERROR.
	if report.Node.Installed() {
		t.Error("a not-found unit reports as installed")
	}
}

// THE QUESTION THE UNIT FILE CANNOT ANSWER: replace the binary without a
// restart and the unit's ExecStart resolves to the NEW file while the service
// goes on executing the old one. A check comparing only ExecStart reports
// agreement at exactly the moment an operator needs to be told otherwise.
func TestAReplacedBinaryIsSeenInTheRunningProcess(t *testing.T) {
	h := newHost(t)
	old := h.file(t, "billet-old", "the binary the service started with")
	h.procExe[1643] = old

	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: measured(map[string]string{
			"ExecStart": "{ path=" + h.selfPath + " ; argv[]=" + h.selfPath + " server ; ignore_errors=no }",
		}),
		deploy.NodeUnitName: absent(),
	}}

	report, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", "")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	// The unit agrees — which is precisely why this cannot be the only check.
	if report.Server.ExecStartIsThisBuild != Yes {
		t.Fatalf("the fixture is wrong: the unit should name the running binary, got %s",
			report.Server.ExecStartIsThisBuild)
	}
	if report.Server.RunningIsThisBuild != No {
		t.Errorf("a service executing a replaced binary reports %s, want no (%s)",
			report.Server.RunningIsThisBuild, report.Server.RunningWhy)
	}
	if !strings.Contains(report.Server.RunningWhy, "1643") {
		t.Errorf("the finding does not name the pid: %q", report.Server.RunningWhy)
	}
}

// AND ITS UNCERTAINTIES, each its own state. An unprivileged caller looking at
// a root service cannot read /proc/<pid>/exe, and that is not evidence of a
// mismatch any more than it is evidence of a match.
func TestTheRunningJudgmentSeparatesCannotTellFromNo(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*host, map[string]string)
		reason string
	}{
		{
			name: "the service is not running",
			set: func(_ *host, over map[string]string) {
				over["ActiveState"] = "inactive"
				over["SubState"] = "dead"
			},
			reason: "not running",
		},
		{
			name:   "the process cannot be read",
			set:    func(h *host, _ map[string]string) { h.procErr[1643] = os.ErrPermission },
			reason: "cannot read the running process",
		},
		{
			name:   "systemd reports no main pid",
			set:    func(_ *host, over map[string]string) { over["MainPID"] = "0" },
			reason: "no main pid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			h.procExe[1643] = h.selfPath

			over := map[string]string{}
			tc.set(h, over)

			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: measured(over),
				deploy.NodeUnitName:   absent(),
			}}

			report, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", "")
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if got := report.Server.RunningIsThisBuild; got != Unknown {
				t.Errorf("RunningIsThisBuild = %s, want unknown (%s)", got, report.Server.RunningWhy)
			}
			if !strings.Contains(report.Server.RunningWhy, tc.reason) {
				t.Errorf("the reason %q does not explain %q", report.Server.RunningWhy, tc.reason)
			}
		})
	}
}

// THE QUERY ITSELF IS PINNED. The fake answers whatever it is asked, so a wrong
// property name would otherwise be invisible until a real host reported an
// empty field.
func TestInspectAsksSystemdForTheFactsItReports(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: measured(nil),
		deploy.NodeUnitName:   absent(),
	}}

	if _, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", ""); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(a.calls) != 2 {
		t.Fatalf("asked systemd %d times, want one query per unit", len(a.calls))
	}

	got := strings.Join(a.calls[0], " ")
	if !strings.HasPrefix(got, "show ") || !strings.HasSuffix(got, " "+deploy.ServerUnitName) {
		t.Errorf("the query is not a show naming the unit last: %q", got)
	}
	for _, want := range []string{
		"LoadState", "ActiveState", "SubState", "UnitFileState", "Result", "Type",
		"User", "Group", "FragmentPath", "DropInPaths", "MainPID", "NRestarts",
		"NeedDaemonReload", "ExecStart",
	} {
		if !strings.Contains(got, "--property="+want) {
			t.Errorf("the query does not ask for %s: %q", want, got)
		}
	}
}

// THE EXECUTABLE COMES FROM SYSTEMD'S OWN PARSE. The path itself is varied —
// not just an argument after it — so a parser hardcoded to the usual location,
// or one reading argv instead of path=, fails here.
func TestExecStartIsTakenFromSystemdsParse(t *testing.T) {
	h := newHost(t)
	odd := h.file(t, "billet at an odd place", "x")

	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: measured(map[string]string{
			"ExecStart": "{ path=" + odd + " ; argv[]=/usr/bin/billet server --config " +
				`"/etc/billet/a file.yaml" ; ignore_errors=no }`,
		}),
		deploy.NodeUnitName: absent(),
	}}

	report, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", "")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if report.Server.ExecStart != odd {
		t.Errorf("ExecStart = %q, want the path systemd parsed (%q)", report.Server.ExecStart, odd)
	}
	// It exists and is NOT the running binary, so the judgment is a definite no
	// rather than the uncertainty an unparsed path would produce.
	if report.Server.ExecStartIsThisBuild != No {
		t.Errorf("a real but different executable reports %s, want no", report.Server.ExecStartIsThisBuild)
	}
}

// A UNIT WITH TWO ExecStart LINES MAKES "which binary" AMBIGUOUS, and the count
// is what lets a caller refuse instead of reading the first as though it were
// the only one.
func TestTwoExecStartsAreCountedRatherThanCollapsed(t *testing.T) {
	h := newHost(t)
	two := measured(nil) +
		"ExecStart={ path=/usr/local/bin/billet ; argv[]=/usr/local/bin/billet server ; ignore_errors=no }\n"

	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: two,
		deploy.NodeUnitName:   absent(),
	}}

	report, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", "")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if report.Server.ExecStartCount != 2 {
		t.Errorf("ExecStartCount = %d, want 2 so a caller can refuse the ambiguity",
			report.Server.ExecStartCount)
	}
}

// A MANAGER THAT CANNOT BE ASKED IS NOT A HOST WITH NO UNITS, and neither is a
// reply this code cannot read. Both are errors; only "not-found" is absence.
func TestAnUnreadableAnswerIsAnErrorRatherThanAnAbsentUnit(t *testing.T) {
	h := newHost(t)

	t.Run("systemd cannot be asked", func(t *testing.T) {
		a := &answers{fail: os.ErrPermission}
		if _, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", ""); err == nil {
			t.Fatal("an unreachable systemd was reported as a host with no units")
		}
	})

	t.Run("the reply carries no LoadState", func(t *testing.T) {
		a := &answers{reply: map[string]string{deploy.ServerUnitName: "ActiveState=active\n"}}
		_, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", "")
		if err == nil {
			t.Fatal("a truncated reply was read as an ordinary unit")
		}
		if !strings.Contains(err.Error(), "LoadState") {
			t.Errorf("the failure does not name what was missing: %v", err)
		}
	})
}

// A SLOW SYSTEMD IS BOUNDED. Driven by a runner that genuinely blocks, because
// one returning an error instantly cannot tell a timeout from any other
// failure.
func TestASlowSystemdIsBounded(t *testing.T) {
	h := newHost(t)
	a := &answers{block: make(chan struct{})}
	t.Cleanup(func() { close(a.block) })

	start := time.Now()
	_, err := h.inspector(a, WithTimeout(50*time.Millisecond)).
		Inspect(t.Context(), "/etc/billet/billet.yaml", "")
	if err == nil {
		t.Fatal("a systemctl that never answered did not time out")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the call was not bounded by the configured timeout: took %s", elapsed)
	}
}

// A PENDING RELOAD MAKES THE UNIT COMPARISON MEANINGLESS rather than false: the
// bytes on disk are then not the unit the manager would run.
func TestAPendingReloadMakesTheUnitComparisonUnknown(t *testing.T) {
	h := newHost(t)
	unitFile := h.file(t, "billet-server.service", deploy.ServerUnit)

	for _, tc := range []struct {
		reload string
		want   Tristate
	}{
		{"no", Yes},
		{"yes", Unknown},
	} {
		t.Run("NeedDaemonReload="+tc.reload, func(t *testing.T) {
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: measured(map[string]string{
					"FragmentPath": unitFile, "NeedDaemonReload": tc.reload,
				}),
				deploy.NodeUnitName: absent(),
			}}

			report, err := h.inspector(a).Inspect(t.Context(), "/etc/billet/billet.yaml", "")
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if got := report.Server.MatchesPackagedUnit; got != tc.want {
				t.Errorf("MatchesPackagedUnit = %s, want %s", got, tc.want)
			}
		})
	}
}

// THE SAME FILE UNDER TWO NAMES IS THE SAME BINARY. Comparing resolved path
// strings would refuse this correct deployment; comparing inodes recognises it.
func TestAHardLinkIsTheSameBinary(t *testing.T) {
	h := newHost(t)
	link := filepath.Join(h.dir, "billet-hardlink")
	if err := os.Link(h.selfPath, link); err != nil {
		t.Skipf("this filesystem cannot hard-link: %v", err)
	}

	self, err := os.Stat(h.selfPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got, why := sameAsPath(self, nil, link); got != Yes {
		t.Errorf("a hard link to the running binary reports %s (%s), want yes", got, why)
	}
}

// "COULD NOT TELL" IS ITS OWN ANSWER, and never a match.
func TestUncertaintyIsNeverAMatch(t *testing.T) {
	h := newHost(t)
	self, err := os.Stat(h.selfPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	for _, tc := range []struct {
		name    string
		self    fs.FileInfo
		selfErr error
		path    string
		want    Tristate
	}{
		{"the running binary is unknown", nil, os.ErrPermission, h.selfPath, Unknown},
		{"the unit names nothing", self, nil, "", Unknown},
		{"the unit's binary is absent", self, nil, filepath.Join(h.dir, "gone"), No},
		{"a different file", self, nil, h.file(t, "other", "different"), No},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := sameAsPath(tc.self, tc.selfErr, tc.path); got != tc.want {
				t.Errorf("sameAsPath = %s, want %s", got, tc.want)
			}
		})
	}
}

// A DIFFERENCE FROM THE PACKAGED UNIT IS A FACT, NOT A VERDICT — an
// Ansible-managed host differs for good reasons — but it must still be
// detected, and an unreadable fragment must not read as a match.
func TestThePackagedUnitComparison(t *testing.T) {
	h := newHost(t)

	// THE SUBSTITUTION IS ASSERTED, not assumed: an anchor that matched nothing
	// would leave the "edited" fixture byte-identical to the packaged unit, and
	// the No case below would then pass for the wrong reason. (Checking that
	// the unit merely CONTAINS the replacement is not enough — Type=exec
	// appears in the unit's own comment explaining why it is not used.)
	body := strings.Replace(deploy.ServerUnit, "\nType=notify\n", "\nType=exec\n", 1)
	if body == deploy.ServerUnit {
		t.Fatal("the fixture did not change the unit, so this proves nothing")
	}

	same := h.file(t, "same.service", deploy.ServerUnit)
	edited := h.file(t, "edited.service", body)

	for _, tc := range []struct {
		name     string
		fragment string
		want     Tristate
	}{
		{"the packaged unit", same, Yes},
		{"a different unit", edited, No},
		{"no fragment at all", "", Unknown},
		{"an unreadable fragment", filepath.Join(h.dir, "absent.service"), Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesPackaged(ServiceFacts{FragmentPath: tc.fragment}, deploy.ServerUnit); got != tc.want {
				t.Errorf("matchesPackaged = %s, want %s", got, tc.want)
			}
		})
	}
}

// A FILE THAT CANNOT BE STATTED IS NOT AN ABSENT FILE. Reporting "no config
// here" for a permissions failure sends an operator to create one they have.
func TestAStatFailureIsNotAnAbsentFile(t *testing.T) {
	h := newHost(t)
	i := h.inspector(&answers{reply: map[string]string{}})

	present := h.file(t, "billet.yaml", "server:\n")
	if err := os.Chmod(present, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if got := i.fileFacts(present); got.Exists != Yes {
		t.Errorf("an existing file reports %s", got.Exists)
	} else if got.Mode.Perm() != 0o640 {
		t.Errorf("mode = %04o, want 0640", got.Mode.Perm())
	}

	if got := i.fileFacts(filepath.Join(h.dir, "absent.yaml")); got.Exists != No {
		t.Errorf("a missing file reports %s, want no", got.Exists)
	}

	// A path whose PARENT is not a directory fails stat with something other
	// than not-exist, which must read as uncertainty.
	got := i.fileFacts(filepath.Join(present, "billet.yaml"))
	if got.Exists != Unknown {
		t.Errorf("an unstattable path reports %s, want unknown", got.Exists)
	}
	if got.Err == nil {
		t.Error("uncertainty came back with no reason attached")
	}
}

// EVERY systemd ActiveState IS CLASSIFIED HERE, and only two of them mean
// nothing is running.
//
// THIS IS THE TEST THAT MUST BE AT THIS LEVEL. `down` waves a definite No
// through its identity refusal — a service proved not running has no process
// whose build could differ — so a state wrongly classified No is a foreign
// build stopped without anybody being asked, and stopping the server tears down
// every lease its listener holds. Injecting an already-classified RunningFacts
// into a command fake cannot see that: the classification IS the bug.
//
// An earlier version wrote this as "No unless exactly active", which made
// `deactivating` — a unit still stopping, process alive — a definite "nothing
// is running".
func TestRunningClassifiesEveryActiveState(t *testing.T) {
	t.Parallel()

	for state, want := range map[string]Tristate{
		// Nothing is running. These two are the whole allowlist.
		"inactive": No,
		"failed":   No,

		// A process is there, whichever direction it is heading.
		"active":       Yes,
		"activating":   Yes,
		"deactivating": Yes,
		"reloading":    Yes,
		"refreshing":   Yes,

		// systemd declined to say.
		"": Unknown,
		// A state billet has no rule for, including one a future systemd adds.
		"maintenance": Unknown,
		"quiescing":   Unknown,
	} {
		t.Run(orNone(state), func(t *testing.T) {
			t.Parallel()

			got := ServiceFacts{Name: "billet-node.service", ActiveState: state}.Running()

			if got.Active != want {
				t.Errorf("ActiveState %q classified %v, want %v", state, got.Active, want)
			}

			// AND THE DANGEROUS DIRECTION, said as its own assertion so the
			// consequence is visible at the point of failure rather than
			// inferred from a tristate.
			if got.Active == No && state != "inactive" && state != "failed" {
				t.Errorf("ActiveState %q was reported as proof nothing is running, which lets "+
					"`down` stop a foreign build without asking", state)
			}
		})
	}
}
