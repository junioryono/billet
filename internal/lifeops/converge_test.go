package lifeops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/junioryono/billet/deploy"
)

const probeConfig = "/etc/billet/billet.yaml"

// unitExec renders systemd's own ExecStart answer for a unit that runs the
// given binary in the given role. The tests build it rather than hand-writing
// it, because the argv the converger requires is derived from these same parts
// and a fixture that drifted would be testing the fixture.
func unitExec(path, role, cfg string) string {
	return "{ path=" + path + " ; argv[]=" + path + " " + role + " --config " + cfg +
		" ; ignore_errors=no ; start_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
}

// unitExecEx is systemd's EXTENDED rendering, which is the same answer plus the
// command's prefix flags — the one field that decides whether a unit naming an
// unprivileged account actually runs as one.
func unitExecEx(path, role, cfg, flags string) string {
	return "{ path=" + path + " ; argv[]=" + path + " " + role + " --config " + cfg +
		" ; flags=" + flags + " ; start_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
}

// healthyServer and healthyNode are units with nothing wrong with them: the
// running binary, the right role, the right identity, the directories the
// config asks for. Every refusal test starts from one of these and breaks one
// thing, so what the test names is the only difference from a host that works.
func healthyServer(t *testing.T, h *host, over map[string]string) string {
	t.Helper()

	fields := map[string]string{
		"ExecStart":      unitExec(h.selfPath, "server", probeConfig),
		"ExecStartEx":    unitExecEx(h.selfPath, "server", probeConfig, ""),
		"Names":          deploy.ServerUnitName,
		"User":           "billet",
		"Group":          "billet",
		"StateDirectory": "billet/server",
	}
	maps.Copy(fields, over)

	return measured(fields)
}

func healthyNode(t *testing.T, h *host, over map[string]string) string {
	t.Helper()

	fields := map[string]string{
		"ExecStart":        unitExec(h.selfPath, "node", probeConfig),
		"ExecStartEx":      unitExecEx(h.selfPath, "node", probeConfig, ""),
		"Names":            deploy.NodeUnitName,
		"User":             "root",
		"Group":            "root",
		"StateDirectory":   "billet/node",
		"RuntimeDirectory": "billet/locks",
	}
	maps.Copy(fields, over)

	return measured(fields)
}

// pulledUnit renders what systemd answers about a unit another one pulls in.
//
// EVERY PROPERTY, because billet requires each to come back: measured on
// systemd 255, `systemctl show` reports all of these even for a unit that does
// not exist, so a property missing from the reply means the reply was truncated
// rather than that the unit declares nothing.
func pulledUnit(over map[string]string) string {
	fields := map[string]string{
		"LoadState": "loaded", "Conflicts": "", "Triggers": "",
		"Requires": "", "Requisite": "", "BindsTo": "", "Wants": "",
		"Upholds": "", "PartOf": "", "OnFailure": "", "OnSuccess": "",
		"JoinsNamespaceOf": "",
	}
	maps.Copy(fields, over)

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(fields[k])
		b.WriteString("\n")
	}

	return b.String()
}

func healthyHost(t *testing.T, h *host) *answers {
	t.Helper()

	return &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, nil),
		deploy.NodeUnitName:   healthyNode(t, h, nil),
	}}
}

// converger builds a Converger over the fixture host, with every operation a
// test cannot really perform replaced.
func (h *host) converger(t *testing.T, a *answers) (*Converger, *hostOps) {
	t.Helper()

	ops := &hostOps{
		users:  map[string]*user.User{"billet": {Uid: "990", Username: "billet"}},
		groups: map[string]*user.Group{"billet": {Gid: "991", Name: "billet"}},
		files:  map[string]fs.FileInfo{},
	}

	base := []ConvergeOption{
		WithStabilityWait(time.Millisecond),
		withHostSeams(ops.lookupUser, ops.lookupGroup, ops.open, ops.root, ops.sleep),
	}

	return NewConverger(h.inspector(a), base...), ops
}

// op is one privileged operation, with the arguments it was given.
//
// THE ARGUMENTS ARE THE POINT. A test that counts calls passes when both land
// on the same file, or use the wrong identity, or set a mode nobody asked for —
// which is the whole content of what this code does.
type op struct {
	kind string
	path string
	uid  int
	gid  int
	mode fs.FileMode
}

// hostOps records the privileged operations that would have happened.
type hostOps struct {
	ops    []op
	users  map[string]*user.User
	groups map[string]*user.Group

	// files answers the no-follow open. A path with no entry is opened for real,
	// so the ordinary case compares against a genuine stat.
	files    map[string]fs.FileInfo
	openErr  map[string]error
	userErr  error
	chmodErr error
	chownErr error

	// tree is what the state repair sees, owners is who owns each entry, and
	// links stages the one fact no temporary directory can: a hard link, which
	// makes an entry the same inode as something outside the directory.
	tree    fstest.MapFS
	owners  map[string]uint32
	links   map[string]uint64
	devices map[string]uint64
	rootEr  error

	// waited records the stability windows that were actually awaited, and
	// released gates what the runner answers afterwards.
	waited   []time.Duration
	released bool
}

func (o *hostOps) record(kind, path string, uid, gid int, mode fs.FileMode) {
	o.ops = append(o.ops, op{kind: kind, path: path, uid: uid, gid: gid, mode: mode})
}

func (o *hostOps) kinds() []string {
	out := make([]string, 0, len(o.ops))
	for _, p := range o.ops {
		out = append(out, p.kind+" "+p.path)
	}

	return out
}

func (o *hostOps) sleep(d time.Duration) {
	o.waited = append(o.waited, d)
	o.released = true
}

// lookupUserErr makes the account database UNREADABLE rather than empty, which
// is a different answer: an NSS failure is not proof the account is missing.
func (o *hostOps) lookupUserErr(err error) { o.userErr = err }

func (o *hostOps) lookupUser(name string) (*user.User, error) {
	if o.userErr != nil {
		return nil, o.userErr
	}

	u, ok := o.users[name]
	if !ok {
		return nil, user.UnknownUserError(name)
	}

	return u, nil
}

func (o *hostOps) lookupGroup(name string) (*user.Group, error) {
	g, ok := o.groups[name]
	if !ok {
		return nil, user.UnknownGroupError(name)
	}

	return g, nil
}

func (o *hostOps) open(path string) (ownedFile, error) {
	if err, ok := o.openErr[path]; ok {
		return nil, err
	}

	info, ok := o.files[path]
	if !ok {
		var err error
		if info, err = os.Stat(path); err != nil {
			return nil, err
		}
	}

	// THE REAL STAT, UNWRAPPED. os.SameFile type-asserts the concrete FileInfo
	// the os package returns, so wrapping it to adjust a field makes that
	// comparison answer "different file" for every input — and that comparison is
	// what several of these tests are about. A fixture that needs a different
	// link count makes a real hard link instead.
	return &fakeFile{ops: o, path: path, info: info}, nil
}

func (o *hostOps) root(string) (rootFS, error) {
	if o.rootEr != nil {
		return nil, o.rootEr
	}

	return &fakeRoot{ops: o}, nil
}

type fakeFile struct {
	ops  *hostOps
	path string
	info fs.FileInfo
	kind string
}

func (f *fakeFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *fakeFile) Close() error               { return nil }

func (f *fakeFile) Chown(uid, gid int) error {
	kind := f.kind
	if kind == "" {
		kind = "chown"
	}
	f.ops.record(kind, f.path, uid, gid, 0)

	return f.ops.chownErr
}

func (f *fakeFile) Chmod(mode fs.FileMode) error {
	f.ops.record("chmod", f.path, -1, -1, mode)

	return f.ops.chmodErr
}

// fakeRoot stands in for an opened directory. It answers ownership from a map,
// which no temporary directory could: every file a test creates is owned by
// whoever runs the test, and the property under test is what happens to files
// owned by ROOT.
type fakeRoot struct{ ops *hostOps }

func (r *fakeRoot) Close() error { return nil }

func (r *fakeRoot) OpenFile(name string, _ int, _ fs.FileMode) (ownedFile, error) {
	if err, ok := r.ops.openErr[name]; ok {
		return nil, err
	}

	info, err := fs.Stat(r.ops.tree, name)
	if err != nil {
		return nil, err
	}

	links := uint64(1)
	if n, ok := r.ops.links[name]; ok {
		links = n
	}

	dev := uint64(64)
	if d, ok := r.ops.devices[name]; ok {
		dev = d
	}

	return &fakeFile{
		ops:  r.ops,
		path: name,
		info: ownedInfo{FileInfo: info, uid: r.ops.owners[name], links: links, dev: dev},
		kind: "lchown",
	}, nil
}

// ownedInfo overrides the facts a fixture needs to control while KEEPING the
// rest of the real stat. Replacing it wholesale broke os.SameFile, which needs
// the device and inode — and that comparison is the thing several of these
// tests are about.
type ownedInfo struct {
	fs.FileInfo
	uid   uint32
	links uint64
	dev   uint64
}

func (o ownedInfo) Sys() any {
	st, ok := o.FileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return statFor(o.uid, o.links, o.dev)
	}

	copied := *st
	copied.Uid = o.uid
	assign(&copied.Nlink, o.links)

	return &copied
}

func upRequest() UpRequest {
	return UpRequest{
		ConfigPath:   probeConfig,
		ServiceUser:  "billet",
		ServiceGroup: "billet",
		WantServer:   true,
		WantNode:     true,
	}
}

func refusalText(refusals []Refusal) []string {
	out := make([]string, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, r.What+" || "+r.Remedy)
	}

	return out
}

// A HEALTHY HOST IS NOT REFUSED. Every test below breaks one thing and asserts
// a refusal; without this one, a converger that refused everything would pass
// all of them.
func TestPlanAcceptsAPreparedHost(t *testing.T) {
	h := newHost(t)
	c, _ := h.converger(t, healthyHost(t, h))

	req := upRequest()
	req.ServerStateDir = "/var/lib/billet/server"
	req.NodeStateDir = "/var/lib/billet/node"
	req.NodeLockDir = "/run/billet/locks"

	plan, err := c.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("a prepared host was refused: %v", refusalText(plan.Refusals))
	}
	if plan.ServerState != "/var/lib/billet/server" {
		t.Errorf("the plan did not resolve the server's state directory from the unit: %q",
			plan.ServerState)
	}
}

