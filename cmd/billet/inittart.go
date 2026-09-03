package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// hostGOARCH is runtime.GOARCH behind a seam, so the Apple-silicon refusal can
// be asserted from either side. CI is Linux-only, so without it the refusal and
// the generation it guards could not both be covered.
//
// Not `hostArch`: that name already belongs to the MANIFEST spelling of this
// machine (x86_64, aarch64), and the two answer different questions.
var hostGOARCH = runtime.GOARCH

// refuseTartOffAppleSilicon stops a tart generation on a machine that is not the
// one it describes.
//
// TART IS APPLE-SILICON ONLY — the backend's own preflight says so, and every
// value billet fills in for this backend comes from the machine running the
// command. Refused here rather than written and then refused at startup, which is
// the rule the whole provider switch follows.
func refuseTartOffAppleSilicon(profile initconfig.Profile, namedHost bool) error {
	if hostOS == "darwin" && hostGOARCH == "arm64" {
		return nil
	}

	// EVERY CLAUSE IS CONDITIONAL ON BILLET ACTUALLY MAKING THAT CHOICE, because
	// this sentence is the reason an operator is being told to move, and one false
	// half is a thing they can check.
	//
	// THE PATHS ONLY OFF DARWIN. An Intel Mac is refused because tart needs Apple
	// silicon, and its service paths are exactly the /usr/local ones a Mac's launch
	// agents read.
	//
	// THE HOSTNAME ONLY WHERE BILLET WOULD READ ONE. With --node-name given the
	// name is the operator's, and telling them it came from this machine is false
	// in the same breath as a refusal that is correct.
	//
	// THE CEILING IS UNCONDITIONAL, and that is a fact about the flags rather than
	// an oversight: --max-vcpu and --max-memory are ec2-only, refuseEC2OnlyFlags
	// rejects them on tart, and the one exemption is an ansible emission this
	// backend refuses outright. There is no tart generation whose ceiling was not
	// measured here.
	derived := []string{"the ceiling is MEASURED here"}
	if !namedHost {
		derived = append(derived, "the node name comes from this hostname")
	}

	// AND THE PATHS ONLY WHERE THEY WOULD BE WRONG, which is narrower than "off
	// darwin" in two directions. An Intel Mac's SERVICE paths are exactly the
	// /usr/local ones a Mac's launch agents read. And a user-session generation
	// has no launch-agent paths at all on any platform — its state and key live
	// under this account's config directory and it configures no lock at all — so
	// naming /usr/local there describes a shape the file does not have.
	switch {
	case hostOS == "darwin":
	case profile == initconfig.ProfileLocalService:
		derived = append(derived, "the state, key and lock paths are this platform's rather "+
			"than the ones a Mac's launch agents read under /usr/local")
	default:
		derived = append(derived, "the state and key paths are under THIS account's config "+
			"directory on this machine")
	}

	return fmt.Errorf("--provider tart runs macOS and native arm64 Linux guests through "+
		"Apple's Virtualization.framework, which needs an Apple-silicon Mac — and this is "+
		"%s/%s. Generate it on that Mac:\n  ssh <mac> billet init --provider tart …\n\n"+
		"Not merely a platform check: what billet fills in for this backend is read from the "+
		"machine running the command — %s. There is no `--emit` path for a Mac either, "+
		"because the junioryono.billet.host role is Linux-only",
		hostOS, hostGOARCH, strings.Join(derived, ", and "))
}

// detectHostCapacity is config.DetectHostCapacity behind a seam, so a generation
// a test drives describes a machine the TEST chose.
//
// MEASURED FROM THE RUNNER OTHERWISE, AND A CI RUNNER IS SMALL. A tart
// generation asking for both guest kinds fits the 12-core reference Mac and does
// not fit GitHub's 4-vCPU Linux runner: the macOS tier takes what there is and
// the Linux one is correctly refused. Same commit, same test, opposite verdicts —
// the rule that a probe on the development machine is not evidence about the
// builder, arriving as a red CI run behind a green `make check`.
var detectHostCapacity = config.DetectHostCapacity

// hostName is os.Hostname behind a seam.
//
// The node name a macOS tier pins comes from here when the operator names none,
// and the interesting case cannot be arranged on a real machine: a stock Mac's
// hostname is frequently not a legal node name ("Junior's MacBook Pro"), and a
// test cannot rename the host it runs on.
var hostName = os.Hostname

// tartInitFlags is the tart-only flag set `billet init --provider tart` reads.
type tartInitFlags struct {
	guestOS    []string
	nodeName   string
	macOSImage string
	linuxImage string
}

// tartOnlyFlagNames are the flags meaningful only to --provider tart.
//
// --node-name IS NOT ONE OF THEM ANY MORE. Both backends that can carry a macOS
// tier need it, and for the same reason: config validation refuses a macOS tier
// that names no node, because a per-host guest limit cannot be enforced against
// a tier pinned to nowhere. What differs is where the name could come from — a
// tart node IS the Mac, so its hostname is at least a candidate, while a
// codebuild node is a small machine calling an API and its hostname says nothing
// about the fleet. Neither derives one; both ask.
var tartOnlyFlagNames = []string{"guest-os", "macos-image", "linux-image"}

