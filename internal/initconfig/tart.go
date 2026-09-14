package initconfig

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// The guest images a generated tart config names.
//
// BOTH WERE MEASURED, and that is the whole reason `billet init` can write this
// backend at all. It refused to for a long time on the grounds that there was no
// image it could name — true when the only published Linux guest was
// `ghcr.io/cirruslabs/ubuntu`, which carries neither the Actions runner nor
// Docker and so cannot run a job at all. Naming an image the operator's jobs
// cannot use is the exact trap this generator exists to remove.
const (
	// DefaultTartMacOSImage is the macOS guest a generated macOS tier boots.
	//
	// A real private-repository Xcode job built an iOS target inside a guest
	// billet launched from this image, and the guest was destroyed afterwards
	// (internal/provider/tart/realguest_test.go). It ships the Actions runner in
	// ~/actions-runner, which is where config.RunnerCommandFor(tart) looks — so
	// the tier needs no `command:` of its own.
	//
	// IT NAMES A macOS RELEASE. cirruslabs publishes a repository per release
	// rather than a tag per release, so this constant is what moves when a
	// deployment moves generation, and a tier that names its own image is
	// unaffected. It is ~87GB in the local store against a 140GB virtual disk.
	DefaultTartMacOSImage = "ghcr.io/cirruslabs/macos-tahoe-xcode:latest"

	// DefaultTartLinuxImage is the native arm64 Linux guest a generated linux
	// tier boots.
	//
	// The `-runner-` image rather than the plain `ubuntu` one, and the difference
	// is not cosmetic: the plain image was measured to carry neither the Actions
	// runner nor Docker. This one carries both, which is what lets a Linux tart
	// tier do the two things a macOS guest on Apple's hypervisor cannot — build a
	// container image, and reach a service container. 11.3GB compressed against a
	// 40GB virtual disk.
	DefaultTartLinuxImage = "ghcr.io/cirruslabs/ubuntu-runner-arm64:latest"
)

// TartParams is what an Apple-silicon config needs and billet cannot detect.
type TartParams struct {
	// GuestOS are the guest kinds this Mac serves. Rendered in a CANONICAL order
	// (macOS first) rather than the order they arrived, so two spellings of the
	// same request generate identical bytes — `billet init` re-run against its own
	// output converges on a byte comparison, and an order-sensitive catalogue
	// would make that depend on how the flags were typed.
	GuestOS []config.GuestOS

	// NodeName is what this Mac calls itself in the deployment.
	//
	// REQUIRED when GuestOS includes macOS, because a macOS tier must pin a host —
	// Apple's limit is two concurrent guests per PHYSICAL machine, so billet has
	// to know which machine to count a guest against — and a pin can only name
	// what the node calls itself. Empty is legal for a linux-only generation,
	// where the node takes its hostname like any other host-run backend.
	NodeName string

	// MacOSImage and LinuxImage override the measured defaults above.
	//
	// TWO FIELDS RATHER THAN Params.Image, because one image is not a coherent
	// idea for a backend that boots two operating systems: a macOS generation and
	// an arm64 Linux generation name different artifacts and no single string can
	// be both. Same reason a multi-provider tier writes launch.<provider>.image
	// instead of image.
	MacOSImage, LinuxImage string
}

// tartGuestOrder is the canonical rendering order. See GuestOS.
var tartGuestOrder = []config.GuestOS{config.GuestMacOS, config.GuestLinux}

// errTartNoGuest is what a tart generation naming no guest kind gets.
var errTartNoGuest = errors.New(
	"--guest-os is required at least once for tart: a Mac can serve macOS guests, native " +
		"arm64 Linux guests, or both, and which one decides the image, the tier's licence " +
		"limit and whether it has to pin this host — so billet will not choose it")

// tartTargetPlatform is the platform a tart generation would actually be for,
// which is not always the one Params names.
//
// THE SERVICE SHAPE IS FOR GOOS; THE USER-SESSION SHAPE IS FOR THIS MACHINE. The
// service paths are constants per platform, so that generation can be written
// from anywhere and GOOS decides. The user-session ones come from
// os.UserConfigDir() of the process running this, so they describe the running
// machine whatever GOOS says — naming another platform there does not move them,
// it just makes the file claim something it is not.
func tartTargetPlatform(p Params) string {
	if p.Profile == ProfileLocalService {
		return p.platform()
	}

	return serviceOS
}