// EVERY REASON AT ONCE. An operator who has to re-run a command to find the
// next thing wrong with their host is paying for a diagnostic that already knew
// — so a plan collects refusals instead of returning the first.
func TestPlanCollectsEveryRefusalRatherThanTheFirst(t *testing.T) {
	h := newHost(t)
	elsewhere := h.file(t, "billet-elsewhere", "not the running binary")

	// THE SERVER'S PROBLEMS ARE ITS OWN and the node's are the node's: the
	// server names the running binary so its only complaints are the mask and
	// the drop-in, while the node is otherwise healthy and would run something
	// else entirely. Sharing a reason between them would let a plan that
	// stopped after the first unit still satisfy every assertion below.
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{
			"LoadState":   "masked",
			"DropInPaths": "/etc/systemd/system/billet-server.service.d/z.conf",
		}),
		deploy.NodeUnitName: healthyNode(t, h, map[string]string{
			"ExecStart": unitExec(elsewhere, "node", probeConfig),
		}),
	}}

	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	joined := strings.Join(refusalText(plan.Refusals), "\n")
	for _, want := range []string{"masked", "drop-in", "not this billet"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the plan does not refuse for %q:\n%s", want, joined)
		}
	}

	// BOTH UNITS ARE REACHED. Asserting only the reasons let a plan that
	// stopped at the first unit pass, because the server happened to produce a
	// reason the node was supposed to contribute.
	for _, unit := range []string{deploy.ServerUnitName, deploy.NodeUnitName} {
		if !strings.Contains(joined, unit) {
			t.Errorf("nothing was refused for %s, so the plan stopped early:\n%s", unit, joined)
		}
	}

	// AND EVERY REFUSAL CARRIES ITS REMEDY. A refusal an operator cannot act on
	// is a dead end, and this is the command that tells them what to do.
	for _, r := range plan.Refusals {
		if r.Remedy == "" {
			t.Errorf("the refusal %q carries no remedy", r.What)
		}
	}

	// AND NOTHING IS PLANNED FOR A UNIT THAT WAS REFUSED.
	if len(plan.Units) > 0 {
		t.Errorf("a refused host still produced a plan of work: %+v", plan.Units)
	}
}

// EACH STATE ITS OWN REFUSAL, so an operator is told which one they are in.
func TestPlanRefusesEachUnusableUnitState(t *testing.T) {
	cases := []struct {
		name string
		over map[string]string
		want string
	}{
		{"absent", map[string]string{"LoadState": "not-found"}, "not installed"},
		{"masked", map[string]string{"LoadState": "masked"}, "masked"},
		{"linked", map[string]string{"UnitFileState": "linked"}, "link to a unit file outside"},
		{"pending reload", map[string]string{"NeedDaemonReload": "yes"}, "changed on disk"},
		{"drop-ins", map[string]string{
			"DropInPaths": "/etc/systemd/system/billet-server.service.d/z.conf",
		}, "drop-in"},

		// A unit whose readiness protocol is not notify makes `up`'s central
		// claim untrue: with Type=exec, start returns on execve and a process
		// that dies a moment later was already called ready.
		{"legacy readiness", map[string]string{"Type": "exec"}, "rather than Type=notify"},
		{"unknown readiness", map[string]string{"Type": "REMOVE"}, "rather than Type=notify"},

		// The server is deliberately unprivileged; a unit edited to run it as
		// root is not one billet starts on an operator's behalf.
		{"server as root", map[string]string{"User": "root", "Group": "root"}, "runs as root:root"},
		{"no identity", map[string]string{"User": "REMOVE"}, "(no answer)"},

		// A transition is not a stopped service, and `deactivating` is a drain.
		{"draining", map[string]string{"ActiveState": "deactivating"}, "transition is already in progress"},
		{"activating", map[string]string{"ActiveState": "activating"}, "transition is already in progress"},
		{"unreadable state", map[string]string{"ActiveState": "sideways"}, "does not recognise"},

		// Enablement that expires at the next reboot is not enablement.
		{"runtime enablement", map[string]string{"UnitFileState": "enabled-runtime"}, "until the next reboot"},
		{"unreadable enablement", map[string]string{"UnitFileState": "wat"}, "does not recognise"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, tc.over),
				deploy.NodeUnitName:   healthyNode(t, h, nil),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("a %s unit is not refused by name:\n%s", tc.name, joined)
			}

			// AND THE UNIT IS NOT PLANNED FOR. A refusal that still leaves work
			// scheduled against the unit is a refusal in name only.
			for _, u := range plan.Units {
				if u.Name == deploy.ServerUnitName {
					t.Errorf("a %s unit was planned for anyway: %+v", tc.name, u)
				}
			}
		})
	}
}

// WHICH ROLE, NOT ONLY WHICH BINARY. The executable-identity check proves the
// unit runs this build; it says nothing about what that build is told to do. A
// unit named billet-node.service whose command line says `server` starts a
// control plane — and a node-only config skips the GitHub proof entirely, so
// that control plane would come up on an organization nothing verified.
func TestPlanRefusesAUnitThatWouldRunTheWrongCommand(t *testing.T) {
	cases := []struct {
		name string
		argv string
	}{
		{"the other role", unitExec("SELF", "server", probeConfig)},
		{"another config", unitExec("SELF", "node", "/tmp/somebody-elses.yaml")},
		{"extra arguments", "{ path=SELF ; argv[]=SELF node --config " + probeConfig +
			" --authorize ; ignore_errors=no }"},
		{"no command line at all", "{ path=SELF ; ignore_errors=no }"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			exec := strings.ReplaceAll(tc.argv, "SELF", h.selfPath)

			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, nil),
				deploy.NodeUnitName:   healthyNode(t, h, map[string]string{"ExecStart": exec}),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, "would run") {
				t.Fatalf("a node unit running %q was accepted:\n%s", tc.name, joined)
			}
			// It names what it wanted, so the operator can see the difference.
			if !strings.Contains(joined, "node --config "+probeConfig) {
				t.Errorf("the refusal does not say what it expected:\n%s", joined)
			}
			// AND IT DOES NOT TELL AN OPERATOR TO BREAK AN UPGRADE. The host
			// role's transaction installs probe units whose command line
			// deliberately differs, and "restore the packaged unit" is how a
			// half-finished upgrade becomes an unrecoverable one.
			if !strings.Contains(joined, "--upgrade-probe") {
				t.Errorf("the remedy does not account for an upgrade in progress:\n%s", joined)
			}
			for _, u := range plan.Units {
				if u.Name == deploy.NodeUnitName {
					t.Errorf("the unit was planned for anyway: %+v", u)
				}
			}
		})
	}
}

// A PREFIX IS NOT CONTAINMENT, and the unit's own StateDirectory is the
// authority on where a role may write. Under ProtectSystem=strict everything
// else is read-only, so a config naming a sibling directory is a service that
// starts and fails on its first write.
func TestPlanRefusesStateTheUnitCannotWrite(t *testing.T) {
	cases := []struct {
		name, dir, want string
	}{
		{"a sibling that shares a prefix", "/var/lib/billet-evil", "can only write"},
		{"the other role's directory", "/var/lib/billet/node", "can only write"},
		{"a home directory", "/home/ci/billet", "can only write"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			c, _ := h.converger(t, healthyHost(t, h))

			req := upRequest()
			req.ServerStateDir = tc.dir

			plan, err := c.Plan(t.Context(), req)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("%s was accepted as a state directory:\n%s", tc.dir, joined)
			}
			if !strings.Contains(joined, "/var/lib/billet/server") {
				t.Errorf("the refusal does not name the directory the unit does make writable:\n%s", joined)
			}
		})
	}
}

// AND A UNIT THAT DECLARES NO DIRECTORY IS UNCERTAINTY, not permission.
func TestPlanRefusesAUnitThatDeclaresNoStateDirectory(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{"StateDirectory": "REMOVE"}),
		deploy.NodeUnitName:   healthyNode(t, h, nil),
	}}
	c, _ := h.converger(t, a)

	req := upRequest()
	req.ServerStateDir = "/var/lib/billet/server"

	plan, err := c.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(strings.Join(refusalText(plan.Refusals), "\n"), "declares no directory") {
		t.Fatalf("a unit that declares no state directory was accepted:\n%v",
			refusalText(plan.Refusals))
	}
}

// A HOST WITH NO SERVICE ACCOUNT IS TOLD WHERE THE ACCOUNT COMES FROM. Creating
// it here would put a second implementation in charge of whether an existing
// account named "billet" may hold an organization's credentials.
func TestPlanRefusesAHostWithNoServiceAccount(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))
	delete(ops.users, "billet")

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	joined := strings.Join(refusalText(plan.Refusals), "\n")
	if !strings.Contains(joined, "no \"billet\" account") {
		t.Fatalf("a host with no service account is not refused:\n%s", joined)
	}
	if !strings.Contains(joined, "useradd") {
		t.Errorf("the refusal does not say how to create it:\n%s", joined)
	}
	// AND NO OWNERSHIP IS PLANNED, because there is no identity to chown to.
	if len(plan.Ownership) > 0 {
		t.Errorf("ownership was planned against an account that does not exist: %v", plan.Ownership)
	}
}

// A LOOKUP THAT FAILED IS NOT AN ACCOUNT THAT IS MISSING. "Create the account"
// is the wrong remedy on a host that already has one and could not be asked.
func TestPlanSeparatesAnUnreadableAccountFromAnAbsentOne(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))
	ops.users = nil
	ops.lookupUserErr(errors.New("nss: directory unavailable"))

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	joined := strings.Join(refusalText(plan.Refusals), "\n")
	if !strings.Contains(joined, "could not tell") {
		t.Fatalf("an unreadable account database was reported as an absent account:\n%s", joined)
	}
	if strings.Contains(joined, "useradd") {
		t.Errorf("billet recommended creating an account it could not prove was missing:\n%s", joined)
	}
}

// A RUNNING SERVICE IS LEFT ALONE. A restart is a drain, and a drain destroys
// the jobs the host is holding.
func TestPlanNeverRestartsARunningService(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, nil),
		deploy.NodeUnitName: healthyNode(t, h, map[string]string{
			"ActiveState": "inactive", "SubState": "dead",
			"UnitFileState": "disabled", "MainPID": "0",
		}),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("a healthy host was refused: %v", refusalText(plan.Refusals))
	}
	if len(plan.Units) != 2 {
		t.Fatalf("planned %d units, want 2", len(plan.Units))
	}

	if plan.Units[0].Start || plan.Units[0].Enable {
		t.Errorf("an already running and enabled service was planned for change: %+v", plan.Units[0])
	}
	if !plan.Units[1].Start || !plan.Units[1].Enable {
		t.Errorf("a stopped, disabled service was not planned to start and enable: %+v", plan.Units[1])
	}
}

// THE SERVER IS PLANNED BEFORE THE NODE, which is the ordering the install
// requires: a node that starts first has nothing to register with.
func TestPlanOrdersTheServerBeforeTheNode(t *testing.T) {
	h := newHost(t)
	stopped := map[string]string{
		"ActiveState": "inactive", "SubState": "dead",
		"UnitFileState": "disabled", "MainPID": "0",
	}

	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, stopped),
		deploy.NodeUnitName:   healthyNode(t, h, stopped),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Units) != 2 || plan.Units[0].Name != deploy.ServerUnitName {
		t.Fatalf("the server is not first in the plan: %+v", plan.Units)
	}
}

// A SERVICE THAT STARTS AND DIES IS NOT UP. Restart=on-failure turns that into
// a crash loop reading "active" at any single instant, which is why readiness
// alone cannot be the answer.
//
// THE SECOND SAMPLE IS GATED ON THE WAIT. The fixture keeps answering the first
// sample until the stability window is actually awaited, so a converger that
// dropped the wait would compare one instant against itself and call a crash
// loop stable — which is exactly the mutation this must not survive.
func TestStartAndProveRefusesAServiceThatDoesNotHoldItsProcess(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{}}

	c, ops := h.converger(t, a)
	c.inspector.run = func(_ context.Context, _ string, args []string) ([]byte, error) {
		a.calls = append(a.calls, args)
		if args[0] != "show" {
			return nil, nil
		}
		if !ops.released {
			return []byte(measured(map[string]string{"MainPID": "1000", "NRestarts": "0"})), nil
		}

		return []byte(measured(map[string]string{"MainPID": "1200", "NRestarts": "1"})), nil
	}

	_, err := c.StartAndProve(t.Context(), deploy.ServerUnitName)
	if err == nil {
		t.Fatal("a service that replaced its process was reported as up")
	}
	for _, want := range []string{"did not hold the process", "journalctl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}

	// AND THE WINDOW WAS THE ONE THAT WAS ASKED FOR. A wait of zero is not a
	// window, and a converger that waited a different amount than it was
	// configured for is not observing what the operator set.
	if len(ops.waited) != 1 || ops.waited[0] != time.Millisecond {
		t.Errorf("the stability window awaited was %v, want one wait of 1ms", ops.waited)
	}
}

