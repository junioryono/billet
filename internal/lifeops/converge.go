package lifeops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/deploy"
)

// DefaultStabilityWait is how long a service must keep the process it started
// before `up` calls it up.
//
// SIX SECONDS, matching the host role's own probe. It is not a health check and
// does not pretend to be: no finite window proves future life. What it catches
// is the failure this command would otherwise report as success — a service
// that reaches READY=1 and then dies, which Restart=on-failure turns into a
// crash loop that looks "active" at any single instant.
const DefaultStabilityWait = 6 * time.Second

// systemd creates a system unit's StateDirectory= under /var/lib and its
// RuntimeDirectory= under /run, and makes exactly those writable while
// ProtectSystem=strict holds everything else read-only. billet resolves the
// directories a role may use from the unit rather than from a constant here, so
// that moving them in the unit cannot leave this file quietly disagreeing.
const (
	stateRoot   = "/var/lib"
	runtimeRoot = "/run"
)

// Refusal is a reason `up` will not proceed, and what to do about it. Both
// halves are required: a refusal an operator cannot act on is a dead end.
type Refusal struct {
	What   string
	Remedy string
}

func (r Refusal) Error() string { return r.What + " — " + r.Remedy }

// OwnershipChange is one file whose owner or mode `up` will correct.
type OwnershipChange struct {
	Path  string
	Owner string
	Group string
	Mode  fs.FileMode
	// Why names the failure this prevents, for the operator reading --dry-run.
	Why string

	// at is what the planner saw at Path. Applying a change re-opens the path
	// and refuses unless it is still the same file, because between planning and
	// applying billet is about to chown as root and a pathname is not a promise.
	at fs.FileInfo
}

// UnitPlan is what `up` will do with one service.
type UnitPlan struct {
	Name string
	// Start is false when the unit is already active: `up` never restarts a
	// running service, because a restart is a drain and a drain destroys jobs.
	Start bool
	// Enable is false when the unit is already enabled.
	Enable bool
	// EnableBeforeStart inverts the order of the two steps above, because some
	// service managers cannot start a service that is not enabled.
	//
	// systemd can: `systemctl start` works on a disabled unit, which is exactly
	// what lets `up` prove a service runs BEFORE committing it to every future
	// boot. launchd cannot — a label carrying a disabled override refuses to
	// bootstrap at all — so on that manager there is no order in which the proof
	// comes first.
	//
	// WHERE THIS IS SET, THE PROOF NO LONGER PRECEDES THE COMMITMENT, and what
	// protects the host instead is the unwinding: a run that fails afterwards
	// undoes exactly the enablement it performed. That is a weaker guarantee
	// than the Linux one, so `up` says which one it is giving rather than
	// letting the two read alike.
	EnableBeforeStart bool
	// Detail is this manager's own phrase for what enabling a service does,
	// printed verbatim.
	Detail string
}

// UpPlan is everything `up` would do, computed before anything is touched so
// that --dry-run and the real run are the same decision.
type UpPlan struct {
	Refusals  []Refusal
	Ownership []OwnershipChange
	Units     []UnitPlan
	// ServerState is the directory the server unit keeps its ledger in, taken
	// from the unit's own StateDirectory=. Empty when no server is wanted.
	ServerState string
}

// UpRequest is what the caller wants converged.
type UpRequest struct {
	ConfigPath string
	KeyPath    string
	// ServiceUser and ServiceGroup are the identity the packaged units run as.
	ServiceUser  string
	ServiceGroup string
	// ServerStateDir and NodeStateDir are what the CONFIG says, so they can be
	// checked against what the units actually make writable.
	ServerStateDir string
	NodeStateDir   string
	NodeLockDir    string
	// WantServer and WantNode follow the config's own sections: a host converges
	// the roles its configuration defines, not a fixed pair.
	WantServer bool
	WantNode   bool
}

// unitSpec is what a unit MUST be before `up` will act on it. Every field is a
// thing systemd reports and billet can therefore check, rather than assume.
type unitSpec struct {
	// role is the subcommand this unit must run.
	role string
	// configPath is the config it must be given.
	configPath string
	// user and group are the identity it must run as.
	user, group string
	// stateDir and lockDir are what the config asks for, to be checked against
	// what the unit makes writable. Empty means the config asks for nothing.
	stateDir, lockDir string
}

// Converger changes a host. It is a separate type from Inspector because the
// two answer to different rules — an inspection may run anywhere and reports
// what it finds, while this refuses to act on anything it cannot establish.
type Converger struct {
	inspector *Inspector

	lookup func(name string) (*user.User, error)
	group  func(name string) (*user.Group, error)
	// openNoFollow opens a path for ownership repair without following a final
	// symlink. A seam because a test cannot be root, and this is the operation
	// that would hand an arbitrary file to the service account if it were wrong.
	openNoFollow func(path string) (ownedFile, error)
	// repairRoot opens a directory tree the repair walk is confined to.
	repairRoot func(dir string) (rootFS, error)

	stabilityWait func(time.Duration)
	wait          time.Duration
}

// ownedFile is the subset of *os.File ownership repair uses. Chown and Chmod on
// an open file are fchown and fchmod: they act on the descriptor, so nothing
// that happens to the PATH between opening and changing it can redirect them.
type ownedFile interface {
	Stat() (fs.FileInfo, error)
	Chown(uid, gid int) error
	Chmod(mode fs.FileMode) error
	Close() error
}

// rootFS is the subset of *os.Root the state repair uses. Every operation is
// resolved inside the directory that was opened, so a symlink planted in the
// state directory cannot walk the repair out of it.
type rootFS interface {
	// OpenFile resolves inside the opened directory and answers with a
	// descriptor, so the checks and the change land on ONE inode. The state
	// directory is writable by the service account: a name checked and then
	// chowned is a name that can be replaced in between.
	OpenFile(name string, flag int, perm fs.FileMode) (ownedFile, error)
	Close() error
}

// ConvergeOption configures a Converger.
type ConvergeOption func(*Converger)

// WithStabilityWait sets how long a started service must hold its process.
func WithStabilityWait(d time.Duration) ConvergeOption {
	return func(c *Converger) {
		if d > 0 {
			c.wait = d
		}
	}
}

// withHostSeams replaces the operations a test cannot perform: chown needs
// root, and account lookup reads the host's own database.
func withHostSeams(
	lookup func(string) (*user.User, error),
	group func(string) (*user.Group, error),
	open func(string) (ownedFile, error),
	repair func(string) (rootFS, error),
	sleep func(time.Duration),
) ConvergeOption {
	return func(c *Converger) {
		if lookup != nil {
			c.lookup = lookup
		}
		if group != nil {
			c.group = group
		}
		if open != nil {
			c.openNoFollow = open
		}
		if repair != nil {
			c.repairRoot = repair
		}
		if sleep != nil {
			c.stabilityWait = sleep
		}
	}
}

