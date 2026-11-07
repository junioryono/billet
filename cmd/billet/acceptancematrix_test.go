package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lease"
	"github.com/junioryono/billet/internal/store/ebss3"

	"gopkg.in/yaml.v3"
)

// THE TEARDOWN'S DELETE REACHES THIS DEPLOYMENT'S STATE OBJECTS AND NO OTHER'S. Runs
// share the bucket's prefix and are told apart by their owner digest, so a grant on
// owners/*/state/* would let one run's teardown delete another's (2026-11-07).
func TestTheTeardownPolicyNamesOnlyThisDeploymentsState(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Node: &config.NodeConfig{Site: "us-west-2a", EBSS3: &config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "cache-bucket", Prefix: "billet-cache",
	}}}
	policy, ok, err := matrixTeardownPolicy(cfg, "d1")
	if err != nil || !ok || len(policy.Statement) != 1 || len(policy.Statement[0].Resource) != 1 {
		t.Fatalf("teardown policy = %+v, %v, %v", policy, ok, err)
	}
	statement := policy.Statement[0]
	if statement.Effect != "Allow" || !slices.Equal(statement.Action, []string{"s3:DeleteObject"}) {
		t.Fatalf("the teardown grants %s %v, want only s3:DeleteObject", statement.Effect, statement.Action)
	}
	resource := statement.Resource[0]
	granted := func(key string) bool {
		return strings.HasPrefix("arn:aws:s3:::cache-bucket/"+key, strings.TrimSuffix(resource, "*"))
	}
	mine := ebss3.StatePrefix(*cfg.Node.EBSS3, "d1/us-west-2a")
	if !strings.HasSuffix(resource, "*") || !granted(mine+"k.json") {
		t.Fatalf("%s does not reach this deployment's state object %s", resource, mine+"k.json")
	}
	if other := ebss3.StatePrefix(*cfg.Node.EBSS3, "d2/us-west-2a"); granted(other + "k.json") {
		t.Fatalf("%s reaches another deployment's state object %s", resource, other+"k.json")
	}
	if archive := strings.TrimSuffix(mine, "state/") + "archives/x"; granted(archive) {
		t.Fatalf("%s reaches this deployment's archive %s", resource, archive)
	}

	if _, ok, err := matrixTeardownPolicy(&config.Config{Node: &config.NodeConfig{}}, "d1"); ok || err != nil {
		t.Fatalf("a config with no ebs-s3 cache rendered a teardown policy: %v, %v", ok, err)
	}
	if _, _, err := matrixTeardownPolicy(cfg, ""); err == nil {
		t.Fatal("a workspace with no deployment id rendered a policy for an unknown prefix")
	}
}

// A CELL WANTS THE LEDGER'S WORD FOR SUCCESS, which is the scale-set message's, never
// the REST API's: the first real run (42358345989, 2026-11-07) failed every cell whose
// jobs succeeded, each "GitHub reported "succeeded", want "success"".
func TestEveryRunCellWantsTheLedgersWordForSuccess(t *testing.T) {
	t.Parallel()
	for _, c := range matrixCells() {
		if c.Run != nil && c.Run.GitHub != alloc.ResultSucceeded {
			t.Errorf("%s %s %s %s wants GitHub's %q, which the ledger never records", c.Backend, c.Arch,
				c.Cache, c.Case, c.Run.GitHub)
		}
	}
}

// THE MATRIX IS THE WHOLE CROSS PRODUCT: every backend, architecture, cache and case
// once, so a combination nobody declared is a failure here rather than a silence in a
// run; and a cell that is not run says why.
func TestTheMatrixIsTheWholeCrossProduct(t *testing.T) {
	t.Parallel()
	cells := matrixCells()
	want := len(matrixBackends) * len(matrixArchs) * len(matrixCaches) * len(matrixCases)
	if len(cells) != want {
		t.Fatalf("%d cells, want %d", len(cells), want)
	}
	seen := map[string]bool{}
	for i := range cells {
		c := &cells[i]
		key := strings.Join([]string{string(c.Backend), string(c.Arch), c.Cache, c.Case}, "/")
		if seen[key] {
			t.Errorf("%s is declared twice", key)
		}
		seen[key] = true
		switch {
		case c.Run != nil && (c.Unsupported || c.Why != ""):
			t.Errorf("%s is run and also says why it is not: %+v", key, c)
		case c.Run == nil && c.Why == "":
			t.Errorf("%s is not run and says no reason", key)
		case c.Why == "an unknown case":
			t.Errorf("%s fell through to the unknown case", key)
		}
	}
	for _, c := range matrixCaches {
		if _, ok := matrixObservation[c]; !ok {
			t.Errorf("cache %s names no observation", c)
		}
	}
}

