package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/awspolicy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lease"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// The acceptance matrix (#554): every backend, architecture, cache and case billet
// claims to serve, declared in one closed list, and judged from the ledger's own record
// of each job rather than from the job's log.
//
// A CELL THAT IS NOT RUN SAYS WHY, and is never a pass. A cell billet does not support
// is listed as unsupported with the reason, so the matrix is the whole cross product and
// a missing cell is a test failure rather than a silence.
//
// THE VERDICT HAS TWO WITNESSES. The job asserts what it can see for itself (a sticky
// disk's cache-hit, an Actions restore that must hit, a marker a failed writer must not
// have published) and fails when it is wrong, so GitHub's result carries that; and the
// node's observation of the job, in the closed vocabulary internal/lease declares, says
// what billet did. Either one wrong is a fail.

// matrixWorkflows is where the matrix's workflows live in this repository. They are
// copied into the repository the acceptance App serves, and the run refuses a copy that
// differs, so what runs is what was reviewed.
const matrixWorkflows = "scripts/acceptance/matrix"

// matrixResult is a cell's verdict.
type matrixVerdict string

const (
	verdictPass        matrixVerdict = "pass"
	verdictFail        matrixVerdict = "fail"
	verdictNotRun      matrixVerdict = "not run"
	verdictUnsupported matrixVerdict = "unsupported"
)

// matrixBackend is a backend and, for a pool's Firecracker hosts, the source that rents
// them (ADR-030); per-job EC2 has none.
type matrixBackend struct {
	kind   config.ProviderKind
	source string
}

// The axes. Every combination of them is a cell.
var (
	matrixBackends = []matrixBackend{
		{kind: config.ProviderEC2},
		{kind: config.ProviderFirecracker, source: config.PoolSourceAWSMetal},
		{kind: config.ProviderFirecracker, source: config.PoolSourcePhoenixNAP},
	}
	matrixArchs  = []config.Arch{config.ArchX64, config.ArchARM64}
	matrixCaches = []string{
		"docker-images", "sticky-disks", "docker-builder", "actions", "git", "go", "bazel", "buck2", "npm",
	}
	matrixCases = []string{
		"cold", "warm", "two-writers-cas", "two-writers-lww", "writer-fails", "instance-lost", "store-unreachable",
	}
)

// matrixCell is one backend, architecture, cache and case, and how it is run.
type matrixCell struct {
	Backend config.ProviderKind `json:"backend"`
	// Source is the pool source a Firecracker cell's hosts are rented from, empty for
	// per-job EC2.
	Source string      `json:"source,omitempty"`
	Arch   config.Arch `json:"arch"`
	Cache  string      `json:"cache"`
	Case   string      `json:"case"`
	// Run is how the cell is run; nil when it is not.
	Run *matrixRun `json:"run,omitempty"`
	// Unsupported says billet does not offer this cell at all, and Why says why; a
	// cell that is not unsupported and has no Run is supported and not run yet, and
	// Why says what it waits for.
	Unsupported bool   `json:"unsupported,omitempty"`
	Why         string `json:"why,omitempty"`
}

// matrixRun is one job of one of the matrix's workflows, and what it must end as.
type matrixRun struct {
	// Workflow is the file's name under matrixWorkflows.
	Workflow string `json:"workflow"`
	// Job is the job's name as GitHub reports it.
	Job string `json:"job"`
	// GitHub is the result GitHub must report for the job, in the ledger's words, which
	// are the scale-set message's (alloc.ResultSucceeded), never the REST API's
	// "success": the first real run failed every cell that passed on that word.
	GitHub string `json:"github"`
	// Observe names the node's observation the cell reads (matrixObservation), and
	// Want the token it must hold.
	Observe string `json:"observe"`
	Want    string `json:"want"`
}

// matrixCells is the whole declaration: every combination of the axes, in their order.
func matrixCells() []matrixCell {
	out := make([]matrixCell, 0, len(matrixBackends)*len(matrixArchs)*len(matrixCaches)*len(matrixCases))
	for _, b := range matrixBackends {
		for _, a := range matrixArchs {
			for _, c := range matrixCaches {
				for _, k := range matrixCases {
					cell := matrixCellFor(c, k)
					if waits, ok := poolCellsNotRun[b.source]; ok {
						cell = waits
					}
					cell.Backend, cell.Source, cell.Arch, cell.Cache, cell.Case = b.kind, b.source, a, c, k
					out = append(out, cell)
				}
			}
		}
	}

	return out
}

