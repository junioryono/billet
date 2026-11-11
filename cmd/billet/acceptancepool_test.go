package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// poolJob is a matrix job that ran on a pool host, pool-p1, of a pool tier.
func poolJob() acceptanceJob {
	j := coldGitJob()
	j.LeaseID, j.Tier, j.Node, j.Provider, j.InstanceType = "l-pool", "accept-fc", "pool-p1", "firecracker", ""

	return j
}

// poolEvidence is evidence of one AWS metal order, p1, whose host joined as pool-p1.
func poolEvidence(jobs ...acceptanceJob) acceptanceEvidenceDoc {
	return acceptanceEvidenceDoc{DeploymentID: "d1", Jobs: jobs, Pool: []acceptancePoolOrder{{
		ID: "p1", Source: config.PoolSourceAWSMetal, Arch: string(config.ArchX64), State: alloc.PoolReleased,
		Node: "pool-p1", ReleaseRequested: "2026-11-05T11:00:00Z",
		ReleaseReason: "idle since 2026-11-05T10:30:30Z, past server.pool.idle_after 5m0s",
	}}}
}

func resultOf(t *testing.T, doc matrixDoc, backend config.ProviderKind, source string) matrixResult {
	t.Helper()
	for i := range doc.Results {
		r := &doc.Results[i]
		if r.Backend == backend && r.Source == source && r.Arch == config.ArchX64 && r.Cache == "git" &&
			r.Case == "cold" {
			return *r
		}
	}
	t.Fatalf("no %s/%s x64 git cold cell", backend, source)

	return matrixResult{}
}

// A POOL CELL'S JOB IS ONE ON A HOST OF ITS SOURCE, as the pool's own orders say: the same
// Firecracker job on a node no order names is no pool's, and a pool host's job is never
// an EC2 cell's.
func TestAPoolCellReadsOnlyJobsOnItsSourcesHosts(t *testing.T) {
	t.Parallel()

	doc := judgeMatrix(matrixCells(), poolEvidence(poolJob()), matrixScope{tiers: matrixTestTiers()})
	if r := resultOf(t, doc, config.ProviderFirecracker, config.PoolSourceAWSMetal); r.Verdict != verdictPass {
		t.Errorf("the pool host's job left the aws-metal cell %s: %s", r.Verdict, r.Reason)
	}

	owned := poolJob()
	owned.Node = "fc-1"
	doc = judgeMatrix(matrixCells(), poolEvidence(owned), matrixScope{tiers: matrixTestTiers()})
	if r := resultOf(t, doc, config.ProviderFirecracker, config.PoolSourceAWSMetal); r.Verdict != verdictNotRun {
		t.Errorf("a job on a host no order names left the aws-metal cell %s", r.Verdict)
	}

	onPool := coldGitJob()
	onPool.Node = "pool-p1"
	doc = judgeMatrix(matrixCells(), poolEvidence(onPool), matrixScope{tiers: matrixTestTiers()})
	if r := resultOf(t, doc, config.ProviderEC2, ""); r.Verdict != verdictNotRun {
		t.Errorf("a pool host's job was the EC2 cell's: %s", r.Verdict)
	}
}

func scenarioOf(t *testing.T, doc matrixDoc, name string) matrixScenario {
	t.Helper()
	for _, s := range doc.Scenarios {
		if s.Source == config.PoolSourceAWSMetal && s.Arch == config.ArchX64 && s.Name == name {
			return s
		}
	}
	t.Fatalf("no aws-metal x64 %s scenario", name)

	return matrixScenario{}
}

