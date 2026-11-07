package config

import "testing"

// A FAMILY IS EVERY SETTING BUT A TIER'S NAME, SIZE AND CAPACITY: anything else that
// differs is another kind of tier, and room nothing measures is never made smaller.
// Why a tier is not of the family is said, so a refusal can name it.
func TestATiersFamilyIsEverySettingButItsSize(t *testing.T) {
	t.Parallel()

	const (
		differs = "a setting other than its name, size and capacity differs"
		room    = "it has less disk, shared memory or BuildKit cache mount room"
		itself  = "it is the same tier"
	)
	base := Tier{Label: "small", Target: "default", RunsOn: "runs-small", Provider: ProviderFirecracker,
		GuestOS: GuestLinux, VCPU: 2, Memory: 8 * GiB, Image: "ubuntu-2404-x64@verified", Disk: 20 * GiB}

	for name, tc := range map[string]struct {
		change func(*Tier)
		why    string
	}{
		"a larger size and its own names": {func(t *Tier) { t.Label, t.RunsOn, t.VCPU, t.Memory = "big", "runs-big", 8, 32*GiB }, ""},
		"its own capacity":                {func(t *Tier) { t.Label, t.WarmPool, t.MaxConcurrent, t.Reserved = "big", 2, 4, 1 }, ""},
		"more disk":                       {func(t *Tier) { t.Label, t.Disk = "big", 40*GiB }, ""},
		"less disk":                       {func(t *Tier) { t.Label, t.Disk = "big", 10*GiB }, room},
		"the same default shared memory":  {func(t *Tier) { t.Label, t.SHM = "big", 0 }, ""},
		"a default disk for a set one":    {func(t *Tier) { t.Label, t.Disk = "big", 0 }, room},
		"another image":                   {func(t *Tier) { t.Label, t.Image = "big", "ubuntu-2204-x64@verified" }, differs},
		"another architecture":            {func(t *Tier) { t.Label, t.Arch = "big", ArchARM64 }, differs},
		"another command":                 {func(t *Tier) { t.Label, t.Command = "big", []string{"./x"} }, differs},
		"another target":                  {func(t *Tier) { t.Label, t.Target = "big", "other" }, differs},
		"another runner group":            {func(t *Tier) { t.Label, t.RunnerGroup = "big", "ci" }, differs},
		"itself":                          {func(t *Tier) {}, itself},
	} {
		other := base
		tc.change(&other)
		if got := base.FamilyDifference(other); got != tc.why {
			t.Errorf("%s: FamilyDifference = %q, want %q", name, got, tc.why)
		}
		if got := base.SameFamily(other); got != (tc.why == "") {
			t.Errorf("%s: SameFamily = %t, want %t", name, got, tc.why == "")
		}
	}

	defaulted := base
	defaulted.Disk = 0
	set := defaulted
	set.Label, set.Disk = "big", 40*GiB
	if defaulted.SameFamily(set) {
		t.Error("a set disk counted as no less than a backend's default, which nobody here can say")
	}
	shm := base
	shm.SHM = 2 * GiB
	smaller := shm
	smaller.Label, smaller.SHM = "big", GiB
	if got := shm.FamilyDifference(smaller); got != room {
		t.Errorf("less shared memory: %q, want %q", got, room)
	}
}

// A TRUSTED TIER TAKES ONLY THE WORKFLOWS IT LISTS, its repository as GitHub names it
// (in any case) and its file exactly, at any ref; an untrusted one takes any.
func TestATrustedTierTakesOnlyTheWorkflowsItLists(t *testing.T) {
	t.Parallel()

	trusted := Tier{Trust: WorkloadTrusted, Workflows: []string{"acme/api/.github/workflows/ci.yml@refs/heads/main"}}
	for workflow, want := range map[string]bool{
		"acme/api/.github/workflows/ci.yml":     true,
		"ACME/Api/.github/workflows/ci.yml":     true,
		"acme/api/.github/workflows/CI.yml":     false,
		"acme/web/.github/workflows/ci.yml":     false,
		"acme/api/.github/workflows/deploy.yml": false,
		"acme/api":                              false,
	} {
		if got := trusted.AllowsWorkflow(workflow); got != want {
			t.Errorf("trusted.AllowsWorkflow(%q) = %t, want %t", workflow, got, want)
		}
	}
	if !(Tier{}).AllowsWorkflow("acme/web/.github/workflows/anything.yml") {
		t.Error("an untrusted tier refused a workflow")
	}
}
