package tart

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// spawnedPIDs is where the fake runner records "<pid> <birth>" for every
// process it starts, so cleanup can prove identity before signalling.
const spawnedPIDs = ".billet-test-spawned"

// testOwner is a deployment id in the shape state.DeploymentID mints.
const testOwner = "0123456789abcdef0123456789abcdef"

// foreignOwner is a different deployment on the same machine.
const foreignOwner = "fedcba9876543210fedcba9876543210"

// testImage is the remote reference the ordinary spec names. newStub marks it
// pulled, because an unpulled reference refuses the launch by design.
const testImage = "ghcr.io/cirruslabs/macos-tahoe-base:latest"

// stub is the fake tart binary plus the files it records into.
//
// It keeps VM state where tart does — a file per VM under home/vms/<name> — so
// `list` is genuinely derived from what clone, run, stop and delete did, rather
// than from a canned fixture that agrees with the test by construction. Its
// rename REFUSES to move a staging directory that carries no ownership marker,
// which turns the clone→mark→rename ordering from a comment into something a
// reordering fails against.
type stub struct {
	bin          string
	home         string
	guestHome    string
	argvLog      string
	execStdin    string
	execFails    string
	stopNoop     string
	execLoseResp string
	listLies     string
	listGhost    string
	pulled       string
	execStdinTmp string
	noPS         string
	runFails     string
	// proveSays makes a non-delivery exec print exactly these bytes, which is how
	// a guest chooses what the liveness proof reads on stdout.
	proveSays string
	// listStealsMarker makes `tart list` rewrite a VM's ownership marker as it
	// answers, which places the replacement EXACTLY between the observation and
	// the delete. Deterministic where a sleep is not.
	listStealsMarker string
	// listHangs makes `tart list` block, so a bound on the poll can be observed
	// rather than assumed.
	listHangs string
	// stopSlow holds a countdown: that many `tart list` observations report the
	// VM still running after a stop before it settles. It is the state a real
	// `tart stop` produces — a request, not a completion — and it is what tells a
	// polling proof from a one-shot read.
	stopSlow string
	// getBlind makes `tart get` answer "does not exist" for every name, which is
	// what a TART_HOME mismatch looks like: billet's store holds the directories
	// and tart is looking somewhere else entirely.
	getBlind string
	// execReflects is a guest whose shell copies its stdin to stderr — the
	// smallest thing that would put the runner's registration into a billet log
	// if billet ever rendered a delivery's stderr.
	execReflects string
	// pullEvicts makes a pull exit 0 having left nothing in the store, which is
	// what tart's automatic pruning does on a disk too full to hold the image.
	pullEvicts  string
	execHang    string
	execHanging string
	// cloneStealsName makes `tart clone` also create a VM directory under the
	// name held there, which puts another billet's VM under this lease's name
	// AFTER Launch's inventory read and BEFORE its rename — deterministically,
	// where a sleep would leave the ordering to chance.
	cloneStealsName string
	// cloneBlocks holds the clone open: the stub announces it has reached the
	// clone (cloneReached) and waits for the test to let it go (cloneRelease).
	// That is what lets a test take the store lock in the window between
	// prepareStaging releasing it and publishStaging asking for it, which is the
	// only window where the rename's lock is the thing under test.
	cloneBlocks  string
	cloneReached string
	cloneRelease string
}

