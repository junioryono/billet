package fleetops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// gb is a decimal gigabyte. The tiers here are whole numbers of them so that a
// fraction on a threshold is exactly that fraction of a whole number of bytes, where
// a fraction of a power of two would round to one side of the line it is testing.
const gb = config.ByteSize(1_000_000_000)

// rightsizeTier is a tier of the one family these tests move jobs within.
func rightsizeTier(label string, vcpu int, memory config.ByteSize) config.Tier {
	return config.Tier{Label: label, Target: "default", RunsOn: "runs-" + label, Provider: config.ProviderFirecracker,
		GuestOS: config.GuestLinux, VCPU: vcpu, Memory: memory, Image: "ubuntu-2404-x64@verified"}
}

var rightsizeTiers = []config.Tier{
	rightsizeTier("t2", 2, 8*gb),
	rightsizeTier("t4", 4, 16*gb),
	rightsizeTier("t8", 8, 32*gb),
	rightsizeTier("t16", 16, 64*gb),
}

// runsOf is n runs of one job on tier t4 (4 vCPU, 16 GB), each an hour long: the
// host's series busy for busy of the charged vCPUs, the job's own VM reporting it
// used guest of its memory at most, and the host's peak at host of the charged
// memory, with the usage summary's other figures as adjust leaves them. The summary's
// own CPU is set far from busy, so a reader that took it would be seen to.
func runsOf(name string, n int, busy, guest, host float64, adjust func(i int, j *fixedJob)) []fixedJob {
	jobs := make([]fixedJob, 0, n)
	for i := range n {
		j := fixedJob{lease: fmt.Sprintf("%s-%03d", name, i), tier: "t4", repo: "acme/api", workflow: fixedWorkflow,
			job: name, run: int64(i + 1), attempt: 1, result: "succeeded",
			startedAgo: 24*time.Hour + time.Duration(i)*time.Minute, ran: time.Hour, vcpu: 4,
			memory: int64(16 * gb), series: fraction(busy), guest: fraction(guest),
			usage: &fixedUsage{windowMS: 100 * 3_600_000, cpuUser: 1, guestCPU: 999 * 3_600_000_000,
				memoryPeak: int64(host * float64(16*gb))}}
		if adjust != nil {
			adjust(i, &j)
		}
		jobs = append(jobs, j)
	}

	return jobs
}

// rightsizeOne is the report's one job over rows and the tiers.
func rightsizeOne(t *testing.T, jobs []fixedJob, tiers []config.Tier) rightsizeJob {
	t.Helper()

	report, err := buildRightsize(t.Context(), fixedAllocator(t, jobs), tiers, reportNow.Add(-7*24*time.Hour), reportNow)
	if err != nil {
		t.Fatalf("buildRightsize: %v", err)
	}
	if len(report.Jobs) != 1 {
		t.Fatalf("jobs = %+v, want one", report.Jobs)
	}

	return report.Jobs[0]
}