// THE BURST IS DEMAND'S ORDER WHOSE HOST RAN A JOB: from a floor of none, a host the pool
// ordered joined and ran one of the run's jobs to success. A floor above none, or no job
// on the host, is not one.
func TestTheBurstIsAnOrderedHostThatRanAJob(t *testing.T) {
	t.Parallel()

	doc := judgeMatrix(matrixCells(), poolEvidence(poolJob()), matrixScope{tiers: matrixTestTiers()})
	if s := scenarioOf(t, doc, scenarioBurst); s.Verdict != verdictPass || !s.Owed {
		t.Errorf("burst: %+v; want an owed pass", s)
	}

	doc = judgeMatrix(matrixCells(), poolEvidence(), matrixScope{tiers: matrixTestTiers()})
	if s := scenarioOf(t, doc, scenarioBurst); s.Verdict != verdictFail {
		t.Errorf("an order whose host ran nothing: %+v; want a fail", s)
	}

	doc = judgeMatrix(matrixCells(), poolEvidence(poolJob()), matrixScope{tiers: matrixTestTiers(),
		poolFloors: map[config.Arch]int{config.ArchX64: 1}})
	if s := scenarioOf(t, doc, scenarioBurst); s.Verdict != verdictNotRun || !strings.Contains(s.Reason, "floor") {
		t.Errorf("a pool with a floor: %+v; want not run, saying the floor", s)
	}

	doc = judgeMatrix(matrixCells(), acceptanceEvidenceDoc{DeploymentID: "d1"}, matrixScope{tiers: matrixTestTiers()})
	if s := scenarioOf(t, doc, scenarioBurst); s.Verdict != verdictNotRun || !s.Owed {
		t.Errorf("no order at all: %+v; want an owed not run", s)
	}
}

// THE IDLE SHRINK IS THE POOL'S OWN RELEASE AFTER THE HOST'S LAST JOB, COMPLETED, KILLING
// NONE. A person's release is not the pool's; a release asked before a job on the host
// ended, a job disrupted, or a release still sealed fails it.
func TestTheIdleShrinkIsThePoolsOwnReleaseAfterTheLastJob(t *testing.T) {
	t.Parallel()

	judge := func(doc acceptanceEvidenceDoc) matrixScenario {
		return scenarioOf(t, judgeMatrix(matrixCells(), doc, matrixScope{tiers: matrixTestTiers()}), scenarioShrinkIdle)
	}

	if s := judge(poolEvidence(poolJob())); s.Verdict != verdictPass {
		t.Errorf("the pool's own release after the last job: %+v; want a pass", s)
	}

	byHand := poolEvidence(poolJob())
	byHand.Pool[0].ReleaseReason = "rotating the fleet"
	if s := judge(byHand); s.Verdict != verdictFail || !strings.Contains(s.Reason, "other than by the pool's own") {
		t.Errorf("a person's release: %+v; want a fail", s)
	}

	// ONE HOST'S SHRINK DOES NOT STAND FOR ANOTHER'S THAT NEVER CAME: a second host that
	// ran the later jobs and is still placed fails it, and so does an order whose host never
	// joined and is still being ordered.
	for name, o := range map[string]acceptancePoolOrder{
		"a second host still placed": {ID: "p2", Source: config.PoolSourceAWSMetal, Arch: "x64",
			State: alloc.PoolPlaced, Node: "pool-p2"},
		"an order still being ordered": {ID: "p3", Source: config.PoolSourceAWSMetal, Arch: "x64",
			State: alloc.PoolOrdering},
	} {
		doc := poolEvidence(poolJob())
		doc.Pool = append(doc.Pool, o)
		if s := judge(doc); s.Verdict != verdictFail || !strings.Contains(s.Reason, o.ID+": still "+o.State) {
			t.Errorf("%s: %+v; want a fail naming it", name, s)
		}
	}

	// A RELEASE THAT WITNESSED NO JOB OF THIS RUN IS NOT ONE: the host ran nothing.
	if s := judge(poolEvidence()); s.Verdict != verdictFail || !strings.Contains(s.Reason, "none had run a job") {
		t.Errorf("a released host that ran no job: %+v; want a fail", s)
	}

	late := poolJob()
	late.FinishedAt = "2026-11-05T11:05:00Z"
	if s := judge(poolEvidence(late)); s.Verdict != verdictFail || !strings.Contains(s.Reason, "not before") {
		t.Errorf("a release asked before a job on the host ended: %+v; want a fail", s)
	}

	disrupted := poolJob()
	disrupted.Disruption = "host-lost"
	if s := judge(poolEvidence(disrupted)); s.Verdict != verdictFail || !strings.Contains(s.Reason, "disrupted") {
		t.Errorf("a job disrupted: %+v; want a fail", s)
	}

	sealed := poolEvidence(poolJob())
	sealed.Pool[0].State = alloc.PoolSealed
	if s := judge(sealed); s.Verdict != verdictFail || !strings.Contains(s.Reason, "still sealed") {
		t.Errorf("a release not completed: %+v; want a fail", s)
	}
}