func newStub(t *testing.T) *stub {
	t.Helper()

	dir := t.TempDir()
	s := &stub{
		bin:              filepath.Join(dir, "tart"),
		home:             filepath.Join(dir, "tart-home"),
		guestHome:        filepath.Join(dir, "guest-home"),
		argvLog:          filepath.Join(dir, "argv.log"),
		execStdin:        filepath.Join(dir, "exec-stdin"),
		execFails:        filepath.Join(dir, "exec-fails"),
		stopNoop:         filepath.Join(dir, "stop-noop"),
		execLoseResp:     filepath.Join(dir, "exec-lose-response"),
		listLies:         filepath.Join(dir, "list-lies"),
		listGhost:        filepath.Join(dir, "list-ghost"),
		pulled:           filepath.Join(dir, "pulled"),
		execStdinTmp:     filepath.Join(dir, "exec-stdin-tmp"),
		noPS:             filepath.Join(dir, "no-ps"),
		runFails:         filepath.Join(dir, "run-fails"),
		pullEvicts:       filepath.Join(dir, "pull-evicts"),
		execReflects:     filepath.Join(dir, "exec-reflects"),
		getBlind:         filepath.Join(dir, "get-blind"),
		proveSays:        filepath.Join(dir, "prove-says"),
		stopSlow:         filepath.Join(dir, "stop-slow"),
		listStealsMarker: filepath.Join(dir, "list-steals-marker"),
		listHangs:        filepath.Join(dir, "list-hangs"),
		execHang:         filepath.Join(dir, "exec-hang-after"),
		execHanging:      filepath.Join(dir, "exec-hanging"),
		cloneStealsName:  filepath.Join(dir, "clone-steals-name"),
		cloneBlocks:      filepath.Join(dir, "clone-blocks"),
		cloneReached:     filepath.Join(dir, "clone-reached"),
		cloneRelease:     filepath.Join(dir, "clone-release"),
	}

	if err := os.MkdirAll(filepath.Join(s.home, "vms"), 0o755); err != nil {
		t.Fatalf("mkdir vms: %v", err)
	}

	if err := os.MkdirAll(s.guestHome, 0o755); err != nil {
		t.Fatalf("mkdir guest home: %v", err)
	}

	// The fake guest gets a runner that STAYS UP, because proveRunning asks
	// whether it did. A guest whose runner exits at once is a real failure the
	// provider must report, and it has its own test rather than being the
	// accidental state of every other one.
	// Short enough that a leaked one is gone in half a minute, long enough that
	// no reasonable test races its exit.
	runner := filepath.Join(s.guestHome, "run.sh")
	body := "#!/bin/sh\n" +
		"printf '%s %s\\n' \"$$\" \"$(ps -p $$ -o lstart=)\" >> \"$HOME/" + spawnedPIDs + "\"\n" +
		"exec sleep 30\n"

	if err := os.WriteFile(runner, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake runner: %v", err)
	}

	// Those sleeps are real host processes: without this they outlive the test
	// binary, which is the "goroutines outliving their test" rule one level out.
	t.Cleanup(func() { s.killRunner(t) })

	if err := os.WriteFile(s.pulled, []byte(testImage+"\n"), 0o600); err != nil {
		t.Fatalf("mark test image pulled: %v", err)
	}

	script := `#!/bin/sh
vms="` + s.home + `/vms"
printf '%s\n' "$*" >> "` + s.argvLog + `"
cmd="$1"; shift
case "$cmd" in
--version)
  printf '2.36.0\n'
  ;;
pull)
  # tart reclaims space from its own OCI cache to make an operation fit
  # ("automatic pruning"), so a pull can exit 0 and leave nothing behind — and a
  # later clone can evict what a pull just fetched. Measured on a full disk.
  name="$1"
  if [ -s "` + s.pullEvicts + `" ]; then
    exit 0
  fi
  printf '%s\n' "$name" >> "` + s.pulled + `"
  ;;
fqn)
  # A remote reference resolves to its cached digest only when pulled (the
  # test marks pulled refs in the pulled file); a local name echoes back —
  # both phrasings pinned to tart's own VMStorageOCI behavior.
  name="$1"
  case "$name" in
  */*)
    if [ -s "` + s.pulled + `" ] && grep -qxF "$name" "` + s.pulled + `"; then
      printf '%s@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n' "${name%%:*}"
    else
      echo "OCI storage error: $name is not a digest and doesn't point to a digest" >&2
      exit 1
    fi
    ;;
  *)
    printf '%s\n' "$name"
    ;;
  esac
  ;;
clone)
  src="$1"; dst="$2"
  mkdir -p "$vms/$dst"
  printf 'stopped\n' > "$vms/$dst/.state"
  # Another billet puts a VM under this lease's name while the clone runs —
  # after Launch read the inventory and before it renames into that name.
  if [ -s "` + s.cloneStealsName + `" ]; then
    stolen=$(cat "` + s.cloneStealsName + `")
    mkdir -p "$vms/$stolen"
    printf 'stopped\n' > "$vms/$stolen/.state"
  fi
  # HELD OPEN so a test can act in the window between prepareStaging releasing
  # the store lock and publishStaging asking for it. Bounded, because a test
  # that never releases must fail rather than hang the suite.
  if [ -s "` + s.cloneBlocks + `" ]; then
    # NON-EMPTY, because waitForFile treats an empty file as not yet there.
    printf 'reached\n' > "` + s.cloneReached + `"
    n=0
    while [ ! -f "` + s.cloneRelease + `" ] && [ "$n" -lt 600 ]; do
      sleep 0.05
      n=$((n + 1))
    done
  fi
  ;;
rename)
  src="$1"; dst="$2"
  if [ ! -f "$vms/$src/billet-owner" ]; then
    echo "REORDERED: rename before the ownership marker was written" >&2
    exit 1
  fi
  mv "$vms/$src" "$vms/$dst"
  ;;
set)
  ;;
run)
  name="$1"
  if [ -s "` + s.runFails + `" ]; then
    cat "` + s.runFails + `"
    exit 1
  fi
  printf 'running\n' > "$vms/$name/.state"
  ;;
stop)
  name="$1"
  if [ ! -d "$vms/$name" ]; then
    echo "the specified VM \"$name\" does not exist" >&2
    exit 1
  fi
  state=$(cat "$vms/$name/.state")
  if [ "$state" != running ] && [ "$state" != suspended ]; then
    echo "VM \"$name\" is not running" >&2
    exit 1
  fi
  if [ -s "` + s.stopSlow + `" ]; then
    # A stop that has been REQUESTED and not yet completed: the state stays
    # running for this many further observations.
    :
  elif [ ! -f "` + s.stopNoop + `" ]; then
    printf 'stopped\n' > "$vms/$name/.state"
  fi
  ;;
delete)
  name="$1"
  if [ ! -d "$vms/$name" ]; then
    echo "the specified VM \"$name\" does not exist" >&2
    exit 1
  fi
  rm -rf "$vms/$name"
  ;;
get)
  # Pinned to what a REAL tart answers, measured rather than invented: a name
  # with no directory exits 2 with "does not exist", a directory tart cannot
  # read as a VM exits 1 with the layout complaint, and anything else describes
  # the VM. crossCheckStore tells a corpse from a live guest by exactly this.
  name="$1"
  if [ -s "` + s.getBlind + `" ] || [ ! -d "$vms/$name" ]; then
    echo "the specified VM \"$name\" does not exist" >&2
    exit 2
  fi
  if [ ! -f "$vms/$name/.state" ]; then
    echo "VM is missing files for a supported layout: standalone requires config.json, disk.img and nvram.bin; stacked requires config.json, manifest.json, overlay.asif and nvram.bin" >&2
    exit 1
  fi
  printf '{"CPU":4,"Memory":8192,"Disk":100,"Display":"1024x768"}\n'
  ;;
list)
  if [ -s "` + s.listHangs + `" ]; then
    sleep 60
  fi
  if [ -s "` + s.listStealsMarker + `" ]; then
    # Another deployment takes the name between billet observing this VM and
    # acting on it.
    printf '%s\n' "$(cat "` + s.listStealsMarker + `")" > "$vms/billet-lease1/` + ownerMarker + `"
  fi
  out="["
  sep=""
  for d in "$vms"/*/; do
    [ -d "$d" ] || continue
    name=$(basename "$d")
    # A lying inventory: the named VM exists on disk and is omitted from the
    # answer, which is the silent-shrink failure List must refuse to trust.
    if [ -s "` + s.listLies + `" ] && [ "$name" = "$(cat "` + s.listLies + `")" ]; then
      continue
    fi
    # A directory tart cannot read as a VM is not listed AT ALL — measured
    # against a real tart, which omits it and answers the get subcommand with
    # the layout complaint. The stub used to report it as stopped, which made a
    # corpse indistinguishable from a healthy stopped VM and hid the case
    # crossCheckStore exists for.
    [ -f "$d/.state" ] || continue
    if [ -s "` + s.stopSlow + `" ]; then
      n=$(cat "` + s.stopSlow + `")
      if [ "$n" -gt 0 ]; then
        printf '%s\n' $((n - 1)) > "` + s.stopSlow + `"
      else
        printf 'stopped\n' > "$d/.state"
      fi
    fi
    state=$(cat "$d/.state")
    running=false
    [ "$state" = running ] && running=true
    out="$out$sep{\"Source\":\"local\",\"Name\":\"$name\",\"Running\":$running,\"State\":\"$state\"}"
    sep=","
  done
  # A ghost row: a name the listing reports while no directory backs it, which
  # is what a TART_HOME disagreement between billet and tart looks like.
  if [ -s "` + s.listGhost + `" ]; then
    out="$out$sep{\"Source\":\"local\",\"Name\":\"$(cat "` + s.listGhost + `")\",\"Running\":true,\"State\":\"running\"}"
  fi
  printf '%s]\n' "$out"
  ;;
exec)
  # argv: -i <name> /bin/sh -c <script>; stdin carries the registration. The
  # script is EXECUTED under a fake guest home, not merely recorded: the
  # sentinel, the quoting and the spawn are contracts a grep cannot check.
  if [ "$1" = "--help" ]; then
    echo "USAGE: tart exec [-i] [-t] <name> <command> ..."
    exit 0
  fi
  # REMEMBERED, not just consumed: -i marks a delivery (it carries stdin), and
  # everything else is a query. The talkative-guest branch below needs to tell
  # them apart, and a terminal test cannot: neither call has one.
  interactive=no
  if [ "$1" = "-i" ]; then interactive=yes; shift; fi
  name="$1"; shift
  if [ -s "` + s.execFails + `" ]; then
    n=$(cat "` + s.execFails + `")
    if [ "$n" -gt 0 ]; then
      printf '%s\n' $((n - 1)) > "` + s.execFails + `"
      echo "Failed to connect to the VM using its control socket, is the Tart Guest Agent running?" >&2
      exit 1
    fi
  fi
  # A delivery carries stdin; a query (proveRunning) does not. Only the
  # delivery's stdin is recorded, so a later query cannot erase the credential
  # the delivery test asserts on.
  # A guest that cannot identify a pid: with both of billet_birth's sources
  # shadowed (ps here, and /proc reads through cat) it yields nothing, which is
  # how a launcher reaches its identity check on either platform.
  if [ -s "` + s.noPS + `" ]; then
    PATH="` + s.noPSDir() + `:$PATH"
    export PATH
  fi
  # FAIL SUBSTANTIVELY, THEN HANG. The first N invocations report a reason of
  # their own; every one after that blocks. exec, so the sleep IS the process
  # the context kills — as a child it outlives the shell holding the inherited
  # stderr pipe, and Run() waits for that pipe to close.
  # That is the shape a deadline landing INSIDE an attempt produces, and it is
  # not reproducible by timing alone.
  if [ -s "` + s.execHang + `" ]; then
    n=$(cat "` + s.execHang + `")
    if [ "$n" -gt 0 ]; then
      printf '%s\n' $((n - 1)) > "` + s.execHang + `"
      # A DISTINCTIVE EXIT STATUS rather than a stderr line: billet discards a
      # delivery's stderr, so the status is what can still tell "this attempt
      # failed for its own reason" from "this attempt was cut short".
      exit 3
    fi
    : > "` + s.execHanging + `"
    exec sleep 30
  fi
  if [ -s "` + s.execReflects + `" ]; then
    # The reflecting guest: everything on stdin comes straight back on stderr.
    # RECORDED FIRST, so a test can prove the registration actually arrived —
    # otherwise "no secret in the error" is also true of a delivery that never
    # sent one.
    cat > "` + s.execStdinTmp + `"
    if [ -s "` + s.execStdinTmp + `" ]; then cp "` + s.execStdinTmp + `" "` + s.execStdin + `"; fi
    cat "` + s.execStdinTmp + `" >&2
    exit 1
  fi
  if [ -s "` + s.proveSays + `" ] && [ "$interactive" = no ]; then
    # A talkative guest: the liveness query gets bytes of the guest's choosing.
    cat "` + s.proveSays + `"
    exit 0
  fi
  if [ "$interactive" = no ]; then
    HOME="` + s.guestHome + `" "$@" < /dev/null
  else
    cat > "` + s.execStdinTmp + `"
    if [ -s "` + s.execStdinTmp + `" ]; then cp "` + s.execStdinTmp + `" "` + s.execStdin + `"; fi
    HOME="` + s.guestHome + `" "$@" < "` + s.execStdinTmp + `"
    # PROPAGATED, because real tart returns the remote command's status. Letting
    # the case branch's own status stand made every failed delivery look like a
    # success, which hid the exact class of bug this suite exists to catch.
    rc=$?
    # A LOST RESPONSE: the guest ran the script to completion and the caller is
    # told the call failed. Delivery retries, and nothing may spawn twice.
    if [ -s "` + s.execLoseResp + `" ]; then
      n=$(cat "` + s.execLoseResp + `")
      if [ "$n" -gt 0 ]; then
        printf '%s\n' $((n - 1)) > "` + s.execLoseResp + `"
        echo "connection reset after the command ran" >&2
        exit 1
      fi
    fi
    exit "$rc"
  fi
  ;;
*)
  echo "stub: unknown command $cmd" >&2
  exit 64
  ;;
esac
`

	if err := os.WriteFile(s.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	return s
}

// vm plants a VM directory the way a previous billet (or another deployment,
// or an operator's own tooling) would have left it.
func (s *stub) vm(t *testing.T, name, state, owner string) {
	t.Helper()

	dir := filepath.Join(s.home, "vms", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}

	if err := os.WriteFile(filepath.Join(dir, ".state"), []byte(state+"\n"), 0o600); err != nil {
		t.Fatalf("state %s: %v", name, err)
	}

	if owner != "" {
		if err := os.WriteFile(filepath.Join(dir, ownerMarker), []byte(owner+"\n"), 0o600); err != nil {
			t.Fatalf("marker %s: %v", name, err)
		}
	}
}

// killRunner stops the fake guest's runners.
//
// EVERY recorded pid, not just the last one: a test that removes the claim and
// delivers again overwrites the pid file, and the earlier runner would be left
// behind. And each pid is checked against the BIRTH TOKEN recorded beside it
// before anything is signalled — a fake runner exits on its own, the host
// recycles its number, and a cleanup that signals a bare pid can kill an
// unrelated process on the developer's machine.
func (s *stub) killRunner(t *testing.T) {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(s.guestHome, spawnedPIDs))
	if err != nil {
		return
	}

	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		pid, birth, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}

		n, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}

		// The same identity check proveRunning makes, for the same reason.
		out, err := exec.CommandContext(t.Context(), "ps", "-p", pid, "-o", "lstart=").Output()
		if err != nil || strings.TrimSpace(string(out)) != birth {
			continue
		}

		proc, err := os.FindProcess(n)
		if err != nil {
			continue
		}

		if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Logf("could not stop the fake runner %d: %v", n, err)
		}
	}
}

// stopStandIn kills a test stand-in process AND REAPS IT.
//
// The reaping is the part that matters: a killed child remains a zombie until
// its parent waits for it, and `kill -0` on a zombie succeeds — so a corpse
// still reads as a living runner to the very check under test.
func stopStandIn(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Logf("could not stop the stand-in %d: %v", cmd.Process.Pid, err)
	}

	// The error is the stand-in's own exit status — it was killed, so it is
	// always non-nil and never interesting.
	if err := cmd.Wait(); err != nil {
		t.Logf("stand-in %d exited: %v", cmd.Process.Pid, err)
	}
}

// birthToken returns the identity billet itself would compute for a pid, by
// running the very function the guest runs. Anything else risks agreeing with
// the test and disagreeing with production.
func birthToken(t *testing.T, pid int) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c",
		birthFunc+`billet_birth "$1"`, "sh", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("billet_birth(%d): %v", pid, err)
	}

	if strings.TrimSpace(string(out)) == "" {
		t.Fatalf("billet_birth(%d) returned nothing", pid)
	}

	return string(out)
}

// noPSDir shadows the tools billet_birth reads an identity through, used to
// drive the launcher's identity check.
func (s *stub) noPSDir() string { return filepath.Dir(s.noPS) + "/nops" }