// poolCellsNotRun is every cell of a pool's Firecracker hosts from a source the lane does
// not order from, and why. AWS metal's cells are run as EC2's are, by the same workflows
// on the pool's tier, in the lane's pool job (#829).
var poolCellsNotRun = map[string]matrixCell{
	config.PoolSourcePhoenixNAP: {Why: "a phoenixNAP pool runs with every cache off until its site store is " +
		"bought (#553), and the source has not taken an order through its API (#828)"},
}

// matrixBackendKey is a backend as the tables and the dispatcher name it.
func matrixBackendKey(b matrixBackend) string {
	if b.source != "" {
		return string(b.kind) + "/" + b.source
	}

	return string(b.kind)
}

// matrixBackendName is a cell's backend as the tables print it: the source beside a
// pool's.
func matrixBackendName(c *matrixCell) string {
	if c.Source != "" {
		return string(c.Backend) + "/" + c.Source
	}

	return string(c.Backend)
}

// matrixObservation is the observation each cache is judged by: the Docker builder's
// state is a sticky disk, and Buck2's remote cache is the Bazel one's.
var matrixObservation = map[string]string{
	"docker-images": "image", "sticky-disks": "sticky", "docker-builder": "sticky", "actions": "actions",
	"git": "git", "go": "go", "bazel": "bazel", "buck2": "bazel", "npm": "npm",
}

// contentAddressed are the caches whose entries are named by their content, so two
// writers of one entry write the same bytes and a failed job's writes are content
// another job would have written too.
var contentAddressed = map[string]bool{"go": true, "bazel": true, "buck2": true, "npm": true}

// matrixCellFor says how one cache's case is run, or why it is not. The backend and
// architecture do not change it: a combination the deployment under test has no tier
// for is judged not run when it is judged.
func matrixCellFor(cache, kase string) matrixCell {
	observe := matrixObservation[cache]
	if cache == "buck2" && (kase == "cold" || kase == "warm") {
		return matrixCell{Why: "no workflow yet: the guest writes no buckconfig, so a Buck2 job names the " +
			"node's remote cache itself, and the matrix has no Buck2 project to build"}
	}
	switch kase {
	case "cold", "warm":
		want := kase
		if cache == "actions" {
			// SERVED EITHER WAY: the Actions observation says the site store answered,
			// not whether it held the key. The job asserts the miss and the hit itself.
			want = string(lease.ActionsCacheServed)
		}

		workflow := "matrix-" + kase + ".yml"
		if cache == "docker-images" {
			// ALONE ON THE TIER: every EC2 guest attaches the image store at boot, so
			// every job publishes a generation of it, and one beside the probe could
			// replace the probe's image with its own clone of an empty store.
			workflow = "matrix-images-" + kase + ".yml"
		}

		return matrixCell{Run: &matrixRun{Workflow: workflow, Job: cache, GitHub: alloc.ResultSucceeded,
			Observe: observe, Want: want}}
	case "two-writers-cas", "two-writers-lww":
		mode := strings.TrimPrefix(kase, "two-writers-")
		switch {
		case cache == "sticky-disks":
			return matrixCell{Run: &matrixRun{Workflow: "matrix-writers.yml", Job: "reader-" + mode,
				GitHub: alloc.ResultSucceeded, Observe: observe, Want: string(lease.BuildCacheWarm)}}
		case cache == "docker-builder":
			return matrixCell{Unsupported: true, Why: "the builder's state is a sticky disk published " +
				"last-write-wins; two writers are the sticky-disks cells"}
		case cache == "git":
			return matrixCell{Unsupported: true, Why: "the node fetches the mirror; no job writes it"}
		case contentAddressed[cache]:
			return matrixCell{Unsupported: true, Why: "content-addressed: two writers of one entry write " +
				"the same bytes, so there is no conflict for a publication policy to settle"}
		case cache == "actions" && mode == "lww":
			return matrixCell{Unsupported: true, Why: "the Actions cache has no last-write-wins: a key's " +
				"first finalised writer keeps it"}
		}

		return matrixCell{Why: "no workflow yet runs two writers of one " + cache + " entry at once"}
	case "writer-fails":
		switch cache {
		case "sticky-disks":
			return matrixCell{Run: &matrixRun{Workflow: "matrix-writer-fails.yml", Job: "reader",
				GitHub: alloc.ResultSucceeded, Observe: observe, Want: string(lease.BuildCacheWarm)}}
		case "docker-builder":
			return matrixCell{Unsupported: true, Why: "the builder's state is a sticky disk; a failed " +
				"writer is the sticky-disks cell"}
		case "git":
			return matrixCell{Unsupported: true, Why: "the node fetches the mirror; no job writes it"}
		}

		return matrixCell{Why: "no workflow yet fails a job after it writes a " + cache + " entry"}
	case "instance-lost":
		return matrixCell{Why: "timed from the site store's writer state by hand " +
			"(docs/reference/records/site-acceptance.md); not automated yet"}
	case "store-unreachable":
		return matrixCell{Why: "waits for the site store's breaker (#531)"}
	}

	return matrixCell{Why: "an unknown case"}
}