// AND A STABLE ONE IS ACCEPTED — the pair, because a check that always failed
// would satisfy the test above on its own.
func TestStartAndProveAcceptsAServiceThatHolds(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{deploy.ServerUnitName: measured(nil)}}
	c, _ := h.converger(t, a)

	if _, err := c.StartAndProve(t.Context(), deploy.ServerUnitName); err != nil {
		t.Fatalf("a stable service was rejected: %v", err)
	}

	// It STARTED it, rather than only looking at it.
	var started bool
	for _, call := range a.calls {
		// The `--` is part of the contract, not noise: a unit name can begin
		// with a dash, and one of them is on every Linux host.
		if len(call) == 3 && call[0] == "start" && call[1] == "--" &&
			call[2] == deploy.ServerUnitName {
			started = true
		}
	}
	if !started {
		t.Errorf("the unit was never started: %v", a.calls)
	}
}

// AN ASSERTION-REFUSED START IS DIAGNOSED, AND ONLY THAT. The host role can render the
// server unit with an assertion that deliberately refuses the start: a control
// plane whose ledger volume is mounted and proved but which has no deployment
// identity yet, where starting would MINT one and fork the deployment. systemd
// reports that as an ordinary non-zero start, so an operator running
// `billet local up` would otherwise see only "exit status 1" about a host doing
// exactly what it was told.
//
// The three cases are one test on purpose: the diagnosis must appear when the
// assertion failed, and must NOT appear when the service is simply broken or
// when systemd could not be asked — telling someone to go and copy state onto a
// host whose server is crashing for an unrelated reason is worse than the bare
// error it replaced.
func TestStartAndProveDistinguishesAHeldUnitFromABrokenOne(t *testing.T) {
	startFailed := errors.New("Job for billet-server.service failed")

	for _, tc := range []struct {
		name       string
		assertResp string
		assertErr  error
		wantHeld   bool
	}{
		{name: "held by its assertion", assertResp: "AssertResult=no", wantHeld: true},
		{name: "genuinely broken", assertResp: "AssertResult=yes"},
		{name: "systemd could not be asked", assertErr: errors.New("no manager")},
		{name: "the property came back absent", assertResp: ""},
		// systemd renders this as exactly "no"; anything else is not it, and
		// guessing would send an operator after the wrong problem.
		{name: "an answer that is not the word", assertResp: "AssertResult=No"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{}}
			c, _ := h.converger(t, a)

			var askedFor []string
			c.inspector.run = func(_ context.Context, _ string, args []string) ([]byte, error) {
				a.calls = append(a.calls, args)
				if args[0] == "start" {
					return nil, startFailed
				}
				// THE FAKE ANSWERS ONLY WHAT IT WAS ASKED. A fixture that
				// returned its whole reply would keep passing after the
				// production code stopped asking for AssertResult.
				for _, arg := range args {
					if strings.HasPrefix(arg, "--property=") {
						askedFor = append(askedFor, strings.TrimPrefix(arg, "--property="))
					}
				}
				if tc.assertErr != nil {
					return nil, tc.assertErr
				}

				return []byte(tc.assertResp), nil
			}

			_, err := c.StartAndProve(t.Context(), deploy.ServerUnitName)
			if err == nil {
				t.Fatal("a failed start was reported as success")
			}

			// The ORIGINAL failure survives in every case: a diagnosis that
			// replaced the cause would cost the operator what systemd said.
			if !errors.Is(err, startFailed) {
				t.Errorf("the underlying start failure was lost: %v", err)
			}

			held := strings.Contains(err.Error(), "AssertResult=no")
			if held != tc.wantHeld {
				t.Fatalf("held-diagnosis = %v, want %v: %v", held, tc.wantHeld, err)
			}

			if !tc.wantHeld {
				return
			}
			// The remedy has to name the thing that releases it, or the
			// diagnosis is just a nicer way of being stuck.
			if !strings.Contains(err.Error(), "billet_server_prepare_only") {
				t.Errorf("the diagnosis does not name the flag that releases the hold: %v", err)
			}
			// AND IT MUST NOT CLAIM TO KNOW WHICH ASSERTION FAILED. AssertResult=no
			// proves only that one did, and this is a generic start path: any unit
			// may carry assertions billet did not write. Stating the prepare-only
			// hold as the cause would send someone to copy control-plane state onto
			// a host whose real problem was something else, so the message must
			// offer it conditionally and point at what names the real directive.
			if strings.Contains(err.Error(), "the unit is HELD") {
				t.Errorf("the diagnosis states a cause it cannot know: %v", err)
			}
			if !strings.Contains(err.Error(), "journalctl") {
				t.Errorf("the diagnosis does not say how to find the actual directive: %v", err)
			}
			// THE COMMAND IT RECOMMENDS MUST BE ONE THAT EXISTS. An earlier version
			// suggested `systemctl show -p AssertPathExists`, a property rendering
			// nobody had measured -- the code only ever asks for AssertResult.
			// Without this, reverting to that invented command leaves the test green.
			if !strings.Contains(err.Error(), "systemctl cat") {
				t.Errorf("the diagnosis does not recommend systemctl cat: %v", err)
			}
			if strings.Contains(err.Error(), "-p AssertPathExists") {
				t.Errorf("the diagnosis recommends a property rendering nobody measured: %v", err)
			}
			if len(askedFor) != 1 || askedFor[0] != "AssertResult" {
				t.Errorf("asked systemd for %v, want exactly [AssertResult]", askedFor)
			}
		})
	}
}

// PROVING A RUNNING SERVICE DOES NOT START IT. The already-running case exists
// so a service can be proved stable immediately before it is enabled; if that
// proof issued a start, it would be a restart of a host holding jobs.
func TestProveStableNeverStartsAnything(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{deploy.ServerUnitName: measured(nil)}}
	c, _ := h.converger(t, a)

	if err := c.ProveStable(t.Context(), deploy.ServerUnitName); err != nil {
		t.Fatalf("a stable service was rejected: %v", err)
	}

	for _, call := range a.calls {
		if call[0] == "start" || call[0] == "restart" {
			t.Fatalf("proving stability ran %v on a service that was already running", call)
		}
	}
}

// A FAILED START IS REPORTED, not swallowed into the stability window.
func TestStartAndProveReportsAFailedStart(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{deploy.ServerUnitName: measured(nil)}, fail: os.ErrPermission}
	c, _ := h.converger(t, a)

	_, err := c.StartAndProve(t.Context(), deploy.ServerUnitName)
	if err == nil {
		t.Fatal("a start that failed was reported as up")
	}
	if !strings.Contains(err.Error(), "start "+deploy.ServerUnitName) {
		t.Errorf("the failure does not name the step: %v", err)
	}
}

// OWNERSHIP IS THE NARROW SET whose absence stops a service starting, and the
// modes are ENFORCED rather than requested: a creation mode is reduced by the
// umask, and the server unit itself runs UMask=0077.
func TestOwnershipCoversTheConfigAndKeyOnly(t *testing.T) {
	h := newHost(t)
	cfg := h.file(t, "billet.yaml", "server:\n")
	key := h.file(t, "app-private-key.pem", "-----BEGIN-----\n")

	c, ops := h.converger(t, healthyHost(t, h))

	req := upRequest()
	req.ConfigPath = cfg
	req.KeyPath = key

	plan, err := c.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Ownership) != 2 {
		t.Fatalf("planned %d ownership changes, want the config and the key: %+v",
			len(plan.Ownership), plan.Ownership)
	}

	if err := c.ApplyOwnership(plan.Ownership, 990, 991); err != nil {
		t.Fatalf("ApplyOwnership: %v", err)
	}

	// THE EXACT OPERATIONS, IN ORDER. Counting calls passes when both land on
	// the same file or use the wrong identity — and the ORDER is load-bearing:
	// a chown that succeeds before a chmod that fails leaves a private key owned
	// by the service account with whatever permissions it already had.
	want := []op{
		{kind: "chmod", path: cfg, uid: -1, gid: -1, mode: 0o640},
		{kind: "chown", path: cfg, uid: 0, gid: 991},
		{kind: "chmod", path: key, uid: -1, gid: -1, mode: 0o600},
		{kind: "chown", path: key, uid: 990, gid: 991},
	}
	if len(ops.ops) != len(want) {
		t.Fatalf("applied %v, want %d operations", ops.kinds(), len(want))
	}
	for i, w := range want {
		if ops.ops[i] != w {
			t.Errorf("operation %d was %+v, want %+v", i, ops.ops[i], w)
		}
	}
}

// A SYMLINK IS REFUSED RATHER THAN FOLLOWED. This runs as root: a path that
// named a regular file when the plan was made and a symlink by the time it is
// applied would hand an arbitrary file's ownership to the service account.
func TestOwnershipRefusesAnythingButTheFileItPlanned(t *testing.T) {
	h := newHost(t)
	cfg := h.file(t, "billet.yaml", "server:\n")
	other := h.file(t, "someone-elses", "not the config")

	c, ops := h.converger(t, healthyHost(t, h))

	req := upRequest()
	req.ConfigPath = cfg

	plan, err := c.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// An open that fails stops the run. This says nothing about O_NOFOLLOW —
	// the fake supplies the error — and the flag itself is proved against a real
	// symlink in TestOpenNoFollowRefusesASymlinkForReal.
	t.Run("an open that fails changes nothing", func(t *testing.T) {
		ops.ops = nil
		ops.openErr = map[string]error{cfg: os.ErrPermission}

		err := c.ApplyOwnership(plan.Ownership, 990, 991)
		if err == nil {
			t.Fatal("a file that could not be opened was chowned anyway")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("the failure does not say what the open refuses: %v", err)
		}
		if len(ops.ops) > 0 {
			t.Errorf("something was changed anyway: %v", ops.kinds())
		}
	})

	t.Run("a replaced file is not the planned one", func(t *testing.T) {
		ops.ops = nil
		ops.openErr = nil

		swapped, err := os.Stat(other)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		ops.files = map[string]fs.FileInfo{cfg: swapped}

		if err := c.ApplyOwnership(plan.Ownership, 990, 991); err == nil {
			t.Fatal("a file that had been replaced since planning was chowned anyway")
		}
		if len(ops.ops) > 0 {
			t.Errorf("something was changed anyway: %v", ops.kinds())
		}
	})
}

