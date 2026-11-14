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
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/netutil"
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
// its runner starts, the waiter reports that it is waiting and asks the metadata
// service, with a fresh token each time, for a registration that is not the
// placeholder. It is captured while it asks. A restore installs the fork's own
// metadata before resuming it, so the restored waiter finds a real registration;
// a cold guest is given the same command and finds its registration at once. Both
// arms are therefore measured to the same instruction. The runner takes its one
// job and exits, and the waiter says so; the guest is then destroyed, as the node
// destroys a guest whose job is done.
//
// THE GUEST REPORTS TO THE HOST, never the other way: the node's firewall drops
// everything a guest sends the host but DNS, DHCP, the node's cache port and the
// port `billet images verify` listens on for the same purpose, a guest reporting
// to its host (measured on ubuntu-01 2026-11-14: a guest leased an address and
// answered nothing the host sent it). So the host listens there (startServer) and
// each waiter posts its steps to it, keyed by the sha256 of its registration, and
// asks it whether it may run its runner. Only the host, which minted the
// registration, and the guest know that key.
//
// THE JOBS ARE QUEUED FIRST. Trial i reads its JIT configuration from
// BILLET_SNAPSHOT_START_JITS/<i>.jit, written by whoever minted it for a runner
// whose label only trial i's job asks for and removed once read; the job is already
// queued, so the job's start on GitHub, joined afterwards by the runner's name, is
// the end-to-end measure, and the waiter's steps the host-side one.
//
// FORKS RUN ONE AT A TIME. A restored guest keeps the golden's MAC and address, so
// two at once would share one; and the paused builder keeps its tap until the last
// fork is gone, so no other guest on the node is given the tap, and so the MAC,
// the forks run with.
//
// It skips unless BILLET_SNAPSHOT_START_OUT names a results file, on top of
// everything requireRealHost needs, and it runs only with -test.timeout 0.
// Settings, all from the environment:
//   - BILLET_SNAPSHOT_START_JITS: the directory the JIT configurations arrive in.
//   - BILLET_SNAPSHOT_START_LISTEN: where the host listens for its guests, an
//     address on the bridge and a port the node's firewall lets guests reach
//     (172.31.0.1:7719 on ubuntu-01, the image verification port).
//   - BILLET_SNAPSHOT_START_PORT_LOCK: a lock held while listening, so another
//     user of that port refuses rather than collides (`billet images verify`'s,
//     <node.lock_dir>/billet-imageverify-<deployment>.lock).
//   - BILLET_SNAPSHOT_START_TRIALS: how many, alternating cold and restored, cold first.
//   - BILLET_SNAPSHOT_START_VCPU and _MEMORY_GIB: the guest's shape, both arms.
//   - BILLET_SNAPSHOT_START_LEASES: the bridge's dnsmasq lease file, where the
//     golden's address is found by its MAC.
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
	listen := os.Getenv("BILLET_SNAPSHOT_START_LISTEN")
	if listen == "" {
		t.Fatal("BILLET_SNAPSHOT_START_LISTEN names no address on the bridge for the guests to report to")
	}

	trials := experimentInt(t, "BILLET_SNAPSHOT_START_TRIALS", 10)
	// NO TEST DEADLINE, refused before anything starts: the test binary's timeout
	// panics without running a trial's teardown or any cleanup, which would leave a
	// fork and the builder running. Every step has a deadline of its own instead,
	// and the operator watching the run sees a stop on stderr as it happens.
	if _, ok := t.Deadline(); ok {
		t.Fatal("run with -test.timeout 0: every step has its own deadline, and the binary's timeout would " +
			"skip the teardown")
	}
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

	// THE PORT'S LOCK, THEN THE PORT: a `billet images verify` started while this
	// runs refuses on the lock rather than failing to listen.
	if lockPath := os.Getenv("BILLET_SNAPSHOT_START_PORT_LOCK"); lockPath != "" {
		lock, err := holdLock(lockPath)
		if err != nil {
			t.Fatalf("BILLET_SNAPSHOT_START_PORT_LOCK: %v", err)
		}
		t.Cleanup(func() {
			if err := lock.release(); err != nil {
				t.Errorf("release the port's lock: %v", err)
			}
		})
	}
	server, err := listenForGuests(t.Context(), listen)
	if err != nil {
		t.Fatalf("listen for the guests on %s: %v", listen, err)
	}
	t.Cleanup(server.close)
	host := "http://" + listen

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
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
		defer cancel()
		if err := env.disk.removeGolden(ctx, goldenDiskPrefix+env.lease); err != nil {
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
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
		defer cancel()
		if _, err := goldens.Destroy(ctx, builder); err != nil {
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
				"http://" + mmdsAddress, host, snapshotStartRenew},
			JITConfig: jit,
		}
	}

	bootStarted := time.Now()
	launchCtx, cancelLaunch := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancelLaunch()
	if _, err := goldens.Launch(launchCtx, spec(builder, snapshotStartPlaceholder)); err != nil {
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

	if err := server.enroll(registrationID(snapshotStartPlaceholder)); err != nil {
		t.Fatal(err)
	}

	// CAPTURED WHILE ITS WAITER ASKS, which is after the guest kernel finished
	// booting (Firecracker's condition for a snapshot) and after the agent did
	// everything a job's guest does before its runner starts.
	waiting, err := server.await(t.Context(), registrationID(snapshotStartPlaceholder), "waiting", 5*time.Minute, nil)
	if err != nil {
		t.Fatalf("the builder's waiter: %v", err)
	}
	bootToWaiting := waiting.at.Sub(bootStarted)

	captureStarted := time.Now()
	captureCtx, cancelCapture := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancelCapture()
	evidence, err := goldens.Capture(captureCtx, builder, snapshot)
	if err != nil {
		t.Fatalf("Capture the golden: %v", err)
	}
	captured := time.Now()
	server.retire(registrationID(snapshotStartPlaceholder))
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
		err := runStartTrial(t, p, spec, server, startTrial{
			index: i, arm: arm, jits: jits, leases: leases, snapshot: snapshot, evidence: evidence,
			goldenMAC: goldenMAC, goldenAddr: goldenAddr, captured: captured, outstanding: outstanding,
		}, fields)
		// A TRIAL WHOSE GUEST COULD NOT BE DESTROYED ENDS THE EXPERIMENT, since the
		// next fork would share its MAC and address, and so does a golden's lease
		// the bridge's DHCP no longer holds for it. Its warning, with the time by
		// which the fork must be killed, is reported before anything that could fail.
		stop := errors.Is(err, errTrialSurvived) || errors.Is(err, errLeaseLost)
		if stop {
			// STRAIGHT TO STDERR AS WELL, which the test binary does not buffer: the
			// test's own log is held until it ends, after every cleanup.
			fmt.Fprintf(os.Stderr, "SNAPSHOT START EXPERIMENT STOPPED: trial %d (%s): %v\n", i, arm, err)
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

// startTrial is what a trial needs beyond the provider, its spec and the server.
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

// snapshotStartWaiter is the command both arms run in place of the runner. With
// $1 the golden's placeholder, $2 the metadata service, $3 the host's listener
// and $4 the lease renewal, it:
//   - reports `waiting` under the placeholder's key about once a second while
//     the metadata service holds no registration but the placeholder, asking with
//     a fresh token every time, since a restored VMM honours none the golden's
//     did, and every read bounded to a second, since one in flight at the capture
//     is answered by nobody;
//   - once it has one, reports `registered` and the guest's time in milliseconds
//     under that registration's key, renews its DHCP lease in the background (a
//     fork resumes with its golden's lease timers and lives minutes, so without
//     this none would renew, and the bridge's DHCP would let the golden's address
//     go an hour after the golden booted), and holds until the host, having read
//     the guest's memory, answers its key's release with `go`;
//   - reports `released` in the background, so no retried report stands between
//     the release and the runner, runs the runner, reports `finished` with the time
//     and the runner's status, and holds on until the host destroys the guest.
//
// A report is retried until the host takes it, a few seconds at most.
const snapshotStartWaiter = `set -u
mmds=$2 host=$3
key() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }
report() {
	local i
	for i in $(seq 1 30); do
		if curl -sf --connect-timeout 1 --max-time 2 -X POST --data-binary "$2" "$host/state/$1" >/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	return 1
}
golden=$(key "$1")
n=0
while :; do
	if token=$(curl -sf --connect-timeout 1 --max-time 1 -X PUT "$mmds/latest/api/token" \
		-H "X-metadata-token-ttl-seconds: 60") &&
		jit=$(curl -sf --connect-timeout 1 --max-time 1 -H "X-metadata-token: $token" \
			"$mmds/latest/meta-data/billet/jit-config") &&
		[ -n "$jit" ] && [ "$jit" != "$1" ]; then
		break
	fi
	if [ $((n % 50)) -eq 0 ]; then
		report "$golden" waiting || true
	fi
	n=$((n + 1))
	sleep 0.02
done
id=$(key "$jit")
report "$id" "registered $(date +%s%3N)" || exit 1
( bash -c "$4" >/dev/null 2>&1 & )
until [ "$(curl -sf --connect-timeout 1 --max-time 1 "$host/release/$id")" = go ]; do sleep 0.01; done
released="released $(date +%s%3N)"
( report "$id" "$released" & )
ACTIONS_RUNNER_INPUT_JITCONFIG="$jit" ./run.sh
rc=$?
report "$id" "finished $(date +%s%3N) $rc" || true
exec sleep infinity`

// registrationID is the key a waiter reports under: its registration's sha256.
func registrationID(jit string) string {
	sum := sha256.Sum256([]byte(jit))

	return hex.EncodeToString(sum[:])
}

// startStep is one step a waiter reported and when the host received it.
type startStep struct {
	line string
	at   time.Time
}

// startServer is the host's end of the waiters' protocol: it keeps the steps a
// waiter reports, by key, with the host's time of receipt, and answers a waiter's
// release once the experiment has released its key.
//
// ANY GUEST ON THE BRIDGE CAN REACH IT, so it holds only what the experiment asked
// it to: a key is taken only while enrolled (the golden's, then one trial's at a
// time), each keeps at most maxStartSteps steps and one `waiting`, a step is one
// of the four the waiter sends, whole, and a retired key is forgotten. Connections
// are capped and every read and write bounded.
type startServer struct {
	mu       sync.Mutex
	enrolled map[string]bool
	ever     map[string]bool
	steps    map[string][]startStep
	released map[string]bool
	polls    map[string]int
	http     *http.Server
	addr     string
	done     chan struct{}
}

// maxStartSteps bounds the steps one key keeps: a waiter sends four, and a
// retried report may send one twice.
const maxStartSteps = 8

var (
	startKey  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	startLine = regexp.MustCompile(`^(waiting|registered [0-9]{1,15}|released [0-9]{1,15}|finished [0-9]{1,15} [0-9]{1,3})$`)
)

func newStartServer() *startServer {
	return &startServer{enrolled: map[string]bool{}, ever: map[string]bool{}, steps: map[string][]startStep{},
		released: map[string]bool{}, polls: map[string]int{}, done: make(chan struct{})}
}

// enroll lets key report, once in the server's life: a key enrolled before is
// refused, since its steps would be another trial's.
func (s *startServer) enroll(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ever[key] {
		return fmt.Errorf("the registration %s.. was enrolled before; a trial needs a registration of its own",
			key[:12])
	}
	s.ever[key], s.enrolled[key] = true, true

	return nil
}

// retire forgets key: what it reported, its release and its polls.
func (s *startServer) retire(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.enrolled, key)
	delete(s.steps, key)
	delete(s.released, key)
	delete(s.polls, key)
}

// listenForGuests starts a startServer on addr.
func listenForGuests(ctx context.Context, addr string) (*startServer, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	s := newStartServer()
	s.addr = listener.Addr().String()
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 4 << 10}
	go func() {
		defer close(s.done)
		_ = s.http.Serve(netutil.LimitListener(listener, 32)) //nolint:errcheck // ErrServerClosed at close
	}()

	return s, nil
}