// observed is the token the node recorded for one observation of a job.
func (c acceptanceCache) observed(name string) string {
	switch name {
	case "image":
		return c.Image
	case "sticky":
		return c.StickyDisks
	case "actions":
		return c.Actions
	case "git":
		return c.Git
	case "go":
		return c.Go
	case "bazel":
		return c.Bazel
	case "npm":
		return c.Npm
	}

	return ""
}

// matrixResult is one cell's verdict, and what was measured for it.
type matrixResult struct {
	matrixCell

	Verdict matrixVerdict `json:"verdict"`
	// Reason says why a cell failed or was not run.
	Reason string `json:"reason,omitempty"`
	// Jobs are the leases the cell was judged from.
	Jobs []string `json:"jobs,omitempty"`
	// Observed is what the node recorded, one token per job.
	Observed []string `json:"observed,omitempty"`
	// WaitSeconds is the longest a job waited from GitHub's queue to its start, and
	// RunSeconds the longest it ran.
	WaitSeconds float64 `json:"wait_seconds,omitempty"`
	RunSeconds  float64 `json:"run_seconds,omitempty"`
	// CostUSD is AN ESTIMATE: the jobs' instance-seconds at the configured
	// price_usd_per_hour of the shape each ran on. Volumes, snapshots and requests are
	// not in it.
	CostUSD float64 `json:"cost_usd_estimate,omitempty"`
}

// matrixDoc is what `judge` writes.
type matrixDoc struct {
	Version      int            `json:"version"`
	JudgedAt     string         `json:"judged_at"`
	DeploymentID string         `json:"deployment_id"`
	Results      []matrixResult `json:"results"`
	// Passed, Failed and NotRun count the cells.
	Passed      int `json:"passed"`
	Failed      int `json:"failed"`
	NotRun      int `json:"not_run"`
	Unsupported int `json:"unsupported"`
	// Scenarios are what a pool did as a whole, beside the cells (#829).
	Scenarios []matrixScenario `json:"scenarios,omitempty"`
}

const matrixDocVersion = 1

// matrixTier is what the judge needs of a tier the deployment under test declares.
type matrixTier struct {
	providers []config.ProviderKind
	arch      config.Arch
}

// matrixTiers reads each tier's providers and architecture out of the config the
// evidence was written from.
func matrixTiers(cfg *config.Config) map[string]matrixTier {
	out := make(map[string]matrixTier, len(cfg.Tiers))
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		providers := slices.Clone(t.Providers)
		if t.Provider != "" {
			providers = append(providers, t.Provider)
		}
		out[t.Label] = matrixTier{providers: providers, arch: t.Arch}
	}

	return out
}

// matrixPrices is each EC2 shape's configured price, by instance type.
func matrixPrices(cfg *config.Config) map[string]config.USDPerHour {
	out := map[string]config.USDPerHour{}
	if cfg.Node != nil && cfg.Node.EC2 != nil {
		for _, s := range cfg.Node.EC2.InstanceTypes {
			out[s.Type] = s.PriceUSDPerHour
		}
	}

	return out
}

// workflowFile is the file name a job's workflow ref names
// (owner/repo/.github/workflows/<file>@<ref>), or empty.
func workflowFile(ref string) string {
	before, _, _ := strings.Cut(ref, "@")
	if !strings.Contains(before, "/.github/workflows/") {
		return ""
	}

	return path.Base(before)
}

