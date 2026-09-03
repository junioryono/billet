package initconfig

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// tartParams is a generation for a Mac of the reference shape.
//
// The ceiling it produces (10 vCPU, 28GiB from 12/32) is the one every size
// assertion below is written against, so a change to the headroom rule shows up
// here as a failing size rather than as a silently different catalogue.
func tartParams(guests ...config.GuestOS) Params {
	return Params{
		Org:      "acme",
		Provider: config.ProviderTart,
		// THE PLATFORM THIS GENERATION IS FOR, named rather than inherited. A tart
		// config renders a Mac's shape and Generate refuses any other, so on Linux
		// CI an unset GOOS would make every test here assert that refusal instead of
		// the thing it was written for.
		//
		// AND THE SERVICE PROFILE WITH IT, because that is the only shape a
		// cross-platform generation can honour: the user-session paths come from the
		// RUNNING process's user config directory, so a darwin generation written on
		// Linux would carry Linux paths. Generate refuses that pair rather than
		// rendering it.
		GOOS:    "darwin",
		Profile: ProfileLocalService,
		VCPU:    12,
		Memory:  32 * config.GiB,
		Tart: &TartParams{
			GuestOS:  guests,
			NodeName: "mac-mini-1",
		},
	}
}

// generateTart renders and then LOADS the result, returning both.
//
// Generate already round-trips through config.Parse, so loading again is not a
// second opinion on whether it validates; it is how these tests read the tiers
// billet DECIDED rather than re-deriving them from the text — a test that parsed
// the YAML itself would be asserting against its own reader.
func generateTart(t *testing.T, p Params) (string, *config.Config) {
	t.Helper()

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	return body, loadGenerated(t, body)
}

// THE DEFAULT IMAGES ARE PINNED TO THE ONES THAT WERE MEASURED.
//
// A change-detector on purpose, and the same shape DefaultMacOSVMLimit's test
// has: these are not preferences, they are the two references billet has run a
// real job in — an Xcode build in the first, a docker build and a service
// container in the second. Everything else about the generation is asserted
// THROUGH these constants, so without this a typo in one of them would leave the
// whole suite green while every generated config named an image that does not
// resolve.
//
// Changing one means a new image was measured. Say so, and change this line.
func TestTheDefaultTartImagesAreTheOnesThatWereMeasured(t *testing.T) {
	t.Parallel()

	if DefaultTartMacOSImage != "ghcr.io/cirruslabs/macos-tahoe-xcode:latest" {
		t.Errorf("the default macOS image is %q; the measured one is the Xcode guest a real "+
			"iOS build ran in", DefaultTartMacOSImage)
	}

	if DefaultTartLinuxImage != "ghcr.io/cirruslabs/ubuntu-runner-arm64:latest" {
		t.Errorf("the default Linux image is %q; the measured one is the -runner- guest, and "+
			"the plain `ubuntu` image carries neither the Actions runner nor Docker",
			DefaultTartLinuxImage)
	}
}

