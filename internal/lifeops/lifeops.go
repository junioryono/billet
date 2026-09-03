// Package lifeops answers what a local billet deployment is actually doing.
//
// IT PRINTS NOTHING. Operator output belongs to cmd/billet, the one package
// allowed to write to stdout; lifeops returns facts and lets the command render
// them. That split is what lets every judgment here be asserted by value rather
// than by scraping text out of a terminal.
//
// The facts it gathers are the ones that differ silently. A host can have the
// right config, a valid App key and a running service, and still be executing a
// binary that was replaced hours ago, or a unit an override has quietly
// rewritten — and nothing on the happy path says so.
package lifeops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/deploy"
)

// DefaultTimeout bounds one systemctl call. Generous: these are local queries,
// and the cost of it being too small is a diagnostic that fails on a busy host
// rather than reporting what it found.
const DefaultTimeout = 30 * time.Second

// Tristate is an answer that may be UNKNOWN, because "could not tell" is not
// "no". Collapsing the third state is how a refusal becomes a silent pass on
// the one host where the check mattered — a stat that fails on permissions is
// not evidence a file is absent, and an unreadable unit is not evidence it
// matches.
type Tristate int

const (
	// Unknown is the zero value deliberately: a field nobody filled in reads as
	// uncertain rather than as a pass.
	Unknown Tristate = iota
	No
	Yes
)

// String names the state, so a diagnostic reads as a fact rather than an int.
func (t Tristate) String() string {
	switch t {
	case No:
		return "no"
	case Yes:
		return "yes"
	case Unknown:
		return "unknown"
	}

	return "unknown"
}

// FileFacts is what one path on disk is, as far as this process can see.
type FileFacts struct {
	Path   string
	Exists Tristate
	Mode   fs.FileMode
	Owner  string
	Group  string
	// Info is what the stat returned, kept so a later privileged operation can
	// prove it is acting on the same file rather than on the same NAME.
	Info fs.FileInfo
	// Err records why the facts are incomplete. A stat that failed for a reason
	// other than absence leaves Exists Unknown and this set, so a caller reports
	// the reason instead of inventing an answer.
	Err error
}