// matrixScope is what a job must belong to for the judge to read it, and what it is
// measured against.
type matrixScope struct {
	tiers  map[string]matrixTier
	prices map[string]config.USDPerHour
	// runs are the GitHub runs this matrix run dispatched, and repository the one it
	// dispatched them in. A job of any other run, or another repository, is not a
	// cell's, however its tier, workflow and name match: someone else's run on the same
	// labels must not stand in for a job this run never got. Nil runs reads every run,
	// which only a hand-run judge without --runs does.
	runs       map[int64]bool
	repository string
	// poolNodes is each pool host's node and the source and architecture its order
	// rented it as: a pool cell's job is one that ran on a node of its source and its
	// architecture, and an EC2 cell's one that ran on no pool's.
	poolNodes map[string]poolNode
	// poolFloors is the floor the config's pool keeps on each architecture.
	poolFloors map[config.Arch]int
}

// poolNode is what a pool's order says of the host that joined as a node.
type poolNode struct {
	source string
	arch   config.Arch
}

// belongs reports whether a job is one of this matrix run's.
func (s matrixScope) belongs(j *acceptanceJob) bool {
	if s.repository != "" && !strings.EqualFold(j.Repository, s.repository) {
		return false
	}

	return s.runs == nil || s.runs[j.GitHubRunID]
}

// judgeMatrix judges every cell from the evidence.
func judgeMatrix(cells []matrixCell, doc acceptanceEvidenceDoc, scope matrixScope) matrixDoc {
	out := matrixDoc{Version: matrixDocVersion, JudgedAt: time.Now().UTC().Format(time.RFC3339),
		DeploymentID: doc.DeploymentID}
	if scope.poolNodes == nil {
		scope.poolNodes = map[string]poolNode{}
		for i := range doc.Pool {
			if doc.Pool[i].Node != "" {
				scope.poolNodes[doc.Pool[i].Node] = poolNode{source: doc.Pool[i].Source,
					arch: config.Arch(doc.Pool[i].Arch)}
			}
		}
	}
	out.Scenarios = judgeScenarios(doc, scope)
	for _, cell := range cells {
		r := judgeCell(cell, doc.Jobs, scope)
		switch r.Verdict {
		case verdictPass:
			out.Passed++
		case verdictFail:
			out.Failed++
		case verdictNotRun:
			out.NotRun++
		case verdictUnsupported:
			out.Unsupported++
		}
		out.Results = append(out.Results, r)
	}

	return out
}

func judgeCell(cell matrixCell, jobs []acceptanceJob, scope matrixScope) matrixResult {
	r := matrixResult{matrixCell: cell}
	switch {
	case cell.Unsupported:
		r.Verdict, r.Reason = verdictUnsupported, cell.Why

		return r
	case cell.Run == nil:
		r.Verdict, r.Reason = verdictNotRun, cell.Why

		return r
	}

	var mine []acceptanceJob
	for i := range jobs {
		j := &jobs[i]
		t, ok := scope.tiers[j.Tier]
		if !ok || t.arch != cell.Arch || !slices.Contains(t.providers, cell.Backend) {
			continue
		}
		// ON A HOST OF THE CELL'S SOURCE AND ARCHITECTURE, which only the pool's own orders
		// say: a Firecracker job on a host the operator owns is not a pool's, nor one on a
		// host its order rented as another architecture than its tier's. A job whose
		// history could not be read has no node to tell by, and is kept, as below.
		if node, pooled := scope.poolNodes[j.Node]; j.Unread == "" &&
			(node.source != cell.Source || (pooled && node.arch != cell.Arch)) {
			continue
		}
		if workflowFile(j.WorkflowRef) != cell.Run.Workflow || j.JobName != cell.Run.Job {
			continue
		}
		// A JOB WHOSE HISTORY COULD NOT BE READ HAS NO RUN TO BE TOLD APART BY, so it
		// is kept, and fails the cell below: dropped by the run filter, it would let a
		// sibling's pass stand for evidence the judge could not read.
		if j.Unread == "" && !scope.belongs(j) {
			continue
		}
		mine = append(mine, *j)
	}
	if len(mine) == 0 {
		r.Verdict = verdictNotRun
		r.Reason = fmt.Sprintf("no %s job of %s ran on a %s %s tier", cell.Run.Job, cell.Run.Workflow,
			cell.Backend, cell.Arch)

		return r
	}

	var wrong []string
	for i := range mine {
		j := &mine[i]
		r.Jobs = append(r.Jobs, j.LeaseID)
		got := j.Cache.observed(cell.Run.Observe)
		r.Observed = append(r.Observed, got)
		switch {
		case j.Unread != "":
			wrong = append(wrong, j.LeaseID+": "+j.Unread)
		case j.Trust != string(config.WorkloadTrusted):
			// TRUSTED, because the Actions cache is intercepted only for a trusted
			// pool, and only a trusted pool's sticky disk is published inside the job
			// that wrote it, which the two-writer cells' order depends on.
			wrong = append(wrong, fmt.Sprintf("%s: its tier is %q, and the matrix runs on a trusted one",
				j.LeaseID, dashIfEmpty(j.Trust)))
		case j.Provider != "" && j.Provider != string(cell.Backend):
			wrong = append(wrong, fmt.Sprintf("%s: placed on %s, not %s", j.LeaseID, j.Provider, cell.Backend))
		case j.GitHub != cell.Run.GitHub:
			wrong = append(wrong, fmt.Sprintf("%s: GitHub reported %q, want %q", j.LeaseID, j.GitHub,
				cell.Run.GitHub))
		case got == "":
			wrong = append(wrong, fmt.Sprintf("%s: the node recorded no %s observation", j.LeaseID,
				cell.Run.Observe))
		case got != cell.Run.Want:
			wrong = append(wrong, fmt.Sprintf("%s: the node observed %s %q, want %q", j.LeaseID,
				cell.Run.Observe, got, cell.Run.Want))
		}
		measureJob(&r, j, scope.prices)
	}
	if len(wrong) > 0 {
		r.Verdict, r.Reason = verdictFail, strings.Join(wrong, "; ")

		return r
	}
	r.Verdict = verdictPass

	return r
}