// THE SCENARIOS THE LANE DOES NOT RUN SAY WHY, AND ARE NEVER OWED: a shrink under other
// hosts' running jobs, a zone out of capacity, a quota reached, and phoenixNAP's every one.
func TestScenariosTheLaneDoesNotRunSayWhy(t *testing.T) {
	t.Parallel()

	doc := judgeMatrix(matrixCells(), poolEvidence(poolJob()), matrixScope{tiers: matrixTestTiers()})
	for _, s := range doc.Scenarios {
		run := s.Source == config.PoolSourceAWSMetal && (s.Name == scenarioBurst || s.Name == scenarioShrinkIdle)
		if run {
			continue
		}
		if s.Verdict != verdictNotRun || s.Owed || s.Reason == "" {
			t.Errorf("%s %s %s: %+v; want not run, unowed, with a reason", s.Source, s.Arch, s.Name, s)
		}
	}
}

// A FAILED SCENARIO FAILS THE RUN, whatever backend the run served.
func TestAFailedScenarioFailsTheRun(t *testing.T) {
	t.Parallel()

	late := poolJob()
	late.FinishedAt = "2026-11-05T11:05:00Z"
	doc := judgeMatrix(matrixCells(), poolEvidence(late), matrixScope{tiers: matrixTestTiers()})
	if err := matrixOutcome(doc, nil, []string{"ec2"}); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("a failed idle shrink answered %v", err)
	}
}

// A POOL'S ORDER HOLDS A HOST UNTIL IT IS RELEASED, GONE, REFUSED OR ABANDONED: one being
// ordered, placed, sealed or uncertain may still have one.
func TestAPoolOrderHoldsAHostUntilItLetsGo(t *testing.T) {
	t.Parallel()

	for state, holds := range map[string]bool{
		alloc.PoolOrdering: true, alloc.PoolPlaced: true, alloc.PoolSealed: true, alloc.PoolUncertain: true,
		alloc.PoolReleased: false, alloc.PoolGone: false, alloc.PoolRefused: false, alloc.PoolAbandoned: false,
	} {
		if got := poolOrderHolds(state); got != holds {
			t.Errorf("%s holds a host: %v, want %v", state, got, holds)
		}
	}
}

// fakePoolHosts is EC2's account of a run's pool hosts. An ended host is shutting down
// for its next shutdown reads and terminated after; a terminated host is described by
// its id and left out of the listing, as Hosts.Live leaves it out; and a host in
// unlisted is left out of every listing whatever its state, as an eventually consistent
// DescribeInstances may leave one out.
type fakePoolHosts struct {
	hosts    []ec2.HostRecord
	unlisted map[string]bool
	listErr  error
	shutdown int
	reads    int
	endedAt  map[string]int
	ended    []string
	// purgedAt is the read count when the cache was purged, -1 before.
	purgedAt int
	// hiddenUntil is, by id, the read before which EC2 does not describe a host at all.
	hiddenUntil map[string]int
	// listedFrom and listedUntil are, by id, the reads before and from which every
	// listing leaves a host out.
	listedFrom, listedUntil map[string]int
}

func newFakePoolHosts(shutdown int, hosts ...ec2.HostRecord) *fakePoolHosts {
	return &fakePoolHosts{hosts: hosts, unlisted: map[string]bool{}, shutdown: shutdown, endedAt: map[string]int{},
		purgedAt: -1}
}

func (f *fakePoolHosts) state(h ec2.HostRecord) ec2.HostRecord {
	if at, ok := f.endedAt[h.ID]; ok {
		h.State = "shutting-down"
		if f.reads-at > f.shutdown {
			h.State = "terminated"
		}
	}

	return h
}

