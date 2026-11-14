package firecracker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// THE SNAPSHOT START EXPERIMENT: a real GitHub Actions job, run by a runner whose
// guest booted cold and by one restored from a golden, alternately, on one node,
// each measured from the moment billet is asked for the guest to the moment its
// runner has its registration, and by its memory. The record is
// docs/reference/records/snapshot-start.md.
//
// THE GOLDEN WAITS FOR A REGISTRATION. It boots like any guest, with a placeholder
// for its JIT configuration and kvm-clock (withKVMClock), and its command is
// snapshotStartWaiter: once the agent has done everything a job's guest does before
// its runner starts, the waiter serves its state on snapshotStartPort and asks the
// metadata service, with a fresh token each time, for a registration that is not
// the placeholder. It is captured while it asks. A restore installs the fork's own
// metadata before resuming it, so the restored waiter finds a real registration and
// starts the runner; a cold guest is given the same command and finds its
// registration at once. Both arms are therefore measured to the same instruction.
// The runner takes its one job and exits, and the waiter says so; the guest is
// then destroyed, as the node destroys a guest whose job is done.
//
// THE JOBS ARE QUEUED FIRST. Trial i reads its JIT configuration from
// BILLET_SNAPSHOT_START_JITS/<i>.jit, written by whoever minted it for a runner
// whose label only trial i's job asks for and removed once read; the job is already
// queued, so the job's start on GitHub, joined afterwards by the runner's name, is
// the end-to-end measure, and the waiter's state the host-side one.
//
// FORKS RUN ONE AT A TIME. A restored guest keeps the golden's MAC and address, so
// two at once would share one; and the paused builder keeps its tap until the last
// fork is gone, so no other guest on the node is given the tap, and so the MAC,
// the forks run with.
//
// It skips unless BILLET_SNAPSHOT_START_OUT names a results file, on top of
// everything requireRealHost needs. Settings, all from the environment:
//   - BILLET_SNAPSHOT_START_JITS: the directory the JIT configurations arrive in.
//   - BILLET_SNAPSHOT_START_TRIALS: how many, alternating cold and restored, cold first.
//   - BILLET_SNAPSHOT_START_VCPU and _MEMORY_GIB: the guest's shape, both arms.
//   - BILLET_SNAPSHOT_START_LEASES: the bridge's dnsmasq lease file, where a guest's
//     address is found by its MAC.
//   - BILLET_SNAPSHOT_START_RUN: a label written on every line.
func TestRealSnapshotStartExperiment(t *testing.T) {
	out := os.Getenv("BILLET_SNAPSHOT_START_OUT")
	if out == "" {
		t.Skip("set BILLET_SNAPSHOT_START_OUT to a results file to run the snapshot start experiment")
	}

	env := requireRealHost(t)

	jits := os.Getenv("BILLET_SNAPSHOT_START_JITS")
	if jits == "" {
		t.Fatal("BILLET_SNAPSHOT_START_JITS names no directory for the JIT configurations")
	}

	trials := experimentInt(t, "BILLET_SNAPSHOT_START_TRIALS", 10)
	vcpu := experimentInt(t, "BILLET_SNAPSHOT_START_VCPU", 2)
	memory := config.ByteSize(experimentInt(t, "BILLET_SNAPSHOT_START_MEMORY_GIB", 4)) * config.GiB
	leases := os.Getenv("BILLET_SNAPSHOT_START_LEASES")
	if leases == "" {
		leases = "/var/lib/billet-dnsmasq/" + env.cfg.Bridge + "/dnsmasq.leases"
	}

	results, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o644)
	if err != nil {
		t.Fatalf("open the results file: %v", err)
	}
	defer results.Close()
	if info, err := results.Stat(); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the results file %s is not a regular file (%v)", out, err)
	}
	encoder := json.NewEncoder(results)
	line := func(fields map[string]any) {
		fields["run"] = os.Getenv("BILLET_SNAPSHOT_START_RUN")
		fields["vcpu"] = vcpu
		fields["memory_bytes"] = int64(memory)
		if err := encoder.Encode(fields); err != nil {
			t.Fatalf("write the results: %v", err)
		}
	}

	p, err := New(selftestDeployment, env.cfg, env.disk, WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	goldens, err := New(selftestDeployment, env.cfg, env.disk, WithBootWait(20*time.Second), withKVMClock())
	if err != nil {
		t.Fatalf("New for the golden: %v", err)
	}

	// THE CAPTURE'S ROOM, before anything is made: its memory file is the guest's
	// whole memory, copied once out of the jail into the store.
	if err := roomFor(p.snapshotsDir(), 3*uint64(memory)); err != nil {
		t.Fatalf("the snapshot store: %v", err)
	}

	builder := provider.InstanceName(env.lease)
	snapshot := "selftest-start-" + env.lease[:12]
	published := filepath.Join(p.snapshotsDir(), snapshot)

	// THE GOLDEN'S DISK GOES LAST, after the builder and every trial (cleanups run in
	// reverse), because each holds a mapping or a clone of it.
	t.Cleanup(func() {
		if err := env.disk.removeGolden(context.WithoutCancel(t.Context()), goldenDiskPrefix+env.lease); err != nil {
			t.Errorf("remove the golden's system disk: %v", err)
		}
	})
	// THE BUILDER HOLDS ITS TAP, AND SO THE GOLDEN'S MAC, while any fork may still
	// run with it. outstanding names every fork started and not yet proved
	// destroyed; a trial's own cleanup, which runs before this one, removes it once
	// a retry succeeds, so a fork still named here leaves the builder too, and no
	// guest the node launches next is given the MAC that fork answers on.
	outstanding := map[string]bool{}
	t.Cleanup(func() {
		if len(outstanding) > 0 {
			t.Errorf("the builder %s is left running, paused, so its tap stays reserved while forks %v, "+
				"which could not be destroyed, may hold the golden's MAC; destroy them, then the builder",
				builder, slices.Sorted(maps.Keys(outstanding)))

			return
		}
		if _, err := goldens.Destroy(context.WithoutCancel(t.Context()), builder); err != nil {
			t.Errorf("cleanup the builder: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := os.RemoveAll(published); err != nil {
			t.Errorf("remove the published snapshot: %v", err)
		}
	})

	spec := func(name, jit string) provider.Spec {
		return provider.Spec{
			Name: name, Image: env.image, VCPU: vcpu, Memory: memory, Trust: provider.TrustTrusted,
			Command: []string{"/bin/bash", "-c", snapshotStartWaiter, "waiter", snapshotStartPlaceholder,
				"http://" + mmdsAddress, strconv.Itoa(snapshotStartPort), snapshotStartRenew, "0.0.0.0"},
			JITConfig: jit,
		}
	}

	bootStarted := time.Now()
	if _, err := goldens.Launch(t.Context(), spec(builder, snapshotStartPlaceholder)); err != nil {
		t.Fatalf("Launch the builder: %v", err)
	}
	builderRes, err := resourcesOf(goldens.jailFor(builder))
	if err != nil {
		t.Fatal(err)
	}
	goldenMAC, err := guestMAC(builderRes.Tap)
	if err != nil {
		t.Fatal(err)
	}
	goldenAddr, err := leasedAddress(t.Context(), leases, goldenMAC, 2*time.Minute)
	if err != nil {
		t.Fatalf("the builder's address: %v", err)
	}

	// CAPTURED WHILE ITS WAITER ASKS, which is after the guest kernel finished
	// booting (Firecracker's condition for a snapshot) and after the agent did
	// everything a job's guest does before its runner starts.
	waiting, err := waiterState(t.Context(), goldenAddr, snapshotStartPort, 5*time.Minute,
		func(s string) bool { return hasLine(s, "waiting") })
	if err != nil {
		t.Fatalf("the builder's waiter: %v", err)
	}
	bootToWaiting := waiting.seen.Sub(bootStarted)

	captureStarted := time.Now()
	evidence, err := goldens.Capture(t.Context(), builder, snapshot)
	if err != nil {
		t.Fatalf("Capture the golden: %v", err)
	}
	captured := time.Now()
	memoryFile, err := os.Stat(filepath.Join(published, snapshotMemoryFile))
	if err != nil {
		t.Fatal(err)
	}
	line(map[string]any{
		"arm": "capture", "boot_to_waiting_ms": bootToWaiting.Milliseconds(),
		"capture_ms": captured.Sub(captureStarted).Milliseconds(), "memory_file_bytes": memoryFile.Size(),
		"golden_mac": goldenMAC, "loadavg": loadAverage(),
	})

	for i := range trials {
		arm := "cold"
		if i%2 == 1 {
			arm = "restore"
		}

		fields := map[string]any{"arm": arm, "trial": i, "loadavg": loadAverage()}
		err := runStartTrial(t, p, spec, startTrial{
			index: i, arm: arm, jits: jits, leases: leases, snapshot: snapshot, evidence: evidence,
			goldenMAC: goldenMAC, goldenAddr: goldenAddr, captured: captured, outstanding: outstanding,
		}, fields)
		// A TRIAL WHOSE GUEST COULD NOT BE DESTROYED ENDS THE EXPERIMENT, since the
		// next fork would share its MAC and address, and so does a golden's lease
		// the bridge's DHCP no longer holds for it. Its warning, with the time by
		// which the fork must be killed, is reported before anything that could fail.
		stop := errors.Is(err, errTrialSurvived) || errors.Is(err, errLeaseLost)
		if stop {
			t.Errorf("trial %d (%s): %v", i, arm, err)
		}
		if err != nil {
			fields["error"] = err.Error()
		}
		line(fields)
		if stop {
			t.FailNow()
		}
		if err != nil {
			t.Errorf("trial %d (%s): %v", i, arm, err)
		}
	}
}

// errTrialSurvived is a trial whose guest could not be destroyed.
var errTrialSurvived = errors.New("its guest could not be destroyed")

// errLeaseLost is a golden address the bridge's DHCP no longer holds for the
// golden's MAC, which a fork would then share with whoever it was given to.
var errLeaseLost = errors.New("the golden's address is no longer leased to it")

// startTrial is what a trial needs beyond the provider and its spec.
type startTrial struct {
	index                       int
	arm, jits, leases, snapshot string
	evidence                    Evidence
	goldenMAC, goldenAddr       string
	captured                    time.Time
	outstanding                 map[string]bool
}

// snapshotStartPlaceholder is the golden's JIT configuration: what its waiter
// waits for the metadata to stop saying.
const snapshotStartPlaceholder = "billet-snapshot-start-placeholder"

// snapshotStartRenew is what a guest's waiter runs once registered: a renewal of
// its DHCP lease, as the runner account, which may sudo in the image.
const snapshotStartRenew = "sudo -n networkctl renew eth0"

// The budget a fork runs on, from the lease check, all of it before the golden's
// lease can lapse whether or not the fork's renewal works. trialBudget is the
// restore, five minutes to register and twenty to finish its job; teardownBudget
// is its Destroy after that; and a fork that survives both leaves killMargin
// before the expiry, by which it is to be killed by hand, and operatorBudget for
// someone to do it. A restore is admitted only while the lease has all four left.
const (
	trialBudget    = 26 * time.Minute
	teardownBudget = 5 * time.Minute
	operatorBudget = 10 * time.Minute
	killMargin     = 5 * time.Minute
	forkLeaseNeed  = trialBudget + teardownBudget + operatorBudget + killMargin
)

// snapshotStartPort is where a waiter serves its state, inside the guest.
const snapshotStartPort = 47231

// snapshotStartWaiter is the command both arms run in place of the runner. It
// serves its state on address $5, port $3, one line appended per step (waiting; registered and
// the guest's time in milliseconds; released and the time; finished, the time and
// the runner's status), so no step's time is lost to a later one; waits for the
// metadata service at $2 to hold a registration that is not $1; holds there until
// the host, having sampled the guest's memory, releases it with a POST carrying
// the registration's sha256, which only the host that minted it and this guest
// know, and which is written, after any release left from the golden is cleared,
// only once the registration is (so a release before it, or from a peer on the
// bridge, is refused); runs the
// runner with it; and then holds on, listener and all, until the host destroys the
// guest, since the agent's service ending would take the listener with it. Once
// registered it runs $4 in the background, which in a guest renews its DHCP lease
// (snapshotStartRenew): a fork resumes with its golden's lease timers and lives
// minutes, so without this none would renew, and the bridge's DHCP would let the
// golden's address go an hour after the golden booted.
//
// A fresh token for every read, since a restored VMM knows none the golden's did,
// and every read bounded to a second, since one in flight at the capture is
// answered by nobody.
const snapshotStartWaiter = `set -u
state=${TMPDIR:-/tmp}/billet-snapshot-start
rm -f "$state.release" "$state.token"
printf 'waiting\n' >"$state"
python3 -c '
import http.server, sys
path = sys.argv[1]
class State(http.server.BaseHTTPRequestHandler):
    def reply(self, body):
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        try:
            body = open(path, "rb").read()
        except OSError:
            body = b"unreadable\n"
        self.reply(body)
    def do_POST(self):
        body = self.rfile.read(min(int(self.headers.get("Content-Length") or 0), 128))
        try:
            token = open(path + ".token", "rb").read().strip()
        except OSError:
            token = b""
        if not token or body.strip() != token:
            self.send_response(403)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        open(path + ".release", "w").close()
        self.reply(b"released\n")
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer((sys.argv[3], int(sys.argv[2])), State).serve_forever()
' "$state" "$3" "$5" &
mmds=$2
while :; do
	if token=$(curl -sf --connect-timeout 1 --max-time 1 -X PUT "$mmds/latest/api/token" \
		-H "X-metadata-token-ttl-seconds: 60") &&
		jit=$(curl -sf --connect-timeout 1 --max-time 1 -H "X-metadata-token: $token" \
			"$mmds/latest/meta-data/billet/jit-config") &&
		[ -n "$jit" ] && [ "$jit" != "$1" ]; then
		break
	fi
	sleep 0.02
done
rm -f "$state.release"
printf '%s' "$jit" | sha256sum | cut -d' ' -f1 >"$state.token.part" && mv "$state.token.part" "$state.token"
printf 'registered %s\n' "$(date +%s%3N)" >>"$state"
( bash -c "$4" >/dev/null 2>&1 & )
while [ ! -e "$state.release" ]; do sleep 0.01; done
printf 'released %s\n' "$(date +%s%3N)" >>"$state"
ACTIONS_RUNNER_INPUT_JITCONFIG="$jit" ./run.sh
rc=$?
printf 'finished %s %s\n' "$(date +%s%3N)" "$rc" >>"$state"
exec sleep infinity`

// hasLine reports whether a waiter's state holds a line starting with step.
func hasLine(state, step string) bool {
	_, ok := lineOf(state, step)

	return ok
}

// lineOf is the rest of a waiter's line for step, without the step's own word.
// Only a line its newline ends counts: a read can land in the middle of an append.
func lineOf(state, step string) (string, bool) {
	lines := strings.Split(state, "\n")
	for _, l := range lines[:len(lines)-1] {
		if l == step {
			return "", true
		}
		if rest, ok := strings.CutPrefix(l, step+" "); ok {
			return rest, true
		}
	}

	return "", false
}

// releaseToken is what releases a waiter holding jit: the registration's sha256.
func releaseToken(jit string) string {
	sum := sha256.Sum256([]byte(jit))

	return hex.EncodeToString(sum[:])
}

// releaseWaiter tells a waiter holding at its registration to run the runner,
// and answers when the release was first sent, which is no later than the waiter
// acted on it: a first POST whose reply was lost may already have released it.
func releaseWaiter(ctx context.Context, addr string, port int, token string) (time.Time, error) {
	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://%s:%d/release", addr, port)
	first := time.Now()
	deadline := first.Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(token))
		if err != nil {
			return first, err
		}
		resp, err := client.Do(req)
		if err == nil {
			err = resp.Body.Close()
			if err == nil && resp.StatusCode == http.StatusOK {
				return first, nil
			}
		}
		if time.Now().After(deadline) {
			return first, fmt.Errorf("release the waiter at %s: %w", url, err)
		}
		select {
		case <-ctx.Done():
			return first, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// runStartTrial runs one trial and fills its fields: when billet was asked, when
// it answered, when the waiter had its registration by the guest's clock and when
// the host first read it, the VMM's memory while the runner is held there and
// later, and when the runner finished its one job. The guest is destroyed however
// the trial ends, before the next starts; one that could not be is
// errTrialSurvived.
func runStartTrial(t *testing.T, p *Provider, spec func(name, jit string) provider.Spec, tr startTrial,
	fields map[string]any,
) (err error) {
	t.Helper()
	arm, leases, goldenMAC, goldenAddr := tr.arm, tr.leases, tr.goldenMAC, tr.goldenAddr

	jit, err := takeJIT(t.Context(), filepath.Join(tr.jits, strconv.Itoa(tr.index)+".jit"), 10*time.Minute)
	if err != nil {
		return err
	}

	// A FORK ONLY WHILE THE GOLDEN'S ADDRESS IS STILL ITS OWN for longer than the
	// fork can live, whether or not the fork's renewal works: a lease that could
	// lapse under it, or the address given to another MAC, would put the fork on an
	// address a production guest holds. A renewal that fails ends the restores
	// early instead.
	checked := time.Now()
	killBy := checked.Add(forkLeaseNeed - killMargin)
	if arm == "restore" {
		left, err := goldenLeaseHolds(leases, goldenMAC, goldenAddr, forkLeaseNeed, checked)
		if err != nil {
			return fmt.Errorf("%w: %w", errLeaseLost, err)
		}
		fields["golden_lease_left_s"] = left.Seconds()
		// BY THE PROVEN EXPIRY, which is no earlier than the budget assumes.
		if left < time.Duration(1<<63-1) {
			killBy = checked.Add(left - killMargin)
		}
	}

	// THE TRIAL AND ITS TEARDOWN END WITHIN THEIR BUDGETS FROM THE CHECK, its
	// restore included, leaving the operator's time and the margin before the
	// lease can lapse.
	trialEnds := checked.Add(trialBudget)
	teardownEnds := trialEnds.Add(teardownBudget)
	ctx, cancel := context.WithDeadline(t.Context(), trialEnds)
	defer cancel()

	name := provider.InstanceName(freshLease(t))
	fields["instance"] = name

	// DESTROYED HOWEVER THE TRIAL ENDS, and done only once it is: a Destroy that
	// failed is tried again at cleanup, and a Destroy of one that never started is
	// a no-op.
	destroyed := false
	destroy := func() error {
		if destroyed {
			return nil
		}
		dctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), teardownEnds)
		defer cancel()
		if _, err := p.Destroy(dctx, name); err != nil {
			return err
		}
		destroyed = true
		delete(tr.outstanding, name)

		return nil
	}
	t.Cleanup(func() {
		if err := destroy(); err != nil {
			t.Errorf("cleanup %s: %v", name, err)
		}
	})
	// A FORK THAT CANNOT BE DESTROYED IS REPORTED AT ONCE, with its lease still
	// running: the experiment ends, the builder keeps the golden's MAC, and the
	// message says by when the fork's VMM must be killed by hand, before the bridge's
	// DHCP may give its address to another guest. The harness touches no tap: a
	// Destroy that failed may already have released the fork's to a production guest.
	defer func() {
		derr := destroy()
		if derr == nil {
			return
		}
		err = errors.Join(err, fmt.Errorf("%w: %s (jail %s): %w", errTrialSurvived, name, p.jailFor(name).dir(),
			derr))
		if arm == "restore" {
			err = fmt.Errorf("%w; it may run on the golden's address %s: kill its VMM by %s, %s before the "+
				"golden's lease can lapse", err, goldenAddr, killBy.Format(time.RFC3339), killMargin)
		}
	}()

	asked := time.Now()
	fields["asked_epoch_ms"] = asked.UnixMilli()
	if arm == "cold" {
		_, err = p.Launch(ctx, spec(name, jit))
	} else {
		// OUTSTANDING FROM BEFORE THE CALL, since a restore that failed may still
		// have started a VMM with the golden's MAC.
		tr.outstanding[name] = true
		fields["snapshot_age_s"] = asked.Sub(tr.captured).Seconds()
		_, err = p.Restore(ctx, spec(name, jit), tr.snapshot, tr.evidence)
	}
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	fields["answered_ms"] = time.Since(asked).Milliseconds()

	j := p.jailFor(name)
	res, err := resourcesOf(j)
	if err != nil {
		return err
	}
	addr := goldenAddr
	if arm == "cold" {
		mac, err := guestMAC(res.Tap)
		if err != nil {
			return err
		}
		if addr, err = leasedAddress(ctx, leases, mac, 2*time.Minute); err != nil {
			return fmt.Errorf("the guest's address: %w", err)
		}
	} else {
		// THE HOST'S NEIGHBOUR ENTRY FOR THE GOLDEN'S ADDRESS IS ASKED AGAIN, so the
		// host finds the fork on its own tap rather than the paused builder on its.
		// An entry that is not there is what this wants.
		if out, err := exec.CommandContext(ctx, "ip", "neigh", "del", goldenAddr, "dev",
			p.bridgeFor(provider.TrustTrusted)).CombinedOutput(); err != nil &&
			!strings.Contains(string(out), "No such file") {
			return fmt.Errorf("forget the host's neighbour entry for %s: %w: %s", goldenAddr, err, out)
		}
	}

	registered, err := waiterState(ctx, addr, snapshotStartPort, 5*time.Minute,
		func(s string) bool { return hasLine(s, "registered") })
	if err != nil {
		return fmt.Errorf("the waiter: %w", err)
	}
	// HELD AT ITS REGISTRATION, or the memory below is not of the same instruction.
	if hasLine(registered.state, "released") || hasLine(registered.state, "finished") {
		return fmt.Errorf("the waiter was already past its registration: %q", registered.state)
	}
	stamp, _ := lineOf(registered.state, "registered")
	guestMS, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return fmt.Errorf("the waiter answered %q", registered.state)
	}
	fields["registered_guest_epoch_ms"] = guestMS
	fields["registered_seen_ms"] = registered.seen.Sub(asked).Milliseconds()
	fields["registered_guest_ms"] = guestMS - asked.UnixMilli()
	// HOW FAR THE GUEST'S STAMP PRECEDES THE HOST'S FIRST READ OF IT: the poll's
	// interval and latency, plus however far the guest's clock is behind the
	// host's. A large value says the clock is behind; a negative one, ahead.
	fields["guest_stamp_before_read_ms"] = registered.seen.UnixMilli() - guestMS

	// THE MEMORY WHILE THE RUNNER IS HELD AT ITS REGISTRATION, the same instruction
	// in both arms, then the release.
	pid, err := vmmPID(j)
	if err != nil {
		return err
	}
	fields["memory_at_registered"] = smapsRollup(pid)
	// THE SAMPLE THIRTY SECONDS ON COUNTS FROM THE FIRST POST, which is no later than
	// the release; how long it took to be acknowledged is recorded beside it.
	released, err := releaseWaiter(ctx, addr, snapshotStartPort, releaseToken(jit))
	fields["release_acknowledged_ms"] = time.Since(released).Milliseconds()
	if err != nil {
		return err
	}

	if arm == "restore" {
		// THE FORK IS THE GOLDEN ON THE WIRE: its MAC, on its own tap.
		fdb, err := exec.CommandContext(ctx, "bridge", "fdb", "show", "dev", res.Tap).Output()
		fields["fork_mac_is_golden"] = err == nil && strings.Contains(string(fdb), goldenMAC)
	}

	// SAMPLED UNTIL THE RUNNER FINISHES ITS JOB: the memory thirty seconds after
	// the release, while the job runs, and the peak.
	var peak int64
	sampled := false
	finished, err := waiterState(ctx, addr, snapshotStartPort, 20*time.Minute, func(s string) bool {
		m := smapsRollup(pid)
		if pss, ok := m["Pss"]; ok && pss > peak {
			peak = pss
		}
		if !sampled && time.Since(released) >= 30*time.Second {
			fields["memory_30s_after_release"] = m
			sampled = true
		}

		return hasLine(s, "finished")
	})
	fields["pss_peak_kib"] = peak
	if err != nil {
		return fmt.Errorf("the runner: %w", err)
	}
	if stamp, ok := lineOf(finished.state, "released"); ok {
		if ms, err := strconv.ParseInt(stamp, 10, 64); err == nil {
			fields["released_guest_ms"] = ms - asked.UnixMilli()
		}
	}
	rest, _ := lineOf(finished.state, "finished")
	var finishedMS int64
	var status int
	if _, err := fmt.Sscanf(rest, "%d %d", &finishedMS, &status); err != nil {
		return fmt.Errorf("the waiter answered %q", finished.state)
	}
	fields["finished_guest_ms"] = finishedMS - asked.UnixMilli()
	fields["runner_status"] = status
	if status != 0 {
		return fmt.Errorf("the runner exited %d", status)
	}

	return nil
}