// measureJob adds one job's wait, run time and instance cost to its cell's numbers.
func measureJob(r *matrixResult, j *acceptanceJob, prices map[string]config.USDPerHour) {
	queued, qErr := time.Parse(time.RFC3339Nano, j.QueuedAt)
	started, sErr := time.Parse(time.RFC3339Nano, j.StartedAt)
	finished, fErr := time.Parse(time.RFC3339Nano, j.FinishedAt)
	if qErr == nil && sErr == nil {
		r.WaitSeconds = max(r.WaitSeconds, started.Sub(queued).Seconds())
	}
	if sErr == nil && fErr == nil {
		ran := finished.Sub(started)
		r.RunSeconds = max(r.RunSeconds, ran.Seconds())
		if price, ok := prices[j.InstanceType]; ok {
			r.CostUSD += ran.Hours() * float64(price) / 1e6
		}
	}
}

func cmdAcceptanceMatrix(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("billet acceptance matrix needs a subcommand: cells or judge")
	}
	switch args[0] {
	case "cells":
		return cmdMatrixCells(env, args[1:])
	case "tiers":
		return cmdMatrixTiers(env, args[1:])
	case "jobs":
		return cmdMatrixJobs(env, args[1:])
	case "judge":
		return cmdMatrixJudge(env, args[1:])
	case "teardown-policy":
		return cmdMatrixTeardownPolicy(env, args[1:])
	case "pool-init-args":
		return cmdMatrixPoolInitArgs(env, args[1:])
	case "pool-host-iam":
		return cmdMatrixPoolHostIAM(ctx, env, args[1:])
	}

	return fmt.Errorf("billet acceptance matrix %s: unknown subcommand; it is cells, tiers, jobs, judge, "+
		"teardown-policy, pool-init-args or pool-host-iam", args[0])
}

// cmdMatrixTeardownPolicy prints the IAM policy a run's teardown needs beside the node's
// own: s3:DeleteObject on this deployment's cache state objects, which `billet
// decommission` deletes and billet's rendered node policy never lets a node delete
// (2026-11-07). It names this deployment's own state prefix, the store's own
// derivation, so the grant reaches no other deployment's state under the same bucket
// prefix. A workspace whose config has no ebs-s3 cache prints nothing.
func cmdMatrixTeardownPolicy(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix teardown-policy", env.Stdout)
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		return err
	}
	policy, ok, err := matrixTeardownPolicy(cfg, ws.DeploymentID)
	if err != nil || !ok {
		return err
	}

	return writeJSON(env.Stdout, policy)
}