// ServiceFacts is what systemd says about one unit, plus the judgments a
// caller cannot make from the raw properties alone.
type ServiceFacts struct {
	Name string

	LoadState     string
	ActiveState   string
	SubState      string
	UnitFileState string
	Result        string
	Type          string
	User          string
	Group         string
	FragmentPath  string
	DropInPaths   []string
	MainPID       int
	NRestarts     int

	// ExecStart is the executable systemd WILL run, taken from systemd's own
	// parse rather than from the unit file's text.
	ExecStart string
	// ExecStartCount is how many ExecStart directives the unit carries. More
	// than one makes "which binary does this unit run" ambiguous, which a
	// caller must treat as a refusal rather than reading the first.
	ExecStartCount int
	// ExecStartArgv is the whole command line systemd would run. The path alone
	// does not say which ROLE a unit starts, and a billet that runs `server`
	// under the node unit's name is a control plane nothing authorised.
	ExecStartArgv string

	// ExecExtra is every OTHER command the unit would run — ExecStartPre and
	// its relatives. They are not covered by the ExecStart checks and several
	// of them run before the main process, which for the server unit is the
	// difference between an unprivileged control plane and a root one.
	ExecExtra map[string]string
	// Namespace is the set of properties that can replace the filesystem the
	// unit sees. A RootDirectory or a bind mount makes the path in ExecStart
	// name a different file than the one billet compared against.
	Namespace map[string]string
	// Elevation is the set of properties that change WHO the process is, past
	// the User= and Group= the unit names. Measured on the packaged units: each
	// of these is empty on both, and SupplementaryGroups is the sharpest —
	// adding `docker` to the unprivileged server is root by another route.
	//
	// Environment is deliberately NOT here: an operator behind a proxy has a
	// real reason to set it, and it cannot by itself change what runs or as
	// whom.
	Elevation map[string]string

	// ExecStartFlags is what systemd's EXTENDED answer says about the command's
	// prefixes, and it is the only trustworthy source for them.
	//
	// Measured on systemd 255: `ExecStart=+/usr/bin/billet server …` runs with
	// full privileges whatever User= says, and `systemctl show -p ExecStart`
	// renders it byte-identically to the unprefixed form — the prefix is simply
	// not in that answer. `-p ExecStartEx` carries `flags=privileged` for the
	// same unit and `flags=` for an ordinary one.
	ExecStartFlags string
	// ExecStartExCount is how many extended entries came back. Zero means
	// systemd did not answer at all — the property landed in v246 — which is
	// uncertainty about privilege rather than an absence of it.
	ExecStartExCount int
	// ExecStartFlagsKnown says whether the flags field was found at all.
	ExecStartFlagsKnown bool

	// Names is every name this unit answers to. An Alias= makes a second name
	// resolve to the same unit, which is a way for `enable` to write links
	// billet did not ask for.
	Names []string
	// Actions is the set of job settings that can act on the HOST or on other
	// units when this one fails or finishes. Measured on the packaged units:
	// OnFailureJobMode is "replace" and every other one is "none", so each has an
	// ordinary value rather than being empty — `isolate` stops everything else,
	// and `reboot` is exactly what it says.
	Actions map[string]string

	// Transaction is what starting or stopping this unit would do to OTHER
	// units. `Conflicts=` on one billet unit stops the other as part of its own
	// start, and `Requires=` starts it — neither is visible from anything else
	// billet checks.
	Transaction map[string]string

	// StateDirectory and RuntimeDirectory are the directories systemd itself
	// creates and makes writable for this unit, relative to /var/lib and /run.
	// They are the authority on where a role may keep state: under
	// ProtectSystem=strict everything else is read-only, so a config naming a
	// different path is a service that will fail on its first write.
	StateDirectory   string
	RuntimeDirectory string

	// ReloadPending is systemd's own NeedDaemonReload: the unit file on disk has
	// changed since the manager read it, so the bytes on disk and the unit the
	// manager would run are different things.
	ReloadPending Tristate

	// ExecStartIsThisBuild answers whether the file the unit WOULD run is the
	// same FILE as the running billet — by inode, so a hardlink or bind mount is
	// recognised and a replacement at the same path is not mistaken for it.
	ExecStartIsThisBuild Tristate
	ExecStartWhy         string

	// RunningIsThisBuild answers the DIFFERENT question that matters once a
	// service is up: whether the process systemd is running right now is this
	// build. Replacing a binary without restarting leaves these two disagreeing,
	// which is exactly the state nothing else on the host reports.
	RunningIsThisBuild Tristate
	RunningWhy         string

	// MatchesPackagedUnit answers whether the effective unit file is
	// byte-identical to the one the PACKAGE ships. A difference is not an
	// accusation: the Ansible role deliberately renders its own units, so a
	// role-managed host differs here for good reasons.
	MatchesPackagedUnit Tristate
}

// Installed reports whether systemd knows this unit at all.
func (s ServiceFacts) Installed() bool { return s.LoadState != "not-found" }

// Active reports whether systemd is running this unit right now.
func (s ServiceFacts) Active() bool { return s.ActiveState == "active" }