// A DIRECTORY IS NOT A FILE BILLET CHOWNS, and this is reachable rather than
// theoretical: os.Stat succeeds on a directory, so a github.private_key_path
// that names one is planned for exactly like a file — with the planner's own
// identity check satisfied, because it really is the object that was planned.
// Only asking what KIND of object it is refuses it.
func TestOwnershipRefusesADirectoryEvenWhenItIsTheOneItPlanned(t *testing.T) {
	h := newHost(t)
	cfg := h.file(t, "billet.yaml", "server:\n")

	keyDir := filepath.Join(h.dir, "app-private-key.pem")
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatalf("make the directory: %v", err)
	}

	c, ops := h.converger(t, healthyHost(t, h))

	req := upRequest()
	req.ConfigPath = cfg
	req.KeyPath = keyDir

	plan, err := c.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var planned bool
	for _, ch := range plan.Ownership {
		if ch.Path == keyDir {
			planned = true
		}
	}
	if !planned {
		t.Fatal("the fixture did not reach the case: no ownership was planned for the directory")
	}

	applyErr := c.ApplyOwnership(plan.Ownership, 990, 991)
	if applyErr == nil {
		t.Fatal("a directory was chowned")
	}
	if !strings.Contains(applyErr.Error(), "regular file") {
		t.Errorf("the refusal does not say what was wrong: %v", applyErr)
	}

	// The config came first and was corrected; the directory stopped the run
	// rather than being handed to the service account.
	for _, o := range ops.ops {
		if o.path == keyDir {
			t.Errorf("the directory was modified anyway: %+v", o)
		}
	}
}

// AN UNREADABLE ACCOUNT ID IS A REFUSAL, NOT A DEFAULT. chown reads -1 as
// "leave this as it is", so a uid billet could not parse would otherwise become
// a successful no-op and the service would fail later on a file nobody changed.
func TestIdentityRefusesAnAccountItCannotResolveNumerically(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))
	ops.users["billet"] = &user.User{Uid: "not-a-number", Username: "billet"}

	uid, gid, err := c.Identity(upRequest())
	if err == nil {
		t.Fatal("an account with an unreadable uid was accepted")
	}
	if uid != -1 || gid != -1 {
		t.Errorf("Identity returned (%d, %d) alongside its error, which chown reads as "+
			"\"leave this alone\"", uid, gid)
	}
	if !strings.Contains(err.Error(), "uid") {
		t.Errorf("the refusal does not name what could not be read: %v", err)
	}
}

// A STATE DIRECTORY THAT DOES NOT EXIST YET IS THE ORDINARY FIRST RUN. systemd
// creates it, with the right owner, when the service starts — so this is not a
// failure and must not stop `up`.
func TestRepairServerStateAcceptsADirectoryThatIsNotThereYet(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))
	ops.rootEr = fs.ErrNotExist

	repaired, err := c.RepairServerState("/var/lib/billet/server", 990, 991)
	if err != nil {
		t.Fatalf("an absent state directory was treated as a failure: %v", err)
	}
	if len(repaired) > 0 {
		t.Errorf("something was reported as repaired: %v", repaired)
	}
}

// ENABLEMENT IS READ BACK AS ITS OWN STATE. Folding systemd's dozen enablement
// states into a boolean made every one of them mean what `disabled` means, which
// is permission to enable.
func TestEnabledNowAnswersWithTheStateItself(t *testing.T) {
	for _, state := range []string{
		"enabled", "enabled-runtime", "disabled", "masked", "masked-runtime",
		"static", "indirect", "generated", "transient", "alias", "linked", "bad",
	} {
		t.Run(state, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: measured(map[string]string{"UnitFileState": state}),
			}}
			c, _ := h.converger(t, a)

			got, err := c.EnabledNow(t.Context(), deploy.ServerUnitName)
			if err != nil {
				t.Fatalf("EnabledNow: %v", err)
			}

			// THE WORD COMES BACK for the diagnostic, and the VERDICT is what
			// `up` acts on. Only the two words billet has a rule for are
			// classified; everything else is Unknown, which is what makes `up`
			// refuse a masked or static unit rather than treat it as "not
			// enabled yet".
			if got.How != state {
				t.Errorf("How = %q for a %q unit", got.How, state)
			}

			want := Unknown
			switch state {
			case "enabled":
				want = Yes
			case "disabled":
				want = No
			}

			if got.Enabled != want {
				t.Errorf("EnabledNow(%q) = %v, want %v", state, got.Enabled, want)
			}
		})
	}

	// An answer systemd did not give comes back empty rather than as a state.
	t.Run("no answer", func(t *testing.T) {
		h := newHost(t)
		a := &answers{reply: map[string]string{
			deploy.ServerUnitName: measured(map[string]string{"UnitFileState": "REMOVE"}),
		}}
		c, _ := h.converger(t, a)

		got, err := c.EnabledNow(t.Context(), deploy.ServerUnitName)
		if err != nil {
			t.Fatalf("EnabledNow: %v", err)
		}
		if got.How != "" {
			t.Errorf("an absent property was reported as %q", got.How)
		}

		// AND AN ABSENT ANSWER IS UNCERTAINTY, not "not enabled". Reading it as
		// No would make `up` enable a unit systemd never described.
		if got.Enabled != Unknown {
			t.Errorf("an absent property was classified %v, want Unknown", got.Enabled)
		}
	})
}

// THE NO-FOLLOW OPEN IS TESTED FOR REAL. The earlier version injected ELOOP
// through the seam, which meant the FAKE produced the refusal — it would have
// passed with O_NOFOLLOW deleted, which is the one thing it existed to prove.
func TestOpenNoFollowRefusesASymlinkForReal(t *testing.T) {
	dir := t.TempDir()

	target := filepath.Join(dir, "the-real-key")
	if err := os.WriteFile(target, []byte("-----BEGIN-----\n"), 0o600); err != nil {
		t.Fatalf("write the target: %v", err)
	}

	link := filepath.Join(dir, "link-to-it")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem cannot make a symlink: %v", err)
	}

	if _, err := openNoFollow(link); err == nil {
		t.Fatal("a symlink was opened; as root that would chown whatever it points at")
	}

	// AND THE ORDINARY CASE STILL OPENS, so the test above is not passing
	// because the function refuses everything.
	f, err := openNoFollow(target)
	if err != nil {
		t.Fatalf("a regular file was refused: %v", err)
	}
	_ = f.Close()
}

// // Contained is component-aware: a sibling sharing a prefix is not inside.
func TestContainedIsNotAStringPrefix(t *testing.T) {
	cases := []struct {
		dir, path string
		want      bool
	}{
		{"/var/lib/billet", "/var/lib/billet", true},
		{"/var/lib/billet", "/var/lib/billet/server", true},
		{"/var/lib/billet", "/var/lib/billet-evil", false},
		{"/var/lib/billet", "/var/lib/billet/../../../etc/shadow", false},
		{"/var/lib/billet", "/etc/billet", false},
		{"", "/var/lib/billet", false},
	}

	for _, tc := range cases {
		if got := Contained(tc.dir, tc.path); got != tc.want {
			t.Errorf("Contained(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}

// A PRIVILEGE PREFIX IS INVISIBLE TO THE ORDINARY ANSWER, AND MEASURED SO. On
// systemd 255, `ExecStart=+/usr/bin/billet server …` — which runs the command
// with full privileges no matter what User= says — renders through
// `systemctl show -p ExecStart` byte-identically to the unprefixed form. Every
// check built on that answer therefore passes it, and the server that holds an
// organization's App key comes up as root. The extended property is where the
// prefix survives.
func TestPlanRefusesACommandThatCarriesPrefixFlags(t *testing.T) {
	for _, flags := range []string{"privileged", "ignore-failure", "privileged ignore-failure"} {
		t.Run(flags, func(t *testing.T) {
			h := newHost(t)

			// The ORDINARY answer is the healthy one: that is the whole point —
			// it cannot be told apart from a correct unit's.
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{
					"ExecStartEx": unitExecEx(h.selfPath, "server", probeConfig, flags),
				}),
				deploy.NodeUnitName: healthyNode(t, h, nil),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, "carries the flags") {
				t.Fatalf("a command with flags %q was accepted:\n%s", flags, joined)
			}
			if !strings.Contains(joined, "runs the command with full privileges") {
				t.Errorf("the refusal does not say why the flags matter:\n%s", joined)
			}
			for _, u := range plan.Units {
				if u.Name == deploy.ServerUnitName {
					t.Errorf("it was planned for anyway: %+v", u)
				}
			}
		})
	}
}

// AND A systemd THAT DOES NOT ANSWER AT ALL IS UNCERTAINTY. ExecStartEx landed
// in v246; on anything older billet cannot tell a root command from an ordinary
// one, and an empty answer must not read as "no flags".
func TestPlanRefusesWhenSystemdCannotReportTheFlags(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{"ExecStartEx": "REMOVE"}),
		deploy.NodeUnitName:   healthyNode(t, h, nil),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	joined := strings.Join(refusalText(plan.Refusals), "\n")
	if !strings.Contains(joined, "did not tell billet what prefixes") {
		t.Fatalf("a systemd that could not report the flags was treated as reporting none:\n%s",
			joined)
	}
	if !strings.Contains(joined, "v246") {
		t.Errorf("the refusal does not say what is missing:\n%s", joined)
	}
}

// A UNIT'S TRANSACTION REACHES BEYOND ITSELF. `Conflicts=billet-node.service`
// on the server means `systemctl start billet-server.service` STOPS the node —
// a drain of a host holding jobs, performed by the command that promised never
// to touch a running service. `Requires=` in the other direction starts a
// control plane on a node-only host, where the GitHub proof is skipped
// precisely because no server was supposed to start.
func TestPlanRefusesAUnitWhoseTransactionReachesTheOtherUnit(t *testing.T) {
	for _, prop := range []string{
		"Conflicts", "Requires", "Requisite", "BindsTo", "PartOf", "Upholds", "Wants",
		"OnFailure", "OnSuccess", "JoinsNamespaceOf",
	} {
		t.Run(prop, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{
					// Alongside the ordinary dependencies a real unit carries.
					prop: "sysinit.target " + deploy.NodeUnitName,
				}),
				deploy.NodeUnitName: healthyNode(t, h, nil),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, "would act on "+deploy.NodeUnitName) {
				t.Fatalf("a server unit declaring %s=%s was accepted:\n%s",
					prop, deploy.NodeUnitName, joined)
			}
			if !strings.Contains(joined, "holding jobs") {
				t.Errorf("the refusal does not say what it is protecting:\n%s", joined)
			}
		})
	}
}

// AND THE ORDINARY DEPENDENCIES ARE NOT REFUSED. Measured on the packaged
// units: they require sysinit.target, -.mount and system.slice, want
// network-online.target and tmp.mount, and conflict with shutdown.target. A
// rule of "declare nothing" would refuse every correct host, and an operator
// adding a dependency of their own is not the hazard.
func TestPlanAcceptsTheDependenciesARealUnitCarries(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{
			"Requires":  "-.mount system.slice sysinit.target",
			"Wants":     "network-online.target tmp.mount",
			"Conflicts": "shutdown.target",
			"OnFailure": "notify-the-operator.service",
		}),
		deploy.NodeUnitName: healthyNode(t, h, nil),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("a unit with ordinary dependencies was refused: %v", refusalText(plan.Refusals))
	}
}