func (f *fakePoolHosts) PoolHosts(context.Context) ([]ec2.HostRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.reads++
	var out []ec2.HostRecord
	for _, h := range f.hosts {
		if h = f.state(h); h.State != "terminated" && !f.unlisted[h.ID] &&
			f.reads >= f.listedFrom[h.ID] && (f.listedUntil[h.ID] == 0 || f.reads < f.listedUntil[h.ID]) {
			out = append(out, h)
		}
	}

	return out, nil
}

func (f *fakePoolHosts) PoolHost(_ context.Context, id string) (ec2.HostRecord, error) {
	f.reads++
	if f.reads < f.hiddenUntil[id] {
		return ec2.HostRecord{}, ec2.ErrHostNotFound
	}
	for _, h := range f.hosts {
		if h.ID == id {
			return f.state(h), nil
		}
	}

	return ec2.HostRecord{}, ec2.ErrHostNotFound
}

func (f *fakePoolHosts) FindPoolHost(_ context.Context, order string) (ec2.HostRecord, bool, error) {
	for _, h := range f.hosts {
		if h.Order == order {
			return f.state(h), true, nil
		}
	}

	return ec2.HostRecord{}, false, nil
}

func (f *fakePoolHosts) TerminatePoolHost(_ context.Context, id string) error {
	f.ended = append(f.ended, id)
	if _, ok := f.endedAt[id]; !ok {
		f.endedAt[id] = f.reads
	}

	return nil
}

type fakePoolCache struct {
	purged   int
	residue  ebss3.Residue
	residErr error
	hosts    *fakePoolHosts
}

func (f *fakePoolCache) Purge(context.Context) (ebss3.PurgeReport, error) {
	f.purged++
	if f.hosts != nil {
		f.hosts.purgedAt = f.hosts.reads
	}

	return ebss3.PurgeReport{Snapshots: 2}, nil
}

func (f *fakePoolCache) Residue(context.Context) (ebss3.Residue, error) { return f.residue, f.residErr }

func quietEnv(out *bytes.Buffer) cli.Env {
	return cli.Env{Stdout: out, Stderr: io.Discard, Stdin: strings.NewReader(""), Getenv: os.Getenv}
}

// order is a pool order of the ledger's, as the evidence records it.
func order(id, state, host string) acceptancePoolOrder {
	return acceptancePoolOrder{ID: id, Source: config.PoolSourceAWSMetal, Arch: "x64", State: state, HostID: host}
}

// THE TEARDOWN ENDS WHAT THE POOL DID NOT RELEASE, WAITS FOR EC2 TO DESCRIBE IT
// TERMINATED, AND ONLY THEN PURGES THE CACHE: a host still up may hold a session no lease
// names. A host already shutting down is not ended again.
func TestAPoolsTeardownEndsItsHostsBeforeThePurge(t *testing.T) {
	t.Parallel()

	hosts := newFakePoolHosts(3, ec2.HostRecord{ID: "i-1", State: "running", Order: "p1"},
		ec2.HostRecord{ID: "i-2", State: "running", Order: "p2"})
	hosts.endedAt["i-2"] = 0
	cache := &fakePoolCache{hosts: hosts}
	p := acceptancePool{hosts: hosts, cache: cache, gone: time.Minute, poll: time.Millisecond}

	var out bytes.Buffer
	orders := []acceptancePoolOrder{order("p1", alloc.PoolPlaced, "i-1"), order("p2", alloc.PoolReleased, "i-2")}
	if _, err := p.decommission(t.Context(), quietEnv(&out), orders); err != nil {
		t.Fatalf("decommission: %v\n%s", err, out.String())
	}
	if len(hosts.ended) != 1 || hosts.ended[0] != "i-1" {
		t.Errorf("ended %v, want i-1 alone (i-2 was shutting down already)", hosts.ended)
	}
	if cache.purged != 1 || hosts.purgedAt < hosts.endedAt["i-1"]+hosts.shutdown+1 {
		t.Errorf("purged %d time(s), at read %d, with i-1 ended at read %d; want once, after it was terminated",
			cache.purged, hosts.purgedAt, hosts.endedAt["i-1"])
	}
}