// A CELL WANTS A TOKEN THE NODE CAN WRITE: one outside the observation's closed
// vocabulary could never pass.
func TestEveryRunCellWantsATokenTheNodeRecords(t *testing.T) {
	t.Parallel()
	for _, c := range matrixCells() {
		if c.Run == nil {
			continue
		}
		var ok bool
		switch c.Run.Observe {
		case "image":
			ok = lease.ImageCache(c.Run.Want).Valid()
		case "actions":
			ok = lease.ActionsCache(c.Run.Want).Valid()
		case "sticky", "git", "go", "bazel", "npm":
			ok = lease.BuildCache(c.Run.Want).Valid()
		}
		if !ok {
			t.Errorf("%s/%s wants %s=%q, which the node never records", c.Cache, c.Case, c.Run.Observe, c.Run.Want)
		}
	}
}

// matrixWorkflowJobs reads a workflow's job ids from this repository's copy.
func matrixWorkflowJobs(t *testing.T, file string) map[string]matrixWorkflowJob {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", matrixWorkflows, file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var wf struct {
		On   map[string]any               `yaml:"on"`
		Jobs map[string]matrixWorkflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Errorf("%s is not dispatched by hand", file)
	}

	return wf.Jobs
}

// matrixWorkflowFiles is every workflow in this repository's copy.
func matrixWorkflowFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", matrixWorkflows))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yml") {
			out = append(out, e.Name())
		}
	}

	return out
}

type matrixWorkflowJob struct {
	Name     string `yaml:"name"`
	RunsOn   string `yaml:"runs-on"`
	Strategy any    `yaml:"strategy"`
	Steps    []struct {
		Uses string            `yaml:"uses"`
		With map[string]string `yaml:"with"`
	} `yaml:"steps"`
}

// EVERY CACHE KEY A MATRIX JOB NAMES CARRIES ITS TIER AND ITS RUN'S TAG: the matrix
// dispatches the same tag to every architecture's tier, and a key two tiers share
// would let one architecture's writer stand in for another's, or a rerun find its
// own last run's entry.
func TestEveryMatrixCacheKeyCarriesItsTierAndTag(t *testing.T) {
	t.Parallel()
	keyed := 0
	for _, file := range matrixWorkflowFiles(t) {
		for id, job := range matrixWorkflowJobs(t, file) {
			for _, step := range job.Steps {
				for _, field := range []string{"key", "cache-key"} {
					key, ok := step.With[field]
					if !ok {
						continue
					}
					keyed++
					if !strings.Contains(key, "${{ inputs.runs_on }}") || !strings.Contains(key, "${{ inputs.tag }}") {
						t.Errorf("%s: job %s's %s %s (%q) does not carry its tier and tag", file, id, step.Uses, field, key)
					}
				}
			}
		}
	}
	if keyed == 0 {
		t.Fatal("no matrix job names a cache key; this test reads nothing")
	}
}