// RunningFacts is what ANY service manager can say about a service that is
// running right now: which service, whether it is running at all, and whether
// the process it is running is this build of billet.
//
// A DELIBERATELY NARROW SET, and the narrowness is the point. ServiceFacts
// carries a dozen properties that only systemd has — FragmentPath, DropInPaths,
// ReloadPending, ExecStartFlags, Namespace, Elevation — and every one of them
// exists because a systemd refusal is computed from it. Promoting those to a
// shared type gives a struct half of whose fields are permanently empty under
// the other manager, and a reader who has to know which half. These four are
// facts both managers genuinely answer, which is what makes them shareable.
type RunningFacts struct {
	Name string
	// Active is whether a PROCESS IS RUNNING for this service — not whether the
	// manager considers it healthy, and not whether it is settled. A service
	// that is starting up or shutting down has a live process, and for every
	// question this type is used to answer that is the same situation as one
	// that is running.
	//
	// Three-valued because "billet could not tell" is an ordinary answer — a
	// manager that did not reply, a state this build has no rule for — and it is
	// NOT the same fact as "nothing is running". Collapsing it to a bool made
	// the identity refusal skip a service it could not see, which is the
	// direction that stops somebody else's job.
	Active Tristate
	// IsThisBuild is whether the process currently executing is the same build
	// as the billet asking. Three-valued because "could not tell" is an ordinary
	// answer — a process that exited between the two reads, a manager that
	// reported no pid — and it is not the same fact as "no, it is different".
	IsThisBuild Tristate
	// Why explains the judgment, for a diagnostic that has to be actionable.
	Why string
}

// Running renders the subset of these facts that describes what the service is
// executing at this moment.
//
// ONLY TWO STATES PROVE NOTHING IS RUNNING, and this is an allowlist for the
// same reason `StopAndProve` uses one. A denylist here was a defect: it read
// "not exactly active" as No, which made `deactivating` — a unit still stopping,
// with its process ALIVE — a definite "nothing is running". `down` waves a
// definite No through its identity refusal, so a foreign build caught mid-reload
// or mid-stop would have been stopped without anybody being asked, and stopping
// the server destroys the leases its listener holds.
//
// An empty answer is systemd declining to say, and a state a future systemd adds
// is one billet has no rule for. Both are Unknown, which refuses.
func (s ServiceFacts) Running() RunningFacts {
	var active Tristate

	switch s.ActiveState {
	case "inactive", "failed":
		active = No
	case "active", "activating", "deactivating", "reloading", "refreshing":
		active = Yes
	default:
		active = Unknown
	}

	return RunningFacts{
		Name:        s.Name,
		Active:      active,
		IsThisBuild: s.RunningIsThisBuild,
		Why:         s.RunningWhy,
	}
}

// Report is what one inspection found.
type Report struct {
	ConfigPath string
	Config     FileFacts
	AppKey     FileFacts

	// Binary is the running billet's path, for display. The identity judgments
	// deliberately do not use it: a pathname cannot survive being replaced.
	Binary    string
	BinaryErr error

	Server ServiceFacts
	Node   ServiceFacts
}

// Inspector reads a host. Every external dependency is a seam so the whole
// thing is testable without root, without systemd and without a real account
// database — none of which a unit test can supply.
type Inspector struct {
	run       runner
	systemctl string
	timeout   time.Duration

	// selfPath names the running executable, for display only.
	selfPath func() (string, error)
	// selfExe and processExe are the identity seams.
	selfExe    func() (fs.FileInfo, error)
	processExe func(pid int) (fs.FileInfo, error)

	// lookupUID and lookupGID name an account. Injected because NSS reads the
	// HOST's account database, which no temporary directory can stand in for.
	lookupUID func(uid string) (string, error)
	lookupGID func(gid string) (string, error)
}

// Option configures an Inspector.
type Option func(*Inspector)

// WithSystemctl names the systemctl binary, skipping the PATH lookup.
func WithSystemctl(path string) Option {
	return func(i *Inspector) {
		if path != "" {
			i.systemctl = path
		}
	}
}

// WithTimeout bounds each systemctl call.
func WithTimeout(d time.Duration) Option {
	return func(i *Inspector) {
		if d > 0 {
			i.timeout = d
		}
	}
}

// withRunner replaces process execution. Unexported because its parameter is:
// an exported option nothing outside this package can construct is a worse API
// than one that is honestly package-private.
func withRunner(r runner) Option {
	return func(i *Inspector) {
		if r != nil {
			i.run = r
		}
	}
}