// A HOST THE LISTING LEFT OUT IS ENDED AND WAITED FOR BY THE ID ITS ORDER NAMES:
// DescribeInstances is eventually consistent, and a host one listing leaves out is not a
// host that is gone.
func TestAPoolsTeardownEndsAHostTheListingLeftOut(t *testing.T) {
	t.Parallel()

	hosts := newFakePoolHosts(2, ec2.HostRecord{ID: "i-3", State: "running", Order: "p3"},
		ec2.HostRecord{ID: "i-4", State: "running", Order: "p4"})
	hosts.unlisted["i-3"], hosts.unlisted["i-4"] = true, true
	cache := &fakePoolCache{hosts: hosts}
	p := acceptancePool{hosts: hosts, cache: cache, gone: time.Minute, poll: time.Millisecond}

	var out bytes.Buffer
	// p4 names no host in the ledger, and EC2 finds it by its order's tag.
	orders := []acceptancePoolOrder{order("p3", alloc.PoolSealed, "i-3"), order("p4", alloc.PoolUncertain, "")}
	if _, err := p.decommission(t.Context(), quietEnv(&out), orders); err != nil {
		t.Fatalf("decommission: %v\n%s", err, out.String())
	}
	if strings.Join(hosts.ended, ",") != "i-3,i-4" {
		t.Errorf("ended %v, want both hosts the listing left out", hosts.ended)
	}
	if cache.purged != 1 || hosts.purgedAt <= hosts.endedAt["i-4"]+hosts.shutdown {
		t.Errorf("purged %d time(s) at read %d; want once, after both were terminated", cache.purged,
			hosts.purgedAt)
	}
}

// AN ORDER WHOSE HOST EC2 CANNOT NAME IS RESIDUE FOR A PERSON, and the cache is kept: the
// host could be one EC2 has not listed yet. An order its source refused holds none.
func TestAnOrderWhoseHostEC2CannotNameIsResidue(t *testing.T) {
	t.Parallel()

	hosts := newFakePoolHosts(0)
	cache := &fakePoolCache{hosts: hosts}
	p := acceptancePool{hosts: hosts, cache: cache, gone: time.Minute, poll: time.Millisecond}

	var out bytes.Buffer
	if _, err := p.decommission(t.Context(), quietEnv(&out),
		[]acceptancePoolOrder{order("p5", alloc.PoolRefused, "")}); err != nil {
		t.Errorf("a refused order with no host answered %v", err)
	}
	cache.purged = 0
	_, err := p.decommission(t.Context(), quietEnv(&out), []acceptancePoolOrder{order("p6", alloc.PoolOrdering, "")})
	if !errors.Is(err, errAcceptanceResidue) || !strings.Contains(err.Error(), "p6 (ordering)") {
		t.Fatalf("an order EC2 names no host for answered %v", err)
	}
	if cache.purged != 0 {
		t.Error("the cache was purged beside an order that may hold a host")
	}
	if err := p.sweep(t.Context(), quietEnv(&out), "d1",
		[]acceptancePoolOrder{order("p6", alloc.PoolOrdering, "")}, nil); !errors.Is(err, errAcceptanceResidue) {
		t.Errorf("the sweep of an order EC2 names no host for answered %v", err)
	}
}