// EACH RECOMMENDATION COMES FROM THE FIGURE ITS REASON NAMES, and from the
// thresholds as stated: a value on the line takes the recommendation and one just
// off it does not.
func TestARecommendationFollowsItsThreshold(t *testing.T) {
	t.Parallel()

	oom := func(i int, j *fixedJob) {
		if i == 0 {
			j.usage.oomKills = 1
			j.result = "failed"
		}
	}
	// Pressure is over the summary's window, which is a hundred hours.
	pressure := func(share float64) func(int, *fixedJob) {
		return func(_ int, j *fixedJob) { j.usage.cpuSome = int64(share * 100 * float64(time.Hour.Microseconds())) }
	}
	hostOnly := func(_ int, j *fixedJob) { j.guest = nil }

	for _, tc := range []struct {
		name               string
		busy, guest, host  float64
		adjust             func(int, *fixedJob)
		want, tier, reason string
	}{
		{"idle by the VM's word moves to the smallest that holds it", 0.05, 0.10, 2.0, nil, rightsizeSmaller, "t2",
			"memory the job's own VM reported at p95 10%"},
		{"idle by the host's peak alone", 0.05, 0, 0.30, hostOnly, rightsizeSmaller, "t2", "the host's memory peak at p95 30%"},
		{"the host's peak is what a tier is sized by where it proves small", 0.05, 0.05, 0.45, nil, rightsizeSmaller, "",
			"the host's memory peak at p95 45%"},
		{"busy just over the idle line", 0.31, 0.10, 0.10, nil, rightsizeKeep, "", "neither idle nor short of room"},
		{"memory just over the idle line, on both sides", 0.05, 0.46, 0.46, nil, rightsizeKeep, "", "its memory could not be told"},
		{"a host peak over the line and no VM report cannot be told", 0.05, 0, 0.46, hostOnly, rightsizeKeep, "",
			"0 of 40 runs have a whole report from the job's own VM, and the host's peak, which counts the page cache " +
				"and the VMM, is 46% at p95"},
		{"CPU-bound at the line", 0.85, 0.10, 0.10, nil, rightsizeLarger, "t8", "CPU-bound (CPU busy at p90 85%)"},
		{"just under CPU-bound", 0.84, 0.10, 0.10, nil, rightsizeKeep, "", "neither idle nor short of room"},
		{"pressure alone is CPU-bound", 0.50, 0.10, 0.10, pressure(0.25), rightsizeLarger, "t8", "CPU-bound (CPU pressure at p90 25%)"},
		{"just under the pressure line", 0.50, 0.10, 0.10, pressure(0.2499), rightsizeKeep, "", "neither idle"},
		{"near the memory limit at the line, by the VM's word", 0.50, 0.90, 0.10, nil, rightsizeLarger, "t8",
			"near the memory limit (the job's own VM reported using 90% at p95)"},
		{"just under near", 0.50, 0.89, 0.10, nil, rightsizeKeep, "", "neither idle"},
		{"the host's peak never says near", 0.50, 0.10, 2.80, nil, rightsizeKeep, "", "neither idle"},
		{"an OOM kill is over the limit however low the peak", 0.05, 0.10, 0.10, oom, rightsizeLarger, "t8",
			"over the memory limit (1 OOM kills)"},
		{"both reasons are said", 0.90, 0.95, 0.10, nil, rightsizeLarger, "t8",
			"near the memory limit (the job's own VM reported using 95% at p95) and CPU-bound"},
	} {
		got := rightsizeOne(t, runsOf("test", 40, tc.busy, tc.guest, tc.host, tc.adjust), rightsizeTiers)
		if got.Recommendation != tc.want {
			t.Errorf("%s: %s (%s), want %s", tc.name, got.Recommendation, got.Reason, tc.want)
		}
		if !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("%s: the reason %q does not say %q", tc.name, got.Reason, tc.reason)
		}
		var tier string
		if got.Recommended != nil {
			tier = got.Recommended.Label
		}
		if tier != tc.tier {
			t.Errorf("%s: recommended %q, want %q", tc.name, tier, tc.tier)
		}
	}
}