// withExecutables replaces the identity seams.
func withExecutables(path func() (string, error), self func() (fs.FileInfo, error),
	proc func(int) (fs.FileInfo, error),
) Option {
	return func(i *Inspector) {
		if path != nil {
			i.selfPath = path
		}
		if self != nil {
			i.selfExe = self
		}
		if proc != nil {
			i.processExe = proc
		}
	}
}

// withIdentityLookup replaces account-name resolution.
func withIdentityLookup(uid, gid func(string) (string, error)) Option {
	return func(i *Inspector) {
		if uid != nil {
			i.lookupUID = uid
		}
		if gid != nil {
			i.lookupGID = gid
		}
	}
}

// NewInspector builds an Inspector.
func NewInspector(opts ...Option) *Inspector {
	i := &Inspector{
		run:        execRunner,
		systemctl:  "systemctl",
		timeout:    DefaultTimeout,
		selfPath:   os.Executable,
		selfExe:    selfExe,
		processExe: processExe,
		lookupUID: func(uid string) (string, error) {
			u, err := user.LookupId(uid)
			if err != nil {
				return "", err
			}

			return u.Username, nil
		},
		lookupGID: func(gid string) (string, error) {
			g, err := user.LookupGroupId(gid)
			if err != nil {
				return "", err
			}

			return g.Name, nil
		},
	}

	for _, opt := range opts {
		opt(i)
	}

	return i
}

// Inspect gathers what this machine is doing with the given config.
//
// It reports rather than refuses: a status command must answer on a host that
// is half-configured, which is exactly when somebody runs it. Callers that need
// a decision make it from these facts.
func (i *Inspector) Inspect(ctx context.Context, configPath, keyPath string) (Report, error) {
	report := Report{ConfigPath: configPath}

	report.Config = i.fileFacts(configPath)
	if keyPath != "" {
		report.AppKey = i.fileFacts(keyPath)
	}

	if path, err := i.selfPath(); err != nil {
		report.BinaryErr = fmt.Errorf("resolve the running billet: %w", err)
	} else {
		report.Binary = path
	}

	// Resolved ONCE: every identity judgment below compares against this exact
	// inode, so a binary replaced midway through an inspection cannot leave two
	// units answering against two different selves.
	self, selfErr := i.selfExe()

	for _, unit := range []struct {
		name   string
		packed string
		facts  *ServiceFacts
	}{
		{deploy.ServerUnitName, deploy.ServerUnit, &report.Server},
		{deploy.NodeUnitName, deploy.NodeUnit, &report.Node},
	} {
		facts, err := i.service(ctx, unit.name, unit.packed, self, selfErr)
		if err != nil {
			return report, err
		}
		*unit.facts = facts
	}

	return report, nil
}

