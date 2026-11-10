package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/awspolicy"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/ops/setup"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// THE LEDGER'S ORDERS ARE THE EVIDENCE THE JUDGE READS, field for field: the node a pool
// cell's jobs are matched by, and a release time that reads back as the same instant.
func TestThePoolsOrdersAreTheEvidence(t *testing.T) {
	t.Parallel()

	asked := time.Date(2026, 11, 10, 3, 4, 5, 123456789, time.FixedZone("x", 3600))
	got := acceptancePoolOrders([]alloc.PoolOrder{
		{ID: "p1", Source: config.PoolSourceAWSMetal, Shape: "z1d.metal", Arch: config.ArchX64, State: alloc.PoolGone,
			HostID: "i-1", NodeName: "pool-p1", Reason: alloc.PoolGoneBySource, ReleaseRequested: asked,
			ReleaseReason: alloc.PoolShrinkReason + "then"},
		{ID: "p2", Source: config.PoolSourceAWSMetal, Arch: config.ArchX64, State: alloc.PoolRefused},
	})
	if len(got) != 2 {
		t.Fatalf("%d orders, want 2", len(got))
	}
	o := got[0]
	if o.ID != "p1" || o.Node != "pool-p1" || o.HostID != "i-1" || o.Arch != "x64" || o.State != alloc.PoolGone ||
		o.Reason != alloc.PoolGoneBySource || o.ReleaseReason != alloc.PoolShrinkReason+"then" ||
		o.Source != config.PoolSourceAWSMetal || o.Shape != "z1d.metal" {
		t.Errorf("the order reads %+v", o)
	}
	back, err := time.Parse(time.RFC3339Nano, o.ReleaseRequested)
	if err != nil || !back.Equal(asked) {
		t.Errorf("its release time %q reads back as %v (%v), want %v", o.ReleaseRequested, back, err, asked)
	}
	if got[1].ReleaseRequested != "" || got[1].Node != "" {
		t.Errorf("an order with no release or node reads %+v", got[1])
	}
}

// THE SCENARIOS ARE EXACTLY THE DECLARED ONES: five for each source and architecture,
// the burst and the idle shrink owed on AWS metal, and every other one not run, unowed,
// with its reason.
func TestEveryPoolScenarioIsDeclared(t *testing.T) {
	t.Parallel()

	doc := judgeMatrix(matrixCells(), acceptanceEvidenceDoc{DeploymentID: "d1"}, matrixScope{tiers: matrixTestTiers()})
	var owed, unowed []string
	for _, s := range doc.Scenarios {
		key := s.Source + " " + string(s.Arch) + " " + s.Name
		if s.Owed {
			owed = append(owed, key)
		} else {
			unowed = append(unowed, key)
		}
		if s.Verdict != verdictNotRun || s.Reason == "" {
			t.Errorf("%s with no evidence: %s, %q; want not run, with a reason", key, s.Verdict, s.Reason)
		}
	}
	slices.Sort(owed)
	want := []string{"aws-metal arm64 burst", "aws-metal arm64 shrink-idle", "aws-metal x64 burst",
		"aws-metal x64 shrink-idle"}
	if !slices.Equal(owed, want) {
		t.Errorf("owed %q, want %q", owed, want)
	}
	if len(unowed) != 16 {
		t.Errorf("%d scenarios not owed, want 3 for each AWS metal architecture and 5 for each phoenixNAP one: %q",
			len(unowed), unowed)
	}
}