// WHICH PLATFORM A TART GENERATION IS FOR DEPENDS ON ITS SHAPE.
//
// The SERVICE shape's paths are constants per platform, so GOOS decides and the
// file can be written from anywhere. The USER-SESSION shape's come from
// os.UserConfigDir() of the process running this, so it describes the running
// machine whatever GOOS says — naming darwin there does not move the paths, it
// only makes the file claim something it is not.
//
// Written absolutely rather than relative to the runner: a service generation for
// linux is nonsense on any machine, and a user-session one is legal exactly where
// the machine is a Mac. Both halves are deterministic on both platforms.
func TestGenerateTartRefusesAPlatformItsPathsWouldNotBeFor(t *testing.T) {
	t.Parallel()

	t.Run("a service shape for another platform is refused", func(t *testing.T) {
		t.Parallel()

		p := tartParams(config.GuestMacOS)
		p.GOOS = "linux"

		body, _, err := Generate(p)
		if err == nil {
			t.Fatalf("a tart config was generated for a Linux service shape\n\n%s", body)
		}
		if !strings.Contains(err.Error(), string(ProfileLocalService)) {
			t.Errorf("Generate = %v, want it to name the shape whose paths decided", err)
		}
	})

	t.Run("a service shape for a Mac is not", func(t *testing.T) {
		t.Parallel()

		if _, _, err := Generate(tartParams(config.GuestMacOS)); err != nil {
			t.Errorf("a darwin service generation was refused: %v", err)
		}
	})

	t.Run("a user-session shape follows the machine, not GOOS", func(t *testing.T) {
		t.Parallel()

		// GOOS deliberately names the OTHER platform, to prove it decides nothing
		// here: the paths are this machine's either way.
		p := tartParams(config.GuestMacOS)
		p.Profile = ProfileLocal
		p.GOOS = "linux"
		if serviceOS != "darwin" {
			p.GOOS = "darwin"
		}

		_, _, err := Generate(p)

		if serviceOS == "darwin" {
			if err != nil {
				t.Errorf("a user-session generation on a Mac was refused: %v", err)
			}

			return
		}

		if err == nil {
			t.Fatalf("a user-session tart config was generated on %s, whose user config "+
				"directory is the one it would have carried", serviceOS)
		}
		if !strings.Contains(err.Error(), "only be produced on the Mac itself") {
			t.Errorf("Generate = %v, want it to say the shape cannot be written elsewhere", err)
		}
	})
}

// A MAC GENERATION RUNS UNTRUSTED WORK, AND SAYS HOW IT CONFINES IT.
//
// The trust follows the backend's isolation, as it does for firecracker and ec2:
// a tart guest has its own kernel. What a tart guest does NOT have is its own
// network — tart's default is shared NAT — so the isolation mechanism has to be
// named or the node refuses every untrusted launch.
func TestGenerateTartIsUntrustedAndNamesItsIsolation(t *testing.T) {
	t.Parallel()

	body, cfg := generateTart(t, tartParams(config.GuestMacOS))

	if cfg.Node.Tart == nil || cfg.Node.Tart.UntrustedIsolation != config.IsolationSoftnet {
		t.Fatalf("node.tart.untrusted_isolation = %v, want softnet\n\n%s", cfg.Node.Tart, body)
	}

	for i := range cfg.Tiers {
		if cfg.Tiers[i].Trust != config.WorkloadUntrusted {
			t.Errorf("tier %q is %q, want untrusted", cfg.Tiers[i].Label, cfg.Tiers[i].Trust)
		}
	}
}

// AND A TRUSTED GENERATION DOES NOT NAME IT, which is the opposite of what the
// firecracker generator does with its untrusted bridge.
//
// An unused bridge costs nothing. An unused untrusted_isolation costs the whole
// preflight: `billet check` is FATAL on a node that offers to run untrusted work
// on a host whose softnet carries no setuid-root grant. Writing it into a config
// with no untrusted tier would fail the check over a mechanism nothing uses — on
// a host that has done nothing wrong.
func TestGenerateTartTrustedDoesNotPromiseIsolationNothingUses(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestMacOS)
	p.RunnerGroup = "Billet trusted"
	p.Workflows = []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"}

	body, cfg := generateTart(t, p)

	if cfg.Node.Tart != nil && cfg.Node.Tart.UntrustedIsolation != "" {
		t.Fatalf("a trusted generation named untrusted_isolation %q, which makes `billet check` "+
			"fail on a host with no softnet grant\n\n%s", cfg.Node.Tart.UntrustedIsolation, body)
	}

	// ASSERTED AGAINST THE LOADED CONFIG, not the text: the trusted form still
	// EXPLAINS softnet in a comment, so grepping the body for "untrusted_isolation"
	// would fail against a config that is correct.
	for i := range cfg.Tiers {
		if cfg.Tiers[i].Trust != config.WorkloadTrusted {
			t.Errorf("tier %q is %q, want trusted", cfg.Tiers[i].Label, cfg.Tiers[i].Trust)
		}
	}
}