// service reads one unit and forms the judgments about it.
func (i *Inspector) service(ctx context.Context, unit, packaged string,
	self fs.FileInfo, selfErr error,
) (ServiceFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	props, err := i.properties(ctx, unit,
		"LoadState", "ActiveState", "SubState", "UnitFileState", "Result",
		"Type", "User", "Group", "FragmentPath", "DropInPaths",
		"StateDirectory", "RuntimeDirectory",
		"ExecStartPre", "ExecStartPost", "ExecCondition", "ExecStopPost", "ExecReload",
		"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "MountImages",
		"SupplementaryGroups", "AmbientCapabilities", "PAMName", "ExecSearchPath", "DynamicUser",
		"ExecStartEx",
		"Requires", "Requisite", "BindsTo", "PartOf", "Upholds", "Conflicts", "Wants",
		"OnFailure", "OnSuccess", "JoinsNamespaceOf",
		"OnFailureJobMode", "FailureAction", "SuccessAction", "StartLimitAction",
		"JobTimeoutAction", "Names",
		"MainPID", "NRestarts", "NeedDaemonReload", "ExecStart")
	if err != nil {
		return ServiceFacts{Name: unit}, fmt.Errorf("ask systemd about %s: %w", unit, err)
	}

	facts := ServiceFacts{
		Name:          unit,
		LoadState:     first(props, "LoadState"),
		ActiveState:   first(props, "ActiveState"),
		SubState:      first(props, "SubState"),
		UnitFileState: first(props, "UnitFileState"),
		Result:        first(props, "Result"),
		Type:          first(props, "Type"),
		User:          first(props, "User"),
		Group:         first(props, "Group"),
		FragmentPath:  first(props, "FragmentPath"),

		StateDirectory:   first(props, "StateDirectory"),
		RuntimeDirectory: first(props, "RuntimeDirectory"),

		// Rendered down to the command line: systemd's full answer carries pids
		// and timestamps that make a refusal unreadable.
		ExecExtra: commands(props,
			"ExecStartPre", "ExecStartPost", "ExecCondition", "ExecStopPost", "ExecReload"),
		Namespace: present(props,
			"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "MountImages"),
		Elevation: elevation(props),
		Names:     strings.Fields(first(props, "Names")),
		Actions:   actions(props),
		Transaction: present(props,
			"Requires", "Requisite", "BindsTo", "PartOf", "Upholds", "Conflicts", "Wants",
			"OnFailure", "OnSuccess", "JoinsNamespaceOf"),
		DropInPaths: dropInPaths(first(props, "DropInPaths")),
	}

	// AN EMPTY LoadState IS NOT "not-found". systemd always answers it for a
	// unit it was asked about — even an absent one, which reports not-found — so
	// an empty one means the reply was truncated or is not a reply this code
	// understands. Reporting that as "no such unit" would turn an unread answer
	// into a fact, and the caller acting on it starts services.

	if facts.LoadState == "" {
		return facts, fmt.Errorf("systemd's answer for %s carried no LoadState, so this is "+
			"not a reply billet can read", unit)
	}

	// A NON-NUMERIC ANSWER LEAVES THE ZERO, which is what systemd reports for a
	// unit that is not running anyway. Written out rather than discarded so the
	// intent is the code rather than a blank identifier.
	if pid, err := strconv.Atoi(first(props, "MainPID")); err == nil {
		facts.MainPID = pid
	}
	if n, err := strconv.Atoi(first(props, "NRestarts")); err == nil {
		facts.NRestarts = n
	}

	switch first(props, "NeedDaemonReload") {
	case "yes":
		facts.ReloadPending = Yes
	case "no":
		facts.ReloadPending = No
	}

	facts.ExecStartCount = len(props["ExecStart"])
	if facts.ExecStartCount > 0 {
		facts.ExecStart = execStartPath(props["ExecStart"][0])
		facts.ExecStartArgv = execStartArgv(props["ExecStart"][0])
	}

	// AN ANSWER BILLET CANNOT READ IS NOT AN ANSWER OF "no flags". A rendering
	// without the field — an older systemd, a changed format, a truncated record
	// — must leave this uncertain rather than permitting a start.
	facts.ExecStartExCount = len(props["ExecStartEx"])
	if facts.ExecStartExCount > 0 {
		facts.ExecStartFlags, facts.ExecStartFlagsKnown = execStartFlags(props["ExecStartEx"][0])
	}

	facts.ExecStartIsThisBuild, facts.ExecStartWhy = sameAsPath(self, selfErr, facts.ExecStart)
	facts.RunningIsThisBuild, facts.RunningWhy = i.sameAsProcess(self, selfErr, facts)
	facts.MatchesPackagedUnit = matchesPackaged(facts, packaged)

	return facts, nil
}

// sameAsPath answers whether the file at path is the same FILE as the running
// billet.
//
// BY INODE, NOT BY STRING. Comparing resolved paths refuses a correct
// deployment where the two names are a hardlink or a bind mount of one file,
// and accepts a wrong one where a binary was replaced at a path that still
// resolves. Uncertainty answers Unknown rather than Yes: a caller that must not
// start a service against the wrong binary needs "could not tell" to be its own
// state.
func sameAsPath(self fs.FileInfo, selfErr error, path string) (Tristate, string) {
	if selfErr != nil {
		return Unknown, fmt.Sprintf("cannot resolve the running billet: %v", selfErr)
	}
	if path == "" {
		return Unknown, "the unit names no executable"
	}

	other, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return No, fmt.Sprintf("%s does not exist", path)
		}

		return Unknown, fmt.Sprintf("cannot stat %s: %v", path, err)
	}

	if os.SameFile(self, other) {
		return Yes, ""
	}

	return No, fmt.Sprintf("%s is a different file from the running billet", path)
}