// EVERY OTHER COMMAND THE UNIT WOULD RUN, and anything that can replace the
// filesystem the paths were checked in. An ExecStartPre runs before the process
// billet checked; a RootDirectory makes the inode it proved name something else.
func TestPlanRefusesCommandsAndNamespacesItDidNotAccountFor(t *testing.T) {
	cases := []struct {
		prop, value, want string
	}{
		{"ExecStartPre", "{ path=/bin/sh ; argv[]=/bin/sh -c whatever ; ignore_errors=no }", "ExecStartPre"},
		{"ExecStartPost", "{ path=/bin/true ; argv[]=/bin/true ; ignore_errors=no }", "ExecStartPost"},
		{"ExecCondition", "{ path=/bin/true ; argv[]=/bin/true ; ignore_errors=no }", "ExecCondition"},
		{"ExecStopPost", "{ path=/bin/true ; argv[]=/bin/true ; ignore_errors=no }", "ExecStopPost"},
		{"ExecReload", "{ path=/bin/true ; argv[]=/bin/true ; ignore_errors=no }", "ExecReload"},
		{"RootDirectory", "/srv/somewhere-else", "not the paths"},
		{"RootImage", "/srv/rootfs.img", "not the paths"},
		{"BindPaths", "/etc/billet:/etc/billet", "not the paths"},
		{"BindReadOnlyPaths", "/usr/bin:/usr/bin", "not the paths"},
		{"MountImages", "/srv/extra.img:/mnt", "not the paths"},

		// And the ones that widen the identity past the account User= names.
		// `docker` is the sharpest: it is root by another route, and every
		// identity check above still reads billet:billet.
		{"SupplementaryGroups", "docker", "changes who it runs as"},
		{"AmbientCapabilities", "cap_dac_override", "changes who it runs as"},
		{"PAMName", "login", "changes who it runs as"},
		{"ExecSearchPath", "/opt/bin", "changes who it runs as"},
		{"DynamicUser", "yes", "changes who it runs as"},
	}

	for _, tc := range cases {
		t.Run(tc.prop, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{tc.prop: tc.value}),
				deploy.NodeUnitName:   healthyNode(t, h, nil),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("a unit carrying %s was accepted:\n%s", tc.prop, joined)
			}
			for _, u := range plan.Units {
				if u.Name == deploy.ServerUnitName {
					t.Errorf("it was planned for anyway: %+v", u)
				}
			}
		})
	}
}

// DynamicUser=no IS THE ORDINARY ANSWER, not a value to refuse: unlike every
// other property in that set, its harmless state is a word rather than an empty
// string, and refusing non-empty would refuse every correct host.
func TestPlanAcceptsTheOrdinaryDynamicUserAnswer(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{"DynamicUser": "no"}),
		deploy.NodeUnitName:   healthyNode(t, h, map[string]string{"DynamicUser": "no"}),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("a host answering DynamicUser=no was refused: %v", refusalText(plan.Refusals))
	}
}

// AN ACCOUNT NAMED billet THAT RESOLVES TO ROOT DEFEATS THE WHOLE BOUNDARY.
// User=billet in the unit says which NAME the service runs as; what that name
// resolves to is the account database's answer, and nothing above reads it.
func TestIdentityRefusesAServiceAccountThatIsRoot(t *testing.T) {
	cases := []struct {
		name     string
		uid, gid string
		want     string
	}{
		{"root uid", "0", "991", "resolves to uid 0"},
		{"root gid", "990", "0", "gid 0"},
		{"negative uid", "-1", "991", "cannot read"},
		{"negative gid", "990", "-2", "cannot read"},
		// uid_t is unsigned and 32 bits: this is an ordinary positive int on a
		// 64-bit platform and becomes (uid_t)-1 at the syscall boundary — the
		// same "leave this unchanged" sentinel a negative id would be.
		{"the unsigned sentinel", "4294967295", "991", "cannot read"},
		{"the unsigned sentinel as a gid", "990", "4294967295", "cannot read"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			c, ops := h.converger(t, healthyHost(t, h))
			ops.users["billet"] = &user.User{Uid: tc.uid, Username: "billet"}
			ops.groups["billet"] = &user.Group{Gid: tc.gid, Name: "billet"}

			uid, gid, err := c.Identity(upRequest())
			if err == nil {
				t.Fatalf("an account resolving to %s:%s was accepted", tc.uid, tc.gid)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the problem: %v", err)
			}
			if uid != -1 || gid != -1 {
				t.Errorf("Identity returned (%d, %d) beside its error", uid, gid)
			}
		})
	}
}

// THE REPAIR TOUCHES ONLY WHAT THE PREFLIGHT CAN CREATE, BY NAME. The recursive
// walk this replaced could be escaped through: os.Root confines pathname
// resolution, not inodes and not mount boundaries, and a bind mount inside the
// state directory commonly shares its device — so a same-filesystem mount was
// descended into and the root-owned files below it reached a privileged chown.
// A list of names has nothing to descend.
func TestRepairServerStateTouchesOnlyThePreflightsOwnFiles(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))

	// EVERY ONE OF THE FIVE, each independently root-owned: a name dropped from
	// the list must fail this, which a fixture covering only two of them cannot
	// notice.
	ops.tree = fstest.MapFS{
		"billet.db":     &fstest.MapFile{Data: []byte("ledger")},
		"billet.db-wal": &fstest.MapFile{Data: []byte("wal")},
		"billet.db-shm": &fstest.MapFile{Data: []byte("shm")},
		"billet.lock":   &fstest.MapFile{},
		"deployment-id": &fstest.MapFile{Data: []byte("id")},
		// Not names the preflight creates, and not this command's to touch.
		"surprise.db":   &fstest.MapFile{Data: []byte("nobody knows")},
		"ca/issued.pem": &fstest.MapFile{Data: []byte("cert")},
	}
	ops.owners = map[string]uint32{
		".": 990, "ca": 0, "surprise.db": 0, "ca/issued.pem": 0,
		"billet.db": 0, "billet.db-wal": 0, "billet.db-shm": 0,
		"billet.lock": 0, "deployment-id": 0,
	}

	repaired, err := c.RepairServerState("/var/lib/billet/server", 990, 991)
	if err != nil {
		t.Fatalf("RepairServerState: %v", err)
	}

	var changed []string
	for _, o := range ops.ops {
		if o.uid != 990 || o.gid != 991 {
			t.Errorf("%s was given to %d:%d, want the service account", o.path, o.uid, o.gid)
		}
		changed = append(changed, o.path)
	}
	sort.Strings(changed)

	want := "billet.db,billet.db-shm,billet.db-wal,billet.lock,deployment-id"
	if strings.Join(changed, ",") != want {
		t.Errorf("the repair changed %v, want every file the preflight creates (%s)", changed, want)
	}
	if len(repaired) != 5 {
		t.Errorf("the repair reported %v, want the five it changed", repaired)
	}
	// And it says nothing about files it never looked at.
	joined := strings.Join(repaired, "\n")
	if strings.Contains(joined, "surprise") || strings.Contains(joined, "ca/") {
		t.Errorf("the repair reported on files outside its contract:\n%s", joined)
	}
}

// The repair gives back the authority DIRECTORY, not only its files.
//
// A root-owned 0700 ca/ cannot be TRAVERSED by the service account, so an
// authority underneath one is unreachable however its own files are owned —
// which is what a rehearsed restore on a real Linux host found. It is also the
// entry the link-count check would have refused for a hazard it cannot have:
// every directory has at least two links, and a directory cannot be hard linked
// at all.
func TestRepairPathsGivesBackTheAuthorityDirectoryItself(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))

	ops.tree = fstest.MapFS{
		"ca/ca.key":         &fstest.MapFile{Data: []byte("key")},
		"ca/ca.crt":         &fstest.MapFile{Data: []byte("cert")},
		"authority-created": &fstest.MapFile{Data: []byte("2026-08-30")},
	}
	ops.owners = map[string]uint32{
		".": 990, "ca": 0, "ca/ca.key": 0, "ca/ca.crt": 0, "authority-created": 0,
	}
	// A REAL DIRECTORY'S LINK COUNT. Without it the fixture would pass against a
	// version that still applied the hard-link refusal to directories.
	ops.links = map[string]uint64{"ca": 2}

	repaired, err := c.RepairPaths("/var/lib/billet/server", []RepairTarget{
		{Name: "ca", Dir: true},
		{Name: "ca/ca.key"},
		{Name: "ca/ca.crt"},
		{Name: "authority-created"},
	}, 990, 991)
	if err != nil {
		t.Fatalf("RepairPaths: %v", err)
	}

	var changed []string
	for _, o := range ops.ops {
		if o.uid != 990 || o.gid != 991 {
			t.Errorf("%s was given to %d:%d, want the service account", o.path, o.uid, o.gid)
		}
		changed = append(changed, o.path)
	}
	sort.Strings(changed)

	want := "authority-created,ca,ca/ca.crt,ca/ca.key"
	if strings.Join(changed, ",") != want {
		t.Errorf("the repair changed %v, want the whole authority (%s)", changed, want)
	}

	if len(repaired) != 4 {
		t.Errorf("the repair reported %v, want the four entries it changed", repaired)
	}
}

// An entry that is not the KIND it was named as is refused.
//
// A name declared a directory that is a file, or a file that is a directory, is
// not the thing this run created — whichever way round it is — and chowning it
// would hand the service account something billet cannot account for.
func TestRepairPathsRefusesAnEntryThatIsNotTheKindItWasNamedAs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target RepairTarget
	}{
		{"a file named as a directory", RepairTarget{Name: "billet.db", Dir: true}},
		{"a directory named as a file", RepairTarget{Name: "ca"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			c, ops := h.converger(t, healthyHost(t, h))

			ops.tree = fstest.MapFS{
				"billet.db": &fstest.MapFile{Data: []byte("ledger")},
				"ca/ca.key": &fstest.MapFile{Data: []byte("key")},
			}
			ops.owners = map[string]uint32{".": 990, "billet.db": 0, "ca": 0, "ca/ca.key": 0}
			ops.links = map[string]uint64{"ca": 2}

			if _, err := c.RepairPaths("/var/lib/billet/server",
				[]RepairTarget{tc.target}, 990, 991); err == nil {
				t.Fatal("the repair accepted an entry that was not the kind it was named as")
			}

			if len(ops.ops) > 0 {
				t.Errorf("it changed something anyway: %v", ops.ops)
			}
		})
	}
}