// NewConverger builds one around an Inspector.
func NewConverger(i *Inspector, opts ...ConvergeOption) *Converger {
	c := &Converger{
		inspector:     i,
		lookup:        user.Lookup,
		group:         user.LookupGroup,
		openNoFollow:  openNoFollow,
		repairRoot:    openRepairRoot,
		stabilityWait: time.Sleep,
		wait:          DefaultStabilityWait,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// openNoFollow opens a regular file for ownership repair, refusing a symlink.
//
// O_NOFOLLOW applies to the FINAL component only, so a symlinked parent
// directory still redirects this — the residual is stated rather than claimed
// away. It is bounded by where these paths live: the config is under /etc/billet
// and the key is refused unless it is there too, both root-owned directories.
func openNoFollow(name string) (ownedFile, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// openRepairRoot opens a directory every later operation is resolved inside.
func openRepairRoot(dir string) (rootFS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}

	return &osRoot{root}, nil
}

type osRoot struct{ *os.Root }

func (r *osRoot) OpenFile(name string, flag int, perm fs.FileMode) (ownedFile, error) {
	return r.Root.OpenFile(name, flag, perm)
}

// Plan decides what `up` would do, and every reason it would not.
//
// IT COLLECTS REFUSALS RATHER THAN RETURNING THE FIRST. An operator who has to
// re-run a command to discover the next thing wrong with their host is being
// made to pay for a diagnostic that already knew.
func (c *Converger) Plan(ctx context.Context, req UpRequest) (UpPlan, error) {
	var plan UpPlan

	report, err := c.inspector.Inspect(ctx, req.ConfigPath, req.KeyPath)
	if err != nil {
		return plan, err
	}

	// THE SERVICE ACCOUNT IS THE PACKAGE'S TO CREATE, and this is where a host
	// that only ran install.sh finds out. Handing that decision a second
	// implementation would mean two places deciding whether an existing
	// account named "billet" may hold an organization's credentials.
	_, _, accountRefusals := c.identity(req)
	plan.Refusals = append(plan.Refusals, accountRefusals...)

	// THE SERVER FIRST, ALWAYS: the server starts before the node, and a node that
	// came up first has nothing to register with.
	type wanted struct {
		facts *ServiceFacts
		spec  unitSpec
	}

	var units []wanted
	if req.WantServer {
		units = append(units, wanted{&report.Server, unitSpec{
			role: "server", configPath: req.ConfigPath,
			// The server holds the App key and must NOT be root: that is the
			// privilege boundary the packaged units draw, and a unit that has been
			// edited to run it as root is not one billet starts on an operator's
			// behalf.
			user: req.ServiceUser, group: req.ServiceGroup,
			stateDir: req.ServerStateDir,
		}})
	}
	if req.WantNode {
		units = append(units, wanted{&report.Node, unitSpec{
			role: "node", configPath: req.ConfigPath,
			// The node IS root, deliberately: it creates TAP devices, enters
			// cgroups and chroots, and signals a VMM. A node unit running as the
			// unprivileged account is one whose first job fails.
			user: "root", group: "root",
			stateDir: req.NodeStateDir, lockDir: req.NodeLockDir,
		}})
	}

	for _, u := range units {
		unitPlan, refusals := planUnit(*u.facts, u.spec)

		// WHAT THIS UNIT PULLS IN, AND WHAT THAT WOULD DO. A start is a
		// transaction: a helper billet's own unit merely Wants= runs its own
		// Conflicts=, and that is a STOP of whatever it names. Asked before
		// anything is started, because a check that runs afterwards is a report
		// of destroyed jobs rather than a refusal to destroy them.
		if len(refusals) == 0 {
			pulled, err := c.pulledRefusals(ctx, *u.facts)
			if err != nil {
				return plan, err
			}
			refusals = append(refusals, pulled...)
		}

		plan.Refusals = append(plan.Refusals, refusals...)

		if len(refusals) == 0 {
			plan.Units = append(plan.Units, unitPlan)
		}
	}

	if req.WantServer && report.Server.StateDirectory != "" {
		plan.ServerState = filepath.Join(stateRoot, report.Server.StateDirectory)
	}

	if len(accountRefusals) == 0 {
		plan.Ownership = ownershipChanges(report, req)
	}

	return plan, nil
}

// identity resolves the service account, and refuses when it is absent.
//
// AN NSS FAILURE IS NOT AN ABSENT ACCOUNT. A lookup that could not complete —
// an unreachable directory service, a malformed passwd entry — reports
// uncertainty, because the remedy for "create the account" is wrong and
// destructive-adjacent advice on a host that already has one.
func (c *Converger) identity(req UpRequest) (int, int, []Refusal) {
	uid, gid := -1, -1

	u, err := c.lookup(req.ServiceUser)
	if err != nil {
		//nolint:errcheck // the discarded value is the typed error itself, not a failure; the bool is the answer. errcheck cannot exclude a generic function.
		_, ok := errors.AsType[user.UnknownUserError](err)
		if !ok {
			return uid, gid, []Refusal{{
				What: fmt.Sprintf("billet could not tell whether the %q account exists: %v",
					req.ServiceUser, err),
				Remedy: "resolve the account lookup — this is not evidence the account is missing, " +
					"and creating a second one would be the wrong fix",
			}}
		}

		return uid, gid, []Refusal{{
			What: fmt.Sprintf("there is no %q account, which the packaged units run as", req.ServiceUser),
			Remedy: "install the billet package — its postinstall creates the account and refuses " +
				"to adopt an unrelated one of the same name — or create it yourself with " +
				"`groupadd --system billet` and `useradd --system --gid billet " +
				"--home-dir /var/lib/billet --shell /usr/sbin/nologin billet`",
		}}
	}

	g, err := c.group(req.ServiceGroup)
	if err != nil {
		//nolint:errcheck // the discarded value is the typed error itself, not a failure; the bool is the answer. errcheck cannot exclude a generic function.
		_, ok := errors.AsType[user.UnknownGroupError](err)
		if !ok {
			return uid, gid, []Refusal{{
				What: fmt.Sprintf("billet could not tell whether the %q group exists: %v",
					req.ServiceGroup, err),
				Remedy: "resolve the group lookup before running this again",
			}}
		}

		return uid, gid, []Refusal{{
			What:   fmt.Sprintf("there is no %q group, which the config must be readable by", req.ServiceGroup),
			Remedy: "install the billet package, or run `groupadd --system billet`",
		}}
	}

	// A NON-NUMERIC ID IS A REFUSAL, NOT A DEFAULT. Leaving -1 here would be
	// handed to chown, where it means "leave this as it is" — so an unreadable
	// account id would silently become a successful no-op, and the service would
	// fail later on a file nobody changed.
	uid, err = numericID(u.Uid)
	if err != nil {
		return -1, -1, []Refusal{{
			What:   fmt.Sprintf("the %q account has a uid billet cannot read (%q)", req.ServiceUser, u.Uid),
			Remedy: "billet chowns by numeric id; resolve the account database before running this again",
		}}
	}

	gid, err = numericID(g.Gid)
	if err != nil {
		return -1, -1, []Refusal{{
			What:   fmt.Sprintf("the %q group has a gid billet cannot read (%q)", req.ServiceGroup, g.Gid),
			Remedy: "billet chowns by numeric id; resolve the account database before running this again",
		}}
	}

	// THE SERVICE ACCOUNT MAY NOT BE ROOT, and User=billet in the unit is not
	// evidence of that: what "billet" resolves to is the account database's
	// answer, not the unit's. An account resolving to 0 would run the control
	// plane — which holds a key that can mint tokens for a whole organization —
	// with full privileges, past every check that reads the unit.
	if uid == 0 || gid == 0 {
		return -1, -1, []Refusal{{
			What: fmt.Sprintf("the %q account resolves to uid %d, gid %d", req.ServiceUser, uid, gid),
			Remedy: "the server unit is deliberately unprivileged because it holds an App key " +
				"for a whole organization; an account named billet that resolves to root " +
				"defeats that entirely. Resolve the account database before running this again",
		}}
	}

	return uid, gid, nil
}

// planUnit decides what to do with one service, and every reason not to.
//
// THE EFFECTIVE UNIT IS WHAT MATTERS, not the file on disk. systemd builds a
// unit from a fragment plus drop-ins, and a mask, a link or an override can
// make what it will run bear no relation to what an operator reads in /etc.
// The upgrade transaction in the Ansible role refuses these same states for the
// same reason: they cannot be reasoned about from the file alone.
func planUnit(s ServiceFacts, spec unitSpec) (UnitPlan, []Refusal) {
	if !s.Installed() {
		return UnitPlan{Name: s.Name}, []Refusal{{
			What: fmt.Sprintf("%s is not installed", s.Name),
			Remedy: "install the billet package, which ships the units; `billet local up` " +
				"deliberately does not write unit files, because a unit it wrote could " +
				"silently shadow a later package install",
		}}
	}

	refusals := unitRefusals(s, spec)

	start, startRefusal := startDecision(s)
	if startRefusal != nil {
		refusals = append(refusals, *startRefusal)
	}

	enable, enableRefusal := enableDecision(s)
	if enableRefusal != nil {
		refusals = append(refusals, *enableRefusal)
	}

	return UnitPlan{Name: s.Name, Start: start, Enable: enable}, refusals
}

// unitRefusals is every reason the DEFINITION of a unit disqualifies it.
func unitRefusals(s ServiceFacts, spec unitSpec) []Refusal {
	var refusals []Refusal

	// UNKNOWN IS NOT NO. systemd answers this for every unit it knows, so
	// anything but a definite "no" leaves billet unable to say whether the
	// bytes on disk are the unit the manager would run.
	if s.ReloadPending != No {
		what := fmt.Sprintf("%s changed on disk since systemd read it", s.Name)
		if s.ReloadPending == Unknown {
			what = fmt.Sprintf("billet cannot tell whether %s changed on disk since systemd read it", s.Name)
		}

		refusals = append(refusals, Refusal{
			What:   what,
			Remedy: "run `systemctl daemon-reload`, then run this again",
		})
	}

	if s.LoadState != "loaded" {
		refusals = append(refusals, Refusal{
			What:   fmt.Sprintf("%s is %s rather than loaded", s.Name, s.LoadState),
			Remedy: fmt.Sprintf("unmask it with `systemctl unmask %s`, or resolve why it will not load", s.Name),
		})
	}

	if len(s.DropInPaths) > 0 {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s has drop-in overrides: %v", s.Name, s.DropInPaths),
			Remedy: "fold them into the unit or remove them; a drop-in can replace ExecStart, the " +
				"account, or the readiness protocol, and billet cannot start a service whose " +
				"effective definition it has not accounted for",
		})
	}

	// READINESS IS SYSTEMD'S ANSWER OR IT IS NOTHING. `up` treats a successful
	// `systemctl start` as proof the process reached READY=1, and that is only
	// true of a notify unit: with Type=exec or Type=simple, start returns as soon
	// as the kernel has execve'd the binary, so a process that dies during its
	// first second would be reported as ready.
	if s.Type != "notify" {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s is Type=%s rather than Type=notify", s.Name, orUnknown(s.Type)),
			Remedy: "reinstall the billet package or re-run the junioryono.billet.host role; " +
				"`up` treats a successful start as readiness, which only a notify unit " +
				"establishes — under any other type it would call a process that is about " +
				"to die ready",
		})
	}

	if s.User != spec.user || s.Group != spec.group {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s runs as %s:%s rather than %s:%s",
				s.Name, orUnknown(s.User), orUnknown(s.Group), spec.user, spec.group),
			Remedy: "restore the packaged unit; the server is deliberately unprivileged because " +
				"it holds an App key that can mint tokens for a whole organization, and the " +
				"node is deliberately root because it creates devices and signals a VMM",
		})
	}

	// EVERY OTHER COMMAND THE UNIT WOULD RUN. ExecStartPre and its relatives are
	// not covered by anything above, and on the server unit they run before the
	// process billet checked — so a unit carrying one starts work billet has not
	// accounted for, at the identity the unit chooses.
	for _, name := range sortedKeys(s.ExecExtra) {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s carries %s=%s", s.Name, name, s.ExecExtra[name]),
			Remedy: "restore the packaged unit; billet accounts for the one command a unit " +
				"runs, and will not start a service that runs others it has not seen",
		})
	}

	// AND A UNIT THAT CAN REPLACE THE FILESYSTEM IT RUNS IN. Under a
	// RootDirectory or a bind mount, the path billet compared by inode names a
	// different file inside the service's own view.
	for _, name := range sortedKeys(s.Namespace) {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s sets %s=%s, so the paths billet checked are not the paths "+
				"the service would see", s.Name, name, s.Namespace[name]),
			Remedy: "restore the packaged unit; an executable proved by inode means nothing " +
				"if the service runs in a filesystem where that path is something else",
		})
	}

	// AND ANYTHING THAT CHANGES WHO IT RUNS AS past the account User= names.
	// SupplementaryGroups is the sharpest of these: adding `docker` to the
	// unprivileged server is root by another route, and the identity check above
	// still reads billet:billet.
	for _, name := range sortedKeys(s.Elevation) {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s sets %s=%s, which changes who it runs as past %s:%s",
				s.Name, name, s.Elevation[name], orUnknown(s.User), orUnknown(s.Group)),
			Remedy: "restore the packaged unit; the identity a unit names is only the identity " +
				"it runs as when nothing else widens it",
		})
	}

	// WHAT THIS UNIT DOES TO THE HOST WHEN IT FAILS. `OnFailureJobMode=isolate`
	// stops everything else on the machine, and FailureAction or
	// StartLimitAction can reboot it — from a service billet is about to start.
	for _, name := range sortedKeys(s.Actions) {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s sets %s=%s, which acts on the host or on other units rather "+
				"than on itself", s.Name, name, s.Actions[name]),
			Remedy: "restore the packaged unit; billet will not start a service whose failure " +
				"isolates or reboots the machine it is running on",
		})
	}

	// AND A SECOND NAME IS A SECOND SET OF LINKS. An Alias= makes `enable` write
	// for a name billet did not ask about. An EMPTY answer is not "no aliases":
	// systemd always reports a unit's own name, so nothing coming back means
	// this is not a reply billet can read.
	switch {
	case len(s.Names) == 0:
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("systemd did not say what names %s answers to", s.Name),
			Remedy: "billet cannot tell whether enabling this unit would write links for a name " +
				"it did not ask about; resolve why `systemctl show -p Names` answers nothing",
		})
	default:
		for _, name := range s.Names {
			if name != s.Name {
				refusals = append(refusals, Refusal{
					What: fmt.Sprintf("%s also answers to %s", s.Name, name),
					Remedy: "restore the packaged unit; enabling a unit with an alias writes " +
						"links for a name billet did not account for, and disabling it " +
						"removes them",
				})
			}
		}
	}

	refusals = append(refusals, transactionRefusals(s)...)
	refusals = append(refusals, execRefusals(s, spec)...)
	refusals = append(refusals, directoryRefusals(s, spec)...)

	return refusals
}