// refuseTartOnlyFlags stops a tart-only flag from being silently ignored on
// another backend, where it would read as configured and do nothing.
//
// PRESENCE, NOT VALUE, for the reason refuseEC2OnlyFlags gives: `set` is what
// the operator actually passed, so `--node-name ""` is a misuse rather than an
// omission and is caught the same as a real one.
func refuseTartOnlyFlags(kind config.ProviderKind, set map[string]bool) error {
	var used []string
	for _, name := range tartOnlyFlagNames {
		if set[name] {
			used = append(used, "--"+name)
		}
	}

	if len(used) > 0 {
		return fmt.Errorf("%s can only be used with --provider tart, but this is a %s config",
			strings.Join(used, ", "), kind)
	}

	return nil
}

// defaultTartGuestOS is what a tart generation serves when the operator names no
// guest kind.
//
// macOS, because it is the one thing this backend can do that no other can — a
// Mac that only ever ran Linux containers would be better served by the docker
// trial, and a Mac that runs arm64 Linux GUESTS is the deliberate second case.
// Costed rather than assumed: it is the larger image of the two, so the default
// is also the slower first job, and the guidance says so.
const defaultTartGuestOS = string(config.GuestMacOS)

// tartInitParams turns the tart flags into initconfig inputs.
//
// It resolves exactly two things billet can answer for the operator — the guest
// kind, and this machine's name — and refuses rather than guessing at the second
// when the machine cannot supply a usable one. Everything else is validated by
// initconfig.Generate, which is also reachable without this function.
func tartInitParams(
	notes io.Writer, f tartInitFlags, set map[string]bool,
) (*initconfig.TartParams, error) {
	// PRESENCE, NOT VALUE, and the same reason the ec2 flags are read that way:
	// `--node-name ""` is somebody meaning something, and defaulting it back to
	// the hostname would answer a question they had tried to answer themselves.
	// ONE LIST CARRYING BOTH THE NAME AND THE VALUE, and ordered: a list of names
	// beside a switch that maps them is two places to add a flag to, and the one
	// that gets forgotten fails open.
	for _, flag := range []struct{ name, value string }{
		{"node-name", f.nodeName},
		{"macos-image", f.macOSImage},
		{"linux-image", f.linuxImage},
	} {
		if set[flag.name] && flag.value == "" {
			return nil, fmt.Errorf("--%s was given with an empty value; drop the flag to take "+
				"billet's default", flag.name)
		}
	}

	guests := f.guestOS
	if !set["guest-os"] {
		guests = []string{defaultTartGuestOS}
	}

	raw := make([]config.GuestOS, 0, len(guests))
	for _, g := range guests {
		raw = append(raw, config.GuestOS(g))
	}

	// VALIDATED BEFORE ANYTHING IS READ OFF THE MACHINE, through the rule Generate
	// applies rather than a second copy of it. Two reasons, and the second is the
	// one that bit: everything decided here compares against these values, so
	// `--guest-os " macos "` — which Generate accepts — was missed by the hostname
	// derivation below and then refused for the name that derivation would have
	// supplied; and resolving the name first meant `--guest-os typo` on a Mac whose
	// hostname is not a legal node name reported the NODE NAME, so the same invalid
	// input got a different error depending on the machine it was typed on.
	kinds, err := initconfig.CheckTartGuestOS(raw)
	if err != nil {
		return nil, err
	}

	name, err := tartNodeName(notes, f.nodeName, kinds)
	if err != nil {
		return nil, err
	}

	return &initconfig.TartParams{
		GuestOS:    kinds,
		NodeName:   name,
		MacOSImage: f.macOSImage,
		LinuxImage: f.linuxImage,
	}, nil
}

// tartNodeName resolves the name this Mac carries in the deployment.
//
// A NODE NAME IS AN IDENTITY, SO IT IS NEVER SANITISED INTO ONE. The control
// plane authorises a node by this name and a certificate must later carry it, so
// quietly turning "Junior's MacBook Pro.local" into "Juniors-MacBook-Pro-local"
// would name a host the operator never chose — and they would meet it again the
// first time `billet ca issue` disagreed. The hostname is used when it is
// ALREADY a legal node name and refused, by the flag, when it is not.
//
// RESOLVED FOR EVERY TART GENERATION, not only one carrying a macOS tier.
// Skipping it for a linux-only host looked like a kindness — nothing pins a name
// there, so why ask — and it was not: a config that writes no node.name gets one
// from the machine's hostname AT LOAD, so Generate's own config.Parse proof
// refuses the same unusable name a moment later, naming node.name and a file
// billet had just written rather than the flag that fixes it. What differs by
// guest kind is the PIN, not the name.
func tartNodeName(notes io.Writer, flag string, kinds []config.GuestOS) (string, error) {
	if flag != "" {
		return flag, nil
	}

	pins := slices.Contains(kinds, config.GuestMacOS)

	host, err := hostName()
	if err != nil {
		return "", fmt.Errorf("billet writes this Mac's name into the config%s, and could not "+
			"read this machine's name to fill it in (%w). Pass --node-name",
			pinReason(pins), err)
	}

	host = strings.TrimSpace(host)
	if err := config.ValidateNodeName("--node-name", host); err != nil {
		return "", fmt.Errorf("billet writes this Mac's name into the config%s, and this "+
			"machine's hostname %q cannot be a node name: %w.\n\nThat is ordinary on a Mac — "+
			"the name in System Settings becomes a hostname with spaces, apostrophes or a "+
			".local suffix. Choose one and pass it:\n  billet init --provider tart "+
			"--node-name mac-mini-1 …\n\nIt is the name `billet ca issue` will mint a "+
			"certificate for and the control plane will authorise, so billet will not invent "+
			"one for you", pinReason(pins), host, err)
	}

	fmt.Fprintf(notes, "NOTE: this host is named %q in the config, taken from this machine's "+
		"hostname%s. It is the name `billet ca issue` must mint a certificate for; pass "+
		"--node-name to choose a different one.\n\n", host, pinReason(pins))

	return host, nil
}