// A JOB NOT MEASURED GETS NO RECOMMENDATION, NEVER ONE FROM ZEROS: not with no
// report, not with no series kept, not with a series short of the run, and not under
// the minimum. The summary's own CPU, which reads as busy as can be, is never taken
// in their place.
func TestAJobNotMeasuredGetsNoRecommendation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		jobs   []fixedJob
		reason string
	}{
		{"no report", runsOf("test", 40, 0.05, 0.1, 0.1, func(_ int, j *fixedJob) { j.usage = nil }),
			"40 had no usage report the host read CPU from, 0 no series kept"},
		{"cpu unmeasured", runsOf("test", 40, 0.05, 0.1, 0.1, func(_ int, j *fixedJob) { j.usage.unmeasured = "cpu" }),
			"40 had no usage report"},
		{"no charged shape", runsOf("test", 40, 0.05, 0.1, 0.1, func(_ int, j *fixedJob) { j.vcpu = 0 }), "40 had no usage report"},
		{"no result time", runsOf("test", 40, 0.05, 0.1, 0.1, func(_ int, j *fixedJob) { j.ran = 0; j.series = nil }),
			"not measured"},
		{"no series kept", runsOf("test", 40, 0.05, 0.1, 0.1, func(_ int, j *fixedJob) { j.series = nil }),
			"0 had no usage report the host read CPU from, 40 no series kept"},
		{"a series short of the run", runsOf("test", 40, 0.05, 0.1, 0.1, func(_ int, j *fixedJob) { j.cover = 0.85 }),
			"40 a series covering under 90% of the run"},
		{"under the minimum", runsOf("test", 9, 0.05, 0.1, 0.1, nil), "9 of the 10 measured runs a recommendation needs"},
	} {
		got := rightsizeOne(t, tc.jobs, rightsizeTiers)
		if got.Recommendation != rightsizeNone || got.Recommended != nil || !strings.Contains(got.Reason, tc.reason) ||
			got.Confidence != "" {
			t.Errorf("%s: %s, %q, confidence %q, tier %+v; want none saying %q", tc.name, got.Recommendation, got.Reason,
				got.Confidence, got.Recommended, tc.reason)
		}
	}

	// A series that covers nine tenths of the run is enough.
	if got := rightsizeOne(t, runsOf("test", 40, 0.05, 0.1, 0.1, func(_ int, j *fixedJob) { j.cover = 0.92 }), rightsizeTiers); got.Samples != 40 {
		t.Errorf("a series covering 92%% of each run measured %d runs, want 40", got.Samples)
	}
}

// THE VM'S WORD IS TAKEN ONLY FROM A REPORT THE NODE KEPT WHOLE, since a partial one
// may have lost the peak, and only over enough runs to judge.
func TestTheVMsMemoryIsReadOnlyFromAWholeReport(t *testing.T) {
	t.Parallel()

	partial := rightsizeOne(t, runsOf("test", 40, 0.05, 0.10, 2.0, func(_ int, j *fixedJob) { j.guestPartial = true }), rightsizeTiers)
	if partial.GuestRuns != 0 || partial.GuestP95 != nil || partial.Recommendation != rightsizeKeep {
		t.Errorf("partial reports: %d runs, %v, %s (%s); want none read and nothing proved small", partial.GuestRuns,
			partial.GuestP95, partial.Recommendation, partial.Reason)
	}

	few := rightsizeOne(t, runsOf("test", 40, 0.50, 0.95, 0.10, func(i int, j *fixedJob) {
		if i >= 9 {
			j.guest = nil
		}
	}), rightsizeTiers)
	if few.GuestRuns != 9 || few.Recommendation != rightsizeKeep {
		t.Errorf("nine reports near the limit: %d runs, %s (%s); want them read and not judged", few.GuestRuns,
			few.Recommendation, few.Reason)
	}
}

// THE CONFIDENCE IS THE SAMPLE COUNT'S, at the lines the report states.
func TestAConfidenceComesFromTheSampleCount(t *testing.T) {
	t.Parallel()

	for n, want := range map[int]string{10: "low", 29: "low", 30: "medium", 99: "medium", 100: "high"} {
		if got := rightsizeOne(t, runsOf("test", n, 0.5, 0.5, 0.5, nil), rightsizeTiers); got.Confidence != want || got.Samples != n {
			t.Errorf("%d runs read as %d samples with confidence %q, want %q", n, got.Samples, got.Confidence, want)
		}
	}
}