// execRefusals covers what the unit would RUN: which file, and told to do what.
func execRefusals(s ServiceFacts, spec unitSpec) []Refusal {
	var refusals []Refusal

	if s.ExecStartCount != 1 {
		return append(refusals, Refusal{
			What:   fmt.Sprintf("%s carries %d ExecStart directives", s.Name, s.ExecStartCount),
			Remedy: "leave exactly one, so which binary the unit runs is a question with an answer",
		})
	}

	// THE ONE THAT LOOKS FINE FROM EVERY OTHER ANGLE. The packaged units name
	// /usr/bin/billet while scripts/install.sh writes /usr/local/bin, so a host
	// with both starts a service that will never pick up an upgrade.
	switch s.ExecStartIsThisBuild {
	case Yes:
	case No:
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s would run %s, which is not this billet (%s)",
				s.Name, s.ExecStart, s.ExecStartWhy),
			Remedy: "install the package (it puts the binary at /usr/bin/billet), or run this " +
				"command from the binary the unit names — scripts/install.sh writes " +
				"/usr/local/bin, which the packaged units do not read",
		})
	case Unknown:
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("billet cannot tell whether %s would run this binary: %s", s.Name, s.ExecStartWhy),
			Remedy: "resolve the uncertainty above; starting a service against an unidentified " +
				"binary is exactly what this refuses to do",
		})
	}

	// WHICH ROLE, AND AGAINST WHICH CONFIG. The identity check above proves only
	// which FILE runs. A unit named billet-node.service whose command line says
	// `billet server` starts a control plane — and `up` skips the GitHub proof
	// entirely for a config with no server section, so that control plane would
	// come up on an organization nothing verified. Compared whole rather than
	// parsed: an exact match needs no quoting rules, and anything billet did not
	// expect refuses instead of being interpreted.
	want := spec.role + " --config " + spec.configPath
	if s.ExecStart != "" {
		want = s.ExecStart + " " + want
	}

	// THE PREFIXES, WHICH THE ORDINARY ANSWER DOES NOT CARRY. Measured on
	// systemd 255: `ExecStart=+/usr/bin/billet server …` runs with full
	// privileges no matter what User= says, and `systemctl show -p ExecStart`
	// renders it byte-identically to the unprefixed form. The extended property
	// is where the flags live, and an empty flags field is the ordinary answer.
	switch {
	case s.ExecStartExCount == 0:
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("systemd did not tell billet what prefixes %s's command carries", s.Name),
			Remedy: "billet needs `systemctl show -p ExecStartEx`, which systemd has carried since " +
				"v246; without it a command that runs as root regardless of User= cannot be " +
				"told from an ordinary one, and billet will not guess",
		})
	case !s.ExecStartFlagsKnown:
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("billet could not read the prefixes on %s's command from systemd's "+
				"extended answer", s.Name),
			Remedy: "an answer billet cannot read is not an answer of \"no prefixes\"; resolve why " +
				"`systemctl show -p ExecStartEx` renders this unit the way it does",
		})
	case s.ExecStartFlags != "":
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s's command carries the flags %q", s.Name, s.ExecStartFlags),
			Remedy: "restore the packaged unit. A leading +, ! or !! runs the command with full " +
				"privileges whatever User= says, and the ordinary ExecStart answer renders " +
				"it identically to a command without one",
		})
	}

	if s.ExecStartArgv != want {
		refusals = append(refusals, Refusal{
			What: fmt.Sprintf("%s would run %q rather than %q",
				s.Name, orUnknown(s.ExecStartArgv), want),
			Remedy: "if an upgrade is in progress, let it finish — the host role installs " +
				"probe units carrying --upgrade-probe, and restoring a unit underneath " +
				"that transaction is how a half-finished upgrade becomes an unrecoverable " +
				"one. Otherwise restore the packaged unit: billet starts a unit only when " +
				"it can see which role that unit runs and which config it reads, because " +
				"the name of a unit is not evidence of either",
		})
	}

	return refusals
}