// pinReason is the extra clause a macOS tier earns, and nothing otherwise.
//
// The name is written either way; only the PIN is Apple's doing, so offering that
// reason on a linux-only generation would explain a requirement it does not have.
func pinReason(pins bool) string {
	if !pins {
		return ""
	}

	return " — the macOS tier must PIN it, because Apple counts its two-guest limit per " +
		"physical machine"
}

// defaultTartImageSizes describes what a pull will cost, for the DEFAULT images
// only and only for the guest kinds this generation actually wrote.
func defaultTartImageSizes(tart *initconfig.TartParams) string {
	if tart == nil {
		return ""
	}

	var parts []string

	// Compared as values rather than as flag text: tartInitParams trimmed them on
	// the way in, and Generate does not reach back into this block to normalise it.
	for _, guest := range tart.GuestOS {
		switch {
		case guest == config.GuestMacOS && tart.MacOSImage == "":
			parts = append(parts, "87GB for the macOS image")
		case guest == config.GuestLinux && tart.LinuxImage == "":
			parts = append(parts, "11GB for the arm64 Linux image")
		}
	}

	return strings.Join(parts, " and ")
}

// errTartAnsible is what `--provider tart --emit ansible` gets.
//
// THE EMISSION DESCRIBES A LINUX HOST, unconditionally: `billet init` sets its
// target platform to linux for an emission because the junioryono.billet.host
// role is Linux-only — it installs systemd units, /etc/billet and a lock under
// /run/billet/locks. A tart node is a Mac. Emitting one would render a block
// whose every path is for a machine that cannot run the backend it names, which
// is the same defect as an emission from a Mac describing the Mac.
var errTartAnsible = errors.New(
	"--emit ansible renders a block for the junioryono.billet.host role, which is Linux-only " +
		"and installs systemd units under /etc/billet — and a tart node is an Apple-silicon " +
		"Mac. A Mac is converged by `billet init --profile local-service` and `billet local " +
		"up`, which manage the launch agents billet ships; see the reference Mac in " +
		"docs/reference-hardware.md")

// printTartNext is the host preparation a generated tart config names and billet
// cannot do, printed in the shape the firecracker and ec2 preambles use.
//
// THE SIZES ARE THE DEFAULTS' AND ARE ONLY CLAIMED FOR THEM. They are
// measurements of two specific published images; an operator who passed
// --macos-image has an image billet has never seen, and telling them it is 87GB
// is a number they can check and find false. The same applies to naming a guest
// kind this generation does not have.
func printTartNext(cfgPath string, trusted bool, tart *initconfig.TartParams) {
	pathArg := shellArg(cfgPath)

	fmt.Printf("\nEvery tier's image must already be IN TART'S STORE before the first job — a "+
		"launch refuses one that is not there rather than fetching it, because a guest image "+
		"is tens of gigabytes and a node runs one command at a time. For a registry "+
		"reference, which is what the defaults are:\n\n"+
		"       billet images pull --config %s\n\n"+
		"A local name (one `tart list` already shows) needs no pull. `billet check` lists "+
		"whichever of this node's tier images are missing either way.\n", pathArg)

	if sizes := defaultTartImageSizes(tart); sizes != "" {
		fmt.Printf("\nIn tart's local store that is about %s.\n", sizes)
	}

	if !trusted {
		// SAID BEFORE THE CHECK RATHER THAN AFTER IT, because this is the one
		// generated config whose preflight FAILS on a host that has done nothing
		// wrong: the tiers are untrusted, so the node offers to confine a fork's
		// job, and softnet cannot confine anything until it is setuid-root.
		fmt.Printf("\nThe tiers are untrusted, so this host must be able to CONFINE a guest: " +
			"softnet ships with tart and needs a one-time setuid-root grant. `billet check` " +
			"resolves the real binary — Homebrew's symlink is not the file whose ownership " +
			"decides — and prints the exact command; until it is granted, check fails rather " +
			"than letting this node promise isolation it does not have.\n")
	}
}