// THE BURST AND THE SHRINK READ ONLY THEIR OWN SOURCE'S AND ARCHITECTURE'S ORDERS, and a
// job on the host counts for the burst only when it succeeded; an order whose host never
// joined is matched to no job, whatever ran with no node.
func TestThePoolScenariosReadOnlyTheirOwnOrders(t *testing.T) {
	t.Parallel()

	judge := func(doc acceptanceEvidenceDoc, arch config.Arch, name string) matrixScenario {
		t.Helper()
		for _, s := range judgeMatrix(matrixCells(), doc, matrixScope{tiers: matrixTestTiers()}).Scenarios {
			if s.Source == config.PoolSourceAWSMetal && s.Arch == arch && s.Name == name {
				return s
			}
		}
		t.Fatalf("no aws-metal %s %s scenario", arch, name)

		return matrixScenario{}
	}

	failed := poolJob()
	failed.GitHub = "failed"
	if s := judge(poolEvidence(failed), config.ArchX64, scenarioBurst); s.Verdict != verdictFail {
		t.Errorf("a host whose one job failed: %+v; want the burst failed", s)
	}

	for _, name := range []string{scenarioBurst, scenarioShrinkIdle} {
		if s := judge(poolEvidence(poolJob()), config.ArchARM64, name); s.Verdict != verdictNotRun {
			t.Errorf("arm64 %s from an x64 order: %+v; want not run", name, s)
		}
	}

	other := poolEvidence(poolJob())
	other.Pool[0].Source = config.PoolSourcePhoenixNAP
	if s := judge(other, config.ArchX64, scenarioBurst); s.Verdict != verdictNotRun {
		t.Errorf("aws-metal's burst from a phoenixNAP order: %+v; want not run", s)
	}

	unjoined := poolEvidence(coldGitJob())
	unjoined.Pool[0].Node = ""
	if s := judge(unjoined, config.ArchX64, scenarioBurst); s.Verdict != verdictFail {
		t.Errorf("an order whose host never joined, beside a job with no node: %+v; want the burst failed", s)
	}
}

// A SHRINK COMPLETED BY ITS SOURCE PASSES; ONE A PERSON CLOSED DOES NOT, nor one a person
// asked for in other words than the pool's.
func TestAnIdleShrinkIsCompletedByItsSourceNotAPerson(t *testing.T) {
	t.Parallel()

	judge := func(doc acceptanceEvidenceDoc) matrixScenario {
		return scenarioOf(t, judgeMatrix(matrixCells(), doc, matrixScope{tiers: matrixTestTiers()}), scenarioShrinkIdle)
	}

	gone := poolEvidence(poolJob())
	gone.Pool[0].State, gone.Pool[0].Reason = alloc.PoolGone, alloc.PoolGoneBySource
	if s := judge(gone); s.Verdict != verdictPass {
		t.Errorf("gone on its source's word: %+v; want a pass", s)
	}

	closed := poolEvidence(poolJob())
	closed.Pool[0].State, closed.Pool[0].Reason = alloc.PoolGone, "closed by a person: it stalled"
	if s := judge(closed); s.Verdict != verdictFail || !strings.Contains(s.Reason, "other than by its source") {
		t.Errorf("closed by a person: %+v; want a fail", s)
	}

	// AN ORDER THAT NEVER JOINED IS JUDGED TOO: a host a person released before it joined,
	// or an order a person abandoned, is a person's doing beside the pool's shrink; one its
	// source refused never held a host.
	for name, c := range map[string]struct {
		o    acceptancePoolOrder
		pass bool
	}{
		"an unjoined host a person released": {acceptancePoolOrder{ID: "p4", Source: config.PoolSourceAWSMetal,
			Arch: "x64", State: alloc.PoolReleased, HostID: "i-4", ReleaseRequested: "2026-11-05T09:00:00Z",
			ReleaseReason: "it never booted"}, false},
		"an order a person abandoned": {acceptancePoolOrder{ID: "p5", Source: config.PoolSourceAWSMetal,
			Arch: "x64", State: alloc.PoolAbandoned, Reason: "given up by a person"}, false},
		"an order its source refused": {acceptancePoolOrder{ID: "p6", Source: config.PoolSourceAWSMetal,
			Arch: "x64", State: alloc.PoolRefused, Reason: "InsufficientInstanceCapacity"}, true},
	} {
		doc := poolEvidence(poolJob())
		doc.Pool = append(doc.Pool, c.o)
		s := judge(doc)
		if c.pass && s.Verdict != verdictPass {
			t.Errorf("%s beside a shrunk host: %+v; want a pass", name, s)
		}
		if !c.pass && (s.Verdict != verdictFail || !strings.Contains(s.Reason, c.o.ID+": "+c.o.State+" other than")) {
			t.Errorf("%s beside a shrunk host: %+v; want a fail naming it", name, s)
		}
	}
}