// directoryRefusals checks the config's paths against the directories systemd
// actually makes writable for this unit.
//
// A PREFIX TEST IS NOT CONTAINMENT: "/var/lib/billet-evil" begins with
// "/var/lib/billet" and is a different directory, and neither is writable by a
// unit whose StateDirectory names one path. The unit's own answer is the
// authority, so this cannot drift from what the units say.
func directoryRefusals(s ServiceFacts, spec unitSpec) []Refusal {
	var refusals []Refusal

	for _, d := range []struct {
		what, configured, declared, root string
	}{
		{"state_dir", spec.stateDir, s.StateDirectory, stateRoot},
		{"lock_dir", spec.lockDir, s.RuntimeDirectory, runtimeRoot},
	} {
		if d.configured == "" {
			continue
		}

		if d.declared == "" {
			refusals = append(refusals, Refusal{
				What: fmt.Sprintf("%s declares no directory for %s, so billet cannot tell whether "+
					"%s is writable by it", s.Name, d.what, d.configured),
				Remedy: "restore the packaged unit, which declares the directories systemd creates " +
					"and makes writable",
			})

			continue
		}

		want := filepath.Join(d.root, d.declared)
		if filepath.Clean(d.configured) != want {
			refusals = append(refusals, Refusal{
				What: fmt.Sprintf("%s is %s, but %s can only write %s",
					d.what, d.configured, s.Name, want),
				Remedy: fmt.Sprintf("set %s to %s, or regenerate the config with "+
					"`billet init --profile local-service`; under ProtectSystem=strict "+
					"everything outside the unit's own directories is read-only, and "+
					"ProtectHome=true hides a home directory outright", d.what, want),
			})
		}
	}

	return refusals
}

// startDecision answers whether to start this unit, refusing the states where
// the question has no safe answer.
//
// A TRANSITION IN PROGRESS IS NOT A STOPPED SERVICE. `deactivating` is a node
// draining — the shutdown that waits for the jobs still running — and starting
// through it is how a converge destroys the work it was meant to preserve.
func startDecision(s ServiceFacts) (bool, *Refusal) {
	switch s.ActiveState {
	case "active":
		return false, nil
	case "inactive", "failed":
		return true, nil
	case "activating", "deactivating", "reloading", "refreshing":
		return false, &Refusal{
			What: fmt.Sprintf("%s is %s: a transition is already in progress", s.Name, s.ActiveState),
			Remedy: fmt.Sprintf("wait for it to settle and run this again — `deactivating` is a "+
				"drain, and the jobs still running on this host are what it is waiting "+
				"for (`systemctl status %s`)", s.Name),
		}
	default:
		return false, &Refusal{
			What: fmt.Sprintf("billet does not recognise %s's state %q", s.Name, s.ActiveState),
			Remedy: "resolve it with `systemctl status " + s.Name + "`; billet will not start a " +
				"service whose current state it cannot read, because it cannot tell whether " +
				"that would be a restart",
		}
	}
}

// enableDecision answers whether to enable this unit at boot.
func enableDecision(s ServiceFacts) (bool, *Refusal) {
	// A unit that will not load has already been refused for that; adding a
	// second refusal about its enablement would be the same fact twice.
	if s.LoadState != "loaded" {
		return false, nil
	}

	switch s.UnitFileState {
	case "enabled":
		return false, nil
	case "disabled":
		return true, nil
	case "enabled-runtime":
		return false, &Refusal{
			What: fmt.Sprintf("%s is enabled only until the next reboot", s.Name),
			Remedy: fmt.Sprintf("decide deliberately: `systemctl disable --runtime %s` and run "+
				"this again to enable it permanently, or leave it as it is. billet will "+
				"not quietly convert a temporary enablement into a permanent one", s.Name),
		}
	case "linked", "linked-runtime":
		return false, &Refusal{
			What: fmt.Sprintf("%s is a link to a unit file outside the unit directories", s.Name),
			Remedy: "install the unit properly (the package does), so what runs is not decided " +
				"by a symlink target billet cannot account for",
		}
	default:
		return false, &Refusal{
			What: fmt.Sprintf("billet does not recognise %s's enablement state %q", s.Name, s.UnitFileState),
			Remedy: "resolve it with `systemctl is-enabled " + s.Name + "`; billet will not commit " +
				"a unit to every future boot from a state it cannot read",
		}
	}
}

// orUnknown renders an empty systemd answer as what it is.
func orUnknown(v string) string {
	if v == "" {
		return "(no answer)"
	}

	return v
}

// ownershipChanges is the set of files whose owner or mode would stop a service
// from starting, or stop billet from reading its own credential.
//
// DELIBERATELY NARROW. `up` does not create or chown state directories: systemd
// creates them from StateDirectory= with the unit's own account, and the
// package and the Ansible role disagree about /var/lib/billet itself, so
// anything asserted there would be picking a side.
func ownershipChanges(report Report, req UpRequest) []OwnershipChange {
	var changes []OwnershipChange

	// The config: root-owned so an unprivileged process cannot edit what billet
	// trusts, group-readable or the server cannot start at all.
	if report.Config.Exists == Yes {
		if report.Config.Owner != "root" || report.Config.Group != req.ServiceGroup ||
			report.Config.Mode.Perm() != 0o640 {
			changes = append(changes, OwnershipChange{
				Path: report.Config.Path, Owner: "root", Group: req.ServiceGroup, Mode: 0o640,
				Why: "a config the service cannot read is a service that will not start, and one " +
					"it can WRITE is a service that can rewrite what billet trusts",
				at: report.Config.Info,
			})
		}
	}

	// The App key: owned by the service user at 0600, and it is the one file
	// here that cannot be root-owned-and-group-readable. billet refuses any key
	// with group or other bits set, so 0640 root:billet makes the server refuse
	// to start while 0600 root:root is unreadable by it. The only arrangement
	// satisfying both is the ordinary one: the process that needs the secret
	// owns the secret.
	if report.AppKey.Path != "" && report.AppKey.Exists == Yes {
		if report.AppKey.Owner != req.ServiceUser || report.AppKey.Group != req.ServiceGroup ||
			report.AppKey.Mode.Perm() != 0o600 {
			changes = append(changes, OwnershipChange{
				Path: report.AppKey.Path, Owner: req.ServiceUser, Group: req.ServiceGroup, Mode: 0o600,
				Why: "billet refuses an App key readable beyond its owner, and the service must " +
					"still be able to read it",
				at: report.AppKey.Info,
			})
		}
	}

	return changes
}

// ApplyOwnership makes the changes a plan named.
//
// THROUGH A DESCRIPTOR, NOT A PATHNAME. This runs as root, and os.Chown follows
// symlinks: a path that named a regular file when the plan was made and a
// symlink by the time it is applied would hand an arbitrary file's ownership to
// the service account. The file is opened without following, checked to be the
// same file the planner saw, and changed through the descriptor — so whatever
// happens to the NAME afterwards, the operation lands on the inode that was
// verified.
//
// THE MODE IS TIGHTENED BEFORE THE OWNER IS TRANSFERRED. Reversed, a chown that
// succeeds and a chmod that fails leaves a private key owned by the service
// account with whatever permissions it had — which is how a 0644 App key
// becomes readable by every process running as that account's group.
func (c *Converger) ApplyOwnership(changes []OwnershipChange, uid, gid int) error {
	for _, change := range changes {
		if err := c.applyOne(change, uid, gid); err != nil {
			return err
		}
	}

	return nil
}

func (c *Converger) applyOne(change OwnershipChange, uid, gid int) error {
	f, err := c.openNoFollow(change.Path)
	if err != nil {
		return fmt.Errorf("open %s to correct its ownership (a symlink here is refused rather "+
			"than followed, because this runs as root): %w", change.Path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", change.Path, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file, and billet will not chown one", change.Path)
	}

	// A HARD LINK IS THE SAME INODE UNDER ANOTHER NAME, so chmod and chown here
	// change the file wherever else it is named. The state repair already refuses
	// this; the config and the App key are the more sensitive of the two paths.
	if st, ok := info.Sys().(*syscall.Stat_t); !ok {
		return fmt.Errorf("billet could not read %s's link count, and will not chown a file it "+
			"cannot tell is named only once", change.Path)
	} else if st.Nlink > 1 {
		return fmt.Errorf("%s has %d links, so changing its owner or mode would change the file "+
			"everywhere else it is named; billet will not do that to a config or a private key",
			change.Path, st.Nlink)
	}

	if change.at != nil && !os.SameFile(change.at, info) {
		return fmt.Errorf("%s is not the file billet planned to correct any more; nothing was "+
			"changed. Run this again", change.Path)
	}

	// root is 0 everywhere; every other owner this plan names is the service
	// account the caller already resolved.
	owner := uid
	if change.Owner == "root" {
		owner = 0
	}

	group := -1
	if change.Group != "" {
		group = gid
	}

	// ENFORCED, NOT REQUESTED: a creation mode is reduced by the umask, and
	// the server unit itself runs UMask=0077, so a mode that was merely
	// asked for can land tighter than the service can read.
	if err := f.Chmod(change.Mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", change.Path, err)
	}

	if err := f.Chown(owner, group); err != nil {
		return fmt.Errorf("set the owner of %s: %w", change.Path, err)
	}

	return nil
}