func matrixTeardownPolicy(cfg *config.Config, deployment string) (awspolicy.Policy, bool, error) {
	// THE SITE'S STORE IS THE NODE'S, or for a pool's run the one its init_args hand its
	// hosts, whose cache the teardown purges as a node's decommission would.
	var store *config.EBSS3Config
	var site string
	pool := false
	switch {
	case cfg.Node != nil:
		store, site = cfg.Node.EBSS3, cfg.Node.Site
	case cfg.Server != nil && cfg.Server.Pool != nil:
		site, store = cfg.Server.Pool.SiteStore()
		pool = true
	}
	if store == nil {
		return awspolicy.Policy{}, false, nil
	}
	if deployment == "" {
		return awspolicy.Policy{}, false, errors.New("the workspace records no deployment id, so its " +
			"cache's state prefix is unknown")
	}
	prefix := ebss3.StatePrefix(*store, app.CacheNamespace(deployment, site))

	policy := awspolicy.Policy{Version: "2012-10-17", Statement: []awspolicy.Statement{{
		Sid: "BilletMatrixDecommissionState", Effect: "Allow", Action: []string{"s3:DeleteObject"},
		Resource: []string{fmt.Sprintf("arn:%s:s3:::%s/%s*", awspolicy.PartitionForRegion(store.Region),
			store.Bucket, prefix)},
	}}}
	// A POOL'S RUN PURGES A CACHE IT NEVER RAN AS A NODE: the control plane's own policy
	// is the pool's, and the cache's are its hosts', so the teardown is granted the
	// purge's half of a node's for this deployment as well.
	if pool {
		purge, err := awspolicy.CachePurge(store.Region, deployment,
			awspolicy.Cache{Bucket: store.Bucket, Prefix: store.Prefix})
		if err != nil {
			return awspolicy.Policy{}, false, fmt.Errorf("the pool's cache purge: %w", err)
		}
		policy.Statement = append(policy.Statement, purge...)
	}

	return policy, true, nil
}

// matrixJobsPerArch is how many jobs one dispatch of every matrix workflow runs on one
// tier: the image store's cold and warm one each, the cold and warm runs' seven each, the
// two-writer run's eight and the failed writer's three. `billet acceptance run --jobs` waits for this many per architecture,
// and a test holds it to the workflow files.
const matrixJobsPerArch = 27

// cmdMatrixJobs prints how many jobs a run of the matrix on these architectures waits for.
func cmdMatrixJobs(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix jobs", env.Stdout)
	archs := fs.String("archs", "", "the architectures the run covers, comma-separated")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	covered, err := parseMatrixArchs(*archs)
	if err != nil {
		return err
	}
	if len(covered) == 0 {
		return errors.New("--archs: name the architectures the run covers")
	}
	fmt.Fprintln(env.Stdout, matrixJobsPerArch*len(covered))

	return nil
}

// cmdMatrixTiers prints, for each backend and architecture the matrix declares, the
// workspace's tier label that serves it, one "<backend> <arch> <label>" line each, so a
// dispatcher sends each workflow to the tier the judge will read it on. A combination
// with no tier of its own, or with several, prints no line, and its cells are not run.
func cmdMatrixTiers(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix tiers", env.Stdout)
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		return err
	}
	for _, line := range matrixTierLines(matrixTiers(cfg), matrixPoolSources(cfg)) {
		fmt.Fprintln(env.Stdout, line)
	}

	return nil
}

// matrixPoolSources is, by source, the architectures the config's pool orders hosts of.
func matrixPoolSources(cfg *config.Config) map[string]map[config.Arch]bool {
	out := map[string]map[config.Arch]bool{}
	if cfg.Server == nil || cfg.Server.Pool == nil {
		return out
	}
	for arch, a := range cfg.Server.Pool.Architectures {
		for _, choice := range a.Order {
			if out[choice.Source] == nil {
				out[choice.Source] = map[config.Arch]bool{}
			}
			out[choice.Source][arch] = true
		}
	}

	return out
}

// matrixTierLines is one line per backend and architecture whose tier is the only one
// that serves it. Two tiers serving one would split a cell's jobs between them, and the
// cold and warm jobs must meet on one tier's caches. A pool's backend is one only where
// the config's pool orders from its source on that architecture, as only then are the
// tier's hosts the pool's.
func matrixTierLines(tiers map[string]matrixTier, pools map[string]map[config.Arch]bool) []string {
	var out []string
	for _, b := range matrixBackends {
		for _, a := range matrixArchs {
			if b.source != "" && !pools[b.source][a] {
				continue
			}
			var labels []string
			for label, t := range tiers {
				if t.arch == a && slices.Contains(t.providers, b.kind) && len(t.providers) == 1 {
					labels = append(labels, label)
				}
			}
			if len(labels) == 1 {
				out = append(out, fmt.Sprintf("%s %s %s", matrixBackendKey(b), a, labels[0]))
			}
		}
	}

	return out
}