// A HOST THAT DOES NOT GO IS RESIDUE, AND THE CACHE IS LEFT: purging under it could take
// a session's objects from a job still there. So is a host its order names that EC2 no
// longer describes at all: not found is not terminated.
func TestAPoolHostThatWillNotGoIsResidueAndTheCacheIsKept(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		hosts  *fakePoolHosts
		orders []acceptancePoolOrder
		want   string
	}{
		// NAMED WHATEVER IT LAST READ: under a loaded suite the deadline can pass on the
		// poll that ends it, before EC2 has said it is shutting down.
		"a host that stays up": {newFakePoolHosts(1<<30, ec2.HostRecord{ID: "i-1", State: "running", Order: "p1"}),
			[]acceptancePoolOrder{order("p1", alloc.PoolPlaced, "i-1")}, "i-1 "},
		"a host EC2 does not describe": {newFakePoolHosts(0),
			[]acceptancePoolOrder{order("p1", alloc.PoolReleased, "i-9")}, "i-9 not found"},
	} {
		cache := &fakePoolCache{hosts: c.hosts}
		p := acceptancePool{hosts: c.hosts, cache: cache, gone: 10 * time.Millisecond, poll: time.Millisecond}

		var out bytes.Buffer
		_, err := p.decommission(t.Context(), quietEnv(&out), c.orders)
		if !errors.Is(err, errAcceptanceResidue) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s answered %v; want residue naming %q", name, err, c.want)
		}
		if cache.purged != 0 {
			t.Errorf("%s: the cache was purged under a pool host not proved terminated", name)
		}
		if live := c.orders[0].HostID == "i-1"; live && (len(c.hosts.ended) != 1 || c.hosts.ended[0] != "i-1") {
			t.Errorf("%s: ended %v, want the host that stays up ended once", name, c.hosts.ended)
		}
	}
}

// THE SWEEP FINDS A HOST STILL UP, ONE ITS ORDER NAMES AND THE LISTING LEFT OUT, AND A
// CACHE LEFT; AND A LIST OR A CACHE IT COULD NOT READ IS NOT A CLEAN ONE.
func TestAPoolsSweepAsksEC2AndTheCache(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	gone := newFakePoolHosts(0, ec2.HostRecord{ID: "i-1", State: "running", Order: "p1"})
	gone.endedAt["i-1"] = -1
	clean := acceptancePool{hosts: gone, cache: &fakePoolCache{}}
	if err := clean.sweep(t.Context(), quietEnv(&out), "d1",
		[]acceptancePoolOrder{order("p1", alloc.PoolGone, "i-1")}, nil); err != nil {
		t.Errorf("a clean pool's sweep answered %v", err)
	}

	up := acceptancePool{hosts: newFakePoolHosts(0, ec2.HostRecord{ID: "i-1", State: "stopped", Order: "p1"})}
	if err := up.sweep(t.Context(), quietEnv(&out), "d1", nil, nil); !errors.Is(err, errAcceptanceResidue) ||
		!strings.Contains(err.Error(), "i-1 stopped") {
		t.Errorf("a stopped pool host answered %v", err)
	}

	hidden := newFakePoolHosts(0, ec2.HostRecord{ID: "i-2", State: "running", Order: "p2"})
	hidden.unlisted["i-2"] = true
	if err := (acceptancePool{hosts: hidden}).sweep(t.Context(), quietEnv(&out), "d1",
		[]acceptancePoolOrder{order("p2", alloc.PoolPlaced, "i-2")}, nil); !errors.Is(err, errAcceptanceResidue) ||
		!strings.Contains(err.Error(), "i-2 running") {
		t.Errorf("a host its order names and the listing left out answered %v", err)
	}

	left := acceptancePool{hosts: newFakePoolHosts(0), cache: &fakePoolCache{residue: ebss3.Residue{Snapshots: 1}}}
	if err := left.sweep(t.Context(), quietEnv(&out), "d1", nil, nil); !errors.Is(err, errAcceptanceResidue) {
		t.Errorf("a cache left behind answered %v", err)
	}

	throttled := errors.New("throttled")
	blindHosts := newFakePoolHosts(0)
	blindHosts.listErr = throttled
	if err := (acceptancePool{hosts: blindHosts}).sweep(t.Context(), quietEnv(&out), "d1",
		nil, nil); !errors.Is(err, throttled) || errors.Is(err, errAcceptanceResidue) {
		t.Errorf("a list that could not be read answered %v; want could-not-tell", err)
	}
	blindCache := acceptancePool{hosts: newFakePoolHosts(0), cache: &fakePoolCache{residErr: throttled}}
	if err := blindCache.sweep(t.Context(), quietEnv(&out), "d1", nil, nil); !errors.Is(err, throttled) ||
		errors.Is(err, errAcceptanceResidue) {
		t.Errorf("a cache that could not be read answered %v; want could-not-tell", err)
	}
}