// waiterSeen is a waiter's state and when the host first read it.
type waiterSeen struct {
	state string
	seen  time.Time
}

// waiterState polls a waiter's state every 20 ms until done accepts it.
func waiterState(ctx context.Context, addr string, port int, limit time.Duration,
	done func(string) bool,
) (waiterSeen, error) {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	url := fmt.Sprintf("http://%s:%d/", addr, port)
	deadline := time.Now().Add(limit)
	last := "nothing"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return waiterSeen{}, err
		}
		if resp, err := client.Do(req); err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
			seen := time.Now()
			readErr = errors.Join(readErr, resp.Body.Close())
			if readErr == nil && resp.StatusCode == http.StatusOK {
				last = string(body)
				if done(last) {
					return waiterSeen{state: last, seen: seen}, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return waiterSeen{}, fmt.Errorf("%s answered %q after %s", url, last, limit)
		}
		select {
		case <-ctx.Done():
			return waiterSeen{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// leasedAddress is the address dnsmasq leased to mac, waiting for the lease.
func leasedAddress(ctx context.Context, leases, mac string, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for {
		f, err := os.Open(leases)
		if err != nil {
			return "", err
		}
		scan := bufio.NewScanner(f)
		var addr string
		for scan.Scan() {
			// expiry mac address hostname client-id
			fields := strings.Fields(scan.Text())
			if len(fields) >= 3 && strings.EqualFold(fields[1], mac) {
				addr = fields[2]
			}
		}
		if err := errors.Join(scan.Err(), f.Close()); err != nil {
			return "", err
		}
		if addr != "" {
			return addr, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no lease for %s in %s after %s", mac, leases, limit)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// goldenLeaseHolds refuses unless the lease file gives addr to mac, with at least
// minRemaining left at now (an expiry of 0 never lapses), and gives addr to no
// other MAC, and answers how long it has left (a lease that never lapses, the
// longest a Duration holds).
func goldenLeaseHolds(leases, mac, addr string, minRemaining time.Duration, now time.Time) (time.Duration,
	error,
) {
	raw, err := os.ReadFile(leases)
	if err != nil {
		return 0, err
	}
	held := false
	var left time.Duration
	for _, l := range strings.Split(string(raw), "\n") {
		// expiry mac address hostname client-id
		fields := strings.Fields(l)
		if len(fields) < 3 || fields[2] != addr {
			continue
		}
		if !strings.EqualFold(fields[1], mac) {
			return 0, fmt.Errorf("%s is leased to %s, not the golden's %s", addr, fields[1], mac)
		}
		expiry, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("the lease of %s has expiry %q", addr, fields[0])
		}
		left = time.Duration(1<<63 - 1)
		if expiry != 0 {
			left = time.Unix(expiry, 0).Sub(now)
		}
		if left < minRemaining {
			return 0, fmt.Errorf("the lease of %s to %s has %s left", addr, mac, left.Round(time.Second))
		}
		held = true
	}
	if !held {
		return 0, fmt.Errorf("%s is not leased to %s", addr, mac)
	}

	return left, nil
}

// takeJIT waits for a JIT configuration to arrive, reads it and removes it, so a
// credential is on the node only from the trial that needs it until it is read.
func takeJIT(ctx context.Context, path string, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if err := os.Remove(path); err != nil {
				return "", err
			}
			jit := strings.TrimSpace(string(raw))
			if jit == "" || jit == snapshotStartPlaceholder {
				return "", fmt.Errorf("%s holds no JIT configuration", path)
			}

			return jit, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not arrive in %s", path, limit)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// vmmPID is the VMM's process id, as the jailer recorded it.
func vmmPID(j jail) (int, error) {
	raw, err := os.ReadFile(j.pidFile())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s holds %q, not a pid", j.pidFile(), raw)
	}

	return pid, nil
}

// smapsRollup is a process's memory as the kernel totals it, in KiB: what it has
// resident, its proportional share of what it shares, and what only it holds. An
// unreadable rollup is an empty map, which the results show as such.
func smapsRollup(pid int) map[string]int64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return map[string]int64{}
	}
	want := map[string]bool{"Rss": true, "Pss": true, "Pss_Anon": true, "Pss_File": true,
		"Shared_Clean": true, "Private_Clean": true, "Private_Dirty": true, "Anonymous": true}
	m := map[string]int64{}
	for _, l := range strings.Split(string(raw), "\n") {
		key, rest, ok := strings.Cut(l, ":")
		if !ok || !want[key] {
			continue
		}
		if kib, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64); err == nil {
			m[key] = kib
		}
	}

	return m
}

// roomFor refuses a directory whose filesystem has less than need bytes free,
// asking its nearest existing ancestor when it is not made yet.
func roomFor(dir string, need uint64) error {
	var fs unix.Statfs_t
	for {
		err := unix.Statfs(dir, &fs)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.ENOENT) || dir == filepath.Dir(dir) {
			return err
		}
		dir = filepath.Dir(dir)
	}
	if free := fs.Bavail * uint64(fs.Bsize); free < need {
		return fmt.Errorf("%s has %d bytes free and the capture needs %d", dir, free, need)
	}

	return nil
}

// loadAverage is the host's one-minute load average, as /proc/loadavg says it.
func loadAverage() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unknown"
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "unknown"
	}

	return fields[0]
}