// url is where a guest reaches the server.
func (s *startServer) url() string { return "http://" + s.addr }

// close stops the listener and waits for it.
func (s *startServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.http.Shutdown(ctx); err != nil {
		_ = s.http.Close() // the shutdown's own error is the one that matters
	}
	<-s.done
}

func (s *startServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/state/"):
		key := strings.TrimPrefix(r.URL.Path, "/state/")
		body, err := io.ReadAll(io.LimitReader(r.Body, 64))
		if err != nil || !startKey.MatchString(key) || !startLine.Match(body) {
			http.Error(w, "not a step", http.StatusBadRequest)
			return
		}
		at := time.Now()
		s.mu.Lock()
		taken := s.enrolled[key] && len(s.steps[key]) < maxStartSteps &&
			!(string(body) == "waiting" && slices.ContainsFunc(s.steps[key],
				func(st startStep) bool { return st.line == "waiting" }))
		if taken {
			s.steps[key] = append(s.steps[key], startStep{line: string(body), at: at})
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/release/"):
		key := strings.TrimPrefix(r.URL.Path, "/release/")
		s.mu.Lock()
		go_ := s.released[key]
		if s.enrolled[key] {
			s.polls[key]++
		}
		s.mu.Unlock()
		if !go_ {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprint(w, "go")
	default:
		http.NotFound(w, r)
	}
}

// release lets the waiter reporting under key run its runner.
func (s *startServer) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released[key] = true
}