// RepairServerState gives the server back the state a root-run preflight
// created underneath it.
//
// MEASURED, NOT ASSUMED, and it is the reason this function exists at all:
// `billet check` opens the ledger as the invoking process, so running it as
// root creates billet.db and billet.lock owned by root. systemd's
// StateDirectory= repairs ownership RECURSIVELY when the top directory's owner
// is wrong — verified on systemd 255 — but does nothing at all when the
// directory is already correct, which is every run after the first. The result
// is a billet-owned directory holding a root-owned ledger, and a server that
// starts and cannot write.
//
// The walk is confined to an opened directory rather than composed from
// pathnames, so a symlink planted in the state directory cannot make a root
// chown land outside it. Symlinks are skipped rather than followed, and only
// entries owned by root are touched: this repairs what a root preflight
// created, and is not a recursive chown of whatever it finds.
// preflightState is every file `billet check` can create in the server's state
// directory, by name.
//
// AN EXPLICIT LIST RATHER THAN A WALK, and the walk it replaced is why. os.Root
// confines pathname resolution, not inodes and not mount boundaries: a bind
// mount inside the state directory commonly shares its device, so a
// same-filesystem mount was descended into and the root-owned files beneath it
// reached a privileged chown. Repairing exactly what the preflight can create
// removes the traversal entirely — there is nothing to descend, so nothing to
// escape through — and anything else in that directory was not put there by
// this command and is not this command's to change.
var preflightState = []RepairTarget{
	{Name: "billet.db"}, {Name: "billet.db-wal"}, {Name: "billet.db-shm"},
	{Name: "billet.lock"}, {Name: "deployment-id"},
}

// RepairTarget is one entry a privileged billet command created inside a state
// directory, named relative to it.
//
// Dir says the entry is a DIRECTORY rather than a file, and it is not cosmetic:
// a root-owned 0700 directory cannot even be TRAVERSED by the service account,
// so an authority restored under one is unreachable however its files are
// owned — and a directory's link count is always above one, which is the check
// that refuses a hard-linked file.
type RepairTarget struct {
	Name string
	Dir  bool
}

// RepairServerState gives the server back the state a root-run preflight
// created underneath it.
//
// MEASURED, NOT ASSUMED, and it is the reason this function exists at all:
// `billet check` opens the ledger as the invoking process, so running it as
// root creates billet.db and billet.lock owned by root. systemd's
// StateDirectory= repairs ownership RECURSIVELY when the top directory's owner
// is wrong — verified on systemd 255 — but does nothing at all when the
// directory is already correct, which is every run after the first. The result
// is a billet-owned directory holding a root-owned ledger, and a server that
// starts and cannot write.
//
// Only entries owned by root are touched, only regular files, and only those
// with a single link: a hard link is the same inode under two names, and
// chowning one here would change the file it aliases wherever else that file
// lives.
func (c *Converger) RepairServerState(dir string, uid, gid int) ([]string, error) {
	return c.RepairPaths(dir, preflightState, uid, gid)
}

// RepairPaths gives the service account back a NAMED SET of entries a
// privileged command created in a state directory.
//
// THE CALLER SUPPLIES THE SET, and for `billet local restore` that set is
// derived from the plan it just executed rather than from a list kept here.
// A hand-maintained second list is the failure this whole area keeps producing:
// preflightState covers what `billet check` creates and covers none of what a
// restore publishes — no `ca/`, no `authority-created` — so a restore run as
// root (which is what an operator restoring onto a packaged host IS, because the
// App key lands in root-owned /etc/billet) left every one of those files owned
// by root inside a service-owned directory, where systemd's StateDirectory= will
// never look at them again. Measured in scripts/restore-rehearsal.sh: the
// service account could not open the deployment root had just restored.
//
// There is no walk, which is what makes it safe: nothing is discovered, so
// nothing planted can be reached, and every entry named is one this run put
// there.
func (c *Converger) RepairPaths(
	dir string, targets []RepairTarget, uid, gid int,
) ([]string, error) {
	if dir == "" || uid == 0 || len(targets) == 0 {
		return nil, nil
	}

	root, err := c.repairRoot(dir)
	if err != nil {
		// A state directory that does not exist yet is the ordinary first run:
		// systemd creates it, with the right owner, when the service starts.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("open %s to check what the preflight left there: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	// THE DIRECTORY'S OWN FILESYSTEM decides which files belong to it. Read from
	// a descriptor on the directory rather than by path, for the same reason
	// every other check here is.
	// Kept as the platform's own stat rather than converted: the device field is
	// uint64 on linux/amd64, uint32 on linux/arm64 and signed on darwin, and
	// comparing two of the same kind needs no conversion on any of them.
	var dirStat *syscall.Stat_t
	if d, err := root.OpenFile(".", os.O_RDONLY, 0); err == nil {
		if info, err := d.Stat(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				dirStat = st
			}
		}
		_ = d.Close()
	}

	var repaired []string

	for _, target := range targets {
		// O_NOFOLLOW so a symlink planted here is refused rather than followed,
		// and every check below reads the descriptor this opened rather than the
		// name — the state directory is writable by the service account, so a
		// name checked and then chowned is a name that can be replaced between
		// the two.
		//
		// MEASURED: os.Root.OpenFile opens a directory under O_RDONLY|O_NOFOLLOW
		// and the descriptor stats and chowns like any other, so the authority's
		// own directory needs no second mechanism.
		f, err := root.OpenFile(target.Name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return repaired, fmt.Errorf("open %s to see whether a privileged billet left it "+
				"owned by root: %w", path.Join(dir, target.Name), err)
		}

		change, err := repairOne(f, uid, gid, dirStat, target.Dir)
		_ = f.Close()

		if err != nil {
			return repaired, fmt.Errorf("%s: %w", path.Join(dir, target.Name), err)
		}
		if change {
			repaired = append(repaired, path.Join(dir, target.Name))
		}
	}

	return repaired, nil
}

// repairOne gives one file back, or refuses. It answers whether it changed
// anything, and everything it declines to change is an ERROR rather than a note:
// these are the five files the server has to be able to open, so one of them in
// a state billet cannot account for is a host that will not come up, and saying
// so quietly while starting the service anyway is the failure this whole
// function exists to prevent.
func repairOne(f ownedFile, uid, gid int, dirStat *syscall.Stat_t, wantDir bool) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("billet could not read its ownership")
	}

	// WHAT IT IS COMES BEFORE WHO OWNS IT. Returning early on ownership meant a
	// service-owned directory or a multiply-linked file at one of these five
	// names was accepted — by a function whose contract is that anything it
	// cannot account for is an error.
	//
	// A TARGET DECLARED A DIRECTORY MUST BE ONE, and one declared a file must be
	// a file: an entry that is not the KIND the caller named is not the thing
	// this run created, whichever way round it is.
	switch {
	case wantDir && !info.IsDir():
		return false, errors.New("is owned by root and is not a directory, so billet will " +
			"not give it to the service account")
	case !wantDir && !info.Mode().IsRegular():
		return false, errors.New("is owned by root and is not a regular file, so billet will " +
			"not give it to the service account")
	}

	// A DIRECTORY'S LINK COUNT IS ALWAYS ABOVE ONE — its own entry and every
	// subdirectory's `..` — and a directory cannot be hard-linked, so the check
	// this replaces would refuse every one of them for a hazard they cannot have.
	if !wantDir && st.Nlink > 1 {
		return false, fmt.Errorf("has %d links, so it is the same file as something outside "+
			"this directory and changing its owner would change that too", st.Nlink)
	}

	// AND IT MUST BE ON THE SAME FILESYSTEM AS THE DIRECTORY IT IS IN. A file
	// bind-mounted onto one of these names is an inode from somewhere else
	// wearing an authorized name, and a chown here would land on it.
	//
	// THE RESIDUAL, STATED: a bind mount from the SAME filesystem shares this
	// device and is not caught. Setting one up needs CAP_SYS_ADMIN, and anything
	// holding that can chown the target directly — so what remains is a blast
	// radius rather than an escalation.
	if dirStat != nil && st.Dev != dirStat.Dev {
		return false, errors.New("is on a different filesystem from the directory it is in, so " +
			"it is not state this host's preflight created")
	}

	// Only the CHANGE depends on who owns it — but "not root" is not "the
	// service". A ledger owned by an unrelated account is state billet cannot
	// account for, sitting where the server is about to open it.
	switch st.Uid {
	case 0:
	case uint32(uid):
		return false, nil
	default:
		return false, fmt.Errorf("is owned by uid %d, which is neither root nor the service "+
			"account (%d), so billet cannot account for it", st.Uid, uid)
	}

	if err := f.Chown(uid, gid); err != nil {
		return false, err
	}

	return true, nil
}