// cmdMatrixCells prints the declaration.
func cmdMatrixCells(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix cells", env.Stdout)
	asJSON := fs.Bool("json", false, "print the cells as JSON")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	cells := matrixCells()
	if *asJSON {
		return writeJSON(env.Stdout, cells)
	}
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BACKEND\tARCH\tCACHE\tCASE\tRUN\tWANT\tWHY")
	for i := range cells {
		c := &cells[i]
		run, want := "not run", "-"
		switch {
		case c.Run != nil:
			run, want = c.Run.Workflow+"/"+c.Run.Job, c.Run.Observe+"="+c.Run.Want
		case c.Unsupported:
			run = "unsupported"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", matrixBackendName(c), c.Arch, c.Cache, c.Case, run, want,
			dashIfEmpty(c.Why))
	}

	return w.Flush()
}

// cmdMatrixJudge judges a run's evidence and fails on any cell that failed.
func cmdMatrixJudge(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix judge", env.Stdout)
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	evidence := fs.String("evidence", "", "the evidence to judge (default: <workspace>/"+acceptanceEvidence+")")
	out := fs.String("out", "", "where to write the verdicts (default: <workspace>/matrix.json)")
	archs := fs.String("archs", "", "the architectures this run covered, comma-separated; a cell on one of "+
		"them that was not run fails the run (default: none, so a cell not run is reported, not failed)")
	runsFile := fs.String("runs", "", "a file of the GitHub run ids this matrix run dispatched, one per line; "+
		"only their jobs are judged (default: every job in the evidence)")
	repository := fs.String("repository", "", "the owner/repository the runs were dispatched in; a job of any "+
		"other is not judged")
	backends := fs.String("backends", "ec2", "the backends this run served, comma-separated (ec2, "+
		"firecracker/aws-metal); a cell or scenario of another is reported, never owed")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}
	dir := filepath.Dir(ws.ConfigPath)
	if *evidence == "" {
		*evidence = filepath.Join(dir, acceptanceEvidence)
	}
	if *out == "" {
		*out = filepath.Join(dir, "matrix.json")
	}
	required, err := parseMatrixArchs(*archs)
	if err != nil {
		return err
	}
	served, err := parseMatrixBackends(*backends)
	if err != nil {
		return err
	}
	doc, err := readMatrixEvidence(*evidence, ws.DeploymentID)
	if err != nil {
		return err
	}
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		return err
	}
	scope := matrixScope{tiers: matrixTiers(cfg), prices: matrixPrices(cfg), repository: *repository,
		poolFloors: matrixPoolFloors(cfg)}
	if *runsFile != "" {
		if scope.runs, err = readMatrixRuns(*runsFile); err != nil {
			return err
		}
	}
	verdicts := judgeMatrix(matrixCells(), doc, scope)
	body, err := json.MarshalIndent(verdicts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	printMatrix(env.Stdout, verdicts)

	return matrixOutcome(verdicts, required, served)
}

// readMatrixEvidence reads a run's evidence, refusing one of another schema or another
// deployment, whose verdicts would be about something else.
func readMatrixEvidence(file, deploymentID string) (acceptanceEvidenceDoc, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return acceptanceEvidenceDoc{}, fmt.Errorf("read the evidence: %w", err)
	}
	var doc acceptanceEvidenceDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return acceptanceEvidenceDoc{}, fmt.Errorf("decode the evidence %s: %w", file, err)
	}
	if doc.Version != acceptanceEvidenceVersion {
		return acceptanceEvidenceDoc{}, fmt.Errorf("the evidence %s is version %d; this binary judges version %d",
			file, doc.Version, acceptanceEvidenceVersion)
	}
	if deploymentID == "" || doc.DeploymentID != deploymentID {
		return acceptanceEvidenceDoc{}, fmt.Errorf("the evidence %s is deployment %q's, not this workspace's %q",
			file, doc.DeploymentID, deploymentID)
	}

	return doc, nil
}