// sameAsProcess answers whether the process systemd is RUNNING is this build.
//
// THIS IS THE QUESTION THE UNIT FILE CANNOT ANSWER. Replace /usr/bin/billet
// without restarting and the unit's ExecStart resolves to the new file while the
// service goes on executing the old inode — so a check comparing only ExecStart
// reports agreement at exactly the moment an operator most needs to be told
// otherwise.
func (i *Inspector) sameAsProcess(self fs.FileInfo, selfErr error, facts ServiceFacts) (Tristate, string) {
	if !facts.Active() {
		return Unknown, "the service is not running"
	}
	if selfErr != nil {
		return Unknown, fmt.Sprintf("cannot resolve the running billet: %v", selfErr)
	}
	if facts.MainPID <= 0 {
		return Unknown, "systemd reports no main pid"
	}

	running, err := i.processExe(facts.MainPID)
	if err != nil {
		// Permission is the ordinary case for an unprivileged caller looking at
		// a root service. It is uncertainty, and saying so names the remedy.
		return Unknown, fmt.Sprintf("cannot read the running process's executable: %v", err)
	}

	if os.SameFile(self, running) {
		return Yes, ""
	}

	return No, fmt.Sprintf("pid %d is executing a different file from the running billet; "+
		"a binary replaced without a restart looks exactly like this", facts.MainPID)
}

// matchesPackaged compares the effective unit file against the one the PACKAGE
// ships.
//
// A DIFFERENCE IS NOT AN ACCUSATION. The Ansible role renders its own units and
// they are deliberately not byte-equal to the packaged ones, so a role-managed
// host differs here for good reasons — which is why this fact is named for what
// it compares rather than for a verdict about who edited what.
//
// A pending daemon-reload makes the comparison meaningless rather than false:
// the bytes on disk are then not the unit the manager would run.
func matchesPackaged(facts ServiceFacts, packaged string) Tristate {
	if facts.FragmentPath == "" || facts.ReloadPending == Yes {
		return Unknown
	}

	body, err := os.ReadFile(filepath.Clean(facts.FragmentPath))
	if err != nil {
		return Unknown
	}

	if string(body) == packaged {
		return Yes
	}

	return No
}

// fileFacts stats one path and names its owner.
func (i *Inspector) fileFacts(path string) FileFacts {
	facts := FileFacts{Path: path}

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		facts.Exists = No

		return facts
	case err != nil:
		// NOT absent — unknown. A stat that failed on permissions says nothing
		// about whether the file is there.
		facts.Err = err

		return facts
	}

	facts.Exists = Yes
	facts.Mode = info.Mode().Perm()
	facts.Info = info

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return facts
	}

	uid, gid := strconv.Itoa(int(st.Uid)), strconv.Itoa(int(st.Gid))

	facts.Owner = uid
	if name, err := i.lookupUID(uid); err == nil {
		facts.Owner = name
	}

	facts.Group = gid
	if name, err := i.lookupGID(gid); err == nil {
		facts.Group = name
	}

	return facts
}