// Revalidate re-establishes that one unit is still what the plan decided about,
// immediately before acting on it.
//
// THE PLAN IS OLD BY THE TIME ANYTHING HAPPENS. `billet check` talks to GitHub
// in between, which takes as long as the network takes, and in that window a
// unit can be edited and daemon-reloaded into something else — a different
// ExecStart, a different account, a privilege prefix — or a drain can begin,
// turning the inactive unit the plan saw into one that is deactivating. Acting
// on the older answer would start a service billet never validated, or start
// one through a drain that is waiting for the jobs on this host.
//
// It cannot close the last instant; nothing short of holding systemd still can.
// It closes the seconds that `billet check` spends on the network, which is
// where the window actually is.
func (c *Converger) Revalidate(ctx context.Context, req UpRequest, want UnitPlan) error {
	plan, err := c.Plan(ctx, req)
	if err != nil {
		return fmt.Errorf("look at %s again before acting on it: %w", want.Name, err)
	}

	if len(plan.Refusals) > 0 {
		return fmt.Errorf("%s is no longer a unit billet will act on: %s",
			want.Name, plan.Refusals[0].Error())
	}

	for _, now := range plan.Units {
		if now.Name != want.Name {
			continue
		}

		if now != want {
			return fmt.Errorf("%s changed while billet was checking GitHub: it was %s and is "+
				"now %s. Nothing was done to it; run this again",
				want.Name, describe(want), describe(now))
		}

		return nil
	}

	return fmt.Errorf("%s is no longer in the plan billet made for this host; run this again",
		want.Name)
}

// describe renders what a plan intends for one unit, for a diagnostic.
func describe(u UnitPlan) string {
	switch {
	case u.Start && u.Enable:
		return "to be started and enabled"
	case u.Start:
		return "to be started"
	case u.Enable:
		return "already running, to be enabled"
	default:
		return "already running and enabled"
	}
}

// Identity resolves the service account for a caller that already planned.
func (c *Converger) Identity(req UpRequest) (int, int, error) {
	uid, gid, refusals := c.identity(req)
	if len(refusals) > 0 {
		return -1, -1, refusals[0]
	}

	return uid, gid, nil
}

// StartAndProve starts a unit and establishes that it stayed up.
//
// START, THEN PROVE, THEN THE CALLER ENABLES — never `enable --now`. Enabling
// first commits a unit to every future boot before anything has established it
// can run at all, and unwinding that is a second mutation on a host that has
// just failed one.
//
// The packaged units are Type=notify, so `systemctl start` does not return
// until billet has sent READY=1 or TimeoutStartSec has elapsed: its exit status
// IS the readiness signal. The window that follows exists for what readiness
// cannot cover — a service that reaches ready and then dies, which
// Restart=on-failure turns into a crash loop reading "active" at any instant.
func (c *Converger) StartAndProve(ctx context.Context, unit string) (string, error) {
	if _, err := c.inspector.run(ctx, c.inspector.systemctl, []string{"start", "--", unit}); err != nil {
		// A FAILED START IS NOT ALWAYS A BROKEN ONE. The host role can render the
		// server unit with an assertion that holds it deliberately: a host whose
		// ledger volume is mounted and proved but which has not been given a
		// deployment identity yet, where starting the server would MINT one and
		// silently fork the deployment. systemd reports that as an ordinary
		// non-zero start, so without asking, `local up` says only "exit status 1"
		// about a host that is behaving exactly as intended.
		//
		// This runs only on a path that has ALREADY failed, so it can add a
		// diagnosis but never refuse a host that would otherwise have started —
		// which is the direction a new refusal class here gets wrong.
		if c.startWasHeld(ctx, unit) {
			// NAMES THE MECHANISM, NOT A CAUSE. AssertResult=no proves that an
			// assertion failed, never WHICH one — this is a generic start path and
			// any unit may carry assertions billet did not write. Asserting the
			// prepare-only hold here would send someone off to copy control-plane
			// state onto a host whose real problem was something else entirely, so
			// the one cause billet knows about is offered as a possibility rather
			// than stated as the diagnosis.
			return "", fmt.Errorf(
				"start %s: refused by a unit ASSERTION rather than by the service failing — "+
					"systemd reports AssertResult=no, so ExecStart was never reached. "+
					"`systemctl cat %s` shows the unit's assertions and `journalctl -u %s` logs "+
					"which one failed. If this host was prepared "+
					"with billet_server_prepare_only, that hold is deliberate: place the state "+
					"it is meant to serve, then converge again with it false: %w",
				unit, unit, unit, err)
		}

		return "", fmt.Errorf("start %s: %w", unit, err)
	}

	if err := c.ProveStable(ctx, unit); err != nil {
		return "", err
	}

	return "ready, and still running after the settle window", nil
}

// startWasHeld reports whether systemd refused the start because a unit
// assertion failed, rather than because the service itself could not run.
//
// BEST EFFORT, AND IT FAILS TOWARDS SILENCE: any doubt — systemd cannot be
// asked, the property is absent, the answer is not the exact word — returns
// false and the caller reports the original error unchanged. Guessing "held"
// about a genuinely broken unit would tell an operator to go and copy state
// onto a host whose server is crashing for some entirely different reason.
func (c *Converger) startWasHeld(ctx context.Context, unit string) bool {
	props, err := c.inspector.properties(ctx, unit, "AssertResult")
	if err != nil {
		return false
	}

	// "no" is systemd's exact rendering of a failed assertion. An absent
	// property renders as "" here, which is not it.
	return first(props, "AssertResult") == "no"
}

// ProveStable establishes that a unit is holding the process it has.
//
// It exists separately from StartAndProve because a service that was ALREADY
// running when `up` arrived gets enabled too, and the sample the planner took
// is by then minutes old — `billet check` runs in between and talks to GitHub.
// Enabling on that sample would commit a crash loop to every future boot on the
// evidence of one instant that has since passed.
func (c *Converger) ProveStable(ctx context.Context, unit string) error {
	before, err := c.sample(ctx, unit)
	if err != nil {
		return err
	}

	c.stabilityWait(c.wait)

	after, err := c.sample(ctx, unit)
	if err != nil {
		return err
	}

	if before != after {
		return fmt.Errorf("%s did not hold the process it started: %s became %s. Its journal "+
			"(`journalctl -u %s`) says why; a service that starts and dies is a crash loop that "+
			"reads as active at any single instant", unit, before, after, unit)
	}
	if !after.up() {
		return fmt.Errorf("%s is %s. See `journalctl -u %s`", unit, after, unit)
	}

	return nil
}

// stability is the sample the host role compares across its own probe window.
type stability struct {
	active    string
	sub       string
	result    string
	mainPID   string
	restarts  string
	startedAt string
}

func (s stability) up() bool {
	return s.active == "active" && s.sub == "running" && s.result == "success" && s.mainPID != "0"
}

func (s stability) String() string {
	// THE START TIMESTAMP IS PART OF THE COMPARISON, not decoration. Without it
	// a service can stop and start again between two samples and come back to
	// the same state, substate, result, pid 0 and restart count — and an
	// inactive unit that started and exited looks exactly like one that never
	// moved.
	return fmt.Sprintf("%s/%s (result %s, pid %s, %s restarts, started %s)",
		s.active, s.sub, s.result, s.mainPID, s.restarts, s.startedAt)
}

// sample reads the properties whose CHANGE is what a restart looks like.
func (c *Converger) sample(ctx context.Context, unit string) (stability, error) {
	props, err := c.inspector.properties(ctx, unit,
		"ActiveState", "SubState", "Result", "MainPID", "NRestarts",
		"ExecMainStartTimestampMonotonic")
	if err != nil {
		return stability{}, fmt.Errorf("ask systemd about %s: %w", unit, err)
	}

	// EVERY FIELD MUST HAVE COME BACK. first() renders an absent property as an
	// empty string, so two truncated replies compare equal — and "these two
	// unreadable answers match" would be read as "nothing moved".
	for _, name := range []string{
		"ActiveState", "SubState", "Result", "MainPID", "NRestarts",
		"ExecMainStartTimestampMonotonic",
	} {
		if len(props[name]) != 1 {
			return stability{}, fmt.Errorf("systemd's answer for %s carried %d values for %s, "+
				"so billet cannot tell what that service is doing", unit, len(props[name]), name)
		}
	}

	return stability{
		active:    first(props, "ActiveState"),
		sub:       first(props, "SubState"),
		result:    first(props, "Result"),
		mainPID:   first(props, "MainPID"),
		restarts:  first(props, "NRestarts"),
		startedAt: first(props, "ExecMainStartTimestampMonotonic"),
	}, nil
}

// Snapshot is what a unit looks like right now, as one comparable string.
//
// It exists so a caller can prove that starting ONE service did not disturb
// another. billet refuses a unit that names the other one in its dependencies,
// but a transaction can reach further than that — a unit billet's own unit
// pulls in may itself conflict with the other service — and no practical amount
// of property-reading models what systemd will actually do. Comparing before
// and after does not need to: whatever the mechanism, a service that changed
// while billet was starting something else is a service billet disturbed.
func (c *Converger) Snapshot(ctx context.Context, unit string) (string, error) {
	s, err := c.sample(ctx, unit)
	if err != nil {
		return "", err
	}

	return s.String(), nil
}