// readMatrixRuns reads the run ids a dispatcher wrote, one per line. A file naming none
// is refused: every cell would read as not run, which is a dispatcher's failure to say
// rather than a matrix's verdict.
func readMatrixRuns(file string) (map[int64]bool, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read the dispatched runs: %w", err)
	}
	out := map[int64]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, err := strconv.ParseInt(line, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%s:%d: %q is not a GitHub run id", file, i+1, line)
		}
		out[id] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s names no GitHub run", file)
	}

	return out, nil
}

// matrixOutcome fails a run with any failed cell or scenario, or one that should have
// run on a backend the run served and an architecture it covered, and did not.
func matrixOutcome(d matrixDoc, required []config.Arch, served []string) error {
	var missing int
	for i := range d.Results {
		r := &d.Results[i]
		if r.Verdict == verdictNotRun && r.Run != nil && slices.Contains(required, r.Arch) &&
			slices.Contains(served, matrixBackendName(&r.matrixCell)) {
			missing++
		}
	}
	for i := range d.Scenarios {
		s := &d.Scenarios[i]
		switch {
		case s.Verdict == verdictFail:
			d.Failed++
		case s.Verdict == verdictNotRun && s.Owed && slices.Contains(required, s.Arch) &&
			slices.Contains(served, string(config.ProviderFirecracker)+"/"+s.Source):
			missing++
		}
	}
	switch {
	case d.Failed > 0 && missing > 0:
		return fmt.Errorf("%d cell(s) failed and %d that should have run did not", d.Failed, missing)
	case d.Failed > 0:
		return fmt.Errorf("%d cell(s) failed", d.Failed)
	case missing > 0:
		return fmt.Errorf("%d cell(s) that should have run did not", missing)
	}

	return nil
}

func parseMatrixBackends(s string) ([]string, error) {
	known := make([]string, 0, len(matrixBackends))
	for _, b := range matrixBackends {
		known = append(known, matrixBackendKey(b))
	}
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		b := strings.TrimSpace(part)
		if b == "" {
			continue
		}
		if !slices.Contains(known, b) {
			return nil, fmt.Errorf("--backends: %q is not a backend the matrix declares (%v)", b, known)
		}
		out = append(out, b)
	}
	// NO BACKEND IS NOT A SELECTION: it would owe nothing, and a run that ran nothing
	// would pass.
	if len(out) == 0 {
		return nil, fmt.Errorf("--backends: name the backends this run served (%v)", known)
	}

	return out, nil
}

func parseMatrixArchs(s string) ([]config.Arch, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []config.Arch
	for part := range strings.SplitSeq(s, ",") {
		a := config.Arch(strings.TrimSpace(part))
		if !slices.Contains(matrixArchs, a) {
			return nil, fmt.Errorf("--archs: %q is not an architecture the matrix declares (%v)", a, matrixArchs)
		}
		out = append(out, a)
	}

	return out, nil
}

// printMatrix writes the verdicts as a table, and the counts.
func printMatrix(w io.Writer, d matrixDoc) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BACKEND\tARCH\tCACHE\tCASE\tVERDICT\tOBSERVED\tWAIT\tRAN\tCOST (EST.)\tREASON")
	for i := range d.Results {
		r := &d.Results[i]
		observed, wait, ran, cost := "-", "-", "-", "-"
		if len(r.Observed) > 0 {
			observed = strings.Join(r.Observed, ",")
		}
		if r.Jobs != nil {
			wait = fmt.Sprintf("%.0fs", r.WaitSeconds)
			ran = fmt.Sprintf("%.0fs", r.RunSeconds)
			cost = fmt.Sprintf("$%.4f", r.CostUSD)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", matrixBackendName(&r.matrixCell), r.Arch, r.Cache, r.Case, r.Verdict,
			observed, wait, ran, cost, dashIfEmpty(oneLineReason(r.Reason)))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%d passed, %d failed, %d not run, %d unsupported\n", d.Passed, d.Failed, d.NotRun, d.Unsupported)

	if len(d.Scenarios) == 0 {
		return
	}
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tARCH\tSCENARIO\tVERDICT\tORDERS\tREASON")
	for i := range d.Scenarios {
		s := &d.Scenarios[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Source, s.Arch, s.Name, s.Verdict,
			dashIfEmpty(strings.Join(s.Orders, ",")), dashIfEmpty(oneLineReason(s.Reason)))
	}
	_ = tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func oneLineReason(s string) string { return strings.Join(strings.Fields(s), " ") }

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(v)
}