// A POOL'S RUN SERVES ITS HOSTS ONLY WHERE ITS OPERATOR SAID: a base config with a pool
// needs the fleet's three flags, one with none refuses them, a pool beside a node of the
// run's own is refused, and the addresses are one literal address of this machine's.
func TestAPoolsRunServesItsFleetOnlyWhereItWasTold(t *testing.T) {
	t.Parallel()

	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	if err := os.WriteFile(prelude, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := &config.Config{Server: &config.ServerConfig{Pool: &config.PoolConfig{IdleAfter: "10m"}}}
	good := &acceptanceFleet{Listen: "10.77.0.10:7717", Bootstrap: "10.77.0.10:7718", Prelude: prelude}

	if err := checkAcceptanceFleet(pool, good); err != nil {
		t.Errorf("a pool's fleet: %v", err)
	}
	if err := checkAcceptanceFleet(&config.Config{Server: &config.ServerConfig{}}, nil); err != nil {
		t.Errorf("no pool and no fleet: %v", err)
	}
	for name, c := range map[string]struct {
		cfg   *config.Config
		fleet *acceptanceFleet
		want  string
	}{
		"a fleet with no pool": {&config.Config{Server: &config.ServerConfig{}}, good, "has no server.pool"},
		"a pool with no fleet": {pool, nil, "pass --fleet-listen"},
		"a pool beside a node": {&config.Config{Server: pool.Server, Node: &config.NodeConfig{}}, good, "and a node"},
		"a pool that never shrinks": {&config.Config{Server: &config.ServerConfig{Pool: &config.PoolConfig{}}}, good,
			"no idle_after"},
		"a loopback wire": {pool, &acceptanceFleet{Listen: "127.0.0.1:7717", Bootstrap: "127.0.0.1:7718",
			Prelude: prelude}, "non-loopback"},
		"a wildcard wire": {pool, &acceptanceFleet{Listen: "0.0.0.0:7717", Bootstrap: "0.0.0.0:7718",
			Prelude: prelude}, "non-loopback"},
		"two addresses": {pool, &acceptanceFleet{Listen: "10.77.0.10:7717", Bootstrap: "10.77.0.11:7718",
			Prelude: prelude}, "one address on two ports"},
		"one port": {pool, &acceptanceFleet{Listen: "10.77.0.10:7717", Bootstrap: "10.77.0.10:7717",
			Prelude: prelude}, "one address on two ports"},
		"a relative prelude": {pool, &acceptanceFleet{Listen: good.Listen, Bootstrap: good.Bootstrap,
			Prelude: "prelude.sh"}, "absolute"},
		"a missing prelude": {pool, &acceptanceFleet{Listen: good.Listen, Bootstrap: good.Bootstrap,
			Prelude: prelude + ".gone"}, "not a file this run can read: stat"},
		"a directory for a prelude": {pool, &acceptanceFleet{Listen: good.Listen, Bootstrap: good.Bootstrap,
			Prelude: filepath.Dir(prelude)}, "is not a regular file"},
		"a hostname": {pool, &acceptanceFleet{Listen: "pool.example:7717", Bootstrap: "pool.example:7718",
			Prelude: prelude}, "literal"},
		"port zero": {pool, &acceptanceFleet{Listen: "10.77.0.10:0", Bootstrap: "10.77.0.10:7718",
			Prelude: prelude}, "literal"},
	} {
		if err := checkAcceptanceFleet(c.cfg, c.fleet); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s answered %v; want a refusal saying %q", name, err, c.want)
		}
	}
}