// refuseTartOffApplePlatform stops a tart generation from describing a machine
// that cannot run the backend it names.
//
// SPLIT FROM THE CLI'S REFUSAL, and the split is what each layer can know. This
// one is about the platform the GENERATION IS FOR. cmd/billet's is about the
// platform the COMMAND IS ON: it additionally refuses a non-arm64 Mac and
// explains that the ceiling was measured there and the node name taken from that
// hostname, neither of which Params can express.
//
// Exported callers reach Generate directly, so the half that is expressible here
// lives here.
func refuseTartOffApplePlatform(p Params) error {
	target := tartTargetPlatform(p)
	if target == "darwin" {
		return nil
	}

	// WHICH FACT DECIDED, because the two remedies differ: a service generation
	// for the wrong platform is fixed by naming darwin, and a user-session one can
	// only be produced on the Mac itself.
	why := fmt.Sprintf("this %s generation is written from %s, and its state and key paths "+
		"come from THIS machine rather than from any target — so it can only be produced on "+
		"the Mac itself", ProfileLocal, target)
	if p.Profile == ProfileLocalService {
		why = fmt.Sprintf("this %s generation is for %s, so the state, key and lock paths it "+
			"would write are that platform's rather than the /usr/local ones a Mac's launch "+
			"agents read", ProfileLocalService, target)
	}

	return fmt.Errorf("a tart config is for an Apple-silicon Mac, and %s. Nothing on %s can run "+
		"macOS or arm64 Linux guests through Apple's Virtualization.framework", why, target)
}

// CheckTartGuestOS validates the requested guest kinds and returns them in the
// canonical order.
//
// EXPORTED SO ONE RULE SERVES TWO CALLERS. cmd/billet must know the request is
// meaningful BEFORE it reads anything off the machine: it resolves this host's
// name from the hostname, and doing that first meant `--guest-os typo` on a Mac
// whose hostname is not a legal node name reported the node name instead of the
// typo — the same invalid input getting a different error depending on an
// unrelated property of the machine. Generate re-applies it because it is
// reachable without the CLI, and a second copy of the rule is how the two grow
// apart.
//
// CANONICAL ORDER (macOS first) rather than the order they arrived, so two
// spellings of one request generate identical bytes: `billet init` re-run against
// its own output converges on a byte comparison.
func CheckTartGuestOS(guests []config.GuestOS) ([]config.GuestOS, error) {
	seen := make(map[config.GuestOS]bool, len(guests))

	for _, guest := range guests {
		trimmed := config.GuestOS(strings.TrimSpace(string(guest)))
		switch trimmed {
		case config.GuestMacOS, config.GuestLinux:
		case config.GuestWindows:
			// Named separately because it is a real guest OS billet understands
			// and this backend cannot run — "not one of macos, linux" would read
			// as a typo.
			return nil, errors.New("--guest-os windows: the tart provider runs macOS and " +
				"Linux guests through Apple's Virtualization.framework and cannot run Windows")
		default:
			return nil, fmt.Errorf("--guest-os: %q is not one of macos, linux", string(guest))
		}

		if seen[trimmed] {
			return nil, fmt.Errorf("--guest-os %s is listed twice", trimmed)
		}

		seen[trimmed] = true
	}

	if len(seen) == 0 {
		return nil, errTartNoGuest
	}

	canonical := make([]config.GuestOS, 0, len(seen))
	for _, guest := range tartGuestOrder {
		if seen[guest] {
			canonical = append(canonical, guest)
		}
	}

	return canonical, nil
}