// AND ANYTHING IT CANNOT ACCOUNT FOR IS A REFUSAL, not a note. These five files
// are what the server has to open: one of them root-owned and not a plain file,
// or the same inode as something outside the directory, is a host that will not
// come up — and saying so quietly while starting the service anyway is the
// failure this function exists to prevent.
func TestRepairServerStateRefusesWhatItCannotAccountFor(t *testing.T) {
	cases := []struct {
		name  string
		tree  fstest.MapFS
		links map[string]uint64
		owner uint32
		want  string
	}{
		{
			"a root-owned symlink where the ledger should be",
			fstest.MapFS{"billet.db": &fstest.MapFile{Mode: fs.ModeSymlink}},
			nil, 0, "not a regular file",
		},
		// SERVICE-OWNED, deliberately: an implementation that returned early for
		// anything not owned by root would pass a root-owned fixture and fail
		// only on this one. What it IS has to be established before who owns it
		// decides whether to act.
		{
			"a directory the service owns where the ledger should be",
			fstest.MapFS{"billet.db": &fstest.MapFile{Mode: fs.ModeDir}},
			nil, 990, "not a regular file",
		},
		{
			"a service-owned ledger that is the same inode as something outside",
			fstest.MapFS{"billet.db": &fstest.MapFile{Data: []byte("x")}},
			map[string]uint64{"billet.db": 2}, 990, "has 2 links",
		},
		{
			"a ledger owned by neither root nor the service",
			fstest.MapFS{"billet.db": &fstest.MapFile{Data: []byte("x")}},
			nil, 4242, "neither root nor the service account",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			c, ops := h.converger(t, healthyHost(t, h))
			ops.tree = tc.tree
			ops.links = tc.links
			ops.owners = map[string]uint32{"billet.db": tc.owner}

			_, err := c.RepairServerState("/var/lib/billet/server", 990, 991)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what was wrong: %v", err)
			}
			if len(ops.ops) > 0 {
				t.Errorf("something was changed anyway: %v", ops.kinds())
			}
		})
	}
}

// AN ENTRY IT CANNOT EVEN LOOK AT STOPS THE RUN. Only "it is not there" is an
// ordinary answer; a permission or IO failure is not evidence of absence.
func TestRepairServerStateRefusesWhatItCannotRead(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))
	ops.tree = fstest.MapFS{"billet.db": &fstest.MapFile{Data: []byte("x")}}
	ops.owners = map[string]uint32{"billet.db": 0}
	ops.openErr = map[string]error{"billet.db": os.ErrPermission}

	if _, err := c.RepairServerState("/var/lib/billet/server", 990, 991); err == nil {
		t.Fatal("a file billet could not open was treated as absent")
	}
}

// A SERVICE ACCOUNT THAT IS ROOT NEEDS NO REPAIR, and doing one anyway would
// chown the ledger to root on a host where root is what created it.
func TestRepairServerStateDoesNothingForARootServiceAccount(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))
	ops.tree = fstest.MapFS{"billet.db": &fstest.MapFile{}}
	ops.owners = map[string]uint32{"billet.db": 0}

	repaired, err := c.RepairServerState("/var/lib/billet/server", 0, 0)
	if err != nil {
		t.Fatalf("RepairServerState: %v", err)
	}
	if len(repaired) > 0 || len(ops.ops) > 0 {
		t.Errorf("a root service account still produced changes: %v %v", repaired, ops.kinds())
	}
}

// A STATE DIRECTORY THAT DOES NOT EXIST YET IS THE ORDINARY FIRST RUN. systemd
// creates it, with the right owner, when the service starts — so this is not a
// failure and must not stop `up`.

// A HARD LINK IS THE SAME FILE UNDER ANOTHER NAME, so chmod and chown here
// change it wherever else it is named — and one of the two files this touches
// is a private key. Staged with a REAL link rather than a fabricated link
// count, because the production check reads the count the kernel reports.
func TestOwnershipRefusesAFileThatIsNamedTwice(t *testing.T) {
	h := newHost(t)
	cfg := h.file(t, "billet.yaml", "server:\n")

	elsewhere := filepath.Join(h.dir, "someone-elses-name-for-it")
	if err := os.Link(cfg, elsewhere); err != nil {
		t.Skipf("this filesystem cannot make a hard link: %v", err)
	}

	before, err := os.Stat(elsewhere)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	c, ops := h.converger(t, healthyHost(t, h))

	req := upRequest()
	req.ConfigPath = cfg

	plan, err := c.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	applyErr := c.ApplyOwnership(plan.Ownership, 990, 991)
	if applyErr == nil {
		t.Fatal("a file with two names was chowned")
	}
	if !strings.Contains(applyErr.Error(), "2 links") {
		t.Errorf("the refusal does not say what was wrong: %v", applyErr)
	}
	if len(ops.ops) > 0 {
		t.Errorf("something was changed anyway: %v", ops.kinds())
	}

	// AND THE OTHER NAME IS UNTOUCHED — the point of refusing.
	after, err := os.Stat(elsewhere)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after.Mode() != before.Mode() {
		t.Errorf("the other name's mode changed from %v to %v", before.Mode(), after.Mode())
	}
}

// AN ANSWER BILLET CANNOT READ IS NOT AN ANSWER OF "no prefixes". An older
// systemd, a changed rendering or a truncated record leaves the one field that
// decides privilege absent — and reading that as "ordinary command" is the
// three-valued rule collapsing at the exact point it was added to protect.
func TestPlanRefusesAnExtendedAnswerItCannotRead(t *testing.T) {
	for _, rendered := range []string{
		"garbage",
		"{ path=/usr/bin/billet ; argv[]=/usr/bin/billet server }",
		"{ path=/usr/bin/billet ; flag=privileged }",
	} {
		t.Run(rendered, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{
					"ExecStartEx": rendered,
				}),
				deploy.NodeUnitName: healthyNode(t, h, nil),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(strings.Join(refusalText(plan.Refusals), "\n"),
				"could not read the prefixes") {
				t.Fatalf("an unreadable extended answer was accepted:\n%v",
					refusalText(plan.Refusals))
			}
		})
	}
}

// AND A `flags=` INSIDE THE ARGUMENTS IS NOT THE FIELD. Searching for the first
// substring would read an argument and never reach the field that decides
// privilege — so the parse is by delimited field, and this proves it.
func TestExecStartFlagsReadsTheFieldRatherThanASubstring(t *testing.T) {
	cases := []struct {
		name, rendered, want string
		found                bool
	}{
		{
			"an ordinary command",
			"{ path=/usr/bin/billet ; argv[]=/usr/bin/billet node ; flags= ; pid=0 }",
			"", true,
		},
		{
			"a privileged one",
			"{ path=/usr/bin/billet ; argv[]=/usr/bin/billet node ; flags=privileged ; pid=0 }",
			"privileged", true,
		},
		{
			// The argument contains the marker; the real field says privileged.
			"the marker inside an argument",
			"{ path=/usr/bin/billet ; argv[]=/usr/bin/billet node --config /etc/flags=x ; " +
				"flags=privileged ; pid=0 }",
			"privileged", true,
		},
		{
			"no field at all",
			"{ path=/usr/bin/billet ; argv[]=/usr/bin/billet node ; pid=0 }",
			"", false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := execStartFlags(tc.rendered)
			if found != tc.found {
				t.Errorf("found = %v, want %v", found, tc.found)
			}
			if got != tc.want {
				t.Errorf("flags = %q, want %q", got, tc.want)
			}
		})
	}
}

// A UNIT THAT ACTS ON THE HOST WHEN IT FAILS. `OnFailureJobMode=isolate` stops
// everything else on the machine, and FailureAction or StartLimitAction can
// reboot it — from a service billet is about to start. Measured on the packaged
// units: OnFailureJobMode is "replace" and the rest are "none", so each of these
// has an ordinary value to differ from rather than being empty.
func TestPlanRefusesAUnitThatActsOnTheHost(t *testing.T) {
	cases := []struct{ prop, value string }{
		{"OnFailureJobMode", "isolate"},
		{"FailureAction", "reboot"},
		{"SuccessAction", "poweroff"},
		{"StartLimitAction", "reboot-force"},
		{"JobTimeoutAction", "reboot"},
	}

	for _, tc := range cases {
		t.Run(tc.prop, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{tc.prop: tc.value}),
				deploy.NodeUnitName:   healthyNode(t, h, nil),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, "acts on the host") {
				t.Fatalf("a unit with %s=%s was accepted:\n%s", tc.prop, tc.value, joined)
			}
			if !strings.Contains(joined, "isolates or reboots") {
				t.Errorf("the refusal does not say what it prevents:\n%s", joined)
			}
		})
	}
}

// AND THE ORDINARY VALUES ARE NOT REFUSED, or every correct host would be.
func TestPlanAcceptsTheOrdinaryJobSettings(t *testing.T) {
	h := newHost(t)
	over := map[string]string{
		"OnFailureJobMode": "replace", "FailureAction": "none", "SuccessAction": "none",
		"StartLimitAction": "none", "JobTimeoutAction": "none",
	}
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, over),
		deploy.NodeUnitName:   healthyNode(t, h, over),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("a host with the ordinary job settings was refused: %v",
			refusalText(plan.Refusals))
	}
}

// A SECOND NAME IS A SECOND SET OF LINKS. An Alias= makes `systemctl enable`
// write for a name billet never asked about, and `disable` remove them.
func TestPlanRefusesAUnitThatAnswersToAnotherName(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{
			"Names": deploy.ServerUnitName + " billet-control-plane.service",
		}),
		deploy.NodeUnitName: healthyNode(t, h, nil),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	joined := strings.Join(refusalText(plan.Refusals), "\n")
	if !strings.Contains(joined, "also answers to billet-control-plane.service") {
		t.Fatalf("an aliased unit was accepted:\n%s", joined)
	}
}

// A START IS A TRANSACTION, AND IT IS REFUSED BEFORE IT RUNS. A helper that
// billet's own unit merely Wants= runs its own Conflicts=, which is a STOP of
// whatever it names — and noticing that afterwards is a report of destroyed
// jobs rather than a refusal to destroy them.
func TestPlanRefusesAStartThatWouldReachTheOtherUnitThroughAHelper(t *testing.T) {
	// EVERY WAY A UNIT CAN PULL IN ANOTHER, crossed with what that other one
	// would then do. Testing only `Wants=` would leave the rest of the puller
	// list unexercised — deleting them would keep the suite green.
	cases := []struct {
		name, puller, prop, verb string
	}{
		{"wanted helper that stops the node", "Wants", "Conflicts", "STOP"},
		{"required helper that starts it", "Requires", "Requires", "start"},
		{"requisite helper that wants it", "Requisite", "Wants", "start"},
		{"bound helper that upholds it", "BindsTo", "Upholds", "start"},
		{"upheld helper that binds to it", "Upholds", "BindsTo", "start"},
		{"a part-of helper that fails onto it", "PartOf", "OnFailure", "start"},
		{"a socket that triggers it later", "Wants", "Triggers", "later activate"},
		{"a requisite helper that is part of it", "Requires", "PartOf", "start"},
		{"a wanted helper requisite on it", "Wants", "Requisite", "start"},
		{"a wanted helper that succeeds onto it", "Wants", "OnSuccess", "start"},
		{"a wanted helper sharing its namespace", "Wants", "JoinsNamespaceOf", "start"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{
					tc.puller: "network-online.target billet-helper.target",
				}),
				deploy.NodeUnitName: healthyNode(t, h, nil),
				// The helper is an ordinary unit as far as every check on the
				// server is concerned.
				"billet-helper.target": pulledUnit(map[string]string{
					tc.prop: deploy.NodeUnitName,
				}),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, "pulls in billet-helper.target") {
				t.Fatalf("a start reaching the node through a helper was accepted:\n%s", joined)
			}
			if !strings.Contains(joined, "would "+tc.verb+" "+deploy.NodeUnitName) {
				t.Errorf("the refusal does not say what would happen:\n%s", joined)
			}
			// AND NOTHING IS PLANNED, so it is refused rather than reported.
			for _, u := range plan.Units {
				if u.Name == deploy.ServerUnitName {
					t.Errorf("it was planned anyway: %+v", u)
				}
			}
		})
	}
}