// EnabledNow reports a unit's enablement state at this moment.
//
// The plan's answer is from before `billet check` talked to GitHub, and another
// operator may have enabled the unit in between. Enabling is idempotent, so
// success proves nothing about who did it — and rolling back an enablement this
// run did not perform would disable a service somebody else just committed.
//
// IT ANSWERS WITH THE STATE ITSELF, not a yes or no. Folding it to a boolean
// made `masked`, `static`, `linked`, an empty answer and every state systemd
// adds later mean what `disabled` means — permission to enable. Only the exact
// string is that, and the caller says so.
func (c *Converger) EnabledNow(ctx context.Context, unit string) (Enablement, error) {
	props, err := c.inspector.properties(ctx, unit, "UnitFileState")
	if err != nil {
		return Enablement{}, fmt.Errorf("ask systemd whether %s is enabled: %w", unit, err)
	}

	state := first(props, "UnitFileState")

	// EXACTLY TWO WORDS ARE CLASSIFIED, and everything else is Unknown rather
	// than "not enabled". `static`, `masked`, `linked`, `enabled-runtime`,
	// `indirect`, an empty answer and whatever systemd adds next are each a unit
	// whose enablement billet has no rule for — and `up` acts only on a definite
	// No, so landing them here is what makes it refuse instead of guess.
	switch state {
	case "enabled":
		return Enablement{Enabled: Yes, How: state}, nil
	case unitDisabled:
		return Enablement{Enabled: No, How: state}, nil
	default:
		return Enablement{Enabled: Unknown, How: state}, nil
	}
}

// unitDisabled is systemd's word for the one state `up` may act on.
const unitDisabled = "disabled"

// Enable commits a unit to future boots.
func (c *Converger) Enable(ctx context.Context, unit string) error {
	if _, err := c.inspector.run(ctx, c.inspector.systemctl, []string{"enable", "--", unit}); err != nil {
		return fmt.Errorf("enable %s: %w", unit, err)
	}

	return nil
}

// Enablement is whether a service will start itself at the next boot.
//
// A VERDICT AND A WORD, rather than the manager's raw string. systemd answers
// with one of a dozen words (`enabled`, `disabled`, `static`, `masked`,
// `linked`, `enabled-runtime`, `indirect`, …); launchd has no such string at all
// — a label is enabled when its plist is installed and its entry in the durable
// override database does not say `disabled`. Shared code comparing against the
// literal "disabled" is therefore code only systemd can satisfy, and a second
// backend would have to invent systemd's vocabulary to be understood.
//
// Unknown is the common case rather than the exotic one: every systemd state
// except exactly `enabled` and exactly `disabled` lands there, because those are
// the only two billet has a rule for. That is deliberate — `up` acts only on a
// definite No, so anything it cannot classify refuses instead of being guessed
// at, including a state a future systemd adds.
type Enablement struct {
	Enabled Tristate
	// How is the manager's own word, for a diagnostic an operator can act on.
	How string
}

// StopResult is what a service manager can say about a service it was asked to
// stop, in terms EVERY service manager can answer honestly.
//
// IT IS NOT systemd's VOCABULARY, deliberately. `ActiveState` and `Result` are
// systemd's words for systemd's states, and a second manager has no honest
// value for them: after a launchd `bootout` the service is simply GONE FROM ITS
// DOMAIN, which is a different fact from "inactive (success)". Rendering one as
// the other is not a translation, it is a fabricated proof — and it would be
// read as one by the code whose whole job is to refuse anything that is not
// proof.
// A NON-NIL ERROR NEVER ACCOMPANIES Gone: Yes. The two halves are one answer:
// a backend that proved the process gone reports no error, and a backend that
// returns an error has not proved anything. Callers act on the error first, so
// a `Yes` alongside one would be discarded — and a backend that did that would
// have its successful stops silently reported as failures.
type StopResult struct {
	// Gone is whether the service's process is PROVED gone.
	//
	// THREE-VALUED BECAUSE BOTH MANAGERS REALLY HAVE THE THIRD. An empty
	// systemd ActiveState and a launchctl reply billet cannot parse are both
	// "the manager did not tell us", which is uncertainty rather than absence.
	// A two-valued type loses exactly that arm, and it is the one a caller must
	// refuse on.
	Gone Tristate
	// How is the manager's own account of the ending, printed verbatim by the
	// caller. systemd fills it from ActiveState and Result; launchd from whether
	// the service is still in its domain and whether the pid it named is alive.
	How string
}

// StopAndProve stops a unit and establishes that it is actually gone.
//
// THE EXIT STATUS IS NOT THE ANSWER, which is why this is not a one-line
// wrapper. `systemctl stop` returns when the job it queued completes, and a unit
// whose main process ignored SIGTERM is killed at TimeoutStopSec — after which
// the unit is inactive and `failed`. A caller about to report "this host is
// down" needs the state the manager holds now, not the return code of a command.
func (c *Converger) StopAndProve(ctx context.Context, unit string) (StopResult, error) {
	if _, err := c.inspector.run(ctx, c.inspector.systemctl, []string{"stop", "--", unit}); err != nil {
		return StopResult{}, fmt.Errorf("stop %s: %w", unit, err)
	}

	props, err := c.inspector.properties(ctx, unit, "ActiveState", "SubState", "Result")
	if err != nil {
		return StopResult{}, fmt.Errorf("ask systemd about %s after stopping it: %w", unit, err)
	}

	var (
		active = first(props, "ActiveState")
		result = first(props, "Result")
	)

	// HOW IT ENDED, IN systemd's OWN WORDS, kept apart from the verdict. A unit
	// killed at TimeoutStopSec because it ignored SIGTERM is inactive AND
	// failed, and an operator being told "this host is down" should know which
	// of those it is.
	how := "is " + orUnknown(active)
	if result != "" && result != "success" {
		how += " (result " + result + ")"
	}

	// AN ALLOWLIST, NOT A DENYLIST, and the difference is what the unlisted
	// states are. `deactivating` means the unit is still stopping — its process
	// is alive — and refusing only `active` and `activating` accepted it, after
	// which this went on to stop the other unit and report the host down. An
	// empty answer is systemd not telling us, which is uncertainty rather than
	// absence, and a state a future systemd adds must not be read as proof.
	//
	// `failed` counts as stopped: nothing is running, which is the question here.
	// How it ended is reported separately in Result, because a unit killed at
	// TimeoutStopSec is inactive AND failed, and an operator being told their
	// host is down should know which.
	switch active {
	case "inactive", "failed":
		return StopResult{Gone: Yes, How: how}, nil

	// STATES THAT SAY A PROCESS IS THERE. `deactivating` is the dangerous one —
	// the unit is still stopping and its process is alive — and it is a definite
	// no rather than an uncertainty.
	case "active", "activating", "deactivating", "reloading":
		return StopResult{Gone: No, How: how}, fmt.Errorf("%s is %s after being stopped, so its "+
			"process is not proved gone and this host is not down", unit, active)

	case "":
		return StopResult{Gone: Unknown, How: how}, fmt.Errorf("systemd did not say what state "+
			"%s is in after stopping it, so billet cannot tell whether its process is gone", unit)

	default:
		// A STATE THIS BUILD DOES NOT KNOW PROVES NOTHING IN EITHER DIRECTION.
		// It is not evidence that a process remains, and reporting it as one
		// would be as invented as reporting it stopped. Both refuse; only this
		// one says honestly which it is.
		return StopResult{Gone: Unknown, How: how}, fmt.Errorf("billet does not recognise the "+
			"state %q that %s is in after being stopped, so its process is not proved gone and "+
			"this host is not down", active, unit)
	}
}

// Services names this backend's two services, server first.
//
// SHARED CODE MUST NOT KNOW THE UNIT NAMES. Every place the lifecycle commands
// talk about "the other service" — the bystander snapshot, the enablement
// comparison, the order `down` stops things in — needs a pair of identifiers,
// and reaching for deploy's systemd constants there quietly makes those commands
// systemd-only: a converger for another service manager would be handed
// `billet-server.service`, inspect a service that does not exist, find nothing,
// and report that nothing had changed. Asking the backend is what keeps the
// ORDER shared and the vocabulary local.
// The server is returned first, matching the order `up` starts them in.
func (c *Converger) Services() (string, string) {
	return deploy.ServerUnitName, deploy.NodeUnitName
}

// Running reports what each wanted service is executing right now.
//
// IT BELONGS TO THE BACKEND, not to a shared inspection. `down` refuses to act
// on an installation running a different build of billet, and the facts that
// refusal rests on — is it active, is the running process this build — are
// answered in a completely different way by each service manager. Getting them
// from a systemd inspector meant the refusal silently reported on systemd units
// on a host that has none.
func (c *Converger) Running(ctx context.Context, req UpRequest) ([]RunningFacts, error) {
	report, err := c.inspector.Inspect(ctx, req.ConfigPath, req.KeyPath)
	if err != nil {
		return nil, err
	}

	var facts []RunningFacts

	if req.WantServer {
		facts = append(facts, report.Server.Running())
	}

	if req.WantNode {
		facts = append(facts, report.Node.Running())
	}

	return facts, nil
}