// EVERY RUN CELL NAMES A JOB ITS WORKFLOW RUNS, on the tier the run names, under the
// name GitHub reports: a job id with no display name of its own and no strategy that
// would rename it. And every workflow in the directory is one a cell judges.
func TestEveryRunCellNamesAJobItsWorkflowRuns(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	for _, c := range matrixCells() {
		if c.Run == nil {
			continue
		}
		used[c.Run.Workflow] = true
		jobs := matrixWorkflowJobs(t, c.Run.Workflow)
		job, ok := jobs[c.Run.Job]
		switch {
		case !ok:
			t.Errorf("%s/%s: %s has no job %q", c.Cache, c.Case, c.Run.Workflow, c.Run.Job)
		case job.Name != "" || job.Strategy != nil:
			t.Errorf("%s: job %s is renamed by a name or a strategy, so GitHub reports another name",
				c.Run.Workflow, c.Run.Job)
		}
	}
	for _, file := range matrixWorkflowFiles(t) {
		for id, job := range matrixWorkflowJobs(t, file) {
			if job.RunsOn != "${{ inputs.runs_on }}" {
				t.Errorf("%s: job %s runs on %q, not the tier the run names", file, id, job.RunsOn)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", matrixWorkflows))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yml") && !used[e.Name()] {
			t.Errorf("%s is in %s and no cell judges it", e.Name(), matrixWorkflows)
		}
	}
}

// THE RUN WAITS FOR EVERY JOB THE WORKFLOWS RUN: a count short of them ends the run
// before the warm jobs finish, and one past them waits out --wait for jobs that never
// come.
func TestTheMatrixJobCountIsTheWorkflowsJobs(t *testing.T) {
	t.Parallel()
	n := 0
	for _, file := range matrixWorkflowFiles(t) {
		n += len(matrixWorkflowJobs(t, file))
	}
	if n != matrixJobsPerArch {
		t.Fatalf("the workflows run %d jobs per tier; matrixJobsPerArch says %d", n, matrixJobsPerArch)
	}
}

// THE WARM WORKFLOW REUSES WHAT THE COLD ONE WROTE: the same jobs, so each cache has
// its pair.
func TestTheColdAndWarmWorkflowsRunTheSameJobs(t *testing.T) {
	t.Parallel()
	ids := func(file string) []string {
		var out []string
		for id := range matrixWorkflowJobs(t, file) {
			out = append(out, id)
		}
		slices.Sort(out)

		return out
	}
	if cold, warm := ids("matrix-cold.yml"), ids("matrix-warm.yml"); !slices.Equal(cold, warm) {
		t.Fatalf("cold runs %v, warm %v", cold, warm)
	}
}

func matrixTestTiers() map[string]matrixTier {
	return map[string]matrixTier{
		"accept-x64": {providers: []config.ProviderKind{config.ProviderEC2}, arch: config.ArchX64},
		"accept-arm": {providers: []config.ProviderKind{config.ProviderEC2}, arch: config.ArchARM64},
		"accept-fc":  {providers: []config.ProviderKind{config.ProviderFirecracker}, arch: config.ArchX64},
	}
}

func coldGitJob() acceptanceJob {
	return acceptanceJob{
		LeaseID: "l-accept-x64", Tier: "accept-x64", GitHub: alloc.ResultSucceeded, Provider: "ec2", InstanceType: "c7i.large",
		Trust:       "trusted",
		WorkflowRef: "billet-testbed/acceptance-matrix/.github/workflows/matrix-cold.yml@refs/heads/main",
		JobName:     "git",
		QueuedAt:    "2026-11-05T10:00:00Z", StartedAt: "2026-11-05T10:00:30Z", FinishedAt: "2026-11-05T10:30:30Z",
		Cache: acceptanceCache{Git: "cold"},
	}
}

// cellOf is the EC2 cell of an architecture, cache and case: EC2 is the one backend the
// matrix declares yet.
func cellOf(t *testing.T, arch config.Arch, cache, kase string) matrixCell {
	t.Helper()
	for _, c := range matrixCells() {
		if c.Backend == config.ProviderEC2 && c.Arch == arch && c.Cache == cache && c.Case == kase {
			return c
		}
	}
	t.Fatalf("no cell ec2/%s/%s/%s", arch, cache, kase)

	return matrixCell{}
}

// A CELL PASSES ON BOTH WITNESSES, and fails on either: GitHub's result, and the
// node's observation of the job, which must be the token the cell wants and not
// nothing. A job of another workflow, job, architecture or provider is not the cell's.
func TestACellIsJudgedOnGitHubsResultAndTheNodesObservation(t *testing.T) {
	t.Parallel()
	cell := cellOf(t, config.ArchX64, "git", "cold")
	prices := map[string]config.USDPerHour{"c7i.large": 89_250}
	for name, c := range map[string]struct {
		edit    func(*acceptanceJob)
		verdict matrixVerdict
		reason  string
	}{
		"both witnesses agree":               {func(*acceptanceJob) {}, verdictPass, ""},
		"GitHub failed it":                   {func(j *acceptanceJob) { j.GitHub = "failed" }, verdictFail, `GitHub reported "failed"`},
		"the node saw it warm":               {func(j *acceptanceJob) { j.Cache.Git = "warm" }, verdictFail, `observed git "warm", want "cold"`},
		"the node saw nothing":               {func(j *acceptanceJob) { j.Cache.Git = "" }, verdictFail, "recorded no git observation"},
		"its history was unread":             {func(j *acceptanceJob) { j.Unread = "disk on fire" }, verdictFail, "disk on fire"},
		"placed on another provider":         {func(j *acceptanceJob) { j.Provider = "codebuild" }, verdictFail, "placed on codebuild"},
		"an untrusted tier":                  {func(j *acceptanceJob) { j.Trust = "untrusted" }, verdictFail, `its tier is "untrusted"`},
		"a tier of no trust the config says": {func(j *acceptanceJob) { j.Trust = "" }, verdictFail, `its tier is "-"`},
		"another workflow":                   {func(j *acceptanceJob) { j.WorkflowRef = strings.Replace(j.WorkflowRef, "cold", "warm", 1) }, verdictNotRun, "no git job of matrix-cold.yml"},
		"another job":                        {func(j *acceptanceJob) { j.JobName = "go" }, verdictNotRun, "no git job"},
		"another architecture's tier":        {func(j *acceptanceJob) { j.Tier = "accept-arm" }, verdictNotRun, "ec2 x64 tier"},
		"another provider's tier":            {func(j *acceptanceJob) { j.Tier = "accept-fc" }, verdictNotRun, "ec2 x64 tier"},
		"a tier the config lacks":            {func(j *acceptanceJob) { j.Tier = "gone" }, verdictNotRun, "ec2 x64 tier"},
	} {
		job := coldGitJob()
		c.edit(&job)
		got := judgeCell(cell, []acceptanceJob{job}, matrixScope{tiers: matrixTestTiers(), prices: prices})
		if got.Verdict != c.verdict || !strings.Contains(got.Reason, c.reason) {
			t.Errorf("%s: %s %q, want %s with %q", name, got.Verdict, got.Reason, c.verdict, c.reason)
		}
	}

	got := judgeCell(cell, []acceptanceJob{coldGitJob()}, matrixScope{tiers: matrixTestTiers(), prices: prices})
	if got.WaitSeconds != 30 || got.RunSeconds != 1800 || got.CostUSD < 0.0446 || got.CostUSD > 0.0447 ||
		!slices.Equal(got.Observed, []string{"cold"}) || !slices.Equal(got.Jobs, []string{"l-accept-x64"}) {
		t.Errorf("the numbers are %+v; want 30 s waiting, 1800 s running, half an hour at $0.08925", got)
	}
}

// ONLY THIS MATRIX RUN'S JOBS ARE A CELL'S: a job of a run it did not dispatch, or of
// another repository, does not stand in for one it never got, however its tier,
// workflow and name match.
func TestOnlyThisMatrixRunsJobsAreJudged(t *testing.T) {
	t.Parallel()
	cell := cellOf(t, config.ArchX64, "git", "cold")
	job := coldGitJob()
	job.Repository, job.GitHubRunID = "billet-testbed/acceptance-matrix", 77
	scope := matrixScope{tiers: matrixTestTiers(), runs: map[int64]bool{77: true},
		repository: "billet-testbed/acceptance-matrix"}
	if got := judgeCell(cell, []acceptanceJob{job}, scope); got.Verdict != verdictPass {
		t.Fatalf("this run's job: %+v", got)
	}
	// A JOB OF THIS CELL WHOSE HISTORY COULD NOT BE READ fails it, though it names no
	// run: dropped, it would let this run's passing job stand for evidence unread.
	unread := job
	unread.LeaseID, unread.GitHubRunID, unread.Unread = "l-unread", 0, "its history could not be read: locked"
	if got := judgeCell(cell, []acceptanceJob{job, unread}, scope); got.Verdict != verdictFail ||
		!strings.Contains(got.Reason, "l-unread") {
		t.Fatalf("a passing job beside an unread one: %s %q", got.Verdict, got.Reason)
	}
	for name, edit := range map[string]func(*acceptanceJob){
		"another run":          func(j *acceptanceJob) { j.GitHubRunID = 78 },
		"an unread run":        func(j *acceptanceJob) { j.GitHubRunID = 0 },
		"another repository":   func(j *acceptanceJob) { j.Repository = "billet-testbed/elsewhere" },
		"an unread repository": func(j *acceptanceJob) { j.Repository = "" },
	} {
		other := job
		edit(&other)
		if got := judgeCell(cell, []acceptanceJob{other}, scope); got.Verdict != verdictNotRun {
			t.Errorf("%s: %s %q, want not run", name, got.Verdict, got.Reason)
		}
	}
}

// THE DISPATCHED RUNS ARE READ AS IDS, and a file naming none, or anything that is not
// one, is refused rather than read as a filter that admits nothing.
func TestTheDispatchedRunsAreRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "runs")
	for body, want := range map[string]string{
		"11\n22\n\n":    "",
		"":              "names no GitHub run",
		"11\nnot-a-run": "not a GitHub run id",
		"-4\n":          "not a GitHub run id",
	} {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readMatrixRuns(file)
		switch {
		case want == "" && (err != nil || len(got) != 2 || !got[11] || !got[22]):
			t.Errorf("%q: %v, %v", body, got, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
}

// EVERY JOB OF A CELL IS JUDGED: a rerun that failed beside one that passed fails the
// cell, rather than the passing one standing for both.
func TestEveryJobOfACellIsJudged(t *testing.T) {
	t.Parallel()
	cell := cellOf(t, config.ArchX64, "git", "cold")
	again := coldGitJob()
	again.LeaseID, again.GitHub = "l-again", "failed"
	got := judgeCell(cell, []acceptanceJob{coldGitJob(), again}, matrixScope{tiers: matrixTestTiers()})
	if got.Verdict != verdictFail || !strings.Contains(got.Reason, "l-again") || len(got.Jobs) != 2 {
		t.Fatalf("%+v", got)
	}
}

// THE ACTIONS CACHE IS JUDGED SERVED, cold and warm: the observation says the site
// store answered, and the job asserts the miss or the hit itself.
func TestTheActionsCellsWantServed(t *testing.T) {
	t.Parallel()
	for _, kase := range []string{"cold", "warm"} {
		c := cellOf(t, config.ArchARM64, "actions", kase)
		if c.Run == nil || c.Run.Want != string(lease.ActionsCacheServed) || c.Run.Workflow != "matrix-"+kase+".yml" {
			t.Errorf("actions/%s is %+v", kase, c.Run)
		}
	}
}

// A CELL NOT RUN OR UNSUPPORTED IS NEVER A PASS, and the whole run fails on a failed
// cell, or on a cell that should have run on an architecture the run covered.
func TestARunFailsOnAFailedCellOrOneThatShouldHaveRun(t *testing.T) {
	t.Parallel()
	job := coldGitJob()
	doc := judgeMatrix(matrixCells(), acceptanceEvidenceDoc{DeploymentID: "d1", Jobs: []acceptanceJob{job}},
		matrixScope{tiers: matrixTestTiers()})
	if doc.Passed != 1 || doc.Failed != 0 || doc.Passed+doc.Failed+doc.NotRun+doc.Unsupported != len(doc.Results) {
		t.Fatalf("counts %d/%d/%d/%d of %d", doc.Passed, doc.Failed, doc.NotRun, doc.Unsupported, len(doc.Results))
	}
	for _, r := range doc.Results {
		if r.Verdict == verdictPass && (r.Run == nil || r.Case != "cold" || r.Cache != "git") {
			t.Errorf("%s/%s/%s passed with no job", r.Arch, r.Cache, r.Case)
		}
	}
	if err := matrixOutcome(doc, nil); err != nil {
		t.Errorf("a run covering no architecture failed on cells not run: %v", err)
	}
	if err := matrixOutcome(doc, []config.Arch{config.ArchX64}); err == nil ||
		!strings.Contains(err.Error(), "should have run did not") {
		t.Errorf("a run covering x64 with its cells unrun answered %v", err)
	}
	job.GitHub = "failed"
	doc = judgeMatrix(matrixCells(), acceptanceEvidenceDoc{Jobs: []acceptanceJob{job}}, matrixScope{tiers: matrixTestTiers()})
	if err := matrixOutcome(doc, nil); err == nil || !strings.Contains(err.Error(), "1 cell(s) failed") {
		t.Errorf("a failed cell answered %v", err)
	}
}

func TestAWorkflowRefNamesItsFile(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]string{
		"o/r/.github/workflows/matrix-cold.yml@refs/heads/main": "matrix-cold.yml",
		"o/r/.github/workflows/matrix-cold.yml":                 "matrix-cold.yml",
		"o/r/somewhere/matrix-cold.yml@refs/heads/main":         "",
		"": "",
	} {
		if got := workflowFile(ref); got != want {
			t.Errorf("workflowFile(%q) = %q, want %q", ref, got, want)
		}
	}
}

// EVIDENCE OF ANOTHER DEPLOYMENT OR SCHEMA IS REFUSED: its verdicts would be about
// something else.
func TestTheJudgeReadsOnlyThisWorkspacesEvidence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(doc acceptanceEvidenceDoc) string {
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "evidence.json")
		if err := os.WriteFile(file, body, 0o600); err != nil {
			t.Fatal(err)
		}

		return file
	}
	file := write(acceptanceEvidenceDoc{Version: acceptanceEvidenceVersion, DeploymentID: "d1"})
	if _, err := readMatrixEvidence(file, "d1"); err != nil {
		t.Fatalf("this workspace's evidence: %v", err)
	}
	for name, id := range map[string]string{"another deployment's": "d2", "an unrecorded workspace's": ""} {
		if _, err := readMatrixEvidence(file, id); err == nil || !strings.Contains(err.Error(), "deployment") {
			t.Errorf("%s evidence answered %v", name, err)
		}
	}
	file = write(acceptanceEvidenceDoc{Version: acceptanceEvidenceVersion + 1, DeploymentID: "d1"})
	if _, err := readMatrixEvidence(file, "d1"); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("evidence of another schema answered %v", err)
	}
}