// AND AN ORDINARY HELPER IS NOT REFUSED, so the check above is not passing
// because anything a unit pulls in now fails.
func TestPlanAcceptsAHelperThatDoesNotTouchBillet(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{
			"Wants": "network-online.target some-helper.service",
		}),
		deploy.NodeUnitName: healthyNode(t, h, nil),
		"some-helper.service": pulledUnit(map[string]string{
			"Conflicts": "shutdown.target", "Requires": "sysinit.target",
		}),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("an ordinary helper was refused: %v", refusalText(plan.Refusals))
	}
}

// A UNIT SYSTEMD CANNOT BE ASKED ABOUT IS ONE BILLET CANNOT CLEAR.
func TestPlanFailsWhenAPulledUnitCannotBeInspected(t *testing.T) {
	h := newHost(t)
	a := &answers{
		reply: map[string]string{
			deploy.ServerUnitName: healthyServer(t, h, map[string]string{
				"Wants": "billet-helper.target",
			}),
			deploy.NodeUnitName: healthyNode(t, h, nil),
		},
	}
	c, _ := h.converger(t, a)

	// Only the helper is unanswerable; everything else replies normally.
	base := c.inspector.run
	c.inspector.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if args[len(args)-1] == "billet-helper.target" {
			return nil, os.ErrPermission
		}

		return base(ctx, bin, args)
	}

	if _, err := c.Plan(t.Context(), upRequest()); err == nil {
		t.Fatal("a unit billet could not ask about was cleared anyway")
	}
}

// THE NO-FOLLOW OPEN IN THE STATE REPAIR IS TESTED AGAINST A REAL DIRECTORY.
// The fixture's root ignores open flags, so every test that goes through it
// proves the checks that come after the open and nothing about the open itself
// — which is the one part that stops a symlink planted in the state directory
// from redirecting a root chown.
func TestRepairRootRefusesASymlinkForReal(t *testing.T) {
	dir := t.TempDir()

	outside := filepath.Join(t.TempDir(), "somebody-elses-file")
	if err := os.WriteFile(outside, []byte("not the ledger"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "billet.db")); err != nil {
		t.Skipf("this filesystem cannot make a symlink: %v", err)
	}

	// THROUGH THE PRODUCTION PATH, not through a hand-built open: a test that
	// passes O_NOFOLLOW itself proves that the flag works, which nobody doubts,
	// and would stay green if RepairServerState stopped passing it.
	h := newHost(t)
	c, _ := h.converger(t, healthyHost(t, h))
	c.repairRoot = openRepairRoot

	before, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// THE MESSAGE DISTINGUISHES THE TWO REFUSALS. Without O_NOFOLLOW the open
	// would succeed and repairOne would reject the target for its OWNER — an
	// error either way, so a test that only asserted "an error happened" would
	// pass with the flag deleted.
	_, err = c.RepairServerState(dir, 990, 991)
	if err == nil {
		t.Fatal("a symlink in the state directory was followed; as root that would have " +
			"chowned whatever it points at")
	}
	if strings.Contains(err.Error(), "neither root nor the service account") {
		t.Fatalf("the symlink was FOLLOWED and refused for its target's owner instead: %v", err)
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("the refusal does not say the open was declined: %v", err)
	}

	// AND THE FILE IT POINTED AT IS UNTOUCHED — the point of refusing.
	after, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !os.SameFile(before, after) || after.Mode() != before.Mode() {
		t.Errorf("the symlink's target changed: %v became %v", before.Mode(), after.Mode())
	}
}

// AND THE REAL ROOT STAYS INSIDE THE DIRECTORY IT OPENED.
func TestRepairRootStaysInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()

	root, err := openRepairRoot(dir)
	if err != nil {
		t.Fatalf("openRepairRoot: %v", err)
	}
	defer func() { _ = root.Close() }()

	if _, err := root.OpenFile("../escaped", os.O_RDONLY, 0); err == nil {
		t.Error("a path leaving the state directory was resolved")
	}
}

// A SERVICE THAT STOPPED AND STARTED AGAIN IS NOT ONE THAT NEVER MOVED, and
// between two samples the only thing that says so can be the start timestamp:
// an inactive unit that started and exited comes back to the same state,
// substate, result, pid 0 and restart count it had before.
func TestSnapshotCarriesTheEvidenceOfARestart(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: measured(map[string]string{
			"ExecMainStartTimestampMonotonic": "111",
		}),
	}}
	c, _ := h.converger(t, a)

	before, err := c.Snapshot(t.Context(), deploy.ServerUnitName)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Everything else identical; only the start timestamp moved.
	a.reply[deploy.ServerUnitName] = measured(map[string]string{
		"ExecMainStartTimestampMonotonic": "222",
	})

	after, err := c.Snapshot(t.Context(), deploy.ServerUnitName)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if before == after {
		t.Errorf("a service that started again compares equal to one that did not: %q", before)
	}
}

// AND A REPLY THAT DID NOT ARRIVE IS NOT A SERVICE THAT DID NOT MOVE. Every
// absent property renders as an empty string, so two truncated answers compare
// equal — which would read as "nothing changed" at the exact moment billet
// cannot see anything at all.
func TestSampleRefusesAReplyThatIsMissingAField(t *testing.T) {
	for _, missing := range []string{
		"ActiveState", "SubState", "Result", "MainPID", "NRestarts",
		"ExecMainStartTimestampMonotonic",
	} {
		t.Run(missing, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: measured(map[string]string{missing: "REMOVE"}),
			}}
			c, _ := h.converger(t, a)

			if _, err := c.Snapshot(t.Context(), deploy.ServerUnitName); err == nil {
				t.Fatalf("a reply with no %s was read as a service that is fine", missing)
			}
		})
	}
}

// AN ABSENT ANSWER IS NOT THE ORDINARY ONE. Each of these settings has a
// harmless VALUE rather than being empty, so systemd not reporting one leaves
// billet unable to say whether this unit reboots the machine when it fails.
func TestPlanRefusesAnAbsentAnswerAboutWhatAUnitWouldDo(t *testing.T) {
	for _, missing := range []string{
		"OnFailureJobMode", "FailureAction", "SuccessAction",
		"StartLimitAction", "JobTimeoutAction",
	} {
		t.Run(missing, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{missing: "REMOVE"}),
				deploy.NodeUnitName:   healthyNode(t, h, nil),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			joined := strings.Join(refusalText(plan.Refusals), "\n")
			if !strings.Contains(joined, missing+"=(no answer)") {
				t.Fatalf("an absent %s was read as the ordinary value:\n%s", missing, joined)
			}
		})
	}
}

// AND AN ABSENT Names IS NOT "no aliases": systemd always reports a unit's own
// name, so nothing coming back is a reply billet cannot read.
func TestPlanRefusesAUnitWhoseNamesItCannotRead(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{"Names": "REMOVE"}),
		deploy.NodeUnitName:   healthyNode(t, h, nil),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(strings.Join(refusalText(plan.Refusals), "\n"),
		"did not say what names") {
		t.Fatalf("an absent Names was read as a unit with no aliases:\n%v",
			refusalText(plan.Refusals))
	}
}

// A DEPENDENCY IS A DEPENDENCY WHATEVER KIND OF UNIT IT IS. Filtering the
// pulled units by suffix meant billet had to be right about which unit types
// can carry a Conflicts=, which is a question with no reason to have a short
// answer — a .socket stops a service just as well as a .target does.
func TestPlanLooksAtEveryKindOfUnitAServicePullsIn(t *testing.T) {
	for _, pulled := range []string{
		"billet-helper.target", "billet-helper.socket", "billet-helper.path",
		"billet-helper.mount", "billet-helper.timer", "billet-helper.slice",
	} {
		t.Run(pulled, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{
					"Wants": "network-online.target " + pulled,
				}),
				deploy.NodeUnitName: healthyNode(t, h, nil),
				pulled:              pulledUnit(map[string]string{"Conflicts": deploy.NodeUnitName}),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(strings.Join(refusalText(plan.Refusals), "\n"),
				"pulls in "+pulled) {
				t.Fatalf("a %s that stops the node was not looked at:\n%v",
					pulled, refusalText(plan.Refusals))
			}
		})
	}
}

// A TEMPLATE INSTANCE OF A BILLET UNIT IS A BILLET UNIT. billet ships no
// template units, but a host that has one would have
// `billet-server@somewhere.service` running `billet server` — and exact-name
// matching would clear a dependency on it while the before-and-after
// comparison, which samples `billet-server.service`, never saw it either.
func TestPlanRefusesAStartThatReachesAnInstanceOfABilletUnit(t *testing.T) {
	for _, named := range []string{
		"billet-server@site.service",
		"billet-node@rack-2.service",
	} {
		t.Run(named, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{
					"Wants": "billet-helper.target",
				}),
				deploy.NodeUnitName: healthyNode(t, h, nil),
				"billet-helper.target": pulledUnit(map[string]string{
					"Requires": named,
				}),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(strings.Join(refusalText(plan.Refusals), "\n"), named) {
				t.Fatalf("a dependency on %s was cleared:\n%v", named, refusalText(plan.Refusals))
			}
		})
	}
}

// AND A UNIT THAT MERELY STARTS WITH THE SAME LETTERS IS NOT ONE OF BILLET'S,
// or an operator's own `billet-metrics.service` would be refused.
func TestPlanAcceptsAUnitThatOnlyLooksLikeBillets(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{
			"Wants": "billet-helper.target",
		}),
		deploy.NodeUnitName: healthyNode(t, h, nil),
		"billet-helper.target": pulledUnit(map[string]string{
			"Requires": "billet-metrics.service billet-server-backup.service",
		}),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("an operator's own unit was mistaken for billet's: %v", refusalText(plan.Refusals))
	}
}

// A REPLY BILLET COULD NOT READ IS NOT A UNIT THAT DECLARES NOTHING. Measured
// on systemd 255: every one of these properties comes back even for a unit that
// does not exist, so one missing means the answer was truncated.
func TestPlanRefusesATruncatedAnswerAboutAPulledUnit(t *testing.T) {
	for _, missing := range []string{"Conflicts", "Triggers", "Requires", "LoadState"} {
		t.Run(missing, func(t *testing.T) {
			h := newHost(t)
			reply := pulledUnit(nil)

			// Drop exactly one property from the answer.
			var kept []string
			for _, line := range strings.Split(reply, "\n") {
				if key, _, ok := strings.Cut(line, "="); !ok || key != missing {
					kept = append(kept, line)
				}
			}

			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, map[string]string{
					"Wants": "billet-helper.target",
				}),
				deploy.NodeUnitName:    healthyNode(t, h, nil),
				"billet-helper.target": strings.Join(kept, "\n"),
			}}
			c, _ := h.converger(t, a)

			if _, err := c.Plan(t.Context(), upRequest()); err == nil {
				t.Fatalf("an answer with no %s was read as a helper that declares nothing", missing)
			}
		})
	}
}