// THE SHRINK ANSWERS FOR EVERY JOB ON THE HOST, OF THIS RUN OR NOT: the run's job
// witnesses it, and another run's job on the same host that ended after the release was
// asked, or was disrupted, fails it.
func TestAnIdleShrinkAnswersForEveryJobOnItsHost(t *testing.T) {
	t.Parallel()

	mine := poolJob()
	mine.GitHubRunID = 1
	scope := matrixScope{tiers: matrixTestTiers(), runs: map[int64]bool{1: true}}
	judge := func(doc acceptanceEvidenceDoc) matrixScenario {
		return scenarioOf(t, judgeMatrix(matrixCells(), doc, scope), scenarioShrinkIdle)
	}
	if s := judge(poolEvidence(mine)); s.Verdict != verdictPass {
		t.Fatalf("this run's job alone: %+v; want a pass", s)
	}

	late := poolJob()
	late.LeaseID, late.GitHubRunID, late.FinishedAt = "l-other", 2, "2026-11-05T11:05:00Z"
	if s := judge(poolEvidence(mine, late)); s.Verdict != verdictFail || !strings.Contains(s.Reason, "l-other") {
		t.Errorf("another run's job ending after the release: %+v; want a fail naming it", s)
	}
	cut := poolJob()
	cut.LeaseID, cut.GitHubRunID, cut.Disruption = "l-cut", 2, "host-lost"
	if s := judge(poolEvidence(mine, cut)); s.Verdict != verdictFail || !strings.Contains(s.Reason, "l-cut") {
		t.Errorf("another run's job disrupted on the host: %+v; want a fail naming it", s)
	}

	// EVIDENCE THAT MAY HOLD ONLY SOME OF THE JOBS cannot pass it.
	partial := poolEvidence(mine)
	partial.JobsMayBeMissing = true
	if s := judge(partial); s.Verdict != verdictFail || !strings.Contains(s.Reason, "may be unread") {
		t.Errorf("evidence cut at its limit: %+v; want a fail", s)
	}
	// EVIDENCE WRITTEN BEFORE THE FLAG, decoded, says so by its count.
	full := poolEvidence(slices.Repeat([]acceptanceJob{mine}, acceptanceJobLimit)...)
	body, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var older acceptanceEvidenceDoc
	if err := json.Unmarshal([]byte(strings.Replace(string(body), `"jobs_may_be_missing":true,`, "", 1)),
		&older); err != nil {
		t.Fatal(err)
	}
	if s := judge(older); older.JobsMayBeMissing || s.Verdict != verdictFail || !strings.Contains(s.Reason,
		"may be unread") {
		t.Errorf("older evidence of %d jobs: %+v; want a fail", len(older.Jobs), s)
	}
	// AND A RUN WITH NO POOL IS NOT FAILED BY IT: with no order, there is no shrink to
	// judge, and an EC2 lane's full evidence leaves the pool's scenarios not run.
	ec2Only := older
	ec2Only.Pool = nil
	if s := judge(ec2Only); s.Verdict != verdictNotRun {
		t.Errorf("full evidence of a run with no pool: %+v; want not run", s)
	}
}

// A POOL CELL'S JOB RAN ON A HOST ITS ORDER RENTED AS THE CELL'S ARCHITECTURE: a job of
// an x64 tier on a host ordered as arm64 is no x64 cell's.
func TestAPoolCellReadsOnlyHostsOfItsArchitecture(t *testing.T) {
	t.Parallel()

	doc := poolEvidence(poolJob())
	doc.Pool[0].Arch = string(config.ArchARM64)
	r := resultOf(t, judgeMatrix(matrixCells(), doc, matrixScope{tiers: matrixTestTiers()}),
		config.ProviderFirecracker, config.PoolSourceAWSMetal)
	if r.Verdict != verdictNotRun {
		t.Errorf("an x64 job on an arm64 order's host left the x64 cell %s", r.Verdict)
	}
}

// A RUN OWES ITS BACKEND'S CELLS AND SCENARIOS, and a selection of none is refused rather
// than owing nothing.
func TestAPoolsRunOwesItsBackendAndNoneIsRefused(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", ",", " , "} {
		if _, err := parseMatrixBackends(s); err == nil || !strings.Contains(err.Error(), "name the backends") {
			t.Errorf("--backends %q answered %v", s, err)
		}
	}
	if _, err := parseMatrixBackends("firecracker/nowhere"); err == nil {
		t.Error("an undeclared backend was accepted")
	}
	served, err := parseMatrixBackends("firecracker/aws-metal")
	if err != nil {
		t.Fatal(err)
	}

	doc := judgeMatrix(matrixCells(), acceptanceEvidenceDoc{DeploymentID: "d1"}, matrixScope{tiers: matrixTestTiers()})
	err = matrixOutcome(doc, []config.Arch{config.ArchX64}, served)
	if err == nil || !strings.Contains(err.Error(), "should have run did not") {
		t.Fatalf("a pool's run that ran nothing answered %v", err)
	}
	ec2Only := matrixOutcome(doc, []config.Arch{config.ArchX64}, []string{"ec2"})
	if ec2Only == nil || ec2Only.Error() == err.Error() {
		t.Errorf("the EC2 lane and the pool's owe the same: %v and %v", ec2Only, err)
	}
}