// A macOS TIER PINS THIS HOST, AND THE PIN NAMES WHAT THE NODE CALLS ITSELF.
//
// Apple counts its two-guest limit per physical machine, so config validation
// requires the pin. A pin naming anything other than node.name would validate
// and then never be placed, because no registered host answers to it.
func TestGenerateTartPinsTheMacOSTierToThisNode(t *testing.T) {
	t.Parallel()

	_, cfg := generateTart(t, tartParams(config.GuestMacOS))

	var macOS int

	for i := range cfg.Tiers {
		if cfg.Tiers[i].GuestOS != config.GuestMacOS {
			continue
		}

		macOS++

		if cfg.Tiers[i].Node != cfg.Node.Name {
			t.Errorf("tier %q pins %q, but this node is %q",
				cfg.Tiers[i].Label, cfg.Tiers[i].Node, cfg.Node.Name)
		}
	}

	// EXACTLY ONE, and this is a load-time rule rather than taste: an unset
	// max_concurrent on a macOS tier inherits the HOST's limit, so two generated
	// macOS tiers would sum to 4 against a limit of 2 and validateMacOSHostLimits
	// would refuse the file. Generate's own config.Parse round trip would catch
	// that, but the diagnostic would blame the config rather than the ladder.
	if macOS != 1 {
		t.Errorf("the generation wrote %d macOS tiers; more than one cannot fit Apple's "+
			"per-machine limit once each inherits it", macOS)
	}
}

// A LINUX-ONLY GENERATION PINS NOTHING AND NEEDS NO NAME.
//
// Refusing a Mac whose hostname is not a legal node name would be a refusal of a
// config that never needed the name — the pin is a macOS requirement, not a tart
// one.
func TestGenerateTartLinuxOnlyNeedsNoNodeName(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestLinux)
	p.Tart.NodeName = ""

	body, cfg := generateTart(t, p)

	if strings.Contains(body, "\n  name:") {
		t.Errorf("a linux-only generation wrote node.name, which nothing in it needs\n\n%s", body)
	}

	for i := range cfg.Tiers {
		if cfg.Tiers[i].GuestOS != config.GuestLinux {
			t.Errorf("tier %q is %q, want linux only", cfg.Tiers[i].Label, cfg.Tiers[i].GuestOS)
		}
		if cfg.Tiers[i].Node != "" {
			t.Errorf("tier %q pinned %q; only a macOS tier has to",
				cfg.Tiers[i].Label, cfg.Tiers[i].Node)
		}
	}
}

// THE NODE-NAME COMMENT EXPLAINS ONLY WHAT THIS FILE ACTUALLY REQUIRES.
//
// Apple's limit is why a macOS TIER must name a host; it is not why the key
// exists. A linux-only generation carrying that sentence would explain a
// requirement its own file does not have, and a comment whose stated reason is
// untrue is worse than no comment.
func TestGenerateTartExplainsThePinOnlyWhereSomethingPins(t *testing.T) {
	t.Parallel()

	const pin = "two-guest limit"

	// THE NODE BLOCK, NOT THE WHOLE DOCUMENT. Every macOS TIER carries the same
	// phrase for its own `node:` pin, so searching the file finds it whether or not
	// the node comment says anything — the assertion passed with the clause under
	// test deleted.
	macOS, _ := generateTart(t, tartParams(config.GuestMacOS))
	if !strings.Contains(nodeBlockOf(t, macOS), pin) {
		t.Errorf("a generation with a macOS tier does not explain why node.name is pinned:"+
			"\n%s", macOS)
	}

	linux, _ := generateTart(t, tartParams(config.GuestLinux))
	if strings.Contains(nodeBlockOf(t, linux), pin) {
		t.Errorf("a linux-only generation explains Apple's pin, which nothing in it needs:"+
			"\n%s", linux)
	}

	// AND IT STILL NAMES THE HOST, which is the half that is not guest-specific.
	if !strings.Contains(linux, "\n  name: mac-mini-1\n") {
		t.Errorf("a linux-only generation left out the node name it was given:\n%s", linux)
	}
}

// nodeBlockOf is the generated text from `node:` up to the key that follows the
// name, which is where the node-name comment lives and the tiers do not.
func nodeBlockOf(t *testing.T, body string) string {
	t.Helper()

	start := strings.Index(body, "\nnode:\n")
	if start < 0 {
		t.Fatalf("the generation has no node block:\n%s", body)
	}

	rest := body[start+len("\nnode:\n"):]

	end := strings.Index(rest, "\n  server_addr:")
	if end < 0 {
		t.Fatalf("the node block has no server_addr to end at:\n%s", body)
	}

	return rest[:end]
}