// EnablementCmd renders the command an operator runs to see for themselves what
// billet just told them about enablement.
//
// A REFUSAL AN OPERATOR CANNOT ACT ON IS A DEAD END, so every one of them
// carries the command that resolves it — and that command belongs to the
// service manager rather than to billet, which makes it the backend's to name.
func (c *Converger) EnablementCmd(units ...string) string {
	return "systemctl is-enabled " + strings.Join(units, " ")
}

// DisableCmd renders how an operator undoes an enablement themselves.
func (c *Converger) DisableCmd(unit string) string {
	return "systemctl disable " + unit
}

// ManagerName is what billet calls this service manager in a sentence.
func (c *Converger) ManagerName() string { return "systemd" }

// CollateralNote explains how enabling ONE service can commit another, in this
// manager's own terms.
//
// IT IS THE BACKEND'S SENTENCE because the mechanism is: systemd has
// `[Install] Also=`, which writes links for a unit nobody named and is invisible
// in every property of the one that was. launchd has no such thing and would
// have a different sentence, or none. The shared command detects the collateral
// change the same way on both — by comparing before and after — and only the
// explanation differs.
func (c *Converger) CollateralNote() string {
	return "an `[Install] Also=` commits a unit to every future boot that nothing here has " +
		"checked a credential for"
}

// Disable undoes an Enable this run performed.
//
// ONLY WHAT THIS RUN DID. A failed `up` must not leave a unit committed to
// boot that nothing established can run — and must equally not disable one an
// operator had enabled before it arrived.
func (c *Converger) Disable(ctx context.Context, unit string) error {
	if _, err := c.inspector.run(ctx, c.inspector.systemctl, []string{"disable", "--", unit}); err != nil {
		return fmt.Errorf("disable %s: %w", unit, err)
	}

	return nil
}

// Contained reports whether path is inside dir — the same directory or
// something under it, by path COMPONENT rather than by string prefix.
//
// "/var/lib/billet-evil" has the prefix "/var/lib/billet" and is a different
// directory that no unit makes writable.
func Contained(dir, p string) bool {
	if dir == "" || p == "" {
		return false
	}

	dir = filepath.Clean(dir)
	p = filepath.Clean(p)

	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// sortedKeys makes a map's refusals deterministic, so an operator reading two
// runs sees the same list in the same order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}

// transactionRefusals covers what starting or stopping this unit would do to
// the OTHER one.
//
// A UNIT'S TRANSACTION REACHES BEYOND ITSELF, and nothing else billet checks can
// see it. `Conflicts=billet-node.service` on the server means `systemctl start
// billet-server.service` STOPS the node — a drain of a host holding jobs,
// performed by the command that promised never to touch a running service. And
// `Requires=billet-server.service` on the node means starting the node starts
// the server, which on a node-only config is a control plane billet never
// checked a credential for.
//
// The packaged units carry ordinary dependencies — sysinit.target, -.mount,
// network-online.target, Conflicts=shutdown.target — so the rule cannot be
// "declare nothing". It is that no billet unit may name ANOTHER billet unit:
// that is the whole of the hazard, and it leaves an operator free to add the
// dependencies their own host needs.
func transactionRefusals(s ServiceFacts) []Refusal {
	var refusals []Refusal

	for _, name := range sortedKeys(s.Transaction) {
		for _, named := range strings.Fields(s.Transaction[name]) {
			if named == s.Name || !isBilletUnit(named) {
				continue
			}

			refusals = append(refusals, Refusal{
				What: fmt.Sprintf("%s declares %s=%s, so acting on it would act on %s too",
					s.Name, name, named, named),
				Remedy: "restore the packaged unit; billet decides for itself which of its " +
					"services to start and in which order, and a unit that starts or stops " +
					"the other one takes that decision away — including the decision never " +
					"to restart a node that is holding jobs",
			})
		}
	}

	return refusals
}

// pullers are the properties that bring another unit INTO this one's start
// transaction. A unit reached this way runs its own conflicts, which is how a
// start becomes a stop of something else.
var pullers = []string{"Requires", "Requisite", "BindsTo", "Wants", "Upholds", "PartOf"}

// PulledUnits lists what starting this unit would pull in with it, so a caller
// can ask each of those what IT would do.
//
// ONE LEVEL, DELIBERATELY. The full closure is systemd's to compute through
// units billet has never heard of, and trying to reproduce it was wrong in four
// successive attempts. One level covers the shape anyone has actually
// constructed — billet's own unit naming an ordinary-looking helper that
// conflicts with the other service — and what it cannot cover is caught after
// the fact by comparing the other service before and after.
func (s ServiceFacts) PulledUnits() []string {
	seen := map[string]bool{}

	// EVERY KIND OF UNIT, not just services and targets. A .socket, .path or
	// .mount carries Conflicts= exactly as well, and filtering by suffix was an
	// arbitrary rule that would have to be right about which unit types can hold
	// a dependency — a question with no reason to have a short answer.
	var units []string
	for _, name := range pullers {
		for _, named := range strings.Fields(s.Transaction[name]) {
			if seen[named] || named == s.Name {
				continue
			}
			seen[named] = true
			units = append(units, named)
		}
	}

	sort.Strings(units)

	return units
}

// pulledRefusals asks each unit this one would pull in what IT would do to
// billet's other service.
//
// The answer is one level deep and says so. Two levels down is not covered by
// this, and is what the before-and-after comparison in `up` exists for — but a
// destroyed job is not undone by noticing it afterwards, so everything that CAN
// be refused in advance is.
func (c *Converger) pulledRefusals(ctx context.Context, s ServiceFacts) ([]Refusal, error) {
	var refusals []Refusal

	// EVERY RELATIONSHIP THE DIRECT CHECK REFUSES, asked of the pulled unit too —
	// including Triggers, which is how a pulled socket, path or timer activates
	// a service later, after any before-and-after comparison has finished.
	asked := append([]string{"Conflicts", "Triggers"}, pullers...)
	asked = append(asked, "OnFailure", "OnSuccess", "JoinsNamespaceOf")

	for _, pulled := range s.PulledUnits() {
		props, err := c.inspector.properties(ctx, pulled, append(asked, "LoadState")...)
		if err != nil {
			// A unit systemd cannot be asked about is one billet cannot clear.
			return nil, fmt.Errorf("ask systemd what %s would do, which %s pulls in: %w",
				pulled, s.Name, err)
		}

		// MEASURED: systemd reports each of these even for a unit that does not
		// exist, answering an empty value alongside LoadState=not-found. So a
		// property that did not come back at all is a reply billet could not
		// read, and reading it as "declares nothing" is the same unknown-collapse
		// this package keeps having to remove.
		for _, prop := range append(asked, "LoadState") {
			if len(props[prop]) != 1 {
				return nil, fmt.Errorf("systemd's answer about %s, which %s pulls in, carried "+
					"%d values for %s", pulled, s.Name, len(props[prop]), prop)
			}
		}

		for _, prop := range asked {
			for _, named := range strings.Fields(first(props, prop)) {
				if !isBilletUnit(named) || named == s.Name {
					continue
				}

				verb := "start"
				if prop == "Conflicts" {
					verb = "STOP"
				}
				if prop == "Triggers" {
					verb = "later activate"
				}

				refusals = append(refusals, Refusal{
					What: fmt.Sprintf("starting %s pulls in %s, which declares %s=%s — so the "+
						"start would %s %s", s.Name, pulled, prop, named, verb, named),
					Remedy: "restore the packaged unit, or remove that dependency. billet decides " +
						"for itself which of its services run and in which order; a start " +
						"that stops the other one destroys the jobs it was holding, and a " +
						"start that starts it brings up a control plane nothing here has " +
						"checked a credential for",
				})
			}
		}
	}

	return refusals, nil
}

// isBilletUnit reports whether a unit name is one of billet's own.
//
// INSTANCES COUNT. billet ships no template units, but a host that has one
// would have `billet-server@somewhere.service` running `billet server` — and
// exact-name matching would clear a dependency on it while the before-and-after
// comparison, which samples `billet-server.service`, never saw it either.
func isBilletUnit(name string) bool {
	for _, own := range []string{deploy.ServerUnitName, deploy.NodeUnitName} {
		if name == own {
			return true
		}

		base, suffix, ok := strings.Cut(own, ".")
		if ok && strings.HasPrefix(name, base+"@") && strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}

	return false
}

// numericID parses an account id the way the SYSCALL will read it.
//
// uid_t IS UNSIGNED AND 32 BITS. "4294967295" is a perfectly ordinary positive
// int on a 64-bit platform and becomes (uid_t)-1 at the boundary — the same
// "leave this as it is" sentinel that a negative id would be, which would make
// every ownership repair a silent success that changed nothing. Parsing at the
// width the kernel uses is the only way to see that.
func numericID(v string) (int, error) {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return -1, err
	}

	if n == math.MaxUint32 {
		return -1, fmt.Errorf("%s is the chown sentinel for \"leave this unchanged\"", v)
	}

	return int(n), nil
}