// seedTemplate seeds a pool's run's state directory from the migrated template, so its
// first open proves a schema rather than applying every migration under the suite's load.
func seedTemplate(t *testing.T) func(string) {
	t.Helper()

	return func(dir string) { ledgertest.Seed(t, dir) }
}

// writePoolBase writes poolBaseConfig with these replacements, and loads it.
func writePoolBase(t *testing.T, replace ...string) (string, *config.Config) {
	t.Helper()
	body := poolBaseConfig
	for i := 0; i+1 < len(replace); i += 2 {
		if !strings.Contains(body, replace[i]) {
			t.Fatalf("the pool base holds no %q", replace[i])
		}
		body = strings.Replace(body, replace[i], replace[i+1], 1)
	}
	path := writeAcceptanceBase(t, body)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the pool base does not load: %v", err)
	}

	return path, cfg
}

// A POOL'S TEARDOWN IS GRANTED THE PURGE OF A CACHE IT NEVER RAN AS A NODE, for its
// deployment alone, and the control plane's two policies fit IAM's 10,240 characters of
// inline policy a role may carry.
func TestAPoolsTeardownIsGrantedItsCachesPurge(t *testing.T) {
	t.Parallel()

	const deployment = "0123456789abcdef0123456789abcdef"
	path, cfg := writePoolBase(t, "      region: us-west-2\n",
		"      region: us-west-2\n      instance_profile: accept-pool-host\n",
		"images: {x64: ami-x64}", "images: {x64: ami-0123456789abcdef0}")
	policy, ok, err := matrixTeardownPolicy(cfg, deployment)
	if err != nil || !ok {
		t.Fatalf("the pool's teardown policy: %v, %v", ok, err)
	}
	var sids []string
	for _, st := range policy.Statement {
		sids = append(sids, st.Sid)
	}
	for _, want := range []string{"BilletMatrixDecommissionState", "BilletCacheDelete", "BilletPurgeObjects",
		"BilletPurgeList"} {
		if !slices.Contains(sids, want) {
			t.Errorf("the teardown's policy has %q, and no %s", sids, want)
		}
	}
	for _, refused := range []string{"BilletCacheCreateVolume", "BilletCacheAttach", "BilletCacheSnapshotCreate",
		"BilletCacheObjects", "BilletCacheList", "BilletCacheObjectsEvict"} {
		if slices.Contains(sids, refused) {
			t.Errorf("a teardown is granted %s", refused)
		}
	}
	body, err := policy.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "s3:PutObject") {
		t.Errorf("a purge is granted a write: %s", body)
	}
	// EVERY OBJECT GRANT IS THIS RUN'S OWN NAMESPACE, never the prefix every store in the
	// bucket shares.
	site, store := cfg.Server.Pool.SiteStore()
	own := ebss3.OwnerPrefix(*store, app.CacheNamespace(deployment, site))
	for _, st := range policy.Statement {
		for _, r := range st.Resource {
			if strings.Contains(r, ":s3:::") && strings.Contains(r, "/") &&
				!strings.HasPrefix(r, "arn:aws:s3:::billet-cache-example/"+own) {
				t.Errorf("%s reaches %s, outside this run's %s", st.Sid, r, own)
			}
		}
	}
	if !strings.Contains(string(body), deployment) {
		t.Errorf("the purge's deletes are not scoped to the deployment's tag: %s", body)
	}

	// The control plane's own policy beside it, compacted as IAM counts it.
	var out bytes.Buffer
	if err := setup.Init(t.Context(), quietEnv(&out), []string{"iam", "--pool", "--config", path, "--deployment",
		deployment, "--role-arn", "arn:aws:iam::111122223333:role/accept-pool-host"}); err != nil {
		t.Fatalf("init iam --pool: %v", err)
	}
	size := 0
	for _, doc := range [][]byte{out.Bytes(), body} {
		var compact bytes.Buffer
		if err := json.Compact(&compact, bytes.TrimSpace(doc)); err != nil {
			t.Fatal(err)
		}
		size += compact.Len()
	}
	t.Logf("the control plane's run and teardown policies: %d characters", size)
	if size > 10240 {
		t.Errorf("the control plane's run and teardown policies are %d characters, past a role's 10,240", size)
	}

	one := "p/owners/" + strings.Repeat("a", 64) + "/"
	if _, err := awspolicy.CachePurge("us-west-2", deployment, awspolicy.Cache{Bucket: "b", Prefix: "p"},
		one); err != nil {
		t.Errorf("a purge over one owner's prefix: %v", err)
	}
	if _, err := awspolicy.CachePurge("us-west-2", "", awspolicy.Cache{Bucket: "b", Prefix: "p"},
		one); err == nil {
		t.Error("a purge for no deployment was rendered")
	}
	for _, wide := range []string{"p/", "p/owners/", "p/owners/*/", "q/owners/" + strings.Repeat("a", 64) + "/",
		strings.TrimSuffix(one, "/"), "p/owners/" + strings.Repeat("a", 63) + "*/"} {
		if _, err := awspolicy.CachePurge("us-west-2", deployment, awspolicy.Cache{Bucket: "b", Prefix: "p"},
			wide); err == nil {
			t.Errorf("a purge over %q was rendered", wide)
		}
	}
}