// normalizeTart canonicalises the guest list and refuses what cannot be
// rendered, naming the flag that carried it.
//
// EXPORTED CALLERS REACH Generate DIRECTLY, so every rule the CLI applies is
// re-applied here — the same reason alloc.New re-checks a catalogue it did not
// load. A rule enforced only in cmd/billet has a second entry point that does
// not enforce it.
//
// A FUNCTION RATHER THAN A METHOD, because every other Params method takes a
// value receiver and mixing the two makes the type's method set read
// ambiguously — this one has to mutate, since defaulting an image and
// canonicalising the guest order must reach the copy Generate goes on to render.
func normalizeTart(p *Params) error {
	if p.Tart == nil {
		return errTartNoGuest
	}

	// CLONED BEFORE ANYTHING IS FILLED IN, for the reason Generate clones
	// p.Workflows: Generate takes Params by value, but this field is a POINTER, so
	// defaulting an image or canonicalising the guest order would otherwise reach
	// back into the caller's struct. cmd/billet then could not tell an image it
	// supplied from one billet chose, which is exactly what the printed guidance
	// has to distinguish.
	block := *p.Tart
	block.GuestOS = slices.Clone(block.GuestOS)
	p.Tart = &block

	// A tart tier's image is per guest kind, so the shared field cannot carry
	// one. Refused rather than applied to whichever tier happens to be first.
	if p.Image != "" {
		return errors.New("--image names one image, and a tart host boots a different one per " +
			"guest kind: use --macos-image and --linux-image")
	}

	canonical, err := CheckTartGuestOS(p.Tart.GuestOS)
	if err != nil {
		return err
	}

	seen := make(map[config.GuestOS]bool, len(canonical))
	for _, guest := range canonical {
		seen[guest] = true
	}

	p.Tart.GuestOS = canonical

	// A NAME IS CHECKED WHENEVER ONE IS GIVEN, and required only where it is
	// load-bearing. Validating it only on the macOS path would let a linux-only
	// generation write a name config.Parse then rejects, blaming the generated
	// file rather than the flag.
	if p.Tart.NodeName == "" {
		if slices.Contains(canonical, config.GuestMacOS) {
			return errors.New("--node-name is required for a macOS tier: Apple's limit is two " +
				"concurrent macOS guests per physical Mac, so the tier has to pin the host it " +
				"lands on, and the pin can only name what this node calls itself")
		}
	} else if err := config.ValidateNodeName("--node-name", p.Tart.NodeName); err != nil {
		return err
	}

	// AN OVERRIDE FOR A GUEST KIND NOBODY ASKED FOR IS A TYPO, NOT A NO-OP. The
	// same rule --price follows against --instance-type: ignoring it would let an
	// operator believe they had pinned an artifact while the generated bytes named
	// the default. Checked BEFORE the defaults are filled, which is the only point
	// at which "the operator supplied this" is still distinguishable.
	if p.Tart.MacOSImage != "" && !seen[config.GuestMacOS] {
		return errors.New("--macos-image names the image a macOS tier boots, and this " +
			"generation has no macOS tier: add --guest-os macos, or drop the flag")
	}
	if p.Tart.LinuxImage != "" && !seen[config.GuestLinux] {
		return errors.New("--linux-image names the image an arm64 Linux tier boots, and this " +
			"generation has no Linux tier: add --guest-os linux, or drop the flag")
	}

	if p.Tart.MacOSImage == "" {
		p.Tart.MacOSImage = DefaultTartMacOSImage
	}
	if p.Tart.LinuxImage == "" {
		p.Tart.LinuxImage = DefaultTartLinuxImage
	}

	// The whitespace rule Generate applies to Params.Image, applied to the fields
	// that replaced it: an image is handed to tart verbatim as a POSITIONAL
	// argument, so anything it cannot resolve is a launch failure blaming the
	// registry rather than the flag.
	if err := checkTartImage("--macos-image", p.Tart.MacOSImage); err != nil {
		return err
	}

	return checkTartImage("--linux-image", p.Tart.LinuxImage)
}

// checkTartImage refuses an image reference tart would not read as one.
func checkTartImage(flag, image string) error {
	if idx := strings.IndexFunc(image, badImageRune); idx >= 0 {
		return fmt.Errorf("%s: %q contains whitespace or a control character", flag, image)
	}

	// tart's own parser reads a leading dash as a flag rather than an image, which
	// the backend refuses at launch (checkSpec). Refused here so the diagnostic
	// names the flag instead of arriving as a failed job.
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("%s: %q begins with %q, which tart reads as a flag rather than an "+
			"image name", flag, image, "-")
	}

	return nil
}

// tartTier is one entry in the generated catalogue: a size, plus the guest kind
// that decides its image and whether it pins this host.
type tartTier struct {
	tier

	guest config.GuestOS
	image string
	// node is set only on a macOS tier, where the pin is what makes the per-host
	// licence limit enforceable.
	node string
}