// breakIdentity makes billet_birth unable to answer, ON EITHER PLATFORM.
//
// IT TAKES TWO SHIMS, BECAUSE billet_birth HAS TWO SOURCES. macOS has no /proc
// so it asks `ps -o lstart=`, and refusing that was once enough — but Linux
// reads /proc/<pid>/stat first and never reaches ps at all, so a live pid on a
// Linux runner still yielded a token, the launcher announced, and a test named
// for the failure asserted against a success. The `cat` shim refuses ONLY a
// /proc path, so the identity read fails while the stub's own plumbing — which
// pipes stdin through `cat` with no arguments, after this directory is already
// on PATH — keeps working.
func (s *stub) breakIdentity(t *testing.T) {
	t.Helper()

	dir := s.noPSDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir nops: %v", err)
	}

	shims := map[string]string{
		// macOS: the only source there.
		"ps": "#!/bin/sh\nexit 1\n",
		// Linux: billet_birth reads /proc/<pid>/stat through cat, and treats a
		// failed read as "cannot tell" rather than falling through to ps.
		// Everything else is handed to the real cat.
		"cat": `#!/bin/sh
case "${1:-}" in
  /proc/*) exit 1 ;;
esac
for c in /bin/cat /usr/bin/cat; do
  if [ -x "$c" ]; then exec "$c" "$@"; fi
done
exit 127
`,
	}
	for name, body := range shims {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s shim: %v", name, err)
		}
	}

	if err := os.WriteFile(s.noPS, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm no-ps: %v", err)
	}

	// THE FIXTURE PROVES ITSELF BEFORE THE TEST RELIES ON IT.
	//
	// These shims are files this process wrote and something else then execs BY
	// NAME through PATH. That exec can fail for reasons unrelated to what they
	// contain — on Linux a concurrent fork inheriting the still-open write
	// descriptor makes execve return ETXTBSY until that child exits, which is
	// the mechanism proven for a different package. A shim that cannot be
	// exec'd is not a shim that reports failure: the lookup can reach the real
	// `ps`, identity succeeds, and the launch this test expects to be refused
	// is allowed.
	//
	// That is how this arrived in CI — "Launch succeeded although the launcher
	// could not identify itself", in about a second — and from that message
	// alone there is no way to tell a product defect from a fixture that never
	// took effect. Checking here separates them: if the shim does not resolve
	// and fail as intended, the test says the SETUP is broken.
	// BY ABSOLUTE PATH, because Go resolves a bare name through the PARENT's
	// PATH and ignores cmd.Env — an earlier version of this check set Env and
	// ran the real `ps`, which of course succeeded. What matters here is the
	// file itself: that it can be exec'd at all, and that it fails when it is.
	for _, probe := range []struct{ name, arg string }{
		{"ps", ""},
		// The cat shim only refuses /proc paths and hands everything else to the
		// real cat, which with no argument would read stdin and hang.
		{"cat", "/proc/1/stat"},
	} {
		args := []string(nil)
		if probe.arg != "" {
			args = append(args, probe.arg)
		}

		if err := exec.CommandContext(t.Context(),
			filepath.Join(dir, probe.name), args...).Run(); err == nil {
			t.Fatalf("the %s shim ran and SUCCEEDED, so identity was never broken and this "+
				"test would pass or fail for reasons of its own", probe.name)
		}
	}
}

func (s *stub) dirExists(name string) bool {
	_, err := os.Stat(filepath.Join(s.home, "vms", name))

	return err == nil
}

func (s *stub) argv(t *testing.T) string {
	t.Helper()

	b, err := os.ReadFile(s.argvLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read argv log: %v", err)
	}

	return string(b)
}

func newProvider(t *testing.T, s *stub) *Provider {
	t.Helper()

	p, err := New(testOwner, WithBinary(s.bin), WithHome(s.home))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	p.execRetry = time.Millisecond
	p.proveRetry = time.Millisecond
	// WIDE ENOUGH FOR TWO SAMPLES ON A BUSY MACHINE. proveSamples is 2 and each
	// sample spawns the stub, so under a loaded `make check` a two-second
	// window fits only ONE — and proveRunning then reports "never stayed
	// running (the guest last said alive)", which is a true statement about a
	// budget rather than about the code under test. Seen in another session's
	// run before it was diagnosed here. The retry stays at a millisecond, so a
	// healthy proof is still instant; only a test that deliberately waits out
	// the deadline pays this.
	p.proveWindow = 20 * time.Second
	p.startWindow = 2 * time.Second
	// The WINDOW stays at its default, because an uncontended acquisition never
	// waits; only the tests that deliberately hold the lock shorten it.
	p.storeLockRetry = time.Millisecond

	return p
}

func validSpec(name string) provider.Spec {
	return provider.Spec{
		Name:      name,
		Image:     testImage,
		VCPU:      6,
		Memory:    24 * config.GiB,
		Disk:      100 * config.GiB,
		Trust:     provider.TrustTrusted,
		JITConfig: "eyJ0b2tlbiI6IlNVUEVSU0VDUkVUUkVHSVNUUkFUSU9OIn0=",
		Command:   []string{"./run.sh"},
	}
}

// THE JIT CONFIG MUST NEVER REACH ARGV — on the host, where every process reads
// ps, or in the guest script, which is itself host argv. It travels on stdin to
// the guest agent, so the recorded argv must not contain it and the recorded
// stdin must.
func TestTheJITConfigNeverAppearsInArgv(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	spec := validSpec("billet-lease1")

	inst, err := p.Launch(t.Context(), spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if inst.Name != spec.Name {
		t.Errorf("instance name = %q, want %q", inst.Name, spec.Name)
	}

	argv := s.argv(t)
	if strings.Contains(argv, spec.JITConfig) {
		t.Errorf("the JIT config was passed on a command line, where ps exposes it:\n%s", argv)
	}

	if !strings.Contains(argv, "exec -i "+spec.Name) {
		t.Errorf("no `exec -i` ran, so the registration was never delivered:\n%s", argv)
	}

	// And it was actually delivered: absence alone would pass against a provider
	// that forgot the credential entirely.
	stdin, err := os.ReadFile(s.execStdin)
	if err != nil {
		t.Fatalf("read exec stdin: %v", err)
	}

	if !strings.Contains(string(stdin), spec.JITConfig) {
		t.Error("the registration never reached the guest agent's stdin")
	}

	// The command reaches the guest quoted, and the sentinel guard is present so
	// a redelivery cannot start a second runner.
	if !strings.Contains(argv, "'./run.sh'") {
		t.Errorf("the tier command was not quoted into the guest script:\n%s", argv)
	}

	if !strings.Contains(argv, launchClaim) {
		t.Errorf("the guest script takes no atomic launch claim, so a retried delivery "+
			"could start a second runner:\n%s", argv)
	}
}

// The marker is what List trusts when it decides whose compute a VM is, so it
// must exist before the VM can carry a lease name. The stub's rename fails if
// the marker is absent at rename time, so this test also fails against a
// reordering — and the final state carries this deployment's identity.
func TestLaunchWritesTheOwnerMarkerBeforeTheLeaseName(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	owner, err := p.ownerOf("billet-lease1")
	if err != nil {
		t.Fatalf("ownerOf: %v", err)
	}

	if owner != testOwner {
		t.Errorf("marker = %q, want %q", owner, testOwner)
	}

	if s.dirExists("billet-lease1" + stagingSuffix) {
		t.Error("the staging clone outlived the launch")
	}
}

// Launch is idempotent by lease identity: a running VM under this lease's name
// and this deployment's marker is adopted, not duplicated — and the
// registration is REDELIVERED, because the earlier attempt proved the VM boots
// and said nothing about whether the credential ever arrived. The sentinel is
// what makes that redelivery unable to start a second runner.
func TestLaunchAdoptsARunningLeaseAndRedeliversTheRegistration(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	p := newProvider(t, s)

	inst, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !inst.Running {
		t.Error("the adopted instance must report running")
	}

	if strings.Contains(s.argv(t), "clone") {
		t.Errorf("a second clone ran for a lease that already has a VM:\n%s", s.argv(t))
	}

	stdin, err := os.ReadFile(s.execStdin)
	if err != nil {
		t.Fatalf("read exec stdin: %v", err)
	}

	if !strings.Contains(string(stdin), validSpec("billet-lease1").JITConfig) {
		t.Error("adoption returned success without redelivering the registration; a launch " +
			"that failed between boot and delivery would be recorded as a runner that never registers")
	}
}

// A stopped VM under the lease name is an earlier attempt whose registration
// may already be spent. Booting it would hand a possibly-consumed credential to
// a guest billet believes is fresh, so Launch refuses rather than guesses.
func TestLaunchRefusesAStoppedCorpseUnderTheLeaseName(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "stopped", testOwner)

	p := newProvider(t, s)

	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	if err == nil || !strings.Contains(err.Error(), "destroy it before relaunching") {
		t.Fatalf("Launch = %v, want a refusal naming the stopped VM", err)
	}
}

// Untrusted and unclassified work is refused until this backend manages guest
// network isolation: tart's default NAT reaches the host and other guests.
func TestUntrustedAndUnknownWorkIsRefused(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	for _, trust := range []provider.TrustClass{provider.TrustUntrusted, provider.TrustUnknown} {
		if err := p.Accepts(trust); err == nil {
			t.Errorf("Accepts(%s) = nil, want a refusal", trust)
		}

		spec := validSpec("billet-lease1")
		spec.Trust = trust

		if _, err := p.Launch(t.Context(), spec); err == nil ||
			!strings.Contains(err.Error(), "refusing to run") {
			t.Errorf("Launch(%s) = %v, want a refusal", trust, err)
		}
	}

	if strings.Contains(s.argv(t), "clone") {
		t.Errorf("a refused launch still cloned:\n%s", s.argv(t))
	}
}

// A spec this backend cannot honour fails the launch rather than launching a
// guest that silently lacks what was asked for.
func TestASpecAskingForWhatTartCannotDoIsRefused(t *testing.T) {
	cases := map[string]func(*provider.Spec){
		"cache volumes":  func(s *provider.Spec) { s.Volumes = []provider.VolumeMount{{Device: "/dev/x"}} },
		"cache endpoint": func(s *provider.Spec) { s.CacheEndpoint = "https://127.0.0.1:1" },
		"actions proxy":  func(s *provider.Spec) { s.ActionsProxy = "https://127.0.0.1:1" },
		"instance type":  func(s *provider.Spec) { s.InstanceType = "m7g.large" },
		"shm":            func(s *provider.Spec) { s.SHM = config.GiB },
		"no command":     func(s *provider.Spec) { s.Command = nil },
		"no jit":         func(s *provider.Spec) { s.JITConfig = "" },
		"no image":       func(s *provider.Spec) { s.Image = "" },
		"flag image":     func(s *provider.Spec) { s.Image = "--net-softnet" },
		"foreign name":   func(s *provider.Spec) { s.Name = "someone-elses-vm" },
		// The staging suffix is RESERVED: List reads it as proof a VM was never
		// launched, so a VM launched under one would be classified as a corpse
		// and have its capacity freed while it ran.
		"staging name": func(s *provider.Spec) { s.Name += stagingSuffix },
	}

	for name, mutate := range cases {
		s := newStub(t)
		p := newProvider(t, s)

		spec := validSpec("billet-lease1")
		mutate(&spec)

		if _, err := p.Launch(t.Context(), spec); err == nil {
			t.Errorf("%s: Launch succeeded, want a refusal", name)
		}

		if strings.Contains(s.argv(t), "clone") {
			t.Errorf("%s: a refused launch still cloned", name)
		}
	}
}