// THE PRELUDE IS HANDED THE POOL'S INIT_ARGS LESS WHAT IT SETS ITSELF, and a value it
// could not carry as a word is refused.
func TestAPreludesInitArgsAreThePoolsLessItsOwn(t *testing.T) {
	t.Parallel()

	path, _ := writePoolBase(t, "init_args: ['--site=metal',",
		"init_args: ['--kernel-image=/var/lib/billet/kernels/vmlinux', '--ceph-user=billet', '--site=metal',")
	var out bytes.Buffer
	if err := cmdMatrixPoolInitArgs(quietEnv(&out), []string{"--config", path}); err != nil {
		t.Fatalf("pool-init-args: %v", err)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	for _, own := range []string{"--kernel-image=", "--ceph-user="} {
		if slices.ContainsFunc(got, func(l string) bool { return strings.HasPrefix(l, own) }) {
			t.Errorf("the prelude was handed its own %s: %q", own, got)
		}
	}
	if !slices.Contains(got, "--site=metal") || !slices.Contains(got, "--ebs-s3-bucket=billet-cache-example") {
		t.Errorf("init args %q", got)
	}

	spaced, _ := writePoolBase(t, "'--site=metal',", "'--site=metal', '--ebs-s3-prefix=cache v2',")
	if err := cmdMatrixPoolInitArgs(quietEnv(&out), []string{"--config", spaced}); err == nil ||
		!strings.Contains(err.Error(), "whitespace") {
		t.Errorf("a value with a space answered %v", err)
	}

	// A KERNEL OR A CEPH IDENTITY THE PRELUDE DOES NOT PREPARE is refused: the join would
	// start the node on it.
	for _, other := range []string{"'--kernel-image=/k/vmlinux',", "'--ceph-user=other',",
		"'--ceph-keyring=/etc/ceph/other.keyring',"} {
		mismatched, _ := writePoolBase(t, "'--site=metal',", "'--site=metal', "+other)
		if err := cmdMatrixPoolInitArgs(quietEnv(&out), []string{"--config", mismatched}); err == nil ||
			!strings.Contains(err.Error(), "the prelude prepares") {
			t.Errorf("%s answered %v", other, err)
		}
	}
}

// THE POOL HOST'S NODE CONFIG LOADS, AND READS EVERY VALUE AS THE POOL'S INIT_ARGS WROTE
// IT, so the policy rendered from it is for the namespace the hosts use.
func TestAPoolHostsNodeConfigReadsAsWritten(t *testing.T) {
	t.Parallel()

	_, cfg := writePoolBase(t, "'--site=metal',", "'--site=metal', '--ebs-s3-prefix=cache#v2',")
	site, store := cfg.Server.Pool.SiteStore()
	dir := t.TempDir()
	file := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(file, poolHostNodeConfig(cfg, site, store, dir), 0o600); err != nil {
		t.Fatal(err)
	}
	node, err := config.Load(file)
	if err != nil {
		t.Fatalf("the pool host's node config does not load: %v", err)
	}
	if node.Node.EBSS3 == nil || node.Node.EBSS3.Prefix != "cache#v2" || node.Node.Site != "metal" ||
		node.Node.EBSS3.Bucket != "billet-cache-example" {
		t.Errorf("it reads %+v", node.Node.EBSS3)
	}
}

// THE DISPATCHER IS GIVEN THE POOL'S TIER ONLY WHERE THE CONFIG'S POOL ORDERS: aws-metal
// on x64, from the pool's own architectures.
func TestThePoolsTierIsDispatchedWhereItsPoolOrders(t *testing.T) {
	t.Parallel()

	_, cfg := writePoolBase(t)
	got := matrixTierLines(matrixTiers(cfg), matrixPoolSources(cfg))
	if !slices.Equal(got, []string{"firecracker/aws-metal x64 metal-x64"}) {
		t.Errorf("tiers = %q", got)
	}
}

// A HOST EC2 DOES NOT DESCRIBE AT FIRST IS ENDED WHEN IT IS, ON WHICHEVER POLL THAT IS:
// not found is not gone, and a host that then reads running is not left running through
// the wait.
func TestAHostEC2FindsLateIsEndedWhenItIs(t *testing.T) {
	t.Parallel()

	hosts := newFakePoolHosts(1, ec2.HostRecord{ID: "i-5", State: "running", Order: "p5"})
	hosts.unlisted["i-5"] = true
	hosts.hiddenUntil = map[string]int{"i-5": 4}
	cache := &fakePoolCache{hosts: hosts}
	p := acceptancePool{hosts: hosts, cache: cache, gone: time.Minute, poll: time.Millisecond}

	var out bytes.Buffer
	if _, err := p.decommission(t.Context(), quietEnv(&out),
		[]acceptancePoolOrder{order("p5", alloc.PoolPlaced, "i-5")}); err != nil {
		t.Fatalf("decommission: %v\n%s", err, out.String())
	}
	if len(hosts.ended) != 1 || hosts.ended[0] != "i-5" || hosts.endedAt["i-5"] < 4 {
		t.Errorf("ended %v at read %d; want i-5 once, once EC2 described it", hosts.ended, hosts.endedAt["i-5"])
	}
	if cache.purged != 1 {
		t.Errorf("purged %d time(s), want once", cache.purged)
	}
}

// A HOST ONE LISTING SHOWED IS PROVED GONE BY ITS ID, though the next listing leaves it
// out: the wait and the sweep after it describe it until it reads terminated, and the
// sweep asks again about every host the teardown saw.
func TestAHostOneListingShowedIsProvedGoneByItsID(t *testing.T) {
	t.Parallel()

	hosts := newFakePoolHosts(4, ec2.HostRecord{ID: "i-7", State: "running", Order: "p7"})
	// Left out of the first listing, shown by the wait's first, and left out after.
	hosts.listedFrom, hosts.listedUntil = map[string]int{"i-7": 2}, map[string]int{"i-7": 3}
	cache := &fakePoolCache{hosts: hosts}
	p := acceptancePool{hosts: hosts, cache: cache, gone: time.Minute, poll: time.Millisecond}

	var out bytes.Buffer
	ids, err := p.decommission(t.Context(), quietEnv(&out), nil)
	if err != nil {
		t.Fatalf("decommission: %v\n%s", err, out.String())
	}
	if !slices.Contains(ids, "i-7") || cache.purged != 1 || hosts.purgedAt <= hosts.endedAt["i-7"]+hosts.shutdown {
		t.Errorf("answered %v, purged %d time(s) at read %d with i-7 ended at read %d; want it proved terminated "+
			"first", ids, cache.purged, hosts.purgedAt, hosts.endedAt["i-7"])
	}

	up := newFakePoolHosts(0, ec2.HostRecord{ID: "i-8", State: "running", Order: "p8"})
	up.unlisted["i-8"] = true
	if err := (acceptancePool{hosts: up}).sweep(t.Context(), quietEnv(&out), "d1", nil,
		[]string{"i-8"}); !errors.Is(err, errAcceptanceResidue) || !strings.Contains(err.Error(), "i-8 running") {
		t.Errorf("the sweep of a host the teardown saw and no listing shows answered %v", err)
	}
}

// A POOL'S WORKSPACE IS TORN DOWN AS A POOL'S, and one with a node as before: the
// teardown asks the pool's hosts and orders only of a pool's run.
func TestATeardownKnowsAPoolsRunFromItsConfig(t *testing.T) {
	t.Parallel()

	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	if err := os.WriteFile(prelude, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := writeAcceptanceBase(t, poolBaseConfig)
	ws, err := deriveAcceptance(t.Context(), acceptanceInputs{base: base,
		workspace: t.TempDir(), prefix: "accept-pool",
		fleet: &acceptanceFleet{Listen: "10.77.0.10:7717", Bootstrap: "10.77.0.10:7718", Prelude: prelude}, seedLedger: seedTemplate(t)})
	if err != nil {
		t.Fatal(err)
	}
	_, orders, isPool, err := acceptancePoolFor(t.Context(), ws)
	if !isPool || err != nil || len(orders) != 0 {
		t.Errorf("a pool's run that ordered nothing: pool %v, orders %v, %v", isPool, orders, err)
	}

	// A LOST LEDGER IS NOT A POOL THAT ORDERED NOTHING, and no read of it makes one.
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(cfg.Server.StateDir, "billet.db")
	if err := os.Remove(ledger); err != nil {
		t.Fatalf("remove the run's ledger: %v", err)
	}
	if _, _, isPool, err := acceptancePoolFor(t.Context(), ws); !isPool || !errors.Is(err, state.ErrNoLedger) {
		t.Errorf("a pool's run whose ledger is gone: pool %v, %v; want could-not-tell", isPool, err)
	}
	// NOR DOES A TEARDOWN, OR ITS RETRY, OR THE RUN AND ITS EVIDENCE: each refuses before
	// any open that would make one.
	var out bytes.Buffer
	for attempt := range 2 {
		if err := tearDownAcceptanceWithin(t.Context(), quietEnv(&out), ws, time.Second, false); !errors.Is(err,
			state.ErrNoLedger) {
			t.Errorf("teardown %d of a pool's run whose ledger is gone answered %v", attempt+1, err)
		}
	}
	if err := requireAcceptancePoolLedger(t.Context(), ws); !errors.Is(err, state.ErrNoLedger) {
		t.Errorf("the run's and the evidence's check answered %v", err)
	}
	if _, err := collectAcceptanceEvidence(t.Context(), ws); !errors.Is(err, state.ErrNoLedger) {
		t.Errorf("the evidence of a pool's run whose ledger is gone answered %v", err)
	}
	if _, err := os.Stat(ledger); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading the orders made a ledger in place of the lost one (%v)", err)
	}
	if _, err := deriveAcceptance(t.Context(), acceptanceInputs{base: base,
		workspace: filepath.Dir(ws.ConfigPath), prefix: "accept-pool", fleet: ws.Fleet}); !errors.Is(err,
		state.ErrNoLedger) {
		t.Errorf("a resumed pool's run whose ledger is gone answered %v", err)
	}

	node := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), t.TempDir())
	if _, _, isPool, err := acceptancePoolFor(t.Context(), node); isPool || err != nil {
		t.Errorf("a run with a node was torn down as a pool's: %v, %v", isPool, err)
	}
}