// tartTiers fits each requested guest kind against the same usable ceiling.
//
// GUEST KINDS ARE ALTERNATIVES, so choosing a macOS shape cannot delete a Linux
// shape. A requested kind that cannot run even alone is still refused by name.
func tartTiers(p Params, ceilVCPU int, ceilMemory config.ByteSize) ([]tartTier, error) {
	var out []tartTier

	for _, guest := range p.Tart.GuestOS {
		switch guest {
		case config.GuestMacOS:
			t, err := tartMacOSTier(p, ceilVCPU, ceilMemory)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		case config.GuestLinux:
			fitted := tartLinuxTiers(p, ceilVCPU, ceilMemory)
			if len(fitted) == 0 {
				return nil, tartNoRoomForLinux(ceilVCPU, ceilMemory)
			}
			out = append(out, fitted...)
		default:
			return nil, fmt.Errorf("initconfig: guest_os %q is not rendered for tart", guest)
		}
	}

	return out, nil
}

// tartNoRoomForLinux names the ceiling that cannot hold even the smallest guest.
func tartNoRoomForLinux(vcpu int, memory config.ByteSize) error {
	return fmt.Errorf("billet's ceiling on this machine is %d vCPU and %s, which is not enough "+
		"for a guest of any size — this host has nothing to contribute. Raise "+
		"server.max_vcpu and server.max_memory by hand once you know what else it is doing",
		vcpu, memory)
}

// tartMacOSTier keeps the generator's preferred Xcode shape when it fits alone.
//
// ONE DEFAULT SHAPE IS A GENERATOR CHOICE, not a partition of the host's slots.
// Leaving max_concurrent absent lets each entry follow the host's own limit.
func tartMacOSTier(p Params, vcpuBudget int, memoryBudget config.ByteSize) (tartTier, error) {
	build := func(vcpu int, memory config.ByteSize) tartTier {
		return tartTier{
			tier:  tier{label: fmt.Sprintf("billet-macos-%dvcpu", vcpu), vcpu: vcpu, memory: memory},
			guest: config.GuestMacOS,
			image: p.Tart.MacOSImage,
			node:  p.Tart.NodeName,
		}
	}

	for _, vcpu := range []int{4, 2} {
		memory := config.ByteSize(vcpu) * tierMemoryPerVCPU
		if vcpu <= vcpuBudget && memory <= memoryBudget {
			return build(vcpu, memory), nil
		}
	}

	// THE FLOOR IS THE HYPERVISOR'S; below it even a single guest cannot boot.
	if vcpuBudget < 1 || memoryBudget < config.MinMacOSGuestMemory {
		return tartTier{}, tartNoRoomForMacOS(vcpuBudget, memoryBudget)
	}

	vcpu := min(vcpuBudget, 4)

	return build(vcpu, min(memoryBudget, config.ByteSize(vcpu)*tierMemoryPerVCPU)), nil
}

// tartNoRoomForMacOS says why, and offers the smaller guest only where it fits.
//
// THE SIBLING OF tartNoRoomForLinux, and it had the same defect that one was
// split to fix: "generate an arm64 Linux config instead" is a real remedy on a
// machine that can hold a 1GiB guest and a false one on a machine that cannot —
// which is most of the range below Apple's 4GiB floor. Offering a smaller guest
// that also does not fit sends an operator round a loop.
func tartNoRoomForMacOS(vcpuBudget int, memoryBudget config.ByteSize) error {
	const shared = "billet's ceiling on this machine leaves %d vCPU and %s, and a macOS guest " +
		"needs at least 1 vCPU and %s — Apple's hypervisor refuses a smaller one outright, so " +
		"there is no macOS tier this host could actually run. "

	if vcpuBudget >= 1 && memoryBudget >= config.GiB {
		return fmt.Errorf(shared+"An arm64 Linux guest has no such floor and would fit: "+
			"generate one instead (--guest-os linux), or run this on a larger Mac",
			vcpuBudget, memoryBudget, config.MinMacOSGuestMemory)
	}

	return fmt.Errorf(shared+"Neither would a Linux guest, which needs 1 vCPU and 1GiB, so this "+
		"host has nothing to contribute as it is. Raise server.max_vcpu and server.max_memory "+
		"by hand once you know what else it is doing, or run this on a larger Mac",
		vcpuBudget, memoryBudget, config.MinMacOSGuestMemory)
}