// THE CATALOGUE FITS ITS OWN CEILING, ONE JOB OF EVERY TIER AT ONCE.
//
// Every tier is a scale set and escrows one discovery slot BEFORE it advertises,
// so a catalogue whose tiers individually fit but collectively do not leaves
// every one of them advertising zero and every job queued forever against a
// control plane reporting itself healthy. The macOS tier is fitted first and the
// Linux ladder takes what is left, so this is the assertion that the second half
// consults the first.
func TestGenerateTartCatalogueFitsTheCeilingAllAtOnce(t *testing.T) {
	t.Parallel()

	_, cfg := generateTart(t, tartParams(config.GuestMacOS, config.GuestLinux))

	var (
		vcpu   int
		memory config.ByteSize
	)

	for i := range cfg.Tiers {
		vcpu += cfg.Tiers[i].VCPU
		memory += cfg.Tiers[i].Memory
	}

	if vcpu > cfg.Server.MaxVCPU || memory > cfg.Server.MaxMemory {
		t.Errorf("one job of every tier needs %d vCPU and %s, over a ceiling of %d and %s",
			vcpu, memory, cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	if len(cfg.Tiers) < 2 {
		t.Fatalf("asking for both guest kinds produced %d tier(s); each was named explicitly",
			len(cfg.Tiers))
	}
}

// THE macOS TIER DOES NOT EAT A BUDGET THAT HAD ROOM FOR BOTH.
//
// It is fitted first, and greedily: the larger shape when it fits. At a ceiling
// of exactly 4 vCPU and 16GiB that took everything, left nothing for the Linux
// tier the same command had asked for, and produced a refusal caused by the ORDER
// of the fit rather than by the machine. Reserving the smallest remaining rung is
// what makes the greedy choice safe.
//
// 6 vCPU and 20GiB is the machine whose ceiling is exactly that pair.
func TestGenerateTartLeavesRoomForTheGuestKindItHasNotFittedYet(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestMacOS, config.GuestLinux)
	p.VCPU, p.Memory = 6, 20*config.GiB

	body, cfg := generateTart(t, p)

	if cfg.Server.MaxVCPU != 4 || cfg.Server.MaxMemory != 16*config.GiB {
		t.Fatalf("this test is written against a ceiling of 4 vCPU and 16GiB; the headroom "+
			"rule now produces %d and %s", cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	seen := map[config.GuestOS]bool{}
	for i := range cfg.Tiers {
		seen[cfg.Tiers[i].GuestOS] = true
	}

	if !seen[config.GuestMacOS] || !seen[config.GuestLinux] {
		t.Errorf("both guest kinds fit and only %v were written\n\n%s", seen, body)
	}
}

// AND A RESERVATION IT CANNOT MEET IS DROPPED, NOT ESCALATED INTO A REFUSAL.
//
// Where no macOS shape leaves the smallest Linux rung standing, the fit falls
// back to the largest shape that merely fits — and the smaller a macOS tier is,
// the more the Linux side has to work with. At a ceiling of 3 vCPU and 20GiB
// that is the difference between two tiers and none: holding the reservation
// refuses, dropping it and taking the 2-vCPU shape leaves 1 vCPU the Linux
// fallback can still build a tier out of.
//
// 5 vCPU and 24GiB is the machine whose ceiling is that pair.
func TestGenerateTartFallsBackToASmallerMacOSShapeRatherThanRefusing(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestMacOS, config.GuestLinux)
	p.VCPU, p.Memory = 5, 24*config.GiB

	body, cfg := generateTart(t, p)

	if cfg.Server.MaxVCPU != 3 || cfg.Server.MaxMemory != 20*config.GiB {
		t.Fatalf("this test is written against a ceiling of 3 vCPU and 20GiB; the headroom "+
			"rule now produces %d and %s", cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	seen := map[config.GuestOS]bool{}
	for i := range cfg.Tiers {
		seen[cfg.Tiers[i].GuestOS] = true

		// The larger shape cannot fit here at all, so a macOS tier of 3 vCPU is
		// the fit having given up on the Linux tier rather than having taken the
		// next shape down.
		if cfg.Tiers[i].GuestOS == config.GuestMacOS && cfg.Tiers[i].VCPU != 2 {
			t.Errorf("the macOS tier is %d vCPU; the 2-vCPU shape is what leaves the Linux "+
				"tier anything\n\n%s", cfg.Tiers[i].VCPU, body)
		}
	}

	if !seen[config.GuestMacOS] || !seen[config.GuestLinux] {
		t.Errorf("both guest kinds fit and only %v were written\n\n%s", seen, body)
	}
}

// AND THE RESERVATION CHANGES NOTHING WHERE THERE IS ROOM.
//
// The other half: on a machine that can afford the larger macOS shape beside a
// Linux rung, holding room back must not shrink the macOS tier. A reservation
// that always applied would quietly halve every Xcode guest on a big Mac.
func TestGenerateTartStillTakesTheLargerMacOSShapeWhenBothFit(t *testing.T) {
	t.Parallel()

	_, cfg := generateTart(t, tartParams(config.GuestMacOS, config.GuestLinux))

	// FOUND, not merely not-wrong: a loop that skips every tier passes an
	// assertion about the one it was looking for.
	found := false

	for i := range cfg.Tiers {
		if cfg.Tiers[i].GuestOS != config.GuestMacOS {
			continue
		}

		found = true

		if cfg.Tiers[i].VCPU != 4 {
			t.Errorf("the macOS tier is %d vCPU on a 10 vCPU / 28GiB ceiling, where the "+
				"larger shape fits beside a Linux rung", cfg.Tiers[i].VCPU)
		}
	}

	if !found {
		t.Error("no macOS tier was generated, so nothing above was checked")
	}
}

// THE ORDER THE GUEST KINDS ARRIVE IN DOES NOT CHANGE THE BYTES.
//
// `billet init` re-run against its own output converges only when the file it
// would write is byte-identical, so a catalogue that followed the flag order
// would make convergence depend on how the operator typed the command — and the
// re-run would write a .new file beside a config nothing had changed.
func TestGenerateTartIsIndependentOfGuestOrder(t *testing.T) {
	t.Parallel()

	first, _, err := Generate(tartParams(config.GuestMacOS, config.GuestLinux))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	second, _, err := Generate(tartParams(config.GuestLinux, config.GuestMacOS))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if first != second {
		t.Error("the same two guest kinds in the other order generated different bytes")
	}
}

// A MAC TOO SMALL FOR A macOS GUEST IS REFUSED, WITH THE HYPERVISOR'S REASON.
//
// Below Apple's floor there is no smaller guest to fall back to — the VM does
// not start at all — so generating one would be a config that loads, advertises
// capacity and fails every job with LessThanMinimalResourcesError.
func TestGenerateTartRefusesAMacTooSmallForAMacOSGuest(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestMacOS)
	p.VCPU, p.Memory = 2, 6*config.GiB

	_, _, err := Generate(p)
	if err == nil {
		t.Fatal("a macOS tier was generated for a ceiling that cannot hold one")
	}

	for _, want := range []string{config.MinMacOSGuestMemory.String(), "--guest-os linux"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Generate = %v, want the refusal to mention %q", err, want)
		}
	}
}

// AND A REQUESTED GUEST KIND IS NEVER SILENTLY DROPPED.
//
// tiers() skips a candidate that does not fit, because the ladder was billet's
// idea and a shorter one still serves. Here each guest kind was asked for BY
// NAME, so a config with no tier for one of them answers a different question
// than the one it was put.
func TestGenerateTartRefusesRatherThanDroppingARequestedGuest(t *testing.T) {
	t.Parallel()

	// Enough for the macOS tier and nothing after it.
	p := tartParams(config.GuestMacOS, config.GuestLinux)
	p.VCPU, p.Memory = 4, 12*config.GiB

	body, _, err := Generate(p)
	if err == nil {
		t.Fatalf("the Linux tier was dropped instead of refused\n\n%s", body)
	}

	if !strings.Contains(err.Error(), "arm64 Linux tier") {
		t.Errorf("Generate = %v, want the refusal to name the guest kind that did not fit", err)
	}
}

// A MACHINE TOO SMALL FOR ANY GUEST IS NOT TOLD TO ASK FOR A BIGGER ONE.
//
// The Linux refusal used to be one sentence, and it was untrue whenever nothing
// had been fitted above it: it spoke of "the tiers above" where there were none,
// and offered `--guest-os macos` — a guest with a 4GiB floor — to an operator
// whose ceiling could not hold 1GiB.
func TestGenerateTartTellsATinyMachineTheTruth(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestLinux)
	p.Tart.NodeName = ""
	p.VCPU, p.Memory = 1, config.GiB

	_, _, err := Generate(p)
	if err == nil {
		t.Fatal("a tier was generated for a ceiling that can hold nothing")
	}

	if strings.Contains(err.Error(), "--guest-os macos") {
		t.Errorf("a machine too small for a 1GiB guest was offered a 4GiB one: %v", err)
	}
	if strings.Contains(err.Error(), "tiers above") {
		t.Errorf("the refusal speaks of tiers above it, and there are none: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing to contribute") {
		t.Errorf("the refusal does not say what is actually wrong: %v", err)
	}
}

// AND THE macOS REFUSAL DOES NOT OFFER A GUEST THAT ALSO WILL NOT FIT.
//
// The sibling of the Linux diagnostic above, and it had the same defect: "generate
// an arm64 Linux config instead" is a real remedy on a machine that can hold a
// 1GiB guest, and a loop on one that cannot — which is most of the range below
// Apple's 4GiB floor.
func TestGenerateTartOffersALinuxGuestOnlyWhereOneWouldFit(t *testing.T) {
	t.Parallel()

	t.Run("a machine that could hold a linux guest is offered one", func(t *testing.T) {
		t.Parallel()

		p := tartParams(config.GuestMacOS)
		p.VCPU, p.Memory = 3, 7*config.GiB

		_, _, err := Generate(p)
		if err == nil {
			t.Fatal("a macOS tier was generated under Apple's floor")
		}
		if !strings.Contains(err.Error(), "--guest-os linux") {
			t.Errorf("Generate = %v, want the remedy that would actually work", err)
		}
	})

	t.Run("a machine that could hold neither is not", func(t *testing.T) {
		t.Parallel()

		p := tartParams(config.GuestMacOS)
		p.VCPU, p.Memory = 1, config.GiB

		_, _, err := Generate(p)
		if err == nil {
			t.Fatal("a macOS tier was generated for a ceiling that holds nothing")
		}
		if strings.Contains(err.Error(), "--guest-os linux") {
			t.Errorf("a machine too small for a 1GiB guest was offered one: %v", err)
		}
		if !strings.Contains(err.Error(), "nothing to contribute") {
			t.Errorf("the refusal does not say what is actually wrong: %v", err)
		}
	})
}

// THE IMAGES ARE THE MEASURED ONES, per guest kind.
func TestGenerateTartNamesAnImageForEachGuestKind(t *testing.T) {
	t.Parallel()

	_, cfg := generateTart(t, tartParams(config.GuestMacOS, config.GuestLinux))

	want := map[config.GuestOS]string{
		config.GuestMacOS: DefaultTartMacOSImage,
		config.GuestLinux: DefaultTartLinuxImage,
	}

	for i := range cfg.Tiers {
		t.Run(cfg.Tiers[i].Label, func(t *testing.T) {
			t.Parallel()

			if got := cfg.Tiers[i].ImageFor(config.ProviderTart); got != want[cfg.Tiers[i].GuestOS] {
				t.Errorf("image = %q, want %q", got, want[cfg.Tiers[i].GuestOS])
			}
		})
	}
}

// AN OVERRIDE REACHES THE TIER IT IS FOR AND NOT THE OTHER ONE.
func TestGenerateTartImageOverridesAreNotShared(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestMacOS, config.GuestLinux)
	p.Tart.MacOSImage = "ghcr.io/acme/macos:pinned"

	_, cfg := generateTart(t, p)

	for i := range cfg.Tiers {
		got := cfg.Tiers[i].ImageFor(config.ProviderTart)
		switch cfg.Tiers[i].GuestOS {
		case config.GuestMacOS:
			if got != "ghcr.io/acme/macos:pinned" {
				t.Errorf("macOS tier image = %q, want the override", got)
			}
		case config.GuestLinux:
			if got != DefaultTartLinuxImage {
				t.Errorf("linux tier image = %q, want the untouched default", got)
			}
		case config.GuestWindows:
			t.Errorf("tier %q is windows, which this backend cannot run", cfg.Tiers[i].Label)
		}
	}
}

// Generate IS EXPORTED, so every rule cmd/billet applies is re-applied here.
//
// The same reason alloc.New re-checks a catalogue it did not load: a rule
// enforced only in the CLI has a second entry point that does not enforce it,
// and internal/e2e reaches this one directly.
func TestGenerateTartRefusesWhatTheFlagsWouldHaveRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*Params)
		want   string
	}{
		{
			name:   "no guest kind",
			mutate: func(p *Params) { p.Tart.GuestOS = nil },
			want:   "--guest-os is required",
		},
		{
			name:   "a guest kind billet does not know",
			mutate: func(p *Params) { p.Tart.GuestOS = []config.GuestOS{"freebsd"} },
			want:   "not one of macos, linux",
		},
		{
			// Named separately from a typo, because Windows is a guest OS billet
			// understands and this backend cannot run.
			name:   "windows",
			mutate: func(p *Params) { p.Tart.GuestOS = []config.GuestOS{config.GuestWindows} },
			want:   "cannot run Windows",
		},
		{
			name: "the same guest kind twice",
			mutate: func(p *Params) {
				p.Tart.GuestOS = []config.GuestOS{config.GuestLinux, config.GuestLinux}
			},
			want: "listed twice",
		},
		{
			name:   "no node name for a macOS tier",
			mutate: func(p *Params) { p.Tart.NodeName = "" },
			want:   "--node-name is required",
		},
		{
			name:   "a node name nothing could authorise",
			mutate: func(p *Params) { p.Tart.NodeName = "Junior's Mac.local" },
			want:   "--node-name",
		},
		{
			// One image cannot be two guest kinds' artifacts, so the shared field
			// is refused rather than applied to whichever tier comes first.
			name:   "the shared image field",
			mutate: func(p *Params) { p.Image = "ghcr.io/acme/whatever:latest" },
			want:   "--macos-image and --linux-image",
		},
		{
			// tart reads a leading dash as a flag, so this is option injection
			// with a working-looking config.
			name:   "an image tart would read as a flag",
			mutate: func(p *Params) { p.Tart.LinuxImage = "--net-softnet" },
			want:   "reads as a flag",
		},
		{
			name:   "an image with whitespace in it",
			mutate: func(p *Params) { p.Tart.MacOSImage = "ghcr.io/acme/a b:latest" },
			want:   "whitespace or a control character",
		},
		{
			// A typo, not a no-op: ignoring it lets an operator believe they
			// pinned an artifact while the file names the default.
			name: "an image for a guest kind that was not asked for",
			mutate: func(p *Params) {
				p.Tart.GuestOS = []config.GuestOS{config.GuestLinux}
				p.Tart.MacOSImage = "ghcr.io/acme/macos:pinned"
			},
			want: "no macOS tier",
		},
		{
			name: "a linux image with no linux tier",
			mutate: func(p *Params) {
				p.Tart.GuestOS = []config.GuestOS{config.GuestMacOS}
				p.Tart.LinuxImage = "ghcr.io/acme/linux:pinned"
			},
			want: "no Linux tier",
		},
		{
			name:   "no tart inputs at all",
			mutate: func(p *Params) { p.Tart = nil },
			want:   "--guest-os is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := tartParams(config.GuestMacOS, config.GuestLinux)
			tc.mutate(&p)

			body, _, err := Generate(p)
			if err == nil {
				t.Fatalf("Generate accepted it\n\n%s", body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Generate = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A NODE NAME THAT WAS GIVEN IS WRITTEN, WHICHEVER GUEST KINDS WERE CHOSEN.
//
// It used to be rendered only when a macOS tier pinned it, so a linux-only
// generation validated the name and then dropped it — the "looks configured, is
// inert" shape. The operator believes the host is called what they said, the node
// takes its hostname instead, and a macOS tier they add by hand pins something
// nothing answers to.
func TestGenerateTartWritesANodeNameItWasGiven(t *testing.T) {
	t.Parallel()

	_, cfg := generateTart(t, tartParams(config.GuestLinux))

	if cfg.Node.Name != "mac-mini-1" {
		t.Errorf("node.name = %q, want the name the generation was given", cfg.Node.Name)
	}
}

// NORMALIZATION DOES NOT REACH BACK INTO THE CALLER'S BLOCK.
//
// Generate takes Params by value and TartParams is behind a POINTER, so filling
// in a default image or canonicalising the guest order would mutate the caller's
// struct — the rule Generate already states for p.Workflows. cmd/billet reads
// those fields afterwards to decide whether an image is billet's or the
// operator's, and would be told billet's.
func TestGenerateTartLeavesTheCallersInputsAlone(t *testing.T) {
	t.Parallel()

	p := tartParams(config.GuestLinux, config.GuestMacOS)

	if _, _, err := Generate(p); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if p.Tart.MacOSImage != "" || p.Tart.LinuxImage != "" {
		t.Errorf("Generate filled in the caller's images: %q and %q",
			p.Tart.MacOSImage, p.Tart.LinuxImage)
	}

	if len(p.Tart.GuestOS) != 2 || p.Tart.GuestOS[0] != config.GuestLinux {
		t.Errorf("Generate reordered the caller's guest kinds: %v", p.Tart.GuestOS)
	}
}

// A BACKEND BLOCK SET FOR THE WRONG PROVIDER IS REFUSED, NOT DISCARDED.
//
// The CLI refuses these flags by name, and Generate is exported — internal/e2e
// reaches it directly. Until this, a caller passing docker with a tart block got
// a docker config with every tart value silently thrown away, which is two entry
// points enforcing different contracts. Both blocks, because ec2 had the same
// gap and fixing one of two identical writers leaves a second that does not.
func TestGenerateRefusesABackendBlockThatBelongsToAnotherProvider(t *testing.T) {
	t.Parallel()

	t.Run("tart inputs on a docker config", func(t *testing.T) {
		t.Parallel()

		p := dockerParams()
		p.Tart = &TartParams{GuestOS: []config.GuestOS{config.GuestLinux}}

		_, _, err := Generate(p)
		if err == nil {
			t.Fatal("a docker config was generated with tart inputs, which it discards")
		}
		if !strings.Contains(err.Error(), "tart inputs are set") {
			t.Errorf("Generate = %v, want it to name the discarded block", err)
		}
	})

	t.Run("ec2 inputs on a docker config", func(t *testing.T) {
		t.Parallel()

		p := dockerParams()
		p.EC2 = &EC2Params{Region: "us-west-2"}

		_, _, err := Generate(p)
		if err == nil {
			t.Fatal("a docker config was generated with ec2 inputs, which it discards")
		}
		if !strings.Contains(err.Error(), "ec2 inputs are set") {
			t.Errorf("Generate = %v, want it to name the discarded block", err)
		}
	})
}

// NOTHING A TART GUEST CANNOT HONOUR REACHES THE FILE.
//
// The backend REFUSES a spec that asks for cache volumes, an actions proxy or a
// sized /dev/shm — a launch failure rather than a silently absent cache — and
// node.ceph and node.cache are refused at load. A generation that wrote any of
// them would be a config whose first job fails.
func TestGenerateTartWritesNothingTheBackendRefuses(t *testing.T) {
	t.Parallel()

	body, cfg := generateTart(t, tartParams(config.GuestMacOS, config.GuestLinux))

	if cfg.Node.Ceph != nil || cfg.Node.Cache != nil {
		t.Errorf("the generation carried storage a tart node cannot attach\n\n%s", body)
	}

	for i := range cfg.Tiers {
		t := t
		tier := &cfg.Tiers[i]
		if tier.SHM > 0 {
			t.Errorf("tier %q sizes /dev/shm, which tart cannot configure from the host",
				tier.Label)
		}
		if tier.Intercept {
			t.Errorf("tier %q enables the cache interception a tart guest has no proxy for",
				tier.Label)
		}
	}
}