// A UNIT NAME CAN BEGIN WITH A DASH, AND ONE OF THEM IS ON EVERY LINUX HOST.
// `-.mount` is the root filesystem, and billet's own units pull it in through
// their implicit Requires= — so without `--` systemctl answers `invalid option
// -- '.'`, billet reads that as a unit it cannot ask about, and `local up`
// refuses a host the package had just prepared correctly. Measured, live.
func TestUnitNamesBeginningWithADashAreNotReadAsOptions(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, map[string]string{
			"Requires": "-.mount system.slice sysinit.target",
		}),
		deploy.NodeUnitName: healthyNode(t, h, nil),
	}}
	c, _ := h.converger(t, a)

	// The runner refuses anything that would reach systemctl's option parser,
	// the way systemctl itself does.
	base := c.inspector.run
	c.inspector.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
		for i, arg := range args {
			if strings.HasPrefix(arg, "-") && arg != "--" && !strings.HasPrefix(arg, "--property=") {
				return nil, fmt.Errorf("systemctl: invalid option -- %q", arg)
			}
			// Anything after `--` is a name, whatever it starts with.
			if arg == "--" {
				_ = i

				break
			}
		}

		return base(ctx, bin, args)
	}

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("a host whose units require -.mount was refused: %v", err)
	}
	if len(plan.Refusals) > 0 {
		t.Fatalf("a correct host was refused: %v", refusalText(plan.Refusals))
	}
}

// A FILE FROM ANOTHER FILESYSTEM IS NOT THIS DIRECTORY'S STATE. A regular,
// root-owned, singly-linked file bind-mounted onto one of the five names passes
// every other check — it is an inode from somewhere else wearing an authorised
// name, and the chown would land on it.
func TestRepairServerStateRefusesAFileFromAnotherFilesystem(t *testing.T) {
	h := newHost(t)
	c, ops := h.converger(t, healthyHost(t, h))

	ops.tree = fstest.MapFS{
		".":         &fstest.MapFile{Mode: fs.ModeDir},
		"billet.db": &fstest.MapFile{Data: []byte("from somewhere else")},
	}
	ops.owners = map[string]uint32{".": 990, "billet.db": 0}
	ops.devices = map[string]uint64{".": 64, "billet.db": 99}

	if _, err := c.RepairServerState("/var/lib/billet/server", 990, 991); err == nil {
		t.Fatal("a file from another filesystem was chowned")
	} else if !strings.Contains(err.Error(), "different filesystem") {
		t.Errorf("the refusal does not say what was wrong: %v", err)
	}

	if len(ops.ops) > 0 {
		t.Errorf("something was changed anyway: %v", ops.kinds())
	}
}

// THE PLAN IS OLD BY THE TIME ANYTHING HAPPENS, and `billet check` spends as
// long on the network as the network takes. Revalidate is what stops a converge
// acting on an answer that has since stopped being true.
func TestRevalidateRefusesAUnitThatChangedUnderneathThePlan(t *testing.T) {
	stopped := map[string]string{
		"ActiveState": "inactive", "SubState": "dead",
		"UnitFileState": "disabled", "MainPID": "0",
	}

	cases := []struct {
		name string
		then map[string]string
		want string
	}{
		{
			// Somebody started it while billet was talking to GitHub, so the
			// plan's "start this" is now a restart of a running service.
			"it is running now",
			map[string]string{
				"ActiveState": "active", "SubState": "running", "MainPID": "4242",
			},
			"changed while billet was checking GitHub",
		},
		{
			// A drain began; starting through it destroys the jobs it waits for.
			"a drain began",
			map[string]string{"ActiveState": "deactivating"},
			"no longer a unit billet will act on",
		},
		{
			// Edited and daemon-reloaded into something billet never validated.
			"it was edited and reloaded",
			map[string]string{"NeedDaemonReload": "yes"},
			"no longer a unit billet will act on",
		},
		{
			"it was masked",
			map[string]string{"LoadState": "masked"},
			"no longer a unit billet will act on",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{
				deploy.ServerUnitName: healthyServer(t, h, stopped),
				deploy.NodeUnitName:   healthyNode(t, h, stopped),
			}}
			c, _ := h.converger(t, a)

			plan, err := c.Plan(t.Context(), upRequest())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.Units) == 0 {
				t.Fatal("the fixture planned nothing")
			}

			// The host moves between the plan and the act.
			over := map[string]string{}
			maps.Copy(over, stopped)
			maps.Copy(over, tc.then)
			a.reply[deploy.ServerUnitName] = healthyServer(t, h, over)

			err = c.Revalidate(t.Context(), upRequest(), plan.Units[0])
			if err == nil {
				t.Fatalf("a unit that changed (%s) was acted on anyway", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what happened: %v", err)
			}
		})
	}
}

// AND A HOST THAT DID NOT MOVE IS NOT REFUSED, or `up` could never act at all.
func TestRevalidateAcceptsAHostThatDidNotMove(t *testing.T) {
	h := newHost(t)
	stopped := map[string]string{
		"ActiveState": "inactive", "SubState": "dead",
		"UnitFileState": "disabled", "MainPID": "0",
	}
	a := &answers{reply: map[string]string{
		deploy.ServerUnitName: healthyServer(t, h, stopped),
		deploy.NodeUnitName:   healthyNode(t, h, stopped),
	}}
	c, _ := h.converger(t, a)

	plan, err := c.Plan(t.Context(), upRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, unit := range plan.Units {
		if err := c.Revalidate(t.Context(), upRequest(), unit); err != nil {
			t.Errorf("an unchanged %s was refused: %v", unit.Name, err)
		}
	}
}

// A UNIT THAT IS RUNNING AGAIN AFTER A COMPLETED STOP IS NOT STOPPED.
//
// `systemctl stop` returns when the job it queued completes, and that says
// nothing about whether something started the unit again — a Restart= that
// applies, or another operator. A caller about to tell somebody their host is
// down needs the state the manager holds now, not the exit code of a command.
func TestStopAndProveRefusesAUnitThatCameBack(t *testing.T) {
	t.Parallel()

	h := newHost(t)
	a := &answers{reply: map[string]string{
		"billet-node.service": "ActiveState=active\nSubState=running\nResult=success\n",
	}}

	c := NewConverger(h.inspector(a))

	got, err := c.StopAndProve(t.Context(), "billet-node.service")
	if err == nil {
		t.Fatal("a unit that was active after being stopped was reported as stopped")
	}
	if !strings.Contains(err.Error(), "not proved gone") {
		t.Errorf("the diagnostic does not say what happened: %v", err)
	}

	// THE VERDICT AND THE ACCOUNT BOTH COME BACK, so a caller can report what it
	// found rather than only that something went wrong.
	if got.Gone != No {
		t.Errorf("Gone = %v, want a definite no: the process was still there", got.Gone)
	}
	if !strings.Contains(got.How, "active") {
		t.Errorf("the refusal did not carry the state it saw: %+v", got)
	}

	// AND IT REALLY ASKED, rather than deciding from the stop's exit code.
	var asked bool

	for _, call := range a.calls {
		if len(call) > 0 && call[0] == "show" {
			asked = true
		}
	}

	if !asked {
		t.Error("nothing asked systemd what state the unit was in after the stop")
	}
}

// AND AN INACTIVE ONE IS ACCEPTED, with the verdict systemd gives — a unit
// killed at TimeoutStopSec is inactive AND failed, and an operator being told
// their host is down should know which.
func TestStopAndProveReportsHowTheUnitEnded(t *testing.T) {
	t.Parallel()

	h := newHost(t)
	a := &answers{reply: map[string]string{
		"billet-node.service": "ActiveState=inactive\nSubState=dead\nResult=timeout\n",
	}}

	c := NewConverger(h.inspector(a))

	got, err := c.StopAndProve(t.Context(), "billet-node.service")
	if err != nil {
		t.Fatalf("an inactive unit was refused: %v", err)
	}
	if got.Gone != Yes {
		t.Errorf("Gone = %v, want yes: an inactive unit has no process left", got.Gone)
	}
	if !strings.Contains(got.How, "timeout") {
		t.Errorf("How is %q, want systemd's verdict in it; a unit killed at TimeoutStopSec "+
			"is inactive and failed, and that is not the same as a clean stop", got.How)
	}
}

// STATES THAT DO NOT PROVE THE PROCESS IS GONE ARE REFUSED.
//
// An allowlist rather than a denylist, because the unlisted states are the
// dangerous ones. `deactivating` means the unit is STILL STOPPING — its process
// is alive — and an earlier version refused only `active` and `activating`, so
// it accepted that and went on to stop the other unit and report the host down.
// An empty answer is systemd not telling us, which is uncertainty rather than
// absence.
func TestStopAndProveRefusesEveryStateThatIsNotProofOfAStoppedProcess(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		state string
		want  string
		// gone is the VERDICT, which is not the same fact as the diagnostic. A
		// state billet does not recognise is a definite "still there"; an empty
		// answer is systemd declining to say, and a caller that cannot tell the
		// two apart cannot report honestly which one it hit.
		gone Tristate
	}{
		// These four SAY a process is there. `deactivating` is the dangerous
		// one: the unit is still stopping and its process is alive, and an
		// earlier version that refused only `active` and `activating` accepted
		// it, then went on to stop the other unit and report the host down.
		{"deactivating", "not proved gone", No},
		{"reloading", "not proved gone", No},
		{"activating", "not proved gone", No},
		{"active", "not proved gone", No},
		// `maintenance` is a real systemd state that billet has no rule for. It
		// refuses exactly as the others do; the difference is that it does not
		// CLAIM a process remains, because nothing established that.
		{"maintenance", "does not recognise", Unknown},
		{"", "did not say what state", Unknown},
		// A state a future systemd adds is not proof either — and it is not
		// evidence of a live process either, so the honest verdict is Unknown
		// rather than a definite No. Both refuse; only this says which it is.
		{"quiescing", "does not recognise", Unknown},
	} {
		t.Run(orNone(tc.state), func(t *testing.T) {
			t.Parallel()

			h := newHost(t)
			reply := "SubState=x\nResult=success\n"

			if tc.state != "" {
				reply = "ActiveState=" + tc.state + "\n" + reply
			}

			a := &answers{reply: map[string]string{"billet-node.service": reply}}

			got, err := NewConverger(h.inspector(a)).StopAndProve(t.Context(), "billet-node.service")
			if err == nil {
				t.Fatalf("a unit reporting %q was accepted as stopped", tc.state)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the diagnostic does not explain itself: %v", err)
			}
			if got.Gone != tc.gone {
				t.Errorf("Gone = %v, want %v", got.Gone, tc.gone)
			}
		})
	}
}

// AND `failed` IS STOPPED. Nothing is running, which is the question — how it
// ended is Result's job, and refusing here would make a unit killed at
// TimeoutStopSec impossible to take down.
func TestStopAndProveAcceptsAFailedUnitAsStopped(t *testing.T) {
	t.Parallel()

	h := newHost(t)
	a := &answers{reply: map[string]string{
		"billet-node.service": "ActiveState=failed\nSubState=failed\nResult=exit-code\n",
	}}

	got, err := NewConverger(h.inspector(a)).StopAndProve(t.Context(), "billet-node.service")
	if err != nil {
		t.Fatalf("a failed unit was not accepted as stopped: %v", err)
	}
	if got.Gone != Yes {
		t.Errorf("Gone = %v, want yes: a failed unit is not running", got.Gone)
	}
	if !strings.Contains(got.How, "exit-code") {
		t.Errorf("How is %q, want how it ended", got.How)
	}
}

// orNone names an empty subtest.
func orNone(s string) string {
	if s == "" {
		return "no answer"
	}

	return s
}