// List reports exactly this deployment's lease-named VMs: not another
// deployment's, not an operator's own VMs, not staging clones.
func TestListReportsOnlyThisDeploymentsVMs(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-ours", "running", testOwner)
	s.vm(t, "billet-theirs", "running", foreignOwner)
	s.vm(t, "operators-own-vm", "running", "")
	s.vm(t, "billet-staged"+stagingSuffix, "stopped", testOwner)

	p := newProvider(t, s)

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(instances) != 1 || instances[0].Name != "billet-ours" {
		t.Fatalf("List = %+v, want exactly billet-ours", instances)
	}

	if !instances[0].Running {
		t.Error("a running VM must report running")
	}
}

// A lease-named VM always carries a marker — the rename happens after the sync
// — so one without a readable marker is not billet's crash artifact, and both
// guesses are dangerous: omission resells its capacity, adoption may destroy a
// stranger's guest. List must stop and say so.
func TestListRefusesALeaseNamedVMWithNoMarker(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-mystery", "running", "")

	p := newProvider(t, s)

	_, err := p.List(t.Context())
	if err == nil || !strings.Contains(err.Error(), "ownership marker cannot be read") {
		t.Fatalf("List = %v, want a refusal naming the unreadable marker", err)
	}
}

// A suspended VM is not executing but can return to executing; treating it as
// finished destroys the frozen job. An unknown state counts as running for the
// same reason.
func TestSuspendedAndUnknownStatesCountAsRunning(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-frozen", "suspended", testOwner)
	s.vm(t, "billet-odd", "hibernating", testOwner)
	s.vm(t, "billet-done", "stopped", testOwner)

	p := newProvider(t, s)

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	running := map[string]bool{}
	for _, inst := range instances {
		running[inst.Name] = inst.Running
	}

	if !running["billet-frozen"] {
		t.Error("a suspended VM must count as running")
	}

	if !running["billet-odd"] {
		t.Error("an unrecognised state must count as running")
	}

	if running["billet-done"] {
		t.Error("a stopped VM must not count as running")
	}
}

// Destroy returns TeardownStopped only after tart itself reports the VM
// stopped. A stop that did not stick must keep the VM, its directory and the
// capacity charge.
func TestDestroyProvesStoppedBeforeDeleting(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	if err := os.WriteFile(s.stopNoop, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm stop-noop: %v", err)
	}

	p := newProvider(t, s)
	// Destroy now WAITS for the state to settle, because `tart stop` requests a
	// stop rather than completing one. This VM never stops, so the test is
	// about the timeout being reached — shortened here rather than waiting out
	// the shipped thirty seconds.
	// Long enough for a couple of observations to COMPLETE — the window now
	// bounds the list calls themselves, so one shorter than a single list makes
	// this report a killed command rather than a VM that never stopped.
	p.stopWindow = 2 * time.Second

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err == nil ||
		(!strings.Contains(err.Error(), "still reports state") &&
			!strings.Contains(err.Error(), "did not report itself stopped")) {
		t.Fatalf("Destroy = %v, want an error saying it never stopped", err)
	}

	if got != provider.TeardownRequested {
		t.Errorf("Teardown = %v, want requested: the compute was not proved gone", got)
	}

	if !s.dirExists("billet-lease1") {
		t.Error("the VM directory was deleted although the VM never stopped — the evidence " +
			"that it exists is gone while the VMM keeps executing")
	}
}

// The ordinary destroy: stop, prove stopped, delete, confirm.
func TestDestroyStopsDeletesAndConfirms(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	p := newProvider(t, s)

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if got != provider.TeardownStopped {
		t.Errorf("Teardown = %v, want stopped", got)
	}

	if s.dirExists("billet-lease1") {
		t.Error("the VM directory survived the destroy")
	}
}

// A missing directory is only "already gone" when tart AGREES. A listing that
// still reports the name while billet sees no directory means the two are
// reading different stores — and answering TeardownStopped there frees the
// capacity of every VM on a misconfigured host at once.
func TestDestroyRefusesAMissingDirectoryTartStillReports(t *testing.T) {
	s := newStub(t)

	if err := os.WriteFile(s.listGhost, []byte("billet-ghost"), 0o600); err != nil {
		t.Fatalf("arm list-ghost: %v", err)
	}

	p := newProvider(t, s)

	got, err := p.Destroy(t.Context(), "billet-ghost")
	if err == nil || !strings.Contains(err.Error(), "TART_HOME") {
		t.Fatalf("Destroy = %v, want a refusal naming the store disagreement", err)
	}

	if got != provider.TeardownRequested {
		t.Errorf("Teardown = %v, want requested: nothing was proved about the ghost's compute", got)
	}
}

// Destroying what is already gone is success, and quietly: teardown runs on
// paths that have already failed once.
func TestDestroyOfAMissingVMIsSuccess(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	got, err := p.Destroy(t.Context(), "billet-nothing")
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if got != provider.TeardownStopped {
		t.Errorf("Teardown = %v, want stopped: tart is authoritative about its local store", got)
	}

	if argv := s.argv(t); strings.Contains(argv, "stop") || strings.Contains(argv, "delete") {
		t.Errorf("a destroy of nothing still ran commands:\n%s", argv)
	}
}

// Another deployment's guest is never billet's to destroy, and an unreadable
// marker is not authority either.
func TestDestroyRefusesAForeignOrUnprovableVM(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-theirs", "running", foreignOwner)
	s.vm(t, "billet-mystery", "running", "")

	p := newProvider(t, s)

	for name, want := range map[string]string{
		"billet-theirs":  "another billet deployment",
		"billet-mystery": "ownership cannot be proved",
	} {
		got, err := p.Destroy(t.Context(), name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Destroy(%s) = %v, want an error containing %q", name, err, want)
		}

		if got != provider.TeardownRequested {
			t.Errorf("Destroy(%s) teardown = %v, want requested", name, got)
		}

		if !s.dirExists(name) {
			t.Errorf("Destroy(%s) removed a VM it refused to own", name)
		}
	}
}

// A name that is not billet's shape is refused before anything runs: Destroy
// feeds on reconciliation output, and a stray argument must not become a
// deletion.
func TestDestroyRefusesANonBilletName(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	got, err := p.Destroy(t.Context(), "someones-vm")
	if err == nil || !strings.Contains(err.Error(), "not a billet instance name") {
		t.Fatalf("Destroy = %v, want a refusal naming the shape", err)
	}

	if got != provider.TeardownRequested {
		t.Errorf("Teardown = %v, want requested", got)
	}
}

// The guest agent is not listening while macOS boots, so delivery retries
// until the caller's deadline rather than failing the launch on the first
// connection refusal.
func TestRegistrationDeliveryRetriesWhileTheGuestBoots(t *testing.T) {
	s := newStub(t)

	if err := os.WriteFile(s.execFails, []byte("3"), 0o600); err != nil {
		t.Fatalf("arm exec-fails: %v", err)
	}

	p := newProvider(t, s)

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	stdin, err := os.ReadFile(s.execStdin)
	if err != nil {
		t.Fatalf("read exec stdin: %v", err)
	}

	if !strings.Contains(string(stdin), validSpec("billet-lease1").JITConfig) {
		t.Error("the registration never arrived despite retries")
	}
}

// A leftover staging clone from a crashed attempt is cleared when it is
// provably ours, and refused when it is not.
func TestALeftoverStagingCloneIsClearedOnlyWhenProvablyOurs(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1"+stagingSuffix, "stopped", testOwner)

	p := newProvider(t, s)

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Fatalf("Launch over our own leftover staging: %v", err)
	}

	s2 := newStub(t)
	s2.vm(t, "billet-lease2"+stagingSuffix, "stopped", foreignOwner)

	p2 := newProvider(t, s2)

	if _, err := p2.Launch(t.Context(), validSpec("billet-lease2")); err == nil ||
		!strings.Contains(err.Error(), "another billet deployment") {
		t.Fatalf("Launch = %v, want a refusal over the foreign staging clone", err)
	}

	if !s2.dirExists("billet-lease2" + stagingSuffix) {
		t.Error("a foreign staging clone was removed")
	}
}

// An fqn answer that is neither a digest nor the local name echoed back is a
// reference that could move, and cloning it makes the pin decorative. The stub
// cannot produce this shape, so the seam is exercised directly.
func TestResolveImageRefusesAnUnpinnedRemoteAnswer(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	// A stub that echoes the remote tag back unpinned: the wrapper-or-future-
	// tart failure the fail-closed switch exists for.
	echo := filepath.Join(t.TempDir(), "tart-echo")
	script := "#!/bin/sh\nif [ \"$1\" = fqn ]; then printf '%s\\n' \"$2\"; exit 0; fi\nexit 64\n"

	if err := os.WriteFile(echo, []byte(script), 0o755); err != nil {
		t.Fatalf("write echo stub: %v", err)
	}

	p.tart = echo

	_, err := p.resolveImage(t.Context(), "ghcr.io/example/image:latest")
	if err == nil || !strings.Contains(err.Error(), "reference that could move") {
		t.Fatalf("resolveImage = %v, want a refusal of the unpinned answer", err)
	}
}

// The unpulled classification matches tart's WHOLE diagnostic, so an unrelated
// error that happens to echo the fragment is reported as itself rather than as
// "pull the image" — the wrong remediation hides the actual fault.
func TestAnUnrelatedErrorIsNotMisreadAsUnpulled(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	fail := filepath.Join(t.TempDir(), "tart-fail")
	script := "#!/bin/sh\necho \"registry timeout while checking whether \\\"x doesn't point to a digest\\\" applies\" >&2\nexit 1\n"

	if err := os.WriteFile(fail, []byte(script), 0o755); err != nil {
		t.Fatalf("write failing stub: %v", err)
	}

	p.tart = fail

	_, err := p.resolveImage(t.Context(), "ghcr.io/example/image:latest")
	if err == nil {
		t.Fatal("resolveImage succeeded against a failing tart")
	}

	if strings.Contains(err.Error(), "tart pull") {
		t.Errorf("an unrelated failure was misreported as an unpulled image: %v", err)
	}
}