// ONLY RUNS ON THE CURRENT TIER COUNT, which is the newest run's: a job moved off a
// tier is judged on where it is now. A run that says nothing about its shape is left
// out, and one the host did not measure is counted as such.
func TestOnlyRunsOnTheCurrentTierCount(t *testing.T) {
	t.Parallel()

	jobs := runsOf("test", 12, 0.05, 0.10, 0.10, nil)
	// Older runs on another tier, far busier than the current one.
	jobs = append(jobs, runsOf("test", 20, 0.99, 0.99, 0.99, func(i int, j *fixedJob) {
		j.lease, j.tier = fmt.Sprintf("old-%d", i), "t8"
		j.startedAgo = 72*time.Hour + time.Duration(i)*time.Minute
	})...)
	// A canceled run, a run with no result and a run billet disrupted say nothing,
	// and are as busy as the ones above, so counting them would move the percentile.
	for i, adjust := range []func(j *fixedJob){
		func(j *fixedJob) { j.result = "canceled" },
		func(j *fixedJob) { j.result, j.ran, j.series = "", 0, nil },
		func(j *fixedJob) { j.result, j.disruption = "failed", "spot_interruption" },
	} {
		j := runsOf("test", 1, 0.99, 0.99, 0.99, nil)[0]
		j.lease, j.startedAgo = fmt.Sprintf("ignored-%d", i), 30*time.Hour
		adjust(&j)
		jobs = append(jobs, j)
	}
	// A run on this tier the host did not measure: counted, and not a sample.
	unmeasured := runsOf("test", 1, 0, 0, 0, func(_ int, j *fixedJob) { j.usage = nil })[0]
	unmeasured.lease, unmeasured.startedAgo = "unmeasured", 31*time.Hour
	jobs = append(jobs, unmeasured)

	got := rightsizeOne(t, jobs, rightsizeTiers)
	if got.Tier != "t4" || got.RunsOn != "runs-t4" || got.Samples != 12 || got.OtherTiers != 20 || got.NoReport != 1 {
		t.Errorf("tier %s (%s), %d samples, %d on other tiers, %d with no report; want t4, 12, 20 and 1", got.Tier,
			got.RunsOn, got.Samples, got.OtherTiers, got.NoReport)
	}
	if got.Recommendation != rightsizeSmaller {
		t.Errorf("recommendation %s (%s), want smaller from the current tier's figures alone", got.Recommendation, got.Reason)
	}
}

// THE BUSY FRACTION IS THE HOST SERIES OVER THE JOB'S OWN WINDOW: what the summary
// says over the lease's life, which on a pooled runner includes the hours it stood
// idle, is not read, and neither is what the series holds outside the job.
func TestTheBusyFractionIsTheSeriesOverTheJob(t *testing.T) {
	t.Parallel()

	got := rightsizeOne(t, runsOf("test", 10, 0.5, 0.1, 0.1, func(_ int, j *fixedJob) {
		j.outside = fraction(0.95)
	}), rightsizeTiers)
	if got.BusyP90 == nil || *got.BusyP90 < 0.495 || *got.BusyP90 > 0.505 {
		t.Errorf("busy = %v, want the series' 0.5 over the job's window, not the 0.95 around it", got.BusyP90)
	}
	if got.DemandP90 == nil || *got.DemandP90 < 1.98 || *got.DemandP90 > 2.02 {
		t.Errorf("demand = %v cores, want 2 of the 4 charged", got.DemandP90)
	}
}

// ONLY RUNS AT THE NEWEST RUN'S CHARGED SHAPE COUNT, and a smaller tier is sized by
// the CPU in whole cores: a shape a fallback bought larger than the tier asks for
// is busier in cores than its fraction of the tier would say.
func TestRunsAtAnotherShapeAreLeftOutAndATierIsSizedInCores(t *testing.T) {
	t.Parallel()

	resized := runsOf("test", 12, 0.05, 0.10, 0.10, nil)
	resized = append(resized, runsOf("test", 30, 0.95, 0.95, 0.95, func(i int, j *fixedJob) {
		j.lease, j.vcpu, j.memory = fmt.Sprintf("before-%d", i), 2, int64(8*gb)
		j.startedAgo = 72*time.Hour + time.Duration(i)*time.Minute
	})...)
	got := rightsizeOne(t, resized, rightsizeTiers)
	if got.Samples != 12 || got.OtherShapes != 30 || got.Recommendation != rightsizeSmaller {
		t.Errorf("%d samples, %d at another shape, %s (%s); want 12, 30 and smaller", got.Samples, got.OtherShapes,
			got.Recommendation, got.Reason)
	}

	// Charged 8 vCPU on the 4 vCPU tier and 25% busy: 2 cores, which a 2 vCPU tier
	// would run 100% busy, so there is no smaller tier for it.
	bought := rightsizeOne(t, runsOf("test", 40, 0.25, 0.10, 0.10, func(_ int, j *fixedJob) { j.vcpu = 8 }), rightsizeTiers)
	if bought.Recommendation != rightsizeSmaller || bought.Recommended != nil {
		t.Errorf("a larger bought shape: %s to %+v; want smaller and no tier that holds 2 cores", bought.Recommendation,
			bought.Recommended)
	}
}