// tartLinuxTiers keeps every Linux rung that fits the usable host ceiling.
func tartLinuxTiers(p Params, vcpuBudget int, memoryBudget config.ByteSize) []tartTier {
	build := func(vcpu int, memory config.ByteSize) tartTier {
		return tartTier{
			tier: tier{
				label:  fmt.Sprintf("billet-linux-arm64-%dvcpu", vcpu),
				vcpu:   vcpu,
				memory: memory,
			},
			guest: config.GuestLinux,
			image: p.Tart.LinuxImage,
		}
	}

	var fit []tartTier

	for _, vcpu := range tierLadder {
		memory := config.ByteSize(vcpu) * tierMemoryPerVCPU
		if vcpu <= vcpuBudget && memory <= memoryBudget {
			fit = append(fit, build(vcpu, memory))
		}
	}

	if len(fit) > 0 {
		return fit
	}

	// A SMALL HOST STILL GETS ONE FITTING TIER. A config with no entry for a
	// requested guest kind schedules nothing on it, even when a smaller guest fits.
	if vcpuBudget < 1 || memoryBudget < config.GiB {
		return nil
	}

	return []tartTier{
		build(vcpuBudget, min(memoryBudget, config.ByteSize(vcpuBudget)*tierMemoryPerVCPU)),
	}
}

// tartNodeBlocks is the node.tart configuration, and it is written ONLY for a
// generation that produced untrusted tiers.
//
// THAT IS THE OPPOSITE OF firecrackerNodeBlocks, DELIBERATELY. An unused
// untrusted_bridge costs nothing, so that generator always writes one with a
// comment saying nothing attaches to it yet. An unused untrusted_isolation costs
// the whole preflight: `billet check` treats a node that OFFERS to run untrusted
// work on a host whose softnet has no setuid-root grant as FATAL, because the
// config makes a promise the machine cannot keep. Writing it into a trusted-only
// config would fail the check over a mechanism nothing uses, so the trusted form
// says how to add it instead.
func tartNodeBlocks(trusted bool) string {
	const why = "  # A tart VM is a real kernel boundary and the NETWORK is not: tart's default is\n" +
		"  # shared NAT, where a guest reaches the host and can ARP-spoof the vmnet bridge to\n" +
		"  # read another guest's traffic. So untrusted work is REFUSED until a confinement\n" +
		"  # mechanism is named here.\n"

	if trusted {
		return "\n" + why +
			"  #\n" +
			"  # No tier below is `trust: untrusted`, so nothing is named — and unlike the\n" +
			"  # firecracker bridge, naming one you have not granted is not free: `billet\n" +
			"  # check` FAILS on a node that offers untrusted work its host cannot confine.\n" +
			"  # To add one, install softnet (it ships with tart), grant it the setuid-root\n" +
			"  # bit `billet check` prints — it resolves the real binary, because the Homebrew\n" +
			"  # symlink is not the file whose ownership decides — and then uncomment:\n" +
			"  #\n" +
			"  # tart:\n" +
			"  #   untrusted_isolation: softnet\n"
	}

	return "\n" + why +
		"  #\n" +
		"  # softnet is tart's own userspace packet filter and the one mechanism billet\n" +
		"  # drives. It ships with tart and needs a one-time setuid-root grant on this host;\n" +
		"  # `billet check` resolves the real binary and prints the exact command, and it\n" +
		"  # FAILS until the grant is in place — a node offering to run untrusted work on a\n" +
		"  # host that cannot confine it is worse than one that refuses outright.\n" +
		"  #\n" +
		"  # The tiers below are all `trust: untrusted`, so removing this refuses every job\n" +
		"  # this file schedules — add a trusted tier first.\n" +
		"  tart:\n" +
		"    untrusted_isolation: softnet\n" +
		"    # The resolvers an isolated guest is given, because billet is what took the\n" +
		"    # working one away: softnet blocks the private address space, which is where the\n" +
		"    # guest's DHCP-assigned resolver lives. Egress and TCP/443 keep working, so\n" +
		"    # nothing looks wrong and every job simply fails to resolve github.com. Unset\n" +
		"    # means " + strings.Join(config.DefaultUntrustedDNS(), " and ") + ".\n" +
		"    # untrusted_dns: [1.1.1.1, 8.8.8.8]\n"
}