// pollsOf is how many times the waiter under key has asked for its release.
func (s *startServer) pollsOf(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.polls[key]
}

// reported is every step reported under key so far.
func (s *startServer) reported(key string) []startStep {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.steps[key])
}

// await waits up to limit for the first step under key whose first word is step,
// calling each, when given, on every poll.
func (s *startServer) await(ctx context.Context, key, step string, limit time.Duration,
	each func(),
) (startStep, error) {
	deadline := time.Now().Add(limit)
	for {
		if each != nil {
			each()
		}
		for _, st := range s.reported(key) {
			if first, _, _ := strings.Cut(st.line, " "); first == step {
				return st, nil
			}
		}
		if time.Now().After(deadline) {
			return startStep{}, fmt.Errorf("no %s step under %s.. after %s (had %d)", step, key[:12], limit,
				len(s.reported(key)))
		}
		select {
		case <-ctx.Done():
			return startStep{}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// stepFields are a step's numbers after its word.
func stepFields(st startStep) []int64 {
	var out []int64
	for _, f := range strings.Fields(st.line)[1:] {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}

	return out
}

// runStartTrial runs one trial and fills its fields: when billet was asked, when
// it answered, when the waiter had its registration by the guest's clock and when
// the host received it, the VMM's memory while the runner is held there and
// later, and when the runner finished its one job. The guest is destroyed however
// the trial ends, before the next starts; one that could not be is
// errTrialSurvived.
func runStartTrial(t *testing.T, p *Provider, spec func(name, jit string) provider.Spec, server *startServer,
	tr startTrial, fields map[string]any,
) (err error) {
	t.Helper()
	arm, goldenMAC, goldenAddr := tr.arm, tr.goldenMAC, tr.goldenAddr

	jit, err := takeJIT(t.Context(), filepath.Join(tr.jits, strconv.Itoa(tr.index)+".jit"), 10*time.Minute)
	if err != nil {
		return err
	}
	key := registrationID(jit)
	if err := server.enroll(key); err != nil {
		return err
	}
	defer server.retire(key)

	// A FORK ONLY WHILE THE GOLDEN'S ADDRESS IS STILL ITS OWN for longer than the
	// fork can live, whether or not the fork's renewal works: a lease that could
	// lapse under it, or the address given to another MAC, would put the fork on an
	// address a production guest holds. A renewal that fails ends the restores
	// early instead.
	checked := time.Now()
	killBy := checked.Add(forkLeaseNeed - killMargin)
	if arm == "restore" {
		left, err := goldenLeaseHolds(tr.leases, goldenMAC, goldenAddr, forkLeaseNeed, checked)
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

	registered, err := server.await(ctx, key, "registered", 5*time.Minute, nil)
	if err != nil {
		return fmt.Errorf("the waiter: %w", err)
	}
	stamp := stepFields(registered)
	if len(stamp) != 1 {
		return fmt.Errorf("the waiter reported %q", registered.line)
	}
	guestMS := stamp[0]
	fields["registered_guest_epoch_ms"] = guestMS
	fields["registered_seen_ms"] = registered.at.Sub(asked).Milliseconds()
	fields["registered_guest_ms"] = guestMS - asked.UnixMilli()
	// HOW FAR THE GUEST'S STAMP PRECEDES THE HOST'S RECEIPT OF IT: the report's
	// trip, plus however far the guest's clock is behind the host's. A large value
	// says the clock is behind; a negative one, ahead.
	fields["guest_stamp_before_receipt_ms"] = registered.at.UnixMilli() - guestMS

	// THE MEMORY WHILE THE RUNNER IS HELD AT ITS REGISTRATION, the same instruction
	// in both arms, then the release.
	pid, err := vmmPID(j)
	if err != nil {
		return err
	}
	fields["memory_at_registered"] = smapsRollup(pid)
	released := time.Now()
	server.release(key)

	if arm == "restore" {
		// THE FORK IS THE GOLDEN ON THE WIRE: its MAC, on its own tap, which its
		// report to the host has taught the bridge.
		fdb, err := exec.CommandContext(ctx, "bridge", "fdb", "show", "dev", res.Tap).Output()
		fields["fork_mac_is_golden"] = err == nil && strings.Contains(string(fdb), goldenMAC)
	}

	// SAMPLED UNTIL THE RUNNER FINISHES ITS JOB: the memory thirty seconds after the
	// waiter reported its release, which it does as the runner starts, so a release
	// the waiter was slow to see does not move the sample before the runner; and
	// the peak.
	var peak int64
	sampled := false
	var runnerFrom time.Time
	finished, err := server.await(ctx, key, "finished", 20*time.Minute, func() {
		m := smapsRollup(pid)
		if pss, ok := m["Pss"]; ok && pss > peak {
			peak = pss
		}
		if runnerFrom.IsZero() {
			for _, st := range server.reported(key) {
				if strings.HasPrefix(st.line, "released ") {
					runnerFrom = st.at
					fields["release_seen_after_ms"] = st.at.Sub(released).Milliseconds()
				}
			}
		}
		if !sampled && !runnerFrom.IsZero() && time.Since(runnerFrom) >= 30*time.Second {
			fields["memory_30s_after_release"] = m
			sampled = true
		}
	})
	fields["pss_peak_kib"] = peak
	if err != nil {
		return fmt.Errorf("the runner: %w", err)
	}
	for _, st := range server.reported(key) {
		if strings.HasPrefix(st.line, "released ") {
			if n := stepFields(st); len(n) == 1 {
				fields["released_guest_ms"] = n[0] - asked.UnixMilli()
			}
		}
	}
	end := stepFields(finished)
	if len(end) != 2 {
		return fmt.Errorf("the waiter reported %q", finished.line)
	}
	fields["finished_guest_ms"] = end[0] - asked.UnixMilli()
	fields["runner_status"] = end[1]
	if end[1] != 0 {
		return fmt.Errorf("the runner exited %d", end[1])
	}

	return nil
}

// heldLock is an exclusive flock holdLock took.
type heldLock struct{ f *os.File }

// release unlocks, then closes. UNLOCKED FIRST: a child forked while the
// descriptor was open shares its open file description, and a close alone would
// leave the lock held until that child execs.
func (l *heldLock) release() error {
	return errors.Join(unix.Flock(int(l.f.Fd()), unix.LOCK_UN), l.f.Close())
}

// holdLock takes an exclusive flock on path without waiting.
func holdLock(path string) (*heldLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("%s is held by another user of the port: %w", path, err), f.Close())
	}

	return &heldLock{f: f}, nil
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

// THE HOST TAKES ONLY WHOLE STEPS UNDER A KEY, and releases only the key it was
// told to: a malformed key or step is refused, a step is kept with the time it
// arrived, and one key's release is not another's.
func TestTheStartServerTakesOnlyWholeSteps(t *testing.T) {
	t.Parallel()

	s := newStartServer()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	key, other := registrationID("one"), registrationID("two")
	for _, k := range []string{key, other} {
		if err := s.enroll(k); err != nil {
			t.Fatal(err)
		}
	}

	post := func(k, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/state/"+k,
			strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}

		return resp.StatusCode
	}
	get := func(k string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/release/"+k, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if err := errors.Join(err, resp.Body.Close()); err != nil {
			t.Fatal(err)
		}

		return string(body)
	}

	for _, c := range []struct{ key, body string }{
		{"not-a-key", "waiting"},
		{key, "registered 179\nfinished 1 0"},
		{key, "registered"},
		{key, "registered 1731600000123 extra"},
		{key, "finished 1731600000123"},
		{key, "running 1"},
		{strings.ToUpper(key), "waiting"},
	} {
		if code := post(c.key, c.body); code != http.StatusBadRequest {
			t.Errorf("a step %q under %q answered %d; want it refused", c.body, c.key, code)
		}
	}
	if steps := s.reported(key); len(steps) != 0 {
		t.Fatalf("refused steps were kept: %v", steps)
	}

	before := time.Now()
	for _, body := range []string{"waiting", "registered 1731600000123", "finished 1731600000999 3"} {
		if code := post(key, body); code != http.StatusNoContent {
			t.Errorf("the step %q answered %d; want it taken", body, code)
		}
	}
	steps := s.reported(key)
	if len(steps) != 3 || steps[1].line != "registered 1731600000123" || steps[1].at.Before(before) {
		t.Errorf("the steps kept are %v; want the three, timed on arrival", steps)
	}
	if n := stepFields(steps[2]); len(n) != 2 || n[0] != 1731600000999 || n[1] != 3 {
		t.Errorf("the finished step's fields are %v", n)
	}

	if got := get(key); got != "" {
		t.Errorf("a key not released answered %q", got)
	}
	s.release(other)
	if got := get(key); got != "" {
		t.Errorf("another key's release released this one (%q)", got)
	}
	s.release(key)
	if got := get(key); got != "go" {
		t.Errorf("a released key answered %q; want go", got)
	}
	if code := post(key, "registered 1"); code != http.StatusNoContent || len(s.reported(other)) != 0 {
		t.Errorf("one key's steps reached another's")
	}

	// A KEY NOT ENROLLED KEEPS NOTHING, a key keeps one waiting and at most
	// maxStartSteps, and a retired key is forgotten and cannot be enrolled again.
	stranger := registrationID("never enrolled")
	post(stranger, "waiting")
	if n := len(s.reported(stranger)); n != 0 {
		t.Errorf("a key never enrolled kept %d steps", n)
	}
	for range 3 {
		post(other, "waiting")
	}
	for range 2 * maxStartSteps {
		post(other, "registered 1")
	}
	waits := 0
	for _, st := range s.reported(other) {
		if st.line == "waiting" {
			waits++
		}
	}
	if n := len(s.reported(other)); n != maxStartSteps || waits != 1 {
		t.Errorf("a flooded key kept %d steps, %d of them waiting; want %d and one", n, waits, maxStartSteps)
	}
	s.retire(key)
	if len(s.reported(key)) != 0 || get(key) != "" {
		t.Error("a retired key kept its steps or its release")
	}
	if err := s.enroll(key); err == nil {
		t.Error("a key enrolled before was enrolled again")
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
	path := filepath.Join(t.TempDir(), "dnsmasq.leases")
	if err := os.WriteFile(path, []byte("1731603600 02:00:00:00:00:0a 172.31.0.42 * *\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if left, err := goldenLeaseHolds(path, mac, addr, 0, now); err != nil || left != time.Hour {
		t.Errorf("a lease with an hour left answered %s (%v)", left, err)
	}
	if _, err := goldenLeaseHolds(filepath.Join(t.TempDir(), "absent"), mac, addr, 0, now); err == nil {
		t.Error("a lease file that could not be read was taken to hold the lease")
	}
}

// THE PORT'S LOCK EXCLUDES ANOTHER HOLDER: a second take refuses while the first
// holds it, and succeeds once it is released.
func TestThePortsLockExcludesAnotherHolder(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "port.lock")
	lock, err := holdLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := holdLock(path); err == nil {
		t.Errorf("a lock already held was taken again (its release: %v)", again.release())
	}
	// ITS OPEN FILE DESCRIPTION SHARED WITH ANOTHER DESCRIPTOR, as a child forked
	// while it was held shares it, it is still released.
	shared, err := unix.Dup(int(lock.f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Close(shared); err != nil {
			t.Error(err)
		}
	})
	if err := lock.release(); err != nil {
		t.Fatal(err)
	}
	again, err := holdLock(path)
	if err != nil {
		t.Fatalf("a released lock whose description another descriptor shares could not be taken: %v", err)
	}
	if err := again.release(); err != nil {
		t.Fatal(err)
	}
}

// THE WAITER WAITS FOR A REAL REGISTRATION AND REPORTS WHAT IT DID: against a
// stand-in for the metadata service and the host's own server, it reports waiting
// under the placeholder's key while the service answers the placeholder, asks for
// a fresh token before every read (a restored VMM honours none the golden's did,
// which the stand-in enforces by honouring only the newest token), reports its
// registration under that registration's key, holds there without running the
// runner until that key is released, runs the runner with the registration the
// service gives, reports every step, and holds on after the runner finishes.
func TestTheSnapshotStartWaiterWaitsForARealRegistration(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("the waiter stamps its steps with GNU date's %3N")
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

	server, err := listenForGuests(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.close)
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	// A RECORDER IN PLACE OF THE GUEST'S RENEWAL, so the host's own networking is
	// never touched.
	renewed := filepath.Join(dir, "renewed")
	runner := "#!/bin/sh\nprintf '%s' \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" >" + seen + "\nexit 3\n"
	if err := forkSafeWriteFile(filepath.Join(dir, "run.sh"), []byte(runner), 0o755); err != nil {
		t.Fatal(err)
	}

	// A PROCESS GROUP OF ITS OWN, so the sleep the waiter leaves behind ends with it.
	cmd := exec.CommandContext(t.Context(), "/bin/bash", "-c", snapshotStartWaiter, "waiter",
		snapshotStartPlaceholder, mmds.URL, server.url(), "printf renewed >"+renewed)
	cmd.Dir = dir
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

	goldenKey, realKey := registrationID(snapshotStartPlaceholder), registrationID("a-real-registration")
	for _, k := range []string{goldenKey, realKey} {
		if err := server.enroll(k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := server.await(t.Context(), goldenKey, "waiting", 30*time.Second, nil); err != nil {
		t.Fatalf("the waiter never reported waiting: %v", err)
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
	// NOTHING BUT WAITING WHILE THE PLACEHOLDER IS SERVED.
	for _, st := range server.reported(goldenKey) {
		if st.line != "waiting" {
			t.Errorf("the waiter reported %q before it had a real registration", st.line)
		}
	}

	switched := time.Now()
	mu.Lock()
	jit = "a-real-registration"
	mu.Unlock()

	registered, err := server.await(t.Context(), realKey, "registered", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("the waiter never reported registered: %v", err)
	}
	// STAMPED BETWEEN THE SWITCH AND ITS ARRIVAL, on this host's one clock.
	if n := stepFields(registered); len(n) != 1 || n[0] < switched.UnixMilli() || n[0] > registered.at.UnixMilli() {
		t.Errorf("the registration was stamped %q, not between the switch (%d) and its arrival (%d)",
			registered.line, switched.UnixMilli(), registered.at.UnixMilli())
	}
	// HELD THERE until its own key is released: with another key released, it asks
	// for its own and is refused, more than once, and the runner has not run.
	server.release(goldenKey)
	deadline = time.Now().Add(10 * time.Second)
	for server.pollsOf(realKey) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the waiter asked for its release %d times in 10s", server.pollsOf(realKey))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(seen); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the runner ran before the waiter was released (%v)", err)
	}
	server.release(realKey)

	finished, err := server.await(t.Context(), realKey, "finished", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("the waiter never reported finished: %v", err)
	}
	if n := stepFields(finished); len(n) != 2 || n[1] != 3 {
		t.Errorf("the waiter reported %q; want the runner's status 3", finished.line)
	}
	var words []string
	for _, st := range server.reported(realKey) {
		first, _, _ := strings.Cut(st.line, " ")
		words = append(words, first)
	}
	if !slices.Equal(words, []string{"registered", "released", "finished"}) {
		t.Errorf("the waiter reported %v under its registration; want registered, released, finished", words)
	}
	if got, err := os.ReadFile(seen); err != nil || string(got) != "a-real-registration" {
		t.Errorf("the runner was given %q (%v); want the real registration", got, err)
	}
	// THE RENEWAL RUNS IN THE BACKGROUND, so it is waited for on its own.
	deadline = time.Now().Add(10 * time.Second)
	for {
		got, err := os.ReadFile(renewed)
		if err == nil && string(got) == "renewed" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the waiter did not run its renewal (%q, %v)", got, err)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// AND IT HOLDS ON until the host destroys the guest.
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