// The per-name lock must exclude — at most one holder inside a name's critical
// section — and must reclaim its entry when the last holder leaves, because a
// map that only grows is a leak measured in jobs. Both properties are what the
// reference counting exists for, so both are asserted, including a third
// acquisition racing the final release.
func TestNameLocksExcludeAndReclaim(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	const workers = 8

	var (
		inside  int32
		maxSeen int32
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			unlock := p.lockName("billet-contended")

			mu.Lock()
			inside++

			if inside > maxSeen {
				maxSeen = inside
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()

			unlock()
		}()
	}

	wg.Wait()

	if maxSeen != 1 {
		t.Errorf("%d holders were inside one name's critical section at once", maxSeen)
	}

	// Reclamation: a third acquisition racing the final release must still leave
	// the map empty once everything unlocks.
	unlock := p.lockName("billet-contended")
	unlock()

	p.namesMu.Lock()
	remaining := len(p.names)
	p.namesMu.Unlock()

	if remaining != 0 {
		t.Errorf("%d lock entries survived their holders; the map grows once per job forever", remaining)
	}
}

// shellJoin must keep an argument holding spaces or quotes one word in the
// guest: the runner command is operator config, and re-splitting it in the
// guest shell runs a different command than the tier declared.
func TestShellJoinSurvivesHostileArguments(t *testing.T) {
	got := shellJoin([]string{"./run.sh", "--name", `it's "quoted" and spaced`})

	want := `'./run.sh' '--name' 'it'\''s "quoted" and spaced'`
	if got != want {
		t.Errorf("shellJoin = %s, want %s", got, want)
	}
}

// A launch clones the DIGEST the pulled tag resolves to, not the tag: a
// registry re-tag mid-fleet must not change what a lease already placed here
// boots, and the log records what actually launched.
func TestLaunchClonesTheDigestThePulledTagResolvesTo(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	argv := s.argv(t)
	if !strings.Contains(argv, "clone ghcr.io/cirruslabs/macos-tahoe-base@sha256:") {
		t.Errorf("the clone used the moving tag rather than the resolved digest:\n%s", argv)
	}
}

// An image that is not pulled refuses the launch and names the command, rather
// than starting a pull that starves every queued command's timeout.
func TestLaunchRefusesAnUnpulledImageByName(t *testing.T) {
	s := newStub(t)

	if err := os.WriteFile(s.pulled, nil, 0o600); err != nil {
		t.Fatalf("clear pulled marks: %v", err)
	}

	p := newProvider(t, s)

	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	if err == nil || !strings.Contains(err.Error(), "billet images pull") {
		t.Fatalf("Launch = %v, want a refusal naming the command that fetches it", err)
	}

	// AND THE OTHER CAUSE, because "pre-pull it" alone sends an operator to
	// repeat what they already did. tart reclaims its own OCI cache whenever an
	// operation needs room — a clone as well as a pull — so on a tight disk an
	// image billet fetched an hour ago is gone by the time a job wants it.
	// Measured on this machine, and it cost an hour of misdiagnosis.
	if !strings.Contains(err.Error(), "reclaims its own cache") {
		t.Errorf("Launch = %v, want the refusal to name automatic pruning as the other "+
			"way an image goes missing", err)
	}

	if strings.Contains(s.argv(t), "clone") {
		t.Errorf("a refused launch still cloned:\n%s", s.argv(t))
	}
}

// A VM THAT NEVER STARTS MUST SAY WHY. `tart run` is detached and its exit
// status is never believed, so without this the failure surfaces as a
// registration delivery timing out against a guest agent that was never there —
// and an operator reads a timeout and learns nothing.
//
// The reason is not hypothetical. MEASURED on a real Mac, a third concurrent
// macOS guest is refused with "The number of VMs exceeds the system limit
// (other running VMs: …)" — Apple's per-host limit, and exactly what
// config.DefaultMacOSVMLimit exists to keep billet inside.
func TestAVMThatNeverStartsReportsWhy(t *testing.T) {
	s := newStub(t)

	const reason = "The number of VMs exceeds the system limit " +
		"(other running VMs: billet-other1, billet-other2)"

	if err := os.WriteFile(s.runFails, []byte(reason+"\n"), 0o600); err != nil {
		t.Fatalf("arm run-fails: %v", err)
	}

	p := newProvider(t, s)

	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	if err == nil {
		t.Fatal("Launch succeeded although the VM never started")
	}

	if !strings.Contains(err.Error(), "never started") {
		t.Errorf("Launch = %v, want an error saying the VM never started", err)
	}

	// THE REASON ITSELF, not just that something failed: quoting the run log is
	// the entire point of the check.
	if !strings.Contains(err.Error(), "exceeds the system limit") {
		t.Errorf("Launch = %v, want the host's own reason quoted", err)
	}
}

// A pid proves a NUMBER is in use, not that it is still this runner. Guest
// kernels reuse pids, so without a birth token a recycled number reports a
// healthy runner where there is none — and billet would adopt a VM whose runner
// is gone and let the job sit queued.
func TestARecycledPIDIsNotAcceptedAsTheRunner(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	// The claim is taken, so delivery spawns nothing and goes straight to the
	// proof — and the announcement names a live process (this test) under a
	// birth token that cannot be its own.
	for name, body := range map[string]string{
		launchClaim:     "",
		runnerPIDFile:   strconv.Itoa(os.Getpid()) + "\n",
		runnerBirthFile: "a-birth-this-process-cannot-have\n",
	} {
		if err := os.WriteFile(filepath.Join(s.guestHome, name), []byte(body), 0o600); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}

	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	if err == nil {
		t.Fatal("Launch accepted a recycled pid as its runner")
	}

	if !strings.Contains(err.Error(), "recycled") {
		t.Errorf("Launch = %v, want an error naming the recycled pid", err)
	}
}