// tartNameBlock is the node.name a tart generation writes, or the ordinary
// omitted-name comment when there is no name to write.
//
// WRITTEN WHENEVER ONE WAS GIVEN, not only when a macOS tier needs it. A name
// supplied and then not rendered is the "looks configured, is inert" shape: the
// operator believes the host is called what they said, the node takes its
// hostname instead, and a macOS tier they add by hand pins something nothing
// answers to.
//
// The pin is the fallback rather than the source, and it can only ever agree:
// tartMacOSTier takes the pin from this same field, so the two cannot be written
// from two places and diverge.
func tartNameBlock(p Params, ts []tartTier) string {
	name := ""
	if p.Tart != nil {
		name = p.Tart.NodeName
	}

	pinned := false

	for _, t := range ts {
		if t.node == "" {
			continue
		}

		pinned = true

		if name == "" {
			name = t.node
		}
	}

	if name == "" {
		return omittedNameComment
	}

	// THE PIN CLAUSE ONLY WHERE SOMETHING PINS. Apple's limit is why a macOS tier
	// must name a host; it is not why this key exists, and a linux-only generation
	// carrying that sentence would explain a requirement its own file does not
	// have. A comment whose stated reason is untrue is worse than no comment.
	why := ""
	if pinned {
		why = "  # The macOS tier below must PIN it, because Apple counts its two-guest limit\n" +
			"  # per PHYSICAL machine and billet has to know which Mac to count a guest\n" +
			"  # against.\n  #\n"
	}

	return fmt.Sprintf("  # THIS MAC'S NAME IN THE DEPLOYMENT, written explicitly rather than left\n"+
		"  # to the hostname — which on a Mac is usually not a legal node name at all.\n"+
		"  #\n%s"+
		"  # Once this host has a certificate the name in it is what the control plane\n"+
		"  # authorises, and this key must agree with it.\n"+
		"  name: %s\n", why, yamlScalar(name))
}

// tartTierIntro explains the catalogue in the terms a Mac operator needs.
func tartTierIntro(trusted bool) string {
	const images = "# Each image must already be in tart's store. A launch REFUSES one that is not\n" +
		"# there rather than fetching it, because a guest image is tens of gigabytes and a\n" +
		"# node runs one command at a time — a pull inside a job would time out that job\n" +
		"# and everything queued behind it. Use `billet images pull` when an image is a\n" +
		"# registry reference — the defaults are — and nothing when it is a local name\n" +
		"# `tart list` already shows. `billet check` lists whichever are missing.\n" +
		"#\n" +
		"# A macOS tier carries no `max_concurrent`, so it inherits its host's limit —\n" +
		"# two, which is what Apple's standard licence permits per Apple-branded machine.\n" +
		"# To keep a licensed slot free for interactive use, give this host a `nodes:`\n" +
		"# entry with a lower `macos_vm_limit` (see billet.example.yaml); every macOS\n" +
		"# tier pinned to it tightens with it."

	if trusted {
		return "# A tart guest is a real VM with its own kernel. These tiers are\n" +
			"# `trust: trusted` and bound to your runner group and workflow allowlist below.\n" +
			"#\n" + images
	}

	return "# A tart guest is a real VM with its own kernel, so these tiers are\n" +
		"# `trust: untrusted` and confined by softnet above — tart can run code billet\n" +
		"# cannot vouch for.\n" +
		"#\n" + images
}

// renderTartTiers writes the catalogue, one entry per generated tier.
func renderTartTiers(ts []tartTier, p Params, trusted bool) string {
	var b strings.Builder

	for i, t := range ts {
		if i > 0 {
			b.WriteString("\n")
		}

		fmt.Fprintf(&b, "  - label: %s\n    provider: tart\n    guest_os: %s\n",
			t.label, t.guest)

		if t.node != "" {
			fmt.Fprintf(&b, "    # Required: Apple's two-guest limit is counted per physical\n"+
				"    # machine, so a macOS tier has to say which Mac it lands on.\n"+
				"    node: %s\n", yamlScalar(t.node))
		}

		fmt.Fprintf(&b, "    vcpu: %d\n    memory: %s\n    image: %s\n",
			t.vcpu, t.memory, yamlScalar(t.image))

		renderTierPolicy(&b, p, trusted)
	}

	return b.String()
}