// WITH NOTHING TO SAY OF ITS MEMORY, A JOB THAT IS NOT IDLE IS KEPT WITHOUT A CLAIM
// IT HAS ROOM, and the JSON has no shape where none was recorded.
func TestKeepClaimsNoRoomItCouldNotTell(t *testing.T) {
	t.Parallel()

	got := rightsizeOne(t, runsOf("test", 40, 0.50, 0, 0, func(_ int, j *fixedJob) {
		j.guest = nil
		j.usage.unmeasured = "memory"
	}), rightsizeTiers)
	if got.Recommendation != rightsizeKeep || strings.Contains(got.Reason, "short of room") ||
		!strings.Contains(got.Reason, "its memory could not be told") {
		t.Errorf("%s (%s); want keep, saying memory could not be told", got.Recommendation, got.Reason)
	}

	var out bytes.Buffer
	if err := renderRightsizeJSON(&out, rightsizeReport{Jobs: []rightsizeJob{{Repository: "acme/api"}}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"vcpu": null`, `"memory_bytes": null`, `"current_tier_vcpu": null`, `"current_tier_memory_bytes": null`,
		`"recommended_vcpu": null`, `"recommended_memory_bytes": null`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("an unrecorded shape is not %s:\n%s", want, out.String())
		}
	}
}

// A CANDIDATE DIFFERS ONLY IN ITS NAME AND SIZE, so a move changes how much room a
// job has and nothing else; a trusted tier must allow the workflow; and a job with
// nowhere to go says so rather than naming a tier that does not fit.
func TestARecommendationStaysInTheTiersFamily(t *testing.T) {
	t.Parallel()

	idle := runsOf("test", 40, 0.05, 0.10, 0.10, nil)
	busy := runsOf("test", 40, 0.95, 0.10, 0.10, nil)
	current := rightsizeTier("t4", 4, 16*gb)

	for name, change := range map[string]func(*config.Tier){
		"another command":      func(t *config.Tier) { t.Command = []string{"./other-runner"} },
		"another cache":        func(t *config.Tier) { t.Intercept = true },
		"a smaller disk":       func(t *config.Tier) { t.Disk = 10 * gb },
		"a default for a disk": func(t *config.Tier) {},
		"a disk for a default": func(t *config.Tier) { t.Disk = 30 * gb },
		"another target":       func(t *config.Tier) { t.Target = "other" },
		"another image":        func(t *config.Tier) { t.Image = "ubuntu-2204-x64@verified" },
		"another provider":     func(t *config.Tier) { t.Provider = config.ProviderDocker },
		"another guest OS":     func(t *config.Tier) { t.GuestOS = config.GuestMacOS },
		"another architecture": func(t *config.Tier) { t.Arch = config.ArchARM64 },
		"another runner group": func(t *config.Tier) { t.RunnerGroup = "other" },
		"another site":         func(t *config.Tier) { t.Site = "other" },
		"another node":         func(t *config.Tier) { t.Node = "other" },
		"trusted":              func(t *config.Tier) { t.Trust = config.WorkloadTrusted },
	} {
		small := rightsizeTier("small", 2, 8*gb)
		change(&small)
		from := current
		if name == "a smaller disk" || name == "a default for a disk" {
			from.Disk = 20 * gb
		}
		got := rightsizeOne(t, idle, []config.Tier{from, small})
		if got.Recommendation != rightsizeSmaller || got.Recommended != nil ||
			!strings.Contains(got.Reason, "no smaller tier is configured") {
			t.Errorf("%s: %s to %+v (%s); want smaller and no tier", name, got.Recommendation, got.Recommended, got.Reason)
		}
	}

	if got := rightsizeOne(t, busy, []config.Tier{current, rightsizeTier("small", 2, 8*gb)}); got.Recommended != nil ||
		got.Recommendation != rightsizeLarger || !strings.Contains(got.Reason, "no larger tier is configured") {
		t.Errorf("no larger tier: %s to %+v (%s)", got.Recommendation, got.Recommended, got.Reason)
	}

	// AN IMAGE NAMING NO ARCHITECTURE MAY RUN ON EITHER, so a tier of the same family is
	// still no tier known to run the job, and the reason says which it was.
	noArch := func(name string, vcpu int, memory config.ByteSize) config.Tier {
		out := rightsizeTier(name, vcpu, memory)
		out.Image = "ubuntu-2404@verified"
		return out
	}
	for _, tc := range []struct {
		runs []fixedJob
		want string
	}{{idle, rightsizeSmaller}, {busy, rightsizeLarger}} {
		got := rightsizeOne(t, tc.runs, []config.Tier{noArch("t4", 4, 16*gb), noArch("small", 2, 8*gb),
			noArch("large", 8, 32*gb)})
		if got.Recommendation != tc.want || got.Recommended != nil ||
			!strings.Contains(got.Reason, "the architecture of its tier cannot be told") {
			t.Errorf("no architecture: %s to %+v (%s); want %s and no tier", got.Recommendation, got.Recommended,
				got.Reason, tc.want)
		}
	}

	// A smaller tier the job would overfill is no candidate: 40% of 16 GB is 6.4 GB,
	// which needs 9.1 GB to be no more than 70% full, so t2's 8 GB is out; and its
	// CPU, 30% of 4 vCPU, needs 2 vCPU to be no more than 70% busy, which a 1 vCPU
	// tier has not.
	got := rightsizeOne(t, runsOf("test", 40, 0.25, 0.40, 0.40, nil), rightsizeTiers)
	if got.Recommendation != rightsizeSmaller || got.Recommended != nil {
		t.Errorf("a job that would overfill the smaller tier's memory: %s to %+v", got.Recommendation, got.Recommended)
	}
	got = rightsizeOne(t, runsOf("test", 40, 0.30, 0.10, 0.10, nil), []config.Tier{current, rightsizeTier("one", 1, 8*gb)})
	if got.Recommendation != rightsizeSmaller || got.Recommended != nil {
		t.Errorf("a job that would overfill the smaller tier's CPU: %s to %+v", got.Recommendation, got.Recommended)
	}

	// The tier the job runs on is not in the configuration.
	gone := rightsizeOne(t, idle, []config.Tier{rightsizeTier("t2", 2, 8*gb)})
	if gone.Recommendation != rightsizeNone || !strings.Contains(gone.Reason, "tier t4 is not in this configuration") {
		t.Errorf("a tier the configuration lost: %s (%s)", gone.Recommendation, gone.Reason)
	}

	// How many of a tier are kept warm, run at once or reserved, and a larger disk, are
	// no difference of family.
	capacity := rightsizeTier("small", 2, 8*gb)
	capacity.WarmPool, capacity.MaxConcurrent, capacity.Reserved = 3, 5, 1
	withDisk := current
	withDisk.Disk, capacity.Disk = 20*gb, 30*gb
	if got := rightsizeOne(t, idle, []config.Tier{withDisk, capacity}); got.Recommended == nil || got.Recommended.Label != "small" {
		t.Errorf("a tier differing only in capacity and a larger disk was not recommended: %+v (%s)", got.Recommended,
			got.Reason)
	}

	// A move out of a trusted pool is not a change of size.
	if got := rightsizeOne(t, idle, []config.Tier{func() config.Tier {
		tier := rightsizeTier("t4", 4, 16*gb)
		tier.Trust, tier.Workflows = config.WorkloadTrusted, []string{fixedWorkflow}
		return tier
	}(), rightsizeTier("small", 2, 8*gb)}); got.Recommended != nil {
		t.Errorf("a job in a trusted pool was recommended an untrusted tier: %+v", got.Recommended)
	}

	// A trusted tier is a candidate only for a workflow it allows.
	trusted := func(label string, vcpu int, memory config.ByteSize, workflows ...string) config.Tier {
		tier := rightsizeTier(label, vcpu, memory)
		tier.Trust, tier.RunnerGroup, tier.Workflows = config.WorkloadTrusted, "ci", workflows
		return tier
	}
	const allowed = "acme/api/.github/workflows/ci.yml@refs/heads/main"
	if got := rightsizeOne(t, idle, []config.Tier{trusted("t4", 4, 16*gb, allowed),
		trusted("small", 2, 8*gb, "acme/api/.github/workflows/other.yml@refs/heads/main")}); got.Recommended != nil {
		t.Errorf("a trusted tier that does not allow the workflow was recommended: %+v", got.Recommended)
	}
	got = rightsizeOne(t, idle, []config.Tier{trusted("t4", 4, 16*gb, allowed),
		trusted("small", 2, 8*gb, "acme/api/.github/workflows/ci.yml@refs/tags/v1")})
	if got.Recommended == nil || got.Recommended.Label != "small" {
		t.Errorf("a trusted tier allowing the workflow at another ref was not recommended: %+v", got.Recommended)
	}
}

// THE THRESHOLDS ARE STATED IN THE OUTPUT, EVERY NAME IS QUOTED, A FIGURE THE VM GAVE
// SAYS SO, AND THE JSON IS WHAT THE WORKFLOW COMMAND READS: a figure not measured is
// null, and a recommended tier names the runs-on a workflow would carry.
func TestTheRightsizeReportSaysItsThresholdsAndQuotesWhatAWorkflowChose(t *testing.T) {
	t.Parallel()

	jobs := runsOf("test\nrecommend  forged", 40, 0.05, 0.10, 2.0, func(_ int, j *fixedJob) {
		j.usage.unmeasured = "oom,pressure"
	})
	report, err := buildRightsize(t.Context(), fixedAllocator(t, jobs), rightsizeTiers, reportNow.Add(-7*24*time.Hour), reportNow)
	if err != nil {
		t.Fatalf("buildRightsize: %v", err)
	}

	var out bytes.Buffer
	renderRightsize(&out, report)
	for _, want := range []string{
		"No recommendation under 10 measured runs", "low from 10 runs, medium from 30, high from 100",
		"memory reported by the job's own VM at p95 at 90% or more over 10 runs or more",
		"CPU busy at p90 at 85% or more", "CPU pressure at p90 at 25% or more",
		"CPU busy at p90 at 30% or less and memory at p95 at 45% or less", "no more than 70%",
		`"acme/api" "acme/api/.github/workflows/ci.yml" "test\nrecommend  forged"`,
		"t4 (runs-on runs-t4), 4 vCPU, 14.9 GiB",
		"40 measured on this tier; not measured: 0 with no usage report, 0 with no series kept, 0 with a series short",
		"cpu busy     5.0% at p90", "vm memory    10.0% at p95 over 40 runs, as the job's own VM reported it: " + guestBanner,
		"host memory  peak 29.8 GiB at p95 over 40 runs, 200.0% of the job's charged memory, with the page cache",
		"oom kills    not measured", "cpu pressure not measured",
		"smaller: t2 (runs-on runs-t2, 2 vCPU, 7.5 GiB); idle (CPU busy at p90 5%, memory the job's own VM reported " +
			"at p95 10%); confidence medium (40 runs)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\nrecommend  forged") {
		t.Errorf("a job name forged a line:\n%s", out.String())
	}

	out.Reset()
	if err := renderRightsizeJSON(&out, report); err != nil {
		t.Fatalf("renderRightsizeJSON: %v", err)
	}
	var decoded rightsizeJSON
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	if decoded.Schema != rightsizeSchema || len(decoded.Jobs) != 1 || decoded.Thresholds == "" {
		t.Fatalf("decoded = %+v", decoded)
	}
	job := decoded.Jobs[0]
	if job.Repository != "acme/api" || job.Workflow != "acme/api/.github/workflows/ci.yml" || job.CurrentRunsOn != "runs-t4" ||
		job.Recommendation != rightsizeSmaller || job.RecommendedTier != "t2" || job.RecommendedRunsOn != "runs-t2" ||
		job.Confidence != "medium" || job.CPUBusyP90 == nil || job.GuestMemoryP95 == nil || *job.GuestMemoryP95 != 0.1 ||
		job.GuestRuns != 40 {
		t.Errorf("job = %+v", job)
	}
	if job.OOMKills != nil || job.CPUPressureP90 != nil {
		t.Errorf("figures not measured are not null: oom %v, pressure %v", job.OOMKills, job.CPUPressureP90)
	}
	// EACH TIER'S SHAPE WHEN THE REPORT WAS MADE, under the names the workflow command
	// reads, so a tier resized since is refused there.
	for _, want := range []string{
		`"current_tier_vcpu": 4,`, `"current_tier_memory_bytes": 16000000000,`,
		`"recommended_vcpu": 2,`, `"recommended_memory_bytes": 8000000000`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the JSON does not say %s:\n%s", want, out.String())
		}
	}
}

// THROUGH THE COMMAND, which reads the tiers from the configuration.
func TestJobsRightsizeThroughTheCommand(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	seedLedger(t, stateDir)

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"rightsize", "--config", cfg, "--since", "1d"}); err != nil {
			t.Errorf("billet jobs rightsize: %v", err)
		}
	})
	if !strings.Contains(out, "no job started in the window") || !strings.Contains(out, "No recommendation under 10 measured runs") {
		t.Errorf("an empty ledger's report:\n%s", out)
	}

	err := Jobs(t.Context(), processEnv(), []string{"rightsize", "--config", cfg, "--since", "soon"})
	if err == nil || !strings.Contains(err.Error(), `"soon" is neither whole days`) {
		t.Errorf("--since soon = %v, want it refused", err)
	}
}

// LARGER IS LARGER THAN THE SHAPE CHARGED: a job a fallback ran on 16 vCPU and 64 GB
// near its memory is never moved to an 8 vCPU, 32 GB tier for being larger than the
// 4 vCPU one it asked for.
func TestALargerTierIsLargerThanTheShapeCharged(t *testing.T) {
	t.Parallel()

	got := rightsizeOne(t, runsOf("test", 40, 0.50, 0.95, 0.50, func(_ int, j *fixedJob) {
		j.vcpu, j.memory = 16, int64(64*gb)
	}), rightsizeTiers)
	if got.Recommendation != rightsizeLarger || got.Recommended != nil {
		t.Errorf("%s to %+v (%s); want larger, with no tier above 64 GB to name", got.Recommendation, got.Recommended,
			got.Reason)
	}
}

// A HOST PEAK ABOVE THE IDLE LINE SAYS NOTHING OF ROOM, with no word from the VM, so a
// job that is not idle is kept without the claim.
func TestAHighHostPeakClaimsNoRoom(t *testing.T) {
	t.Parallel()

	got := rightsizeOne(t, runsOf("test", 40, 0.50, 0, 2.80, func(_ int, j *fixedJob) { j.guest = nil }), rightsizeTiers)
	if got.Recommendation != rightsizeKeep || strings.Contains(got.Reason, "short of room") ||
		!strings.Contains(got.Reason, "its memory could not be told") {
		t.Errorf("%s (%s); want keep, saying memory could not be told", got.Recommendation, got.Reason)
	}
}