// ONLY A WHOLE LINE IS A STEP: a read that lands in the middle of an append sees
// a tail without its newline, which is neither found nor parsed.
func TestAWaiterStepIsAWholeLine(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		state, step, rest string
		found             bool
	}{
		{"waiting\nregistered 1731600000123\n", "registered", "1731600000123", true},
		{"waiting\nregistered 179", "registered", "", false},
		{"waiting\nregistered 1731600000123\nfinished 17316", "finished", "", false},
		{"waiting\n", "waiting", "", true},
		{"waiting", "waiting", "", false},
		{"waiting\nregisteredx 1\n", "registered", "", false},
	} {
		rest, found := lineOf(c.state, c.step)
		if found != c.found || rest != c.rest {
			t.Errorf("lineOf(%q, %s) = %q, %v; want %q, %v", c.state, c.step, rest, found, c.rest, c.found)
		}
	}
}

// A FORK RUNS ONLY ON A GOLDEN ADDRESS STILL ITS OWN: the lease must name the
// golden's MAC with ten minutes left, and no other MAC may hold the address.
func TestAForkRunsOnlyOnALeaseStillItsOwn(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_731_600_000, 0)
	const mac, addr = "02:00:00:00:00:0a", "172.31.0.42"
	for _, c := range []struct {
		name, leases string
		ok           bool
	}{
		{"held for an hour", "1731603600 02:00:00:00:00:0a 172.31.0.42 * 01:02\n", true},
		{"held for ever", "0 02:00:00:00:00:0a 172.31.0.42 * *\n", true},
		{"held, the MAC in capitals", "1731603600 02:00:00:00:00:0A 172.31.0.42 * *\n" +
			"1731603600 02:00:00:00:00:0b 172.31.0.43 * *\n", true},
		{"lapsing in five minutes", "1731600300 02:00:00:00:00:0a 172.31.0.42 * *\n", false},
		{"given to another MAC", "1731603600 02:00:00:00:00:09 172.31.0.42 * *\n", false},
		{"not leased at all", "1731603600 02:00:00:00:00:0a 172.31.0.99 * *\n", false},
		{"an unreadable expiry", "soon 02:00:00:00:00:0a 172.31.0.42 * *\n", false},
	} {
		path := filepath.Join(t.TempDir(), "dnsmasq.leases")
		if err := os.WriteFile(path, []byte(c.leases), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := goldenLeaseHolds(path, mac, addr, 10*time.Minute, now); (err == nil) != c.ok {
			t.Errorf("%s: admitted %v (%v), want %v", c.name, err == nil, err, c.ok)
		}
	}
	if left, err := goldenLeaseHolds(anHourLeaseFile(t), mac, addr, 0, now); err != nil || left != time.Hour {
		t.Errorf("a lease with an hour left answered %s (%v)", left, err)
	}
	if _, err := goldenLeaseHolds(filepath.Join(t.TempDir(), "absent"), mac, addr, 0, now); err == nil {
		t.Error("a lease file that could not be read was taken to hold the lease")
	}
}