// One live reading is an INSTANT, not a state. The pid is announced just before
// the exec, so a single sample can catch a runner that is about to exit — the
// very failure being checked for, arriving a moment late. The proof must hold
// across samples.
func TestARunnerThatDiesBetweenSamplesIsNotProved(t *testing.T) {
	s := newStub(t)

	// A REAL process billet's own identity check will accept, planted as the
	// announcement — then killed once the first sample has been taken. Planting
	// beats racing a sleep: the first sample is alive and the second is dead by
	// construction, so a reverted proveSamples cannot pass by timing luck.
	victim := exec.CommandContext(t.Context(), "sleep", "120")
	if err := victim.Start(); err != nil {
		t.Fatalf("start the stand-in runner: %v", err)
	}

	t.Cleanup(func() { stopStandIn(t, victim) })

	pid := victim.Process.Pid

	// THROUGH BILLET'S OWN FUNCTION, not a hand-rolled `ps`. The first version
	// trimmed the output in Go while the shell does not — `ps -o lstart=` emits
	// leading spaces — so the planted token never matched, every launch failed
	// as "recycled", and the test passed without sampling anything at all.
	birth := birthToken(t, pid)

	for name, body := range map[string]string{
		launchClaim:     "",
		runnerBirthFile: birth,
		runnerPIDFile:   strconv.Itoa(pid) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(s.guestHome, name), []byte(body), 0o600); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}

	p := newProvider(t, s)
	p.proveWindow = 30 * time.Second
	// Long enough that the kill below lands between the two samples.
	p.proveRetry = 2 * time.Second

	// A BARRIER, NOT A TIMER: the victim dies once the FIRST proof has actually
	// been taken, so the second sample sees a dead process by construction. A
	// fixed delay raced Launch's own setup and let a one-sample proof pass.
	go func() {
		for {
			// KEYED ON THE PROOF, NOT ON THE DELIVERY. billet_birth appears in
			// both scripts, so waiting for it killed the victim before any
			// sample had been taken and the first reading was already dead —
			// which a one-sample proof passes just as happily. "no-identity" is
			// written only by the proof.
			if b, err := os.ReadFile(s.argvLog); err == nil &&
				strings.Contains(string(b), "no-identity") {
				// Long enough for the first sample to have finished, and well
				// inside the two-second gap before the second one.
				time.Sleep(300 * time.Millisecond)
				// REAPED, not merely killed. A killed child stays a zombie
				// until its parent waits, and `kill -0` on a zombie SUCCEEDS —
				// so without the wait the second sample still reads the corpse
				// as a living runner. In a real guest the runner is reparented
				// to init, which reaps it promptly; here the parent is this
				// test process.
				stopStandIn(t, victim)

				return
			}

			select {
			case <-t.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	if err == nil {
		t.Fatal("Launch proved a runner that died after the first sample")
	}

	if !strings.Contains(err.Error(), "not running") && !strings.Contains(err.Error(), "never stayed") {
		t.Errorf("Launch = %v, want an error naming the dead runner", err)
	}
}

// A LEGACY ANNOUNCEMENT MUST NOT BE SPAWNED BESIDE. A billet older than the
// claim wrote its pid before its post-spawn sentinel, so an interrupted
// delivery from that version leaves a live runner and NO claim. Winning a fresh
// claim over it and spawning would be exactly the two-runners-one-lease failure
// the claim exists to prevent.
func TestALegacyAnnouncementIsNeverSpawnedBeside(t *testing.T) {
	s := newStub(t)

	// The legacy state: an announcement, no claim.
	victim := exec.CommandContext(t.Context(), "sleep", "120")
	if err := victim.Start(); err != nil {
		t.Fatalf("start the legacy runner: %v", err)
	}

	t.Cleanup(func() { stopStandIn(t, victim) })

	if err := os.WriteFile(filepath.Join(s.guestHome, runnerPIDFile),
		[]byte(strconv.Itoa(victim.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatalf("plant the legacy pid: %v", err)
	}

	p := newProvider(t, s)

	spawns := filepath.Join(s.guestHome, spawnedPIDs)
	if _, err := os.Stat(spawns); err == nil {
		t.Fatal("the spawn record exists before the test ran")
	}

	// The launch fails, because a legacy announcement carries no identity and
	// cannot be proved — which is the correct outcome, and custody destroys the
	// VM. What must NOT happen is a second runner starting.
	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err == nil {
		t.Error("Launch proved a legacy announcement it cannot identify")
	}

	// ASSERTING ABSENCE NEEDS A WINDOW. A spawn writes its record from a
	// detached process, so reading the instant Launch returns would call a
	// late-arriving second runner "no second runner". The birth file is the
	// other tell — only a launcher writes one — and neither may appear.
	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(spawns); err == nil && len(strings.Fields(string(b))) > 0 {
			t.Fatalf("a second runner was spawned beside a legacy announcement: %s", b)
		}

		if _, err := os.Stat(filepath.Join(s.guestHome, runnerBirthFile)); err == nil {
			t.Fatal("a launcher ran beside a legacy announcement")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// The launcher refuses to announce a pid it cannot pair with an identity. On a
// guest where billet_birth cannot answer — neither /proc nor `ps` — the
// delivery must report that nothing announced itself, not publish a bare pid
// that the proof would then have to reject one step later.
func TestTheLauncherWillNotAnnounceWithoutAnIdentity(t *testing.T) {
	s := newStub(t)
	s.breakIdentity(t)

	p := newProvider(t, s)
	p.execRetry = time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	_, err := p.Launch(ctx, validSpec("billet-lease1"))
	if err == nil {
		t.Fatal("Launch succeeded although the launcher could not identify itself")
	}

	// BILLET'S OWN WORDS, from the status file the launcher writes, because the
	// delivery's stderr is discarded — the registration travels on that same
	// call's stdin, and a guest that copies stdin to stderr would otherwise
	// reflect a live credential into a node log.
	if !strings.Contains(err.Error(), "never recorded a pid") {
		t.Errorf("Launch = %v, want the delivery to report no announcement", err)
	}
}

// An announcement with a pid and NO birth token is what a billet older than the
// identity check leaves behind. It must read as unprovable, never as alive: the
// pid is just a number, and a recycled one would adopt a stranger.
func TestAnAnnouncementWithNoIdentityIsNotProved(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	for name, body := range map[string]string{
		launchClaim:   "",
		runnerPIDFile: strconv.Itoa(os.Getpid()) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(s.guestHome, name), []byte(body), 0o600); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}

	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	if err == nil {
		t.Fatal("Launch accepted a bare pid with no identity as its runner")
	}

	if !strings.Contains(err.Error(), "no-identity") {
		t.Errorf("Launch = %v, want an error naming the missing identity", err)
	}
}

// THE AMBIGUITY THE ATOMIC CLAIM EXISTS FOR: the guest runs the delivery to
// completion and the response is lost on the way back. Billet cannot tell that
// from a delivery that never arrived, so it retries — and the retry must not
// start a rival runner, because two runners consuming one single-use
// registration is one lease's capacity running two processes and a proof that
// can observe the loser.
func TestALostResponseDoesNotStartASecondRunner(t *testing.T) {
	s := newStub(t)

	// The first delivery acts and then reports failure.
	if err := os.WriteFile(s.execLoseResp, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm exec-lose-response: %v", err)
	}

	p := newProvider(t, s)

	spawns := filepath.Join(s.guestHome, "runner-spawns")
	spec := validSpec("billet-lease1")
	spec.Command = []string{"/bin/sh", "-c",
		`printf '%s\n' "$$" >> "$HOME/runner-spawns"; exec sleep 30`}

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	b, err := os.ReadFile(spawns)
	if err != nil {
		t.Fatalf("read spawns: %v", err)
	}

	if n := len(strings.Fields(string(b))); n != 1 {
		t.Errorf("a lost response produced %d runners, want exactly 1", n)
	}
}

// The claim is taken BEFORE the spawn, and only that ordering makes a retry
// safe when the delivery dies partway.
//
// This is the assertion that separates a claim from the post-spawn sentinel it
// replaced: with a sentinel, a delivery whose spawn never completes leaves no
// record at all and the next attempt spawns again. With the claim, the record
// exists the moment anyone tries — so a guest that cannot start a runner still
// cannot be given two.
func TestTheClaimIsTakenEvenWhenTheSpawnNeverAnnounces(t *testing.T) {
	s := newStub(t)

	// No runner in the guest at all: the spawn cannot announce.
	if err := os.Remove(filepath.Join(s.guestHome, "run.sh")); err != nil {
		t.Fatalf("remove fake runner: %v", err)
	}

	p := newProvider(t, s)
	p.execRetry = time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	if _, err := p.Launch(ctx, validSpec("billet-lease1")); err == nil {
		t.Fatal("Launch succeeded although nothing could start in the guest")
	}

	if _, err := os.Stat(filepath.Join(s.guestHome, launchClaim)); err != nil {
		t.Errorf("no claim was taken (%v), so a retry could spawn a rival runner", err)
	}
}

// THE FAILURE THIS BACKEND EXISTS NOT TO REPRODUCE: a delivery that reports
// success while nothing is left running. Measured against a real guest, the
// tart agent tears down its exec session's process group, and every mechanism
// that failed to escape it still exited 0 — so the launch is only complete once
// the guest has confirmed the runner is alive.
//
// A tier command that exits immediately produces the same observable state
// (the docker default-command bug, one backend over), which is why this test
// drives it that way: whatever the cause, the launch must fail rather than
// report a runner for a job that will sit queued.
func TestALaunchWhoseRunnerDiesImmediatelyIsAFailure(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	spec := validSpec("billet-lease1")
	spec.Command = []string{"/bin/sh", "-c", "exit 0"}

	_, err := p.Launch(t.Context(), spec)
	if err == nil {
		t.Fatal("Launch reported success for a runner that exited immediately")
	}

	if !strings.Contains(err.Error(), "is not running after its registration was delivered") {
		t.Errorf("Launch = %v, want an error naming the dead runner", err)
	}

	// BILLET'S OWN VERDICT, in billet's own words. The launcher records that it
	// reached exec before the command took over, so an operator can tell a runner
	// that started and died from one that was never startable.
	if !strings.Contains(err.Error(), "billet's launcher reached exec") {
		t.Errorf("Launch = %v, want billet's own launch status in the error", err)
	}

	// AND THE GUEST WAS ACTUALLY ASKED. The sentence above is also what a billet
	// that never looked would produce, so on its own it cannot tell an answer
	// from no question — an earlier version of this assertion was mutation-tested
	// and survived. What only a real read produces is the read itself, in argv.
	if !strings.Contains(s.argv(t), launchStatusFile) {
		t.Error("the guest was never asked for billet's launch status, so the error's " +
			"verdict is billet guessing rather than billet reading")
	}
}

// A COMMAND THE GUEST DOES NOT HAVE IS NAMED AS THAT, not as a runner that
// died. It is the failure that actually happened — the tart runner default
// pointed at a path the published images do not have — and the two are
// indistinguishable from outside: nothing is running either way.
func TestAMissingTierCommandIsReportedAsMissing(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	p.execRetry = time.Millisecond

	spec := validSpec("billet-lease1")
	spec.Command = []string{"./no-such-runner.sh"}

	// BOUNDED, because delivery retries until the CALLER's deadline: a command
	// the guest does not have never announces a pid, so every attempt fails the
	// same way and in production that runs out the node's command timeout. The
	// test supplies the deadline production gets from the node.
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	_, err := p.Launch(ctx, spec)
	if err == nil {
		t.Fatal("Launch reported success for a command that does not exist in the guest")
	}

	if !strings.Contains(err.Error(), "could not find the tier's command") {
		t.Errorf("Launch = %v, want the missing command named as the cause", err)
	}
}

// NOTHING THE GUEST WROTE IS EVER QUOTED, and this is the assertion that keeps
// it that way.
//
// An earlier version of this backend put the head of $HOME/billet-runner.log
// into the launch error, which the node logs. The reasoning was that a runner
// which never survived startup cannot have run a job yet, so its first bytes
// are billet's business. Both halves are wrong: GitHub can dispatch a job the
// moment the runner registers, which is while proveRunning is still taking its
// second sample — and the log is a PATHNAME in a filesystem the guest controls,
// so a job can replace it with a symlink to anything readable and choose the
// bytes billet prints. The bound limited the volume, not the sensitivity.
//
// The canary here stands for a secret a job printed. It must not appear in the
// error under any circumstances.
func TestNothingTheGuestWroteReachesTheLaunchError(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	const canary = "ghp-CANARY-THIS-IS-A-JOB-SECRET"

	// The command writes the canary to stdout — which the delivery redirects into
	// the runner log — and, for the symlink case, into the status file billet
	// does read. Both are guest-chosen bytes; neither may be echoed.
	spec := validSpec("billet-lease1")
	spec.Command = []string{"/bin/sh", "-c",
		"echo " + canary + "; printf '%s\\n' " + canary + " > \"$HOME/" + launchStatusFile + "\"; exit 1"}

	_, err := p.Launch(t.Context(), spec)
	if err == nil {
		t.Fatal("Launch reported success for a runner that exited immediately")
	}

	if strings.Contains(err.Error(), canary) {
		t.Errorf("bytes the guest chose reached the launch error, which the node writes to "+
			"its log: %v", err)
	}

	// And the unrecognised value is reported as such, so the failure is still
	// visible rather than silently swallowed.
	if !strings.Contains(err.Error(), "value billet did not write") {
		t.Errorf("Launch = %v, want the unrecognised status reported rather than dropped", err)
	}
}

// A LISTING THAT DESCRIBES NONE OF BILLET'S STORE IS REFUSED, because that is
// what a TART_HOME mismatch looks like: tart enumerates a different directory,
// answers with nothing, and reconciling against it would free the capacity of
// every lease on the host at once.
func TestListRefusesAListingThatMissesTheWholeStore(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	// The only billet VM there is, omitted from the listing — AND tart answers
	// "does not exist" when asked about it directly. That pair is the signature
	// of a store mismatch: billet just enumerated the directory, so a tart that
	// cannot find it at all is reading somewhere else. Classifying it as a
	// corpse instead would free the capacity of every lease on the host, one
	// confident answer at a time.
	if err := os.WriteFile(s.listLies, []byte("billet-lease1"), 0o600); err != nil {
		t.Fatalf("arm list-lies: %v", err)
	}

	if err := os.WriteFile(s.getBlind, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm get-blind: %v", err)
	}

	p := newProvider(t, s)

	if _, err := p.List(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "not the same store") {
		t.Fatalf("List = %v, want a refusal naming the store mismatch", err)
	}
}

// A SINGLE ORPHAN IS NOT THAT, and treating it as though it were wedged a real
// node: one directory left behind by a killed billet — a clone that never
// finished, which tart cannot read and therefore never lists — made every
// launch, teardown and check on that host fail until a human deleted it. Found
// by running a real job, not by review.
//
// Such a directory is also definitionally not a running guest, since tart
// enumerates every VM it can read. So it is excluded and logged, and the VMs
// that ARE listed keep working.
func TestListToleratesASingleUnreadableDirectory(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-good", "running", testOwner)

	// A STAGING directory tart cannot read: no state file, so the stub neither
	// lists it nor can `get` it — exactly what a clone killed before it was
	// finished leaves behind. Nothing is armed to hide it; its absence is
	// genuine, which is the whole distinction the test rests on.
	//
	// The name matters as much as the damage. Only the staging suffix proves
	// nothing was launched — the clone/mark/rename ordering means a directory
	// still carrying it was never renamed to a lease name, so there is no VMM
	// to be wrong about. A lease-named damaged directory is refused instead;
	// see TestListRefusesADamagedLeaseNamedDirectory.
	orphan := filepath.Join(s.home, "vms", "billet-orphan"+stagingSuffix)
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}

	p := newProvider(t, s)

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List refused over one unreadable directory: %v", err)
	}

	if len(instances) != 1 || instances[0].Name != "billet-good" {
		t.Errorf("List = %+v, want just the VM tart can actually see", instances)
	}
}

// AND THE OTHER HALF, which is the one that costs capacity: a VM tart CAN
// describe, missing from the listing, must stop reconciliation.
//
// The pair is what makes either test meaningful. With only the tolerance test
// above, "excluded and logged" and "silently dropped a live guest" are the same
// observation — a partial omission would be tolerated, its lease's capacity
// freed, and another job placed on top of a running one. Two live VMs and one
// dropped row is that state exactly, and it is why List asks tart about each
// absence instead of inferring from it.
func TestListRefusesWhenALiveVMIsMissingFromTheListing(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)
	s.vm(t, "billet-lease2", "running", testOwner)

	// Both are real and startable; only one row comes back.
	if err := os.WriteFile(s.listLies, []byte("billet-lease2"), 0o600); err != nil {
		t.Fatalf("arm list-lies: %v", err)
	}

	p := newProvider(t, s)

	instances, err := p.List(t.Context())
	if err == nil {
		t.Fatalf("List = %+v with no error, although a running VM was missing from the "+
			"listing; its lease's capacity would be freed and resold underneath it", instances)
	}

	if !strings.Contains(err.Error(), "billet-lease2") {
		t.Errorf("List = %v, want the omitted VM named", err)
	}
}

// A marker that does not hold a deployment id is an ERROR, never "someone
// else's": an empty or corrupt marker read as foreign silently omits the VM
// from inventory, and its capacity is resold while it runs.
func TestACorruptMarkerIsAnErrorNotAForeignDeployment(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", "not!a@deployment#id")

	p := newProvider(t, s)

	if _, err := p.List(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "ownership marker cannot be read") {
		t.Fatalf("List = %v, want a refusal over the corrupt marker", err)
	}

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err == nil || got != provider.TeardownRequested {
		t.Fatalf("Destroy = %v, %v; a corrupt marker is not authority to destroy", got, err)
	}
}

// Destroy must not accept a directory whose row `tart list` omits as proof of
// anything: the row must be PRESENT and STOPPED before deletion.
func TestDestroyRefusesToDeleteWhatTheInventoryOmits(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	if err := os.WriteFile(s.listLies, []byte("billet-lease1"), 0o600); err != nil {
		t.Fatalf("arm list-lies: %v", err)
	}

	p := newProvider(t, s)

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err == nil {
		t.Fatal("Destroy succeeded against an inventory omitting the target")
	}

	if got != provider.TeardownRequested {
		t.Errorf("Teardown = %v, want requested", got)
	}

	if !s.dirExists("billet-lease1") {
		t.Error("the VM directory was deleted although its state was never proved")
	}
}

// waitForFile polls until a file exists and is non-empty; the guest spawn is
// backgrounded, so its effects land asynchronously.
func waitForFile(t *testing.T, path string) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("nothing arrived at %s", path)

	return ""
}

// The delivery script really runs in the stub's guest: the registration must
// reach the runner's ENVIRONMENT — never argv — and a redelivery must not start
// a second runner. This is the end-to-end contract the argv grep cannot check:
// the script's syntax, the quoting, the sentinel and the spawn all execute.
func TestDeliveryStartsExactlyOneRunnerWithTheCredentialInItsEnvironment(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	spawns := filepath.Join(s.guestHome, "runner-spawns")
	spec := validSpec("billet-lease1")
	// Records the registration it was given and then STAYS UP, because a runner
	// that exits at once is a launch failure now rather than a passing test.
	spec.Command = []string{"/bin/sh", "-c",
		`printf '%s\n' "$` + jitEnvVar + `" >> "$HOME/runner-spawns"; exec sleep 30`}

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := strings.TrimSpace(waitForFile(t, spawns)); got != spec.JITConfig {
		t.Errorf("the runner saw %q in its environment, want the registration", got)
	}

	// A second delivery — the retry after an ambiguous exec, or an adoption —
	// must observe the sentinel and spawn nothing.
	if err := p.deliverRegistration(t.Context(), spec); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	// THE DETECTOR IS PROVED LIVE BEFORE ITS SILENCE MEANS ANYTHING: a fixed
	// sleep asserting absence passes whenever a defective spawn is merely slow.
	// Removing the sentinel and delivering again MUST produce a second line
	// through the exact pipeline a defective redelivery would use — so once
	// that arrives, the count tells spawns apart rather than racing them.
	// The claim AND the announcement: a winner refuses to spawn beside an
	// existing pid file, which is what protects a legacy runner, so a control
	// spawn has to look like a genuinely fresh VM.
	for _, f := range []string{launchClaim, runnerPIDFile, runnerBirthFile} {
		if err := os.Remove(filepath.Join(s.guestHome, f)); err != nil {
			t.Fatalf("clear %s: %v", f, err)
		}
	}

	// The control carries its own registration value so its line cannot be
	// mistaken for a late defective spawn of the guarded delivery, which would
	// carry the original one.
	control := spec
	control.JITConfig = "control-delivery-registration"

	if err := p.deliverRegistration(t.Context(), control); err != nil {
		t.Fatalf("control delivery: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)

	for {
		b, err := os.ReadFile(spawns)
		if err != nil {
			t.Fatalf("read spawns: %v", err)
		}

		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if !strings.Contains(string(b), control.JITConfig) {
			if time.Now().After(deadline) {
				t.Fatal("the control delivery never spawned; the detector cannot see spawns at all")
			}

			time.Sleep(10 * time.Millisecond)

			continue
		}

		var original int

		for _, line := range lines {
			if line == spec.JITConfig {
				original++
			}
		}

		if original != 1 {
			t.Errorf("the guarded registration spawned %d runners, want exactly 1: the "+
				"sentinel-guarded redelivery must spawn nothing", original)
		}

		break
	}

	// A SETTLE PASS AFTER THE CONTROL: a defective redelivery's child can be
	// scheduled later than the control's, so the count is re-read after a pause
	// that the control just proved is generous for this pipeline.
	time.Sleep(300 * time.Millisecond)

	b, err := os.ReadFile(spawns)
	if err != nil {
		t.Fatalf("re-read spawns: %v", err)
	}

	var original int

	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == spec.JITConfig {
			original++
		}
	}

	if original != 1 {
		t.Errorf("a late duplicate of the guarded registration appeared after the control "+
			"(%d total): the sentinel did not hold", original)
	}
}

// THE ERROR AN OPERATOR READS MUST BE THE DIAGNOSIS, NOT THE CLOCK. Delivery
// retries until the caller's deadline, and when that deadline lands INSIDE an
// attempt rather than between two, the attempt fails with the context's own
// error. Overwriting the last attempt with it produced "context deadline
// exceeded (last attempt: context deadline exceeded)" — the clock, reported
// twice, with the guest's own words discarded.
//
// Staged rather than timed: the stub fails once for a reason of its own and
// then blocks, which is exactly that interleaving and is not reproducible by
// choosing a deadline and hoping.
func TestTheDeliveryErrorKeepsTheLastRealReason(t *testing.T) {
	s := newStub(t)

	if err := os.WriteFile(s.execHang, []byte("1\n"), 0o600); err != nil {
		t.Fatalf("stage the stub: %v", err)
	}

	p := newProvider(t, s)
	p.execRetry = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Cancel once the SECOND attempt is provably inside the guest, so the first
	// one has certainly completed and recorded its reason. A deadline chosen
	// instead can expire during the first attempt on a loaded machine, leaving
	// nothing substantive to keep — the test would then fail for its own reason
	// rather than the code's.
	go func() {
		for {
			if _, err := os.Stat(s.execHanging); err == nil {
				cancel()

				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	_, err := p.Launch(ctx, validSpec("billet-lease1"))
	if err == nil {
		t.Fatal("Launch succeeded although every delivery attempt failed")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Launch = %v, want it to have retried until the cancellation", err)
	}

	// The attempt that failed for its OWN reason exited 3; the one the
	// cancellation cut short did not. Keeping the former is the property under
	// test, and the exit status is what still carries it now that a delivery's
	// stderr is discarded.
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("Launch = %v, want the diagnosis to survive the cut-short attempt", err)
	}
}

// THE POLL IS THE POINT, and only a stop that settles LATE can prove it.
//
// `tart stop` requests a stop rather than completing one, so the state is read
// back — and reading it once, immediately, catches a VM mid-transition and
// fails a teardown that was about to succeed. The never-stops test cannot see
// this: a one-shot check also errors there, so it passes against both.
func TestDestroyWaitsForAStopToSettle(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	// Three observations still running, then stopped.
	if err := os.WriteFile(s.stopSlow, []byte("3"), 0o600); err != nil {
		t.Fatalf("arm stop-slow: %v", err)
	}

	p := newProvider(t, s)

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err != nil {
		t.Fatalf("Destroy = %v; a stop that settles after a few observations is the "+
			"ordinary case, not a failure", err)
	}

	if got != provider.TeardownStopped {
		t.Errorf("Teardown = %v, want stopped: tart did report it stopped in the end", got)
	}

	if s.dirExists("billet-lease1") {
		t.Error("the VM was proved stopped and its directory is still there")
	}
}

// AND OWNERSHIP IS RE-PROVED BEFORE THE DELETE, because the poll above widened
// the window between checking it and acting on it to the whole stop timeout. In
// that window a VM can be removed and another created under the same name — by
// another deployment, or by an operator — and deleting by name would then act on
// a stranger's VM using a decision made about one that no longer exists.
//
// STAGED BY THE STUB, not by a sleep. The marker is rewritten by `tart list`
// itself, which puts the replacement exactly between the observation and the
// delete every run; coordinating it with sleeps meant the race could simply not
// happen, and every assertion would still pass.
func TestDestroyRefusesAVMThatWasReplacedWhileItWaited(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	if err := os.WriteFile(s.listStealsMarker, []byte(foreignOwner), 0o600); err != nil {
		t.Fatalf("arm the replacement: %v", err)
	}

	p := newProvider(t, s)

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err == nil {
		t.Fatal("Destroy deleted a VM that had been replaced while it waited, using an " +
			"ownership decision made about a different one")
	}

	// The SPECIFIC refusal: an unrelated early error would satisfy "some error".
	if !strings.Contains(err.Error(), "belongs to another billet deployment") {
		t.Errorf("Destroy = %v, want it to refuse on the changed ownership", err)
	}

	if got != provider.TeardownRequested {
		t.Errorf("Teardown = %v, want requested: nothing was proved gone", got)
	}

	if !s.dirExists("billet-lease1") {
		t.Fatal("the replacement VM was deleted")
	}

	// And it is still the REPLACEMENT that survived, not billet's own.
	marker, err := os.ReadFile(filepath.Join(s.home, "vms", "billet-lease1", ownerMarker))
	if err != nil || !strings.Contains(string(marker), foreignOwner) {
		t.Errorf("marker = %q (%v), want the other deployment's: the test did not stage "+
			"the replacement it is named for", marker, err)
	}
}

// THE SETTLE WINDOW BOUNDS THE LISTS THEMSELVES, not just the loop around them.
// Handing each `tart list` the caller's context bounds nothing: one hung list
// holds the node's single command slot for the caller's whole timeout — ten
// minutes — while this function claims thirty seconds.
func TestDestroyStopsWaitingWhenAListHangs(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	if err := os.WriteFile(s.listHangs, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm the hanging list: %v", err)
	}

	p := newProvider(t, s)
	p.stopWindow = 300 * time.Millisecond

	start := time.Now()

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err == nil {
		t.Fatal("Destroy succeeded although tart never answered")
	}

	// The caller's context is unbounded here, so anything under the stub's
	// sixty-second sleep proves the provider's own window did the bounding.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("Destroy took %s; the stop window did not bound the list", elapsed)
	}

	if got != provider.TeardownRequested {
		t.Errorf("Teardown = %v, want requested", got)
	}
}

// waitForTheClone blocks until the stub announces it has reached the clone, or
// says why that never happened.
//
// NOT waitForFile, AND THE DIFFERENCE IS BOTH HALVES OF A LESSON. Its five
// seconds are sized for a backgrounded guest spawn, and what is being waited on
// here is three shell subprocesses on a machine that may be running several
// suites at once — measured: the tart package takes 169s alone and 426s beside
// three other `go test ./...` runs, and this fixture failed there on the bound
// rather than on the property. A bound shorter than the work it bounds causes
// the failure it exists to prevent.
//
// And it watches the LAUNCH as well as the marker, because a launch that failed
// before it ever cloned produces exactly the same silence — so waiting on the
// marker alone reports "nothing arrived" about a failure three steps earlier and
// sends the reader to the fixture instead of to the cause.
func waitForTheClone(t *testing.T, s *stub, launched <-chan error) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for {
		if b, err := os.ReadFile(s.cloneReached); err == nil && len(b) > 0 {
			return
		}

		select {
		case err := <-launched:
			t.Fatalf("Launch returned before it reached the clone (%v), so the store lock this "+
				"test holds was never what stopped it", err)
		default:
		}

		if time.Now().After(deadline) {
			t.Fatalf("the stub never reached the clone; argv so far:\n%s", s.argv(t))
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// holdTheStore takes the store lock as a second billet sharing this Mac would,
// and releases it when the test ends.
func holdTheStore(t *testing.T, home string) {
	t.Helper()

	other := newLockProvider(t, home)

	unlock, err := other.lockStore(t.Context(), "stand in for another billet")
	if err != nil {
		t.Fatalf("could not stage a competing holder: %v", err)
	}

	t.Cleanup(unlock)
}

// A DELETE DOES NOT HAPPEN WHILE ANOTHER BILLET HOLDS THE STORE, which is the
// name-resolution race: `tart delete` resolves a NAME, so ownership proved before
// the call is a statement about whatever held that name then.
//
// The assertion that matters is that the VM SURVIVED. An error value is the
// cheapest thing a function produces and says nothing about what else happened —
// a Destroy that returned an error and deleted anyway is exactly the failure
// this exists to catch.
func TestDestroyWillNotDeleteWhileTheStoreLockIsHeld(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	holdTheStore(t, s.home)

	p := newProvider(t, s)
	p.storeLockWindow = 50 * time.Millisecond

	got, err := p.Destroy(t.Context(), "billet-lease1")
	if err == nil {
		t.Fatal("Destroy deleted a VM by name while another billet held the store lock, so " +
			"the ownership proof and the delete are still two separate acts")
	}

	if !s.dirExists("billet-lease1") {
		t.Fatal("the VM was deleted although billet never took the store lock")
	}

	if got != provider.TeardownRequested {
		t.Errorf("Teardown = %v, want requested: nothing was proved gone", got)
	}

	if !strings.Contains(err.Error(), "stayed held for more than") {
		t.Errorf("Destroy = %v, want it to name the contended store lock", err)
	}
}

// AND A LEASE NAME IS NOT PUBLISHED WHILE ANOTHER BILLET HOLDS IT, which is the
// other half of the same pair: a Destroy elsewhere is between its proof and its
// delete, and a rename into that name is what would make the delete destroy
// something else.
//
// STAGED INSIDE THE CLONE, not at the start of the launch. prepareStaging takes
// and releases the same lock before the clone, so a lock held from the outset
// fails the launch there — and a test written that way passes with the rename's
// own lock deleted, which is the mutation it exists to catch.
func TestLaunchWillNotPublishALeaseNameWhileTheStoreLockIsHeld(t *testing.T) {
	s := newStub(t)

	if err := os.WriteFile(s.cloneBlocks, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm the blocking clone: %v", err)
	}

	p := newProvider(t, s)
	p.storeLockWindow = 50 * time.Millisecond

	launched := make(chan error, 1)

	go func() {
		// CLOSED AS WELL AS SENT, so the cleanup below can join whether or not
		// the body already took the value: a receive on a closed buffered channel
		// yields the queued value first and then never blocks.
		defer close(launched)

		_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
		launched <- err
	}()

	// RELEASED AND JOINED WHATEVER FAILS FIRST. Without this a t.Fatal below
	// leaves Launch inside the stub's blocking clone for its full bound while
	// t.TempDir removes the directory underneath it — a goroutine outliving its
	// test, holding the fixture it is being torn down with.
	t.Cleanup(func() {
		if err := os.WriteFile(s.cloneRelease, []byte("1"), 0o600); err != nil {
			t.Logf("releasing the blocked clone: %v", err)
		}

		<-launched
	})

	// The clone announcing itself is what says prepareStaging has RELEASED the
	// lock; taking it before that would prove the wrong thing.
	waitForTheClone(t, s, launched)
	holdTheStore(t, s.home)

	if err := os.WriteFile(s.cloneRelease, []byte("1"), 0o600); err != nil {
		t.Fatalf("release the clone: %v", err)
	}

	err := <-launched
	if err == nil {
		t.Fatal("Launch renamed a clone into a lease name while another billet held the " +
			"store lock, so a concurrent Destroy could delete the guest it just published")
	}

	if s.dirExists("billet-lease1") {
		t.Fatal("the lease name was published although billet never took the store lock")
	}

	if !strings.Contains(err.Error(), "stayed held for more than") {
		t.Errorf("Launch = %v, want it to name the contended store lock", err)
	}

	if strings.Contains(s.argv(t), "rename") {
		t.Error("a rename ran despite the store lock being held")
	}

	// AND THE CLONE WAS RECLAIMED, which is the half a review found missing. The
	// staging deletes used to take this same lock, so a launch refused FOR
	// contention could not clean up either — and what it abandoned was a marked
	// staging clone that List skips, prepareStaging never revisits (the lease is
	// spent), and nothing else reclaims: tens of gigabytes, one log line, gone
	// from every later inventory.
	if s.dirExists("billet-lease1" + stagingSuffix) {
		t.Error("the staging clone was abandoned because cleanup waited on the same lock " +
			"that refused the launch; nothing will ever reclaim it")
	}
}

// AND THE NAME IS RE-CHECKED UNDER THE LOCK. tart's own rename refuses an
// occupied target — measured, exit 64, "delete it first!" — so what this proves
// is the diagnosis rather than the refusal: a name that appeared since billet's
// own inventory read is a second guest for one lease, not a stale VM an operator
// should go and remove.
func TestLaunchRefusesANameThatAppearedSinceTheInventory(t *testing.T) {
	s := newStub(t)

	if err := os.WriteFile(s.cloneStealsName, []byte("billet-lease1"), 0o600); err != nil {
		t.Fatalf("arm the stolen name: %v", err)
	}

	p := newProvider(t, s)

	start := time.Now()

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err == nil {
		t.Fatal("Launch published a lease name that had been taken since its inventory read")
	} else if !strings.Contains(err.Error(), "after billet found the name free") {
		t.Errorf("Launch = %v, want the refusal to say the name was taken since the "+
			"inventory read", err)
	}

	// The staging clone is cleaned up rather than left holding disk.
	if s.dirExists("billet-lease1" + stagingSuffix) {
		t.Error("the staging clone survived a refused launch")
	}

	// AND THE STORE LOCK IS FREE AGAIN, which is what publishStaging's deferred
	// unlock is for. A refusal that returns while still holding it costs nothing
	// here and blocks the NEXT teardown on this node for the whole mutation
	// window — so the property is asserted where it is cheap and deterministic
	// rather than left to a timing bound.
	//
	// An earlier version asserted elapsed time instead, on the reasoning that a
	// leaked lock would stall discardStaging. That stopped being true when the
	// staging deletes came out from under the lock, and the assertion quietly
	// became one that could not fail — which is the whole reason this file
	// mutation-tests rather than reads.
	free := newLockProvider(t, s.home)
	free.storeLockWindow = 100 * time.Millisecond

	release, err := free.lockStore(t.Context(), "prove the refused launch let go")
	if err != nil {
		t.Fatalf("the store lock was still held after a refused launch (%v), so the next "+
			"teardown on this node would wait out the whole window", err)
	}

	release()

	// The refusal is also prompt: nothing on this path waits on a lock.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the refused launch took %s, which is a lock wait it should not have", elapsed)
	}
}