// A DISPATCHER IS GIVEN ONE TIER PER BACKEND AND ARCHITECTURE: one of its own, never a
// tier that mixes providers (its jobs could land elsewhere) and never one of two (the
// cold and warm jobs must meet on one tier's caches).
func TestTheDispatcherIsGivenOneTierPerBackendAndArchitecture(t *testing.T) {
	t.Parallel()
	got := matrixTierLines(map[string]matrixTier{
		"accept-x64":   {providers: []config.ProviderKind{config.ProviderEC2}, arch: config.ArchX64},
		"accept-mixed": {providers: []config.ProviderKind{config.ProviderEC2, config.ProviderFirecracker}, arch: config.ArchX64},
		"accept-arm-1": {providers: []config.ProviderKind{config.ProviderEC2}, arch: config.ArchARM64},
		"accept-arm-2": {providers: []config.ProviderKind{config.ProviderEC2}, arch: config.ArchARM64},
	})
	if !slices.Equal(got, []string{"ec2 x64 accept-x64"}) {
		t.Fatalf("tiers = %q, want only the x64 tier of its own", got)
	}
}

// A TIER IS THE CELL'S BY ANY PROVIDER IT NAMES, whether in provider or providers.
func TestMatrixTiersReadEveryProviderATierNames(t *testing.T) {
	t.Parallel()
	got := matrixTiers(&config.Config{Tiers: []config.Tier{
		{Label: "one", Provider: config.ProviderEC2, Arch: config.ArchX64},
		{Label: "many", Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2}, Arch: config.ArchARM64},
	}})
	if !slices.Contains(got["one"].providers, config.ProviderEC2) || got["one"].arch != config.ArchX64 ||
		!slices.Contains(got["many"].providers, config.ProviderEC2) || got["many"].arch != config.ArchARM64 {
		t.Fatalf("%+v", got)
	}
}