// postRelease sends one release with token to a waiter on this host, and answers
// its status.
func postRelease(t *testing.T, port int, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/release", port), strings.NewReader(token))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post a release: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode
}

// anHourLeaseFile is a lease file giving the test's golden address an hour from its now.
func anHourLeaseFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dnsmasq.leases")
	if err := os.WriteFile(path, []byte("1731603600 02:00:00:00:00:0a 172.31.0.42 * *\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// THE WAITER WAITS FOR A REAL REGISTRATION AND SAYS WHAT IT DID: against a stand-in
// for the metadata service, it serves "waiting" while the service answers the
// placeholder, asks for a fresh token before every read (a restored VMM honours
// none the golden's did, which the stand-in enforces by honouring only the newest
// token), holds at its registration without running the runner until it is
// released, runs the runner with the registration the service then gives, serves
// every step's line, and holds on after the runner finishes.
func TestTheSnapshotStartWaiterWaitsForARealRegistration(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("the waiter stamps its state with GNU date's %3N")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to serve the waiter's state")
	}

	var mu sync.Mutex
	jit := snapshotStartPlaceholder
	issued, live := 0, ""
	puts, reads := 0, 0
	mmds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			puts++
			issued++
			live = "token-" + strconv.Itoa(issued)
			fmt.Fprint(w, live)
		case r.Method == http.MethodGet && r.URL.Path == "/latest/meta-data/billet/jit-config":
			reads++
			// ONLY THE NEWEST TOKEN IS HONOURED, so a waiter that kept one would be
			// refused from the second read on.
			if r.Header.Get("X-Metadata-Token") != live {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, jit)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mmds.Close)

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("a TCP listener answered %T for its address", listener.Addr())
	}
	port := addr.Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	// A RECORDER IN PLACE OF THE GUEST'S RENEWAL, so the host's own networking is
	// never touched.
	renewed := filepath.Join(dir, "renewed")
	runner := "#!/bin/sh\nprintf '%s' \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" >" + seen + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(runner), 0o755); err != nil {
		t.Fatal(err)
	}

	// A PROCESS GROUP OF ITS OWN, so the listener and the sleep the waiter leaves
	// behind are ended with it.
	cmd := exec.CommandContext(t.Context(), "/bin/bash", "-c", snapshotStartWaiter, "waiter",
		snapshotStartPlaceholder, mmds.URL, strconv.Itoa(port), "printf renewed >"+renewed, "127.0.0.1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TMPDIR="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		defer close(exited)
		waitErr = cmd.Wait()
	}()
	t.Cleanup(func() {
		if err := unix.Kill(-cmd.Process.Pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Errorf("stop the waiter's process group: %v", err)
		}
		<-exited
	})

	if _, err := waiterState(t.Context(), "127.0.0.1", port, 30*time.Second,
		func(s string) bool { return hasLine(s, "waiting") }); err != nil {
		t.Fatalf("the waiter never served waiting: %v", err)
	}
	// SEVERAL READS OF THE PLACEHOLDER, so the token rule has had its chance to bite.
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := reads
		mu.Unlock()
		if n >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the waiter read the registration %d times in 10s", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// NOTHING REGISTERED WHILE THE PLACEHOLDER IS SERVED, and no release taken.
	before, err := waiterState(t.Context(), "127.0.0.1", port, 5*time.Second, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"registered", "released", "finished"} {
		if hasLine(before.state, step) {
			t.Errorf("the waiter served %s before it had a real registration: %q", step, before.state)
		}
	}
	for _, token := range []string{releaseToken(snapshotStartPlaceholder), ""} {
		if code := postRelease(t, port, token); code != http.StatusForbidden {
			t.Errorf("a release with token %q before the registration answered %d; want it refused", token, code)
		}
	}

	switched := time.Now()
	mu.Lock()
	jit = "a-real-registration"
	mu.Unlock()

	registered, err := waiterState(t.Context(), "127.0.0.1", port, 30*time.Second,
		func(s string) bool { return hasLine(s, "registered") })
	if err != nil {
		t.Fatalf("the waiter never served registered: %v", err)
	}
	// STAMPED BETWEEN THE SWITCH AND THE READ, on this host's one clock.
	stamp, _ := lineOf(registered.state, "registered")
	if ms, err := strconv.ParseInt(stamp, 10, 64); err != nil ||
		ms < switched.UnixMilli() || ms > registered.seen.UnixMilli() {
		t.Errorf("the registration was stamped %q, not between the switch (%d) and the read (%d)",
			stamp, switched.UnixMilli(), registered.seen.UnixMilli())
	}
	// HELD THERE until released by the registration's own token: the runner has not
	// run a moment later, nor after a release carrying another.
	if code := postRelease(t, port, releaseToken("another-registration")); code != http.StatusForbidden {
		t.Errorf("a release with another registration's token answered %d; want it refused", code)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(seen); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the runner ran before the waiter was released (%v)", err)
	}
	if _, err := releaseWaiter(t.Context(), "127.0.0.1", port, releaseToken("a-real-registration")); err != nil {
		t.Fatal(err)
	}

	finished, err := waiterState(t.Context(), "127.0.0.1", port, 30*time.Second,
		func(s string) bool { return hasLine(s, "finished") })
	if err != nil {
		t.Fatalf("the waiter never served finished: %v", err)
	}
	for _, step := range []string{"waiting", "registered", "released"} {
		if !hasLine(finished.state, step) {
			t.Errorf("the finished state %q lost its %s line", finished.state, step)
		}
	}
	var at int64
	var status int
	rest, _ := lineOf(finished.state, "finished")
	if _, err := fmt.Sscanf(rest, "%d %d", &at, &status); err != nil || status != 3 {
		t.Errorf("the waiter served %q; want the runner's status 3", finished.state)
	}
	if got, err := os.ReadFile(seen); err != nil || string(got) != "a-real-registration" {
		t.Errorf("the runner was given %q (%v); want the real registration", got, err)
	}
	if got, err := os.ReadFile(renewed); err != nil || string(got) != "renewed" {
		t.Errorf("the waiter did not run its renewal (%q, %v)", got, err)
	}
	// AND IT HOLDS ON, listener and all, until the host destroys the guest.
	select {
	case <-exited:
		t.Errorf("the waiter ended (%v) after its runner; want it holding on", waitErr)
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if puts < reads {
		t.Errorf("the waiter asked for %d tokens for %d reads; want a fresh one for each", puts, reads)
	}
}