// A RESUME DERIVES WHAT THE FIRST ATTEMPT DID: the same base resumes, and one edited in
// place since, as a cache prefix changed, is refused before the derived config is
// replaced, since the teardown would then purge another prefix than the run used.
func TestAResumeOfAnEditedBaseIsRefused(t *testing.T) {
	t.Parallel()

	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	if err := os.WriteFile(prelude, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := writeAcceptanceBase(t, poolBaseConfig)
	in := acceptanceInputs{base: base, workspace: t.TempDir(), prefix: "accept-pool",
		fleet: &acceptanceFleet{Listen: "10.77.0.10:7717", Bootstrap: "10.77.0.10:7718", Prelude: prelude}, seedLedger: seedTemplate(t)}
	first, err := deriveAcceptance(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := deriveAcceptance(t.Context(), in); err != nil || again.ConfigSHA256 != first.ConfigSHA256 {
		t.Fatalf("an unchanged resume: %v, digest %s then %s", err, first.ConfigSHA256, again.ConfigSHA256)
	}
	written, err := os.ReadFile(first.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	edited := strings.Replace(poolBaseConfig, "'--site=metal',", "'--site=metal', '--ebs-s3-prefix=other',", 1)
	if err := os.WriteFile(base, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := deriveAcceptance(t.Context(), in); err == nil || !strings.Contains(err.Error(), "derives differently") {
		t.Fatalf("a resume of an edited base answered %v", err)
	}
	if after, err := os.ReadFile(first.ConfigPath); err != nil || !bytes.Equal(after, written) {
		t.Errorf("the refused resume replaced the derived config (%v)", err)
	}

	// A RECORD WITH NO DIGEST resumes only onto the derived config on disk, byte for byte.
	noDigest := first
	noDigest.ConfigSHA256 = ""
	if err := writeAcceptanceWorkspace(filepath.Dir(first.ConfigPath), noDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := deriveAcceptance(t.Context(), in); err == nil || !strings.Contains(err.Error(), "derives differently") {
		t.Errorf("a digestless resume of an edited base answered %v", err)
	}
	if err := os.WriteFile(base, []byte(poolBaseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := deriveAcceptance(t.Context(), in); err != nil || again.ConfigSHA256 != first.ConfigSHA256 {
		t.Errorf("a digestless resume of the same base: %v, digest %s", err, again.ConfigSHA256)
	}
}

// AN ORDER PLACED WHILE THE TEARDOWN RAN FAILS IT: the snapshot the teardown proved gone
// is read again once the control plane is proved stopped again, and an order it did not
// hold is one no step proved gone.
func TestAnOrderPlacedDuringTheTeardownFailsIt(t *testing.T) {
	t.Parallel()

	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	if err := os.WriteFile(prelude, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := deriveAcceptance(t.Context(), acceptanceInputs{base: writeAcceptanceBase(t, poolBaseConfig),
		workspace: t.TempDir(), prefix: "accept-pool",
		fleet: &acceptanceFleet{Listen: "10.77.0.10:7717", Bootstrap: "10.77.0.10:7718", Prelude: prelude}, seedLedger: seedTemplate(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sameAcceptancePoolOrders(t.Context(), ws, nil); err != nil {
		t.Fatalf("a pool that ordered nothing, read twice: %v", err)
	}

	db, cfg, err := openAcceptanceLedger(t.Context(), ws)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.NewAllocator(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	placed := alloc.PoolOrder{ID: "p9", Source: config.PoolSourceAWSMetal, Scope: "aws:us-west-2:subnet-0abc:us-west-2a",
		Shape: "z1d.metal", Location: "us-west-2a", Arch: config.ArchX64}
	if err := a.RecordPoolOrder(t.Context(), placed); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := sameAcceptancePoolOrders(t.Context(), ws, nil); !errors.Is(err, errAcceptanceResidue) ||
		!strings.Contains(err.Error(), "p9") {
		t.Errorf("an order the snapshot did not hold answered %v", err)
	}
	if err := sameAcceptancePoolOrders(t.Context(), ws, []acceptancePoolOrder{{ID: "p9"}}); err != nil {
		t.Errorf("the same orders read twice answered %v", err)
	}
	if err := sameAcceptancePoolOrders(t.Context(), ws, []acceptancePoolOrder{{ID: "p9"}, {ID: "p8"}}); err == nil ||
		!strings.Contains(err.Error(), "p8") || !strings.Contains(err.Error(), "no longer") {
		t.Errorf("an order the second read no longer holds answered %v", err)
	}
}

// THE RUN WAITS FOR THE POOL TO LET EVERY HOST GO, AND SAYS WHICH IT STILL HOLDS: an
// order the pool released ends the wait, one placed or uncertain holds it to its bound,
// and a ledger it cannot read is an error, never a release.
func TestTheRunWaitsForThePoolToLetItsHostsGo(t *testing.T) {
	t.Parallel()

	reads := 0
	releasing := func(context.Context) ([]acceptancePoolOrder, error) {
		reads++
		state := alloc.PoolSealed
		if reads > 2 {
			state = alloc.PoolReleased
		}

		return []acceptancePoolOrder{order("p1", state, "i-1"), order("p2", alloc.PoolRefused, "")}, nil
	}
	var out bytes.Buffer
	if err := awaitPoolReleased(t.Context(), quietEnv(&out), releasing, time.Minute, time.Millisecond); err != nil ||
		reads != 3 {
		t.Errorf("a pool that released after two reads answered %v after %d read(s)", err, reads)
	}

	holding := func(context.Context) ([]acceptancePoolOrder, error) {
		return []acceptancePoolOrder{order("p1", alloc.PoolPlaced, "i-1"), order("p3", alloc.PoolUncertain, "")}, nil
	}
	err := awaitPoolReleased(t.Context(), quietEnv(&out), holding, 5*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "p1 (placed)") || !strings.Contains(err.Error(), "p3 (uncertain)") {
		t.Errorf("a pool that held on answered %v", err)
	}

	blind := errors.New("the ledger is locked")
	err = awaitPoolReleased(t.Context(), quietEnv(&out), func(context.Context) ([]acceptancePoolOrder, error) {
		return nil, blind
	}, time.Minute, time.Millisecond)
	if !errors.Is(err, blind) {
		t.Errorf("a ledger that could not be read answered %v", err)
	}
}