// poolBaseConfig is a pool's base: a control plane with a pool on AWS metal and no node,
// whose own listeners and prelude the derivation replaces with the run's.
const poolBaseConfig = `server:
  listen: 10.0.0.9:7717
  bootstrap_listen: 10.0.0.9:7718
  state_dir: /var/lib/billet/server
  max_vcpu: 48
  max_memory: 360GiB
  pool:
    idle_after: 10m
    join_server: 10.0.0.9:7717
    bootstrap_addr: 10.0.0.9:7718
    boot_prelude: /etc/billet/pool-prelude.sh
    init_args: ['--site=metal', '--ebs-s3-region=us-west-2', '--ebs-s3-availability-zone=us-west-2a',
      '--ebs-s3-bucket=billet-cache-example', '--cache-listen=10.240.0.1:7718',
      '--cache-guest-endpoint=http://10.240.0.1:7718']
    architectures:
      x64:
        floor: 0
        max: 1
        order:
          - {source: aws-metal, shape: z1d.metal}
    aws_metal:
      region: us-west-2
      subnet_id: subnet-0abc
      availability_zone: us-west-2a
      security_group_ids: [sg-0abc]
      images: {x64: ami-x64}
      shapes:
        - {type: z1d.metal, arch: x64}
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/app-private-key.pem
sites:
  - name: metal
    store: ebs-s3
tiers:
  - label: metal-x64
    provider: firecracker
    arch: x64
    site: metal
    vcpu: 4
    memory: 8GiB
    image: ubuntu-2404-x64@verified
    trust: trusted
    runner_group: billet
    workflows:
      - acme/repo/.github/workflows/ci.yml@refs/heads/main
`

// A POOL'S RUN SERVES ITS HOSTS EXACTLY WHERE IT WAS TOLD, with the run's prelude, and
// records it: the derived config listens and enrolls on the fleet's addresses, its pool's
// hosts dial that wire with that prelude, the base's own bootstrap_addr is gone, and the
// workspace names the fleet, which a re-run with another one is refused against.
func TestAPoolsDerivationServesItsFleet(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, poolBaseConfig)
	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	if err := os.WriteFile(prelude, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fleet := &acceptanceFleet{Listen: "10.77.0.10:7717", Bootstrap: "10.77.0.10:7718", Prelude: prelude}
	dir := t.TempDir()

	ws, err := deriveAcceptance(t.Context(), acceptanceInputs{base: base, workspace: dir, prefix: "accept-pool",
		fleet: fleet, seedLedger: seedTemplate(t)})
	if err != nil {
		t.Fatalf("derive a pool's run: %v", err)
	}
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("the derived config does not load: %v", err)
	}
	s := cfg.Server
	switch {
	case s.Listen != fleet.Listen || s.BootstrapListen != fleet.Bootstrap:
		t.Errorf("the derived control plane listens on %s and enrolls on %s", s.Listen, s.BootstrapListen)
	case s.Pool.JoinServer != fleet.Listen || s.Pool.BootPrelude != prelude:
		t.Errorf("the pool's hosts dial %s with prelude %s", s.Pool.JoinServer, s.Pool.BootPrelude)
	case s.Pool.BootstrapAddr != "":
		t.Errorf("the base's own bootstrap_addr %s was kept", s.Pool.BootstrapAddr)
	case ws.Fleet == nil || *ws.Fleet != *fleet || ws.Listen != fleet.Listen:
		t.Errorf("the workspace records %+v listening on %s", ws.Fleet, ws.Listen)
	}
	if !strings.HasPrefix(s.StateDir, dir) {
		t.Errorf("the derived state directory %s is not the workspace's", s.StateDir)
	}

	other := *fleet
	other.Listen, other.Bootstrap = "10.77.0.11:7717", "10.77.0.11:7718"
	if _, err := deriveAcceptance(t.Context(), acceptanceInputs{base: base, workspace: dir, prefix: "accept-pool",
		fleet: &other}); err == nil || !strings.Contains(err.Error(), "serves its pool's hosts") {
		t.Errorf("a re-run with another fleet answered %v", err)
	}

	if _, err := deriveAcceptance(t.Context(), acceptanceInputs{base: base, workspace: t.TempDir(),
		prefix: "accept-pool"}); err == nil || !strings.Contains(err.Error(), "pass --fleet-listen") {
		t.Errorf("a pool's run with no fleet answered %v", err)
	}
}
