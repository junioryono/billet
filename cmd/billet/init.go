package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/state"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// hostOS is runtime.GOOS behind a seam, so the service shape a generation
// writes — systemd's on Linux, the launch agents' on macOS — and the refusal for
// every other platform are testable from either machine.
var hostOS = runtime.GOOS

// repeatedString collects a flag given more than once into a slice, in order.
type repeatedString []string

func (r *repeatedString) String() string { return fmt.Sprint([]string(*r)) }

func (r *repeatedString) Set(v string) error {
	*r = append(*r, v)

	return nil
}

// emitMode is where a generation goes.
//
// TWO DESTINATIONS FOR ONE GENERATOR, rather than a second command that would
// have to redeclare every flag and then drift from this one. The config a
// machine runs and the inventory entry that renders that config are the same
// bytes; only their frame differs.
type emitMode string

const (
	// emitFile writes --config, the original and default behaviour.
	emitFile emitMode = "file"
	// emitAnsible prints the billet_config block for an inventory and writes
	// nothing. STDOUT CARRIES ONLY THE BLOCK — every human-facing line goes to
	// stderr — so appending the output to an inventory cannot land prose in it.
	emitAnsible emitMode = "ansible"
)

// checkEmit refuses an emission that is not one of the two, naming the flag.
func checkEmit(e emitMode) error {
	if e != emitFile && e != emitAnsible {
		return fmt.Errorf("--emit: %q is not one of file, ansible", e)
	}

	return nil
}

// cmdInit writes a billet.yaml that runs.
//
// THE POINT IS THAT NOTHING HAS TO BE HAND-EDITED AFTERWARDS. Copying the
// example meant editing the provider, every tier's image, the state directories
// and the capacity ceiling before anything would start, and each of those is a
// step an operator can get wrong in a way that surfaces much later — a ceiling
// larger than the machine does not fail, it overcommits.
//
// The generation itself lives in internal/initconfig so the same code the CLI
// writes is the code an end-to-end test can prove launches a job. What this
// command cannot know is the GitHub App, because that does not exist yet: the
// file names the org and leaves the ids at zero, and `billet github-app create`
// fills them in rather than printing a block to paste.
//
// AND IT WILL NOT OVERWRITE A CONFIG, WHICH IS NOT THE RULE THAT COMMAND
// FOLLOWS. This one generates a whole file, so it may replace an existing one
// only where it can prove nothing is lost; otherwise the fresh generation lands
// at <path>.new and the original is untouched. `github-app create --config`
// edits one block into a config that already exists, because the App identity is
// the thing no generator can merge for an operator. Both are right for what they
// do, and for a while nothing said which was about to happen — so configEditRule
// is the sentence both of them print.
func cmdInit(ctx context.Context, args []string) error {
	// `billet init iam` is a sub-command: it does not write a config, it prints the
	// IAM policy the written config's node needs. Peeled before flag parsing so its
	// flags are its own.
	if len(args) > 0 && args[0] == "iam" {
		return cmdInitIAM(ctx, args[1:])
	}

	// `billet init hybrid` writes a DIRECTORY -- a Terraform root, an inventory
	// with both hosts, a playbook, the collection pin and a runbook -- rather
	// than one config, so it is its own command with its own flags.
	if len(args) > 0 && args[0] == "hybrid" {
		return cmdInitHybrid(ctx, args[1:])
	}

	fs := newFlagSet("billet init")

	// A PARSE DIAGNOSTIC MUST NOT REACH AN APPENDED INVENTORY. newFlagSet sends
	// help and errors to stdout deliberately, so `-h` stays pipeable — but for an
	// emission stdout is the artefact, and `billet init --emit ansible --typo >>
	// inventory.yml` would append usage text to the file before failing. The
	// destination is only known after parsing, so it is read from the raw args
	// here; a wrong guess costs a diagnostic on the other stream and nothing else.
	if wantsAnsibleEmission(args) {
		fs.SetOutput(os.Stderr)
	}

	cfgPath := addConfigFlag(fs)
	profile := fs.String("profile", string(initconfig.ProfileLocal),
		"path shape: local (user-session, two terminals) or local-service (the services billet "+
			"ships — systemd units on Linux, launch agents on macOS)")
	listen := fs.String("listen", "",
		"loopback address the server binds and the node dials (default "+initconfig.DefaultListen+")")
	org := fs.String("org", "", "the GitHub organization these runners serve (exactly one of --org and --repository)")
	repository := fs.String("repository", "", "the GitHub repository these runners serve, as owner/name")
	provider := fs.String("provider", string(config.ProviderDocker),
		"compute backend for this host")
	image := fs.String("image", "",
		"tier image (default: a runner container for docker, a golden guest generation for firecracker)")
	group := fs.String("runner-group", "",
		"the GitHub runner group a trusted tier belongs to (required for docker)")
	var workflows repeatedString
	fs.Var(&workflows, "workflow",
		"a workflow ref a trusted tier may run (repeatable; required for docker)")
	force := fs.Bool("force", false, "overwrite an existing config")
	emit := fs.String("emit", string(emitFile),
		"where the generation goes: file (write --config) or ansible (print the "+
			"billet_config block for an inventory, writing nothing)")

	// WHERE THE LEDGER LIVES, and it is a property of the CONTROL PLANE rather
	// than of a backend — so unlike every flag below, these apply to any provider.
	stateBackend := fs.String("state-backend", "",
		"where the control-plane ledger lives: sqlite (the default; a file beside the "+
			"deployment identity) or postgres (a database you operate)")
	stateDSNEnv := fs.String("state-dsn-env", "",
		"the environment variable holding the PostgreSQL connection string (required for "+
			"--state-backend postgres; billet never reads a DSN out of the config file)")

	// tart-only. A Mac serves two kinds of guest and each names its own image, so
	// there is no single --image for this backend; and a macOS tier pins the host
	// it lands on, so the node needs a name billet can write into the file.
	var guestOS repeatedString
	fs.Var(&guestOS, "guest-os",
		"a guest kind this Mac serves: macos or linux (repeatable; default macos; tart)")
	nodeName := fs.String("node-name", "",
		"this host's name in the deployment; required for a macOS tier on either backend "+
			"that can carry one (tart, codebuild), because a macOS tier has to pin the host "+
			"its guest limit is enforced against")
	macOSImage := fs.String("macos-image", "",
		"the macOS guest a macOS tier boots (default "+initconfig.DefaultTartMacOSImage+")")
	linuxImage := fs.String("linux-image", "",
		"the arm64 Linux guest a linux tier boots (default "+initconfig.DefaultTartLinuxImage+")")

	// ec2-only placement. billet cannot detect any of these — the compute runs in
	// a region, not on this host — so they are flags rather than measurements.
	region := fs.String("region", "",
		"AWS region (required for ec2 and codebuild); it is signed into every request")
	subnet := fs.String("subnet", "", "subnet id instances launch in (ec2)")
	var securityGroups repeatedString
	fs.Var(&securityGroups, "security-group",
		"a security group for trusted work (repeatable; ec2)")
	var untrustedGroups repeatedString
	fs.Var(&untrustedGroups, "untrusted-security-group",
		"a security group for fork pull-request work (repeatable; ec2)")
	var instanceTypes repeatedString
	fs.Var(&instanceTypes, "instance-type",
		"an EC2 shape billet may buy; its vcpu/memory/price are fetched (repeatable; ec2)")
	var priceOverrides repeatedString
	fs.Var(&priceOverrides, "price",
		"override a shape's fetched price, as type=usd e.g. c7i.xlarge=0.17 (repeatable; ec2)")
	maxVCPU := fs.Int("max-vcpu", 0,
		"the cloud vCPU budget billet may run at once (required for ec2 and codebuild)")
	maxMemory := fs.String("max-memory", "",
		"the cloud memory budget, e.g. 64GiB (required for ec2 and codebuild)")

	// --- what a HOST must provide and billet cannot detect (firecracker) ---
	kernelImage := fs.String("kernel-image", "",
		"the uncompressed guest kernel this host boots microVMs from; a real host pins a "+
			"version, and the conventional <kernel_dir>/vmlinux is only a fallback")
	cephUser := fs.String("ceph-user", "",
		"the RADOS identity billet authenticates as, WITHOUT the `client.` prefix; empty "+
			"means `billet`, and `admin` is refused because an admin key can delete a pool")
	cephKeyring := fs.String("ceph-keyring", "",
		"that identity's keyring; empty leaves Ceph's own search path, which finds "+
			"/etc/ceph/ceph.<user>.keyring")
	cacheListen := fs.String("cache-listen", "",
		"one literal, non-loopback address guests reach the cache on (wildcards are refused)")
	cacheGuestEndpoint := fs.String("cache-guest-endpoint", "",
		"the HTTP origin placed in guest metadata; it must name the same address as "+
			"--cache-listen, and the two are given together or not at all")

	// --- codebuild ---
	cbProject := fs.String("codebuild-project", "",
		"the CodeBuild project billet starts builds in; it must be DEDICATED to this "+
			"deployment, because a build cannot be tagged and the project is half of what "+
			"tells billet's own builds from somebody else's")
	cbEnvironment := fs.String("codebuild-environment", "",
		"the CodeBuild environment: LINUX_CONTAINER, ARM_CONTAINER, LINUX_GPU_CONTAINER, "+
			"LINUX_EC2, ARM_EC2 or MAC_ARM. It decides this node's guest OS")
	cbFleetARN := fs.String("codebuild-fleet-arn", "",
		"a reserved-capacity fleet; empty is on-demand compute. Required for MAC_ARM, "+
			"LINUX_EC2 and ARM_EC2, which have no on-demand form")
	cbFleetCapacity := fs.Int("codebuild-fleet-capacity", 0,
		"how many builds the reserved fleet may run at once (MAC_ARM only): a macOS tier's "+
			"cap is the fleet's capacity, not Apple's per-machine allowance")

	var computeTypes repeatedString

	fs.Var(&computeTypes, "compute-type",
		"a compute type billet may buy, as NAME=vcpu,memory,price — repeat it, most "+
			"preferred first. e.g. BUILD_GENERAL1_MEDIUM=4,7GiB,0.01")

	cbJITPath := fs.String("jit-parameter-path", "",
		"the SSM Parameter Store prefix each build's single-use runner registration is "+
			"written under; the node's IAM policy is scoped to exactly this path")
	cbJITKMS := fs.String("jit-kms-key-id", "",
		"a customer-managed KMS key for those SecureString parameters; empty uses aws/ssm")
	cbLogGroup := fs.String("codebuild-log-group", "",
		"where builds log; empty pins CodeBuild's own default for the project")
	cbPrivileged := fs.Bool("privileged", false,
		"grant the build the privilege Docker needs (container environments only). A job "+
			"that runs `docker build` or a service container fails without it")
	cbBuildTimeout := fs.Int("build-timeout-minutes", 0,
		"the build ceiling billet asks CodeBuild for, 5 to 2160; empty takes the maximum")
	cbQueuedTimeout := fs.Int("queued-timeout-minutes", 0,
		"how long a build may wait for capacity, 5 to 480, after which CodeBuild FAILS it; "+
			"empty takes the maximum")
	cbAcceptCeiling := fs.Bool("accept-external-build-ceiling", false,
		"acknowledge that every job on this node inherits CodeBuild's own limits — a build "+
			"is capped at 36 hours and a queued build FAILS after 8 — which billet cannot "+
			"lift. Required: it changes nothing about how billet behaves and exists so the "+
			"sentence is read before a tier advertises capacity")

	if err := parse(fs, args); err != nil {
		return err
	}

	// Which flags the operator ACTUALLY passed, so an ec2-only flag can be refused
	// on another provider by presence rather than value — `--max-vcpu 0` is a
	// misuse, not an omission.
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	// THE CHEAP FLAGS ARE REFUSED FIRST, before capacity detection or a live AWS
	// fetch: a typoed --profile must not pay a signed EC2 round trip and then die
	// with an error that never names the flag — and the --config redirect below
	// must only be computed from a profile already known to be valid.
	emitValue := emitMode(*emit)
	if err := checkEmit(emitValue); err != nil {
		return err
	}

	// An emission's stdout carries the block and nothing else, so every
	// human-facing line goes to stderr instead. Appending the output to an
	// inventory is the obvious use, and one NOTE landing in the middle of the
	// YAML is a corrupted inventory whose cause is invisible in the file that
	// broke.
	notes := io.Writer(os.Stdout)
	if emitValue == emitAnsible {
		notes = os.Stderr
	}

	// THE BACKEND IS RESOLVED BEFORE THE EMISSION RULES, because two of those
	// rules are the backend's. `--emit ansible` on a Mac ordinarily says "run this
	// on the target" — which is exactly wrong for tart, whose target IS a Mac and
	// whose role cannot converge one — so the tart refusal has to be reached
	// first, and a tart-only flag on another backend must die before capacity
	// detection or a live AWS fetch.
	kind := config.ProviderKind(*provider)
	if !kind.Valid() {
		return fmt.Errorf("provider %q is not one of firecracker, tart, ec2, codebuild, docker",
			*provider)
	}

	if kind != config.ProviderTart {
		if err := refuseTartOnlyFlags(kind, setFlags); err != nil {
			return err
		}
	} else if emitValue == emitAnsible {
		return errTartAnsible
	}

	// THE SAME RULE FOR THE THIRD BACKEND WITH FLAGS OF ITS OWN. A flag accepted
	// and silently discarded leaves somebody believing they asked for something —
	// and on this backend the discarded value would be a project, a fleet or an
	// IAM path, none of which reads as absent from the file that comes out.
	//
	// AN EMISSION IS ALLOWED HERE, unlike tart's. A codebuild node is an
	// orchestrator on an ordinary small Linux machine — it runs the binary, holds
	// AWS credentials and calls an API — so the junioryono.billet.host role
	// converges it exactly as it converges an ec2 orchestrator. What makes tart's
	// emission impossible is that its target IS a Mac and the role is Linux-only.
	if kind != config.ProviderCodeBuild {
		if err := refuseCodeBuildOnlyFlags(kind, setFlags); err != nil {
			return err
		}
	}

	// --node-name IS SHARED BY THE TWO BACKENDS THAT CAN CARRY A macOS TIER, and
	// refused on the other three rather than discarded. Every other generation
	// deliberately writes no node.name at all — with a certificate the name comes
	// from it, and on a loopback machine there is no certificate to disagree with
	// — so a name accepted there would be written nowhere and mean nothing.
	if setFlags["node-name"] &&
		kind != config.ProviderTart && kind != config.ProviderCodeBuild {
		return fmt.Errorf("--node-name can only be used with --provider tart or --provider "+
			"codebuild, but this is a %s config: those are the two backends that can carry a "+
			"macOS tier, and a macOS tier has to pin the host its guest limit is enforced "+
			"against. Every other generation omits node.name, because with a certificate the "+
			"name comes from it", kind)
	}

	// AN EXPLICITLY EMPTY --profile= IS NOT A THIRD SHAPE. CheckProfile accepts it
	// (Generate defaults an unset profile), and fs.Visit reports it as SET — so it
	// skipped the emission's service default, matched neither exact shape below,
	// and emitted user-session paths off Linux, measuring the wrong machine.
	// Normalised to the default here, before anything compares against a shape.
	if *profile == "" {
		*profile = string(initconfig.ProfileLocal)
	}

	// The ansible emission describes a host the junioryono.billet.host role
	// converges, and that role installs the service shape — /etc/billet, state
	// under /var/lib/billet, the lock under /run/billet/locks. A user-session
	// config pasted into an inventory renders a billet.yaml the role's own units
	// cannot read, so the profile follows the destination rather than the default.
	if emitValue == emitAnsible && !setFlags["profile"] {
		*profile = string(initconfig.ProfileLocalService)
	}

	// BOTH OR NEITHER. Half a reading is not a reading, and the half that is
	// missing would silently fall back to measuring THIS machine — which is the
	// mistake the refusal below exists to prevent, arrived at from the other side.
	declaredCapacity := setFlags["max-vcpu"] && setFlags["max-memory"]
	if emitValue == emitAnsible && !declaredCapacity &&
		(setFlags["max-vcpu"] || setFlags["max-memory"]) {
		return errors.New("--max-vcpu and --max-memory describe the machine this block is " +
			"for, so they are given together or not at all; one alone would leave the other " +
			"measured from whichever machine happens to be running this")
	}

	profileValue := initconfig.Profile(*profile)
	if err := initconfig.CheckProfile(profileValue); err != nil {
		return err
	}

	// THE PLATFORM THE GENERATION IS FOR IS NOT ALWAYS THE ONE RUNNING THIS.
	//
	// An ansible emission describes a host the junioryono.billet.host role
	// converges, and that role is Linux-only: it installs the systemd units,
	// /etc/billet, state under /var/lib/billet and the lock under
	// /run/billet/locks. Emitting from a Mac — which is supported, and is what
	// the declared-capacity escape above exists for — would otherwise render
	// /usr/local paths, launch-agent prose and a private_key_path the role never
	// writes, into a block for units that read none of them.
	//
	// A file write is the opposite: it lands on THIS machine, for the services
	// this machine has.
	targetOS := hostOS
	if emitValue == emitAnsible {
		targetOS = "linux"
	}
	if emitValue == emitAnsible && profileValue == initconfig.ProfileLocal {
		return errors.New("--emit ansible describes a host the junioryono.billet.host role " +
			"converges, which installs the service shape; --profile local is the two-terminal " +
			"user-session shape and its paths are unreadable to the role's units")
	}
	if err := initconfig.CheckListen(*listen); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("--config: the path is empty")
	}
	// TWO SHAPES, AND NO THIRD. Linux is systemd's — /etc/billet, /var/lib,
	// /run/billet/locks — and macOS is the launch agents' under /usr/local.
	// Anywhere else billet ships no services at all, so the flag is refused by
	// name rather than writing a file whose every instruction is for a manager
	// that is not there.
	// AN EMISSION IS NOT A LOCAL INSTALL. It writes nothing here, so the service
	// shape's paths are strings describing the target rather than paths this
	// machine must have — the refusal below is about writing for services that
	// do not exist locally, and an emission writes for services elsewhere.
	//
	// What DOES bind an emission to this machine is the measured ceiling: a
	// host-run backend's ceiling comes from the machine the command runs on, so
	// emitting for a 128-thread server from a laptop would put the laptop's
	// capacity under the server's name — a config that loads, starts, and quietly
	// advertises a fraction of the fleet. Declaring the numbers removes that tie,
	// and only that one.
	if emitValue == emitAnsible {
		if !declaredCapacity && hostOS != "linux" {
			return fmt.Errorf("--emit ansible measures THIS machine, and this is %s — the block "+
				"would carry this host's capacity under the target's name. Either run it on "+
				"the target:\n  ssh <host> billet init --emit ansible …\nor say what the "+
				"target has:\n  billet init --emit ansible --max-vcpu 8 --max-memory 32GiB …",
				hostOS)
		}
	} else if profileValue == initconfig.ProfileLocalService &&
		hostOS != "linux" && hostOS != "darwin" {
		// BOTH PLATFORMS billet SHIPS SERVICES FOR. The refusal used to name
		// systemd and so covered macOS too — which closed a loop: `billet local
		// up` refuses a config that is not at the service path and tells the
		// operator to "generate one there with `billet init --profile
		// local-service`", and on a Mac that command then refused. Every guided
		// macOS path ended at two commands each pointing at the other.
		return fmt.Errorf("--profile local-service writes for the services billet ships — "+
			"systemd units on Linux, launch agents on macOS — and this host is %s; use "+
			"--profile local here, or run this on the host that will run them", hostOS)
	}

	// REFUSED RATHER THAN WRITTEN AND THEN REFUSED AT STARTUP. A generated file
	// naming a backend that cannot run is a file the operator has to debug rather
	// than use, and there are different reasons to refuse one, so they say
	// different things.
	switch kind {
	case config.ProviderDocker:
		// The one backend that needs nothing but a daemon.

	case config.ProviderFirecracker:
		// Writable, but the config names host prep billet cannot do (a kernel, two
		// bridges, a Ceph cluster). printInitNext points at how.
		// GENERATED, BUT SAID OUT LOUD. A microVM host needs KVM, two Linux
		// bridges and a Ceph client, none of which a Mac has — and what a Mac
		// runs is the tart backend. This is a NOTE rather than a refusal because
		// generating on one machine for another is legitimate (that is what
		// `--emit ansible` formalises), and a wrong refusal strands somebody
		// doing it by hand. It is on stderr, so an emission's stdout stays the
		// block alone.
		if hostOS != "linux" && emitValue == emitFile {
			fmt.Fprintf(notes, "NOTE: this config describes a Linux microVM host — KVM, the two "+
				"bridges node.firecracker names, and a Ceph client — and this machine is %s, so "+
				"it is for somewhere else. A Mac runs the tart backend instead; see the tart "+
				"section of billet.example.yaml.\n\n", hostOS)
		}

	case config.ProviderEC2:
		// Writable now: the placement billet cannot detect comes from flags, and the
		// shapes' vcpu/memory/price are fetched live.

	case config.ProviderTart:
		// WRITABLE, and it took two measured images to become so. billet still does
		// not BUILD guest images for this backend; what changed is that two
		// published ones were proved to run a real job — see the constants in
		// internal/initconfig. They are pulled rather than built, which is the same
		// staged flow firecracker's `@verified` generation follows.
		//
		// A REFUSAL RATHER THAN THE NOTE THE FIRECRACKER BRANCH GIVES, and the
		// difference is that firecracker has a cross-machine path and this has
		// none. `--emit ansible` formalises writing a Linux host's config from
		// somewhere else, which is why generating a firecracker file on a Mac is
		// merely noted; the junioryono.billet.host role is Linux-only, so there is
		// no such path to a Mac and errTartAnsible already says so.
		//
		// What is left is a file wrong in three ways at once: the ceiling is
		// MEASURED from this machine, the node name is taken from this machine's
		// hostname, and the state, key and lock paths are this platform's rather
		// than the ones a Mac's launch agents read under /usr/local. A generation
		// that has to be edited in three places before it runs is the trap this
		// generator exists to remove.
		if err := refuseTartOffAppleSilicon(profileValue, setFlags["node-name"]); err != nil {
			return err
		}

	case config.ProviderCodeBuild:
		// WRITABLE, AND WRITABLE FROM ANYWHERE. Nothing here is measured from this
		// machine: a codebuild node is an orchestrator on some small Linux box, and
		// every number in the file is one the operator declared. So there is no
		// platform refusal and no note — unlike firecracker, which describes a
		// Linux microVM host, and tart, whose target IS the machine running the
		// command.
		//
		// The ec2 placement flags mean nothing here and are refused rather than
		// discarded: a subnet or a security group written into a codebuild
		// invocation is somebody configuring a network for compute that runs in
		// AWS's own account.
		if err := refuseEC2PlacementFlags(kind, setFlags); err != nil {
			return err
		}

	default:
		return fmt.Errorf("%w: the %s provider is not written by `billet init`",
			errNotImplemented, kind)
	}

	// A local-service config lives where the packaged units' ExecStart points,
	// so an unspecified --config follows the profile rather than the per-user
	// default the units can never read (ProtectHome=true).
	if initconfig.Profile(*profile) == initconfig.ProfileLocalService && !setFlags["config"] {
		*cfgPath = initconfig.ServiceConfigPathFor(hostOS)
	}

	// AN EXISTING FILE IS NEVER SILENTLY REPLACED. What happens instead is
	// decided after generation: a provably pristine file converges (rewritten,
	// App identity carried), anything else gets the fresh generation BESIDE it
	// at <path>.new — and --force keeps its wholesale meaning for an operator
	// who explicitly wants the old file gone.
	var existingRaw []byte
	if raw, err := os.ReadFile(*cfgPath); err == nil {
		existingRaw = raw
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check %s: %w", *cfgPath, err)
	}

	// PRESENCE, NOT VALUE. `--state-backend sqlite` written out explicitly and no
	// flag at all produce the same file — the shorthand — but they are not the
	// same request, and only the flag being SET can carry `--state-dsn-env` into
	// the refusal that names it. An empty struct here would look like "no backend
	// selected" to checkStateParams and refuse a generation nobody asked anything
	// unusual of.
	if err := refuseEmptyStateFlags(setFlags, *stateBackend, *stateDSNEnv); err != nil {
		return err
	}

	params := initconfig.Params{
		Org:         *org,
		Repository:  *repository,
		Provider:    kind,
		Image:       *image,
		RunnerGroup: *group,
		Workflows:   workflows,
		Profile:     initconfig.Profile(*profile),
		// THE PLATFORM THIS GENERATION IS FOR, so the service shape it writes is
		// the one the services on that machine read. Without it the generator
		// falls back to its own view of runtime.GOOS, which is the same answer
		// in production and a different one in every test that pins a platform.
		GOOS:   targetOS,
		Listen: *listen,
		State:  stateParams(setFlags, *stateBackend, *stateDSNEnv),
		// WHAT THIS HOST PROVIDES AND BILLET CANNOT DETECT. Every one is
		// optional; omitting all of them generates exactly what billet generated
		// before they existed. Generate refuses them on any backend but
		// firecracker, so a value here is never silently discarded.
		Host: initconfig.HostInputs{
			KernelImage:        *kernelImage,
			CephUser:           *cephUser,
			CephKeyringPath:    *cephKeyring,
			CacheListen:        *cacheListen,
			CacheGuestEndpoint: *cacheGuestEndpoint,
		},
	}

	// The listen default is applied HERE, not only inside Generate: Generate
	// takes Params by value, so a default filled there never reaches this
	// copy — and the busy-listen probe below would then bind the empty
	// address, which Go resolves to a wildcard socket on a random port.
	if params.Listen == "" {
		params.Listen = initconfig.DefaultListen
	}

	// WHAT A MAC SERVES, AND WHAT IT IS CALLED. Resolved before capacity so an
	// unusable hostname is refused without first measuring a machine the run is
	// about to abandon.
	if kind == config.ProviderTart {
		tartParams, err := tartInitParams(notes, tartInitFlags{
			guestOS:    guestOS,
			nodeName:   *nodeName,
			macOSImage: *macOSImage,
			linuxImage: *linuxImage,
		}, setFlags)
		if err != nil {
			return err
		}

		params.Tart = tartParams
	}

	// The capacity story differs by backend. A host-run backend is MEASURED and its
	// ceiling leaves the machine headroom; an ec2 orchestrator has nothing to
	// measure — the compute runs in a region — so the ceiling is the cloud budget
	// the operator declares, and the shapes it may buy are fetched.
	var report func()

	switch kind {
	case config.ProviderCodeBuild:
		// THE SAME STORY AS EC2, WITHOUT THE FETCH. There is a
		// DescribeInstanceTypes for EC2 shapes and no equivalent for CodeBuild
		// compute types, so the sizes are declared alongside the names rather than
		// looked up — which is what the config would have required anyway.
		cbParams, vcpu, memory, err := codeBuildInitParams(codeBuildInitFlags{
			project:       *cbProject,
			environment:   *cbEnvironment,
			fleetARN:      *cbFleetARN,
			fleetCapacity: *cbFleetCapacity,
			computeTypes:  computeTypes,
			jitPath:       *cbJITPath,
			jitKMSKeyID:   *cbJITKMS,
			logGroup:      *cbLogGroup,
			privileged:    *cbPrivileged,
			buildTimeout:  *cbBuildTimeout,
			queuedTimeout: *cbQueuedTimeout,
			acceptCeiling: *cbAcceptCeiling,
			nodeName:      *nodeName,
			region:        *region,
			maxVCPU:       *maxVCPU,
			maxMemory:     *maxMemory,
		})
		if err != nil {
			return err
		}

		params.CodeBuild = cbParams
		params.VCPU, params.Memory = vcpu, memory
		report = func() {
			fmt.Fprintf(notes, "  cloud budget    %d vCPU, %s (billet never runs more than "+
				"this at once)\n", vcpu, memory)
		}

	case config.ProviderEC2:
		ec2Params, vcpu, memory, err := ec2InitParams(ctx, ec2InitFlags{
			region:          *region,
			subnet:          *subnet,
			securityGroups:  securityGroups,
			untrustedGroups: untrustedGroups,
			instanceTypes:   instanceTypes,
			priceOverrides:  priceOverrides,
			maxVCPU:         *maxVCPU,
			maxMemory:       *maxMemory,
		})
		if err != nil {
			return err
		}

		params.EC2 = ec2Params
		params.VCPU, params.Memory = vcpu, memory
		report = func() {
			fmt.Fprintf(notes, "  cloud budget    %d vCPU, %s (billet never runs more than this at once)\n",
				vcpu, memory)
		}

	default:
		if err := refuseEC2OnlyFlags(kind, setFlags, emitValue == emitAnsible); err != nil {
			return err
		}

		// DECLARED CAPACITY IS WHAT THE MACHINE HAS, not what billet may spend —
		// the same meaning the measured path gives it, so the host still keeps its
		// headroom. That differs from ec2, where the declared budget IS the
		// ceiling because no machine exists to withhold anything from; Params
		// documents both readings.
		vcpu, memory := *maxVCPU, config.ByteSize(0)
		measured := !declaredCapacity

		if declaredCapacity {
			parsed, err := config.ParseByteSize(*maxMemory)
			if err != nil {
				return fmt.Errorf("--max-memory: %w", err)
			}

			memory = parsed
		} else {
			detected, detectedMemory, err := detectHostCapacity()
			if err != nil {
				return fmt.Errorf("detect what this machine has: %w", err)
			}

			vcpu, memory = detected, detectedMemory
		}

		params.VCPU, params.Memory = vcpu, memory
		report = func() {
			source := "you declared  "
			if measured {
				source = "this machine  "
			}

			fmt.Fprintf(notes, "  %s  %d vCPU, %s\n", source, vcpu, memory)
			fmt.Fprintf(notes, "  billet ceiling  %d vCPU, %s (the rest is left for the host)\n",
				initconfig.CeilingVCPU(vcpu), initconfig.CeilingMemory(memory))
		}
	}

	body, trusted, err := initconfig.Generate(params)
	if err != nil {
		return err
	}

	// The three re-run outcomes. BESIDE moves no pointer and touches no
	// existing file, so the identity refusal applies only to the two paths
	// that REPLACE the file: converge and --force.
	//
	// ALL THREE ARE ABOUT WRITING, so none of them runs for an emission. A
	// re-run plan decides where bytes land, the identity refusal guards a file
	// about to be replaced, and creating the config directory is a side effect a
	// command that writes nothing must not have — `--emit ansible` against a
	// path that does not exist would otherwise leave /etc/billet behind.
	writePath := *cfgPath
	converged := false
	if emitValue == emitFile {
		if len(existingRaw) > 0 && !*force {
			switch initconfig.PlanReRun(existingRaw, body) {
			case initconfig.Regenerate:
				converged = true
			case initconfig.WriteBeside:
				writePath = *cfgPath + ".new"
			}
		}

		if len(existingRaw) > 0 && (converged || *force) {
			if err := refuseIdentityMove(*cfgPath, existingRaw, params.ServerStateDir()); err != nil {
				return err
			}
		}

		// A ROOT-OWNED CONFIG IS THE FAILURE, AND `sudo billet init` PRODUCES IT
		// WITHOUT AN ERROR. /usr/local is root-owned on a stock Mac, so the
		// obvious response to the mkdir refusal below is to re-run this under
		// sudo — which succeeds, and writes a config the launch agents cannot
		// read, because they run as the operator and `billet local up` refuses to
		// run as root. Nothing downstream would name the cause: `local up` would
		// fail on config.Load with a bare permission error.
		//
		// Refused BEFORE the mkdir rather than after, so the root run does not
		// first create a root-owned directory the operator then has to undo.
		if params.Profile == initconfig.ProfileLocalService &&
			initconfig.ServiceAccountFor(hostOS) == "" && effectiveUID() == 0 {
			return fmt.Errorf("refusing to write a %s config as root: the launch agents run as "+
				"the operator who installed them, so a root-owned config is one they cannot "+
				"read, and `billet local up` refuses to run as root. Run this as yourself. If "+
				"the directory does not exist yet, make it theirs first:\n%s",
				initconfig.ProfileLocalService, chownAdvice(filepath.Dir(*cfgPath)))
		}

		if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o750); err != nil {
			// AND `sudo billet init` IS NOT THE WAY THROUGH, which is the whole
			// reason this is not a bare wrapped error. /usr/local is root-owned on
			// a stock Mac, so the service profile's very first command fails here
			// — and the obvious response leaves a root-owned config that the
			// launch agents, which run as the operator, cannot read. `billet local
			// up` refuses to run as root, so the deployment would then be stuck
			// with no error naming the cause. Same command `local up` gives for
			// the directories it needs, so the operator sees it once.
			if err := macServiceDirRemedy(*cfgPath, params.Profile, err); err != nil {
				return err
			}

			return fmt.Errorf("create the directory for %s: %w", *cfgPath, err)
		}
	}

	// THE FINAL BYTES ARE COMPLETE BEFORE ANYTHING TOUCHES DISK. The App
	// identity `github-app create` filled into the file being replaced (or the
	// edited file a .new lands beside — a merge base with zeroed ids would
	// send the operator back through onboarding against an App that exists) is
	// rendered into the generation in memory, so no crash can leave a config
	// that lost it. The key path stays the NEW generation's — the profile owns
	// it — with a printed move when it changed.
	final := []byte(body)
	carried := false
	var keyMovedFrom, keyMovedTo string
	if len(existingRaw) > 0 {
		gb, oldKeyPath, ok := existingGitHubBlock(existingRaw)
		switch {
		case ok && !gb.usable():
			// AN APP ID WITH NO ORG IS NOT AN IDENTITY TO CARRY. The carry writes
			// every field, so a blank org would overwrite the requested one and
			// leave real App ids pointing at no organization — while `carried`
			// suppressed the zero-ids remediation that would have said so.
			fmt.Fprintf(notes, "NOTE: the existing App (id %d) is not a complete identity "+
				"(org %q, installation %d), so it was NOT carried — carrying it would "+
				"overwrite good values with incomplete ones. Re-run "+
				"`billet github-app create` to record them together.\n\n",
				gb.AppID, gb.Org, gb.InstallationID)
		case ok && params.TargetPath() != "" && gb.scopePath() != params.TargetPath():
			// The App belongs to the OLD target; silently writing it under a new
			// one would pair an identity with an owner it is not installed on.
			// Said, not guessed around.
			fmt.Fprintf(notes, "NOTE: the existing App (id %d) belongs to %q, but this run is for %q — "+
				"the App identity was NOT carried. Run `billet github-app create` for %q, or "+
				"re-run with %s.\n\n", gb.AppID, gb.scopePath(), params.TargetPath(), params.TargetPath(),
				scopeFlag(gb.Org, gb.Repository))
		case ok:
			gb.PrivateKeyPath = configuredKeyPathOf(body)
			rendered, err := renderGitHubBlock(final, gb)
			if err != nil {
				return fmt.Errorf("carry the App identity into the generation: %w", err)
			}
			final = rendered
			carried = true

			if oldKeyPath != "" && oldKeyPath != gb.PrivateKeyPath {
				keyMovedFrom, keyMovedTo = oldKeyPath, gb.PrivateKeyPath
			}
		}
	}

	// NOTHING REVALIDATED THE CARRY. Generate proves the config it RENDERED
	// validates, and the App identity is written into those bytes afterwards — so
	// an identity that rendered a config config.Parse rejects reached the operator
	// with the command reporting success. `usable` refuses the cases known today;
	// this refuses the ones it does not, and keeps any field added to the carry
	// later inside the same guarantee.
	if carried {
		if err := checkCarried(final); err != nil {
			return err
		}
	}

	// SAID ONLY ONCE THE GENERATION WAS ACTUALLY DELIVERED. Hoisting this to run
	// before both destinations fixed an emission that never reached it, and broke
	// the file path's ordering instead: a failed write, chmod or rename would tell
	// the operator to move a credential for a config that does not exist. Built
	// here, called by whichever destination succeeds.
	sayKeyMoved := func() {
		if keyMovedFrom == "" {
			return
		}

		installCmd := fmt.Sprintf("install -m 0600 %s %s", shellArg(keyMovedFrom), shellArg(keyMovedTo))

		// AN ACCOUNT ONLY WHERE THERE IS ONE. macOS has no service account — the
		// launch agents run as the operator — so `-o billet -g billet` names a
		// user and group that do not exist and `install` refuses with `invalid
		// user`, on the one instruction that moves the credential GitHub issues
		// exactly once.
		if account := initconfig.ServiceAccountFor(targetOS); account != "" &&
			params.Profile == initconfig.ProfileLocalService {
			installCmd = fmt.Sprintf("install -o %s -g %s -m 0600 %s %s",
				account, account, shellArg(keyMovedFrom), shellArg(keyMovedTo))
		}
		where := "on the host that will run billet"
		if emitValue == emitAnsible {
			// TWO MACHINES ARE IN PLAY AND THE COMMAND NAMES A PATH ON EACH. The
			// source is whatever the config on THIS machine named; the
			// destination is on the host the role converges. Saying "the host
			// that will run billet" alone leaves an operator running a target-side
			// command against a source path that is here.
			where = "onto the host the role converges — the source below is the path the config " +
				"here names, so copy it across first"
		}

		fmt.Fprintf(notes, "NOTE: the App key path moved with the profile. Copy the key %s, "+
			"then remove the old copy only after `billet check` passes:\n  %s\n\n",
			where, installCmd)
	}

	if emitValue == emitAnsible {
		block, err := initconfig.AnsibleVars(string(final), initconfig.AnsibleCompanions(kind))
		if err != nil {
			return err
		}
		// CHECKED, because the intended use appends to a file. A full disk after
		// part of the block was accepted would otherwise exit 0 over a corrupted
		// inventory. This cannot un-append what landed; it makes the command say so.
		if _, err := os.Stdout.WriteString(block); err != nil {
			return fmt.Errorf("write the %s block: %w", initconfig.AnsibleVar, err)
		}

		sayKeyMoved()
		fmt.Fprintf(notes, "\nGenerated the %s block. Nothing was written.\n\n",
			initconfig.AnsibleVar)
		report()

		// THE ROLE'S DESTINATION IS FIXED, and it is not necessarily --config.
		// The role always renders to the service path; --config only says where
		// an existing App identity was read FROM. Naming --config as the
		// destination sent an operator who passed one looking for a file the
		// role never writes, and then told them to onboard an App that already
		// existed somewhere else.
		fmt.Fprintf(notes, "\nPut it under this host in the inventory the "+
			"junioryono.billet.host role reads; the role renders it to %s on the target. "+
			"The capacity above was measured HERE, so this must be the machine the role "+
			"converges.\n", initconfig.ServiceConfigPathFor(targetOS))
		if *cfgPath != initconfig.ServiceConfigPathFor(targetOS) {
			// AND THAT RE-RUN NEEDS PRIVILEGE. The role installs /etc/billet as
			// 0750 root:billet and the config 0640, so an operator who is not in
			// the service group cannot read what they are being told to read.
			fmt.Fprintf(notes, "\nAny existing App identity was read from %s, which is NOT "+
				"where the role writes. Once the role has converged, re-run against %s "+
				"instead — with privilege, because the role installs it 0640 root:%s — or "+
				"the next run reads a file the deployment does not use.\n",
				*cfgPath, initconfig.ServiceConfigPathFor(targetOS), initconfig.ServiceGroup)
		}

		// THE ONE THING THE BLOCK CANNOT CARRY. `billet_config` becomes
		// /etc/billet/billet.yaml, and the whole reason `dsn_env` names a variable
		// instead of holding a DSN is that the connection string carries a password
		// — so an inventory that pasted it in would have put the credential in the
		// place this design exists to keep it out of. The role has a separate
		// variable for it, and this is where an operator finds that out: without
		// it, the role converges cleanly and the control plane then fails to start
		// complaining about an empty data source.
		if params.State != nil && params.State.Backend == config.StatePostgres {
			fmt.Fprintf(notes, "\nThis block's ledger is in PostgreSQL, and the DSN is NOT in "+
				"it — %s names an environment variable, because a connection string carries a "+
				"password and this block becomes a file on the target. Set it separately:\n\n"+
				"  billet_server_environment:\n    %s: \"{{ vault_billet_state_dsn }}\"\n\n"+
				"The role writes that to /etc/billet/server.env (0640 root:%s) and names it in "+
				"the unit it renders — never a drop-in, because the transactional upgrade "+
				"refuses effective drop-ins it cannot replace and recover.\n",
				initconfig.AnsibleVar, params.State.DSNEnv, initconfig.ServiceGroup)
		}

		// A RE-EMISSION IS A FRESH GENERATION, NOT A MERGE. The file path plans a
		// re-run and writes beside anything it will not merge; this path has no
		// file to compare against and deliberately skips that plan, carrying only
		// the App identity forward. Everything an operator edited in the inventory
		// — an ec2 AMI in place of the placeholder, a pinned kernel, tiers, the
		// node name, a raised ceiling — is regenerated from flags, so replacing the
		// block wholesale silently discards it. The command writes nothing, but
		// saying "re-run this" without saying that causes the loss.
		if carried {
			fmt.Fprintf(notes, "\nThis is a fresh generation carrying only the App identity "+
				"forward. Anything edited in the inventory since the last emission — an ec2 "+
				"AMI, a pinned kernel, tiers, the node name, a raised ceiling — is NOT in it. "+
				"Diff it against the block in place before replacing.\n")
		}

		if !carried {
			// THE ORDERING HERE IS THE ROLE'S, NOT A PREFERENCE, and both of the
			// sequences this used to offer were dead ends. The role runs `billet
			// check` on every converge, and config refuses a zero app_id or
			// installation_id whether or not the server is enabled — so supplying
			// only the key does not help, and disabling the server does not skip
			// the validation. The identity has to be IN the block before the first
			// converge, which means creating the App against a config on this host
			// and emitting from that.
			fmt.Fprint(notes, noIdentityGuidance(*cfgPath, params.Org, params.Repository,
				generationFlags(generationInputs{
					org:             *org,
					repository:      *repository,
					provider:        *provider,
					image:           *image,
					runnerGroup:     *group,
					workflows:       workflows,
					listen:          *listen,
					region:          *region,
					subnet:          *subnet,
					securityGroups:  securityGroups,
					untrustedGroups: untrustedGroups,
					instanceTypes:   instanceTypes,
					priceOverrides:  priceOverrides,
					maxVCPU:         *maxVCPU,
					maxMemory:       *maxMemory,
					state:           params.State,
				})))
		}

		return nil
	}

	// 0640 for the service shape, 0600 otherwise: the packaged server unit runs
	// as user billet, so the config must be group-readable — the same contract
	// the package's postinstall sets (root:billet).
	//
	// WHERE THERE IS NO GROUP THERE IS NOTHING TO WIDEN IT FOR. A macOS launch
	// agent runs as the operator who wrote the file, so 0640 grants a read to
	// whichever group the file happened to inherit and buys nothing.
	mode := os.FileMode(0o600)
	if params.Profile == initconfig.ProfileLocalService &&
		initconfig.ServiceAccountFor(hostOS) != "" {
		mode = 0o640
	}

	if writePath != *cfgPath {
		// EXCLUSIVE: a previous re-run's .new may hold the operator's
		// half-finished merge — the exact content this path exists to protect.
		f, err := os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists — it may hold a merge in progress. Finish "+
					"or remove it, then re-run", writePath)
			}

			if remedy := macServiceDirRemedy(writePath, params.Profile, err); remedy != nil {
				return remedy
			}

			return fmt.Errorf("create %s: %w", writePath, err)
		}
		if _, err := f.Write(final); err != nil {
			_ = f.Close()

			return fmt.Errorf("write %s: %w", writePath, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("write %s: %w", writePath, err)
		}
	} else {
		// ATOMIC over the live path: a truncating write here is a crash away
		// from a deployment whose ONLY config is half a file. Staged beside,
		// synced, renamed.
		if err := commitConfig(writePath, final, mode); err != nil {
			// THE DIRECTORY CAN ALREADY EXIST AND STILL NOT BE YOURS, in which
			// case MkdirAll succeeded and this is where a stock Mac fails — with
			// a bare permission error naming nothing an operator can act on.
			if remedy := macServiceDirRemedy(writePath, params.Profile, err); remedy != nil {
				return remedy
			}

			return err
		}
	}

	// ENFORCED, NOT REQUESTED: the creation mode is reduced by the umask (the
	// server unit itself runs UMask=0077 hosts), so a service-shape config
	// could land 0600 and the unit could not read it — silently.
	if err := os.Chmod(writePath, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", writePath, err)
	}

	sayKeyMoved()

	if writePath != *cfgPath {
		fmt.Printf("Wrote %s — the existing %s was NOT touched.\n\n", writePath, *cfgPath)
		report()
		fmt.Printf("\nThe existing file carries content this command will not merge for you "+
			"(edited values, sites, extra tiers, or a different billet version's shape). "+
			"Compare and merge deliberately:\n\n  diff -u %s %s\n\nThen move the merged "+
			"result into place yourself.\n", shellArg(*cfgPath), shellArg(writePath))
		// SAME RULE AS THE NEXT STEPS: there is nothing to hand a file to on a
		// Mac. Worse than useless there — an operator who runs it under sudo to
		// make it work leaves a root-owned config that the agents, which run as
		// them, cannot read.
		if params.Profile == initconfig.ProfileLocalService {
			if account := initconfig.ServiceAccountFor(hostOS); account != "" {
				fmt.Printf("After moving it into place: chown root:%s %s && chmod 0640 %s\n",
					account, shellArg(*cfgPath), shellArg(*cfgPath))
			}

			// AND THEN SAY WHAT MAKES IT LIVE. This branch ended at "move the
			// merged result into place yourself", which leaves an operator
			// holding a config the running services have not read — and on a Mac
			// it was the whole of the output, because the chown above is the only
			// other thing it said.
			fmt.Printf("Then have the services read it:\n" +
				"  billet local down --reason 'merged a regenerated config'\n" +
				"  billet local up\n")
		}

		// THE OTHER RULE, SAID WHERE THIS ONE IS LEARNED. An operator meets both
		// commands in one sequence, and this branch is exactly where they find out
		// that init leaves their config alone — so it is where the difference
		// belongs, along with which of the two files the next command wants.
		fmt.Printf("\n%s So when you create the App, point it at %s — the file the deployment "+
			"reads — rather than at %s.\n", configEditRule, shellArg(*cfgPath), shellArg(writePath))

		return nil
	}

	if converged {
		fmt.Printf("Converged %s (it was pristine init output; nothing you wrote was lost)\n\n", *cfgPath)
	} else {
		fmt.Printf("Wrote %s\n\n", *cfgPath)
	}

	if params.Profile == initconfig.ProfileLocalService {
		serviceOwnership(*cfgPath)
	}

	report()
	warnIfListenBusy(ctx, params.Listen)
	printInitNextFor(*cfgPath, params, trusted, carried)

	return nil
}

// existingGitHubBlock reads the App identity out of the file being replaced —
// leniently, because the whole point is surviving a file the strict parser may
// not love. ok is false when there is no filled identity to carry.
func existingGitHubBlock(raw []byte) (githubBlock, string, bool) {
	return existingIdentity(raw, config.DefaultTargetName)
}

// identityDoc is the lenient shape of the identity-bearing blocks.
type identityDoc struct {
	Org            string `yaml:"org"`
	Repository     string `yaml:"repository"`
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	ClientID       string `yaml:"client_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// existingIdentity reads one target's App identity out of a config: the github
// block for the default target, the named targets entry for any other.
func existingIdentity(raw []byte, target string) (githubBlock, string, bool) {
	var doc struct {
		GitHub  identityDoc `yaml:"github"`
		Targets []struct {
			Name        string `yaml:"name"`
			identityDoc `yaml:",inline"`
		} `yaml:"targets"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return githubBlock{}, "", false
	}

	found := doc.GitHub
	name := config.DefaultTargetName

	if target != "" && target != config.DefaultTargetName {
		found = identityDoc{}
		name = target

		for _, entry := range doc.Targets {
			if entry.Name == target {
				found = entry.identityDoc

				break
			}
		}
	}

	if found.AppID == 0 {
		return githubBlock{}, "", false
	}

	return githubBlock{
		Target:         name,
		Org:            found.Org,
		Repository:     found.Repository,
		AppID:          found.AppID,
		InstallationID: found.InstallationID,
		ClientID:       found.ClientID,
	}, found.PrivateKeyPath, true
}

// targetName is the block's target name, spelled out for the default.
func (b githubBlock) targetName() string {
	if b.isDefault() {
		return config.DefaultTargetName
	}

	return b.Target
}

// describe names the block's scope the way an operator reads it, quoted.
func (b githubBlock) describe() string {
	if b.Repository != "" {
		return fmt.Sprintf("repository %q", b.Repository)
	}

	return fmt.Sprintf("org %q", b.Org)
}

// scopePath is the block's GitHub path, whichever scope it names.
func (b githubBlock) scopePath() string {
	if b.Repository != "" {
		return b.Repository
	}

	return b.Org
}

// configuredKeyPathOf reads the key path out of a generated body.
func configuredKeyPathOf(body string) string {
	var doc struct {
		GitHub struct {
			PrivateKeyPath string `yaml:"private_key_path"`
		} `yaml:"github"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return ""
	}

	return doc.GitHub.PrivateKeyPath
}

// warnIfListenBusy probes the generated listen address and says so when
// something already holds it — usually this deployment's own running server,
// which an operator re-running init mid-flight should hear about now rather
// than from a bind error at the next start.
func warnIfListenBusy(ctx context.Context, listen string) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", listen)
	if err != nil {
		fmt.Printf("NOTE: %s is already in use (likely a running billet server). The config "+
			"is written; a second server on this address will not start until the first "+
			"stops, and a different --listen needs both ends regenerated.\n\n", listen)

		return
	}

	_ = l.Close()
}

// serviceOwnership gives a freshly written local-service config the group the
// packaged server unit reads it with, and says what to do when it cannot.
//
// BEST-EFFORT WITH AN HONEST FALLBACK rather than an error: the billet group is
// created by the package's postinstall, so on a machine where the package is
// not installed yet there is nothing to chown to — and the config is still
// correct, it just cannot be read by a unit that also does not exist yet. The
// note names the exact remedy instead of leaving a permission failure for
// systemd to report later.
func serviceOwnership(path string) {
	// THERE IS NOTHING TO HAND OVER ON macOS. A launch agent runs as the operator
	// who installed it, so the file they just wrote is already the one the
	// service reads — and telling them to chown it to a `billet` group that does
	// not exist is a first instruction that cannot be followed.
	if initconfig.ServiceAccountFor(hostOS) == "" {
		return
	}

	grp, err := user.LookupGroup(initconfig.ServiceGroup)
	if err == nil {
		if gid, convErr := strconv.Atoi(grp.Gid); convErr == nil {
			if err := os.Chown(path, -1, gid); err == nil {
				return
			}
		}
	}

	// The remedy covers the DIRECTORY too: when init created /etc/billet itself
	// it is root:root 0750, and the billet group cannot traverse it no matter
	// what mode the file has.
	fmt.Printf("NOTE: could not set %s to group %s (the group the packaged billet-server "+
		"unit reads it with). Install the billet package — its postinstall creates the user and "+
		"group — or run `chown root:%s %s %s` before `billet local up`.\n\n",
		path, initconfig.ServiceGroup, initconfig.ServiceGroup, filepath.Dir(path), path)
}

// printInitNextFor is printInitNext, except when the App identity was carried
// over from the file being converged — an App that exists must not be created
// again, so the guidance starts at check.
// stateParams turns the two ledger flags into what the generator takes.
//
// PRESENCE DECIDES, NOT VALUE. Neither flag set means nil, which is the
// `state_dir` shorthand every generation has always written — a struct carrying
// an empty backend would instead reach checkStateParams as "a backend was
// selected and not named" and refuse a run nobody asked anything unusual of.
//
// An UNSET --state-backend beside a set --state-dsn-env resolves to sqlite rather
// than to nothing, deliberately: sqlite is what an absent flag means, so the
// operator gets "that variable means nothing without --state-backend postgres"
// instead of a sentence about a backend they never mentioned.
func stateParams(set map[string]bool, backend, dsnEnv string) *initconfig.StateParams {
	if !set["state-backend"] && !set["state-dsn-env"] {
		return nil
	}

	resolved := config.StateBackend(backend)
	if !set["state-backend"] {
		resolved = config.StateSQLite
	}

	return &initconfig.StateParams{Backend: resolved, DSNEnv: dsnEnv}
}

// refuseEmptyStateFlags rejects a ledger flag typed with nothing after it.
//
// A BLANK-BUT-PRESENT VALUE IS NOT AN ABSENT ONE, and only this layer can tell
// them apart: by the time the generator sees a StateParams, `--state-dsn-env=`
// and "no flag at all" are both the empty string — and under the sqlite default
// that is exactly what saying nothing looks like, so the flag was accepted and
// silently discarded. The same rule --runner-group and the tart flags follow.
//
// SEPARATE FROM stateParams because the two answer different questions: this one
// can refuse and that one cannot fail, so folding them together would mean a
// builder that returns a nil value beside a nil error for the ordinary case.
func refuseEmptyStateFlags(set map[string]bool, backend, dsnEnv string) error {
	for _, f := range []struct {
		name  string
		value string
		what  string
	}{
		{"--state-backend", backend, "it selects where the control-plane ledger lives, and " +
			"an empty selection is not one of the two backends billet has"},
		{"--state-dsn-env", dsnEnv, "it names the environment variable billet reads the " +
			"PostgreSQL connection string from, and an empty name is one nothing could export"},
	} {
		if set[strings.TrimPrefix(f.name, "--")] && strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("%s was given with no value: %s. Omit the flag, or give it one",
				f.name, f.what)
		}
	}

	return nil
}

// printStateNext says what a PostgreSQL generation still needs, which is the one
// thing the config deliberately does not carry.
//
// BEFORE THE NUMBERED STEPS, and printed on every path including the one that
// carried an App identity over: the DSN is about the service's ENVIRONMENT rather
// than about GitHub, so a regenerated config needs it just as much as a fresh
// one. Without it the control plane starts, finds the variable empty, and
// complains about a data source rather than about the step nobody was told to
// take.
func printStateNext(p initconfig.Params) {
	if p.State == nil || p.State.Backend != config.StatePostgres {
		return
	}

	fmt.Printf("\nThis config's ledger is in PostgreSQL, and the connection string is NAMED "+
		"rather than written: billet reads it from $%s, because a DSN carries a password and a "+
		"secret in this file ends up in a backup. Nothing starts until that variable is "+
		"exported to the control plane.\n", p.State.DSNEnv)

	if p.Profile == initconfig.ProfileLocalService {
		fmt.Printf("\nFor the packaged unit, an EnvironmentFile is the place for it — the " +
			"junioryono.billet.host role writes /etc/billet/server.env from " +
			"billet_server_environment and names it in the unit it renders. Never a systemd " +
			"drop-in: the transactional host upgrade refuses effective drop-ins it cannot " +
			"replace and recover.\n")
	}

	fmt.Printf("\nWhat billet needs of that database: a schema of its own (point the DSN's " +
		"search_path at it), a role that can create tables in it, synchronous_commit not off " +
		"— billet checks that at startup and refuses — and PostgreSQL 13 or later. No " +
		"extension.\n")

	fmt.Printf("\nAnd `billet local backup` REFUSES the ledger on this profile. It archives " +
		"server.identity_dir — the deployment identity, the node-wire CA and the App key — and " +
		"a consistent copy of the database is pg_dump or your provider's snapshot, which is " +
		"yours to take. Restore the two halves from the same moment.\n")
}

func printInitNextFor(cfgPath string, p initconfig.Params, trusted, carried bool) {
	if !carried {
		printInitNext(cfgPath, p, trusted)

		return
	}

	printStateNext(p)

	pathArg := shellArg(cfgPath)

	fmt.Printf("\nNext (the GitHub App identity was carried over — do not create it again):\n\n")
	fmt.Printf("  1. Confirm the config, its host prerequisites and any runner-group policy:\n")
	fmt.Printf("       billet check --config %s\n", pathArg)
	if p.Profile == initconfig.ProfileLocalService {
		// A DRAIN AND A START, NOT A RESTART. `systemctl restart` does not exist
		// on a Mac at all, and on Linux it skips both halves that matter: `down`
		// seals admission and waits for the jobs already running rather than
		// killing them, and `up` re-proves the App against the regenerated file
		// before a control plane starts on somebody's organization.
		fmt.Printf("  2. Drain the host and bring it back on the regenerated file:\n")
		fmt.Printf("       billet local down --reason 'regenerated the config'\n")
		fmt.Printf("       billet local up\n")
	} else {
		fmt.Printf("  2. Restart the control plane and the compute host so they read the " +
			"regenerated file:\n")
		fmt.Printf("       billet server --config %s\n", pathArg)
		fmt.Printf("       billet node   --config %s\n", pathArg)
	}
}

// printInitNext walks the operator from a written config to a running trial, in
// the order the steps actually depend on each other.
//
// SPELLED OUT BECAUSE THE TRIAL IS SAFE ONLY AT THE END. A docker tier is trusted
// and bound to a runner group and workflow allowlist, so the only jobs that reach
// it are the workflows named here from the repositories the group permits.
//
// It does NOT tie trust to the job's event: launch authority is the tier's static
// `trust:`, and pool runners are not bound to the assignment that created them, so
// billet cannot refuse a single `pull_request` job while accepting a `push` to the
// same allowlisted workflow. A docker container shares the host kernel, so the
// honest instruction is to treat everything that runs on a docker tier as trusted
// — never let an allowlisted workflow check out or execute a revision you do not
// control, such as a fork's pull request. A firecracker guest has its own kernel,
// so its tiers default to untrusted and need host prep the config only names.
//
// trusted is what Generate DECIDED, not what the provider defaults to: a
// firecracker host given a runner group and workflow allowlist emits trusted
// tiers, and telling that operator their jobs run isolated on the untrusted
// bridge would be a dangerous falsehood — the guidance follows the real trust.
func printInitNext(cfgPath string, p initconfig.Params, trusted bool) {
	kind, profile := p.Provider, p.Profile

	// These lines are meant to be copy-pasted, so every interpolated value is
	// shell-quoted. A config path with a space would otherwise split into two
	// arguments. The absent-org placeholder is `<your-org>` — unmistakably a
	// placeholder rather than a real organization someone might paste as-is — and
	// shellArg single-quotes it so the shell does not read its `<` as input
	// redirection. An ordinary path or org name is left untouched.
	orgFlag := scopeFlag(p.Org, p.Repository)
	pathArg := shellArg(cfgPath)

	if kind == config.ProviderFirecracker {
		fmt.Printf("\nBefore this config can launch a guest, the host must provide what billet " +
			"cannot: a guest kernel you place at node.firecracker.kernel_image yourself, the two " +
			"bridges node.firecracker names, and a Ceph cluster for the cache. `billet check` " +
			"reports what is missing; the junioryono.billet.host Ansible role installs the bridges " +
			"and bootstraps Ceph, but the guest kernel is yours to build (see the config's comment " +
			"on kernel_image).\n")
	}

	if kind == config.ProviderTart {
		printTartNext(cfgPath, trusted, p.Tart)
	}

	printStateNext(p)

	if kind == config.ProviderEC2 {
		fmt.Printf("\nEvery tier's `image:` is a PLACEHOLDER (%s): an EC2 tier launches an AMI, and "+
			"none exists yet. Build one and paste its id over the placeholder:\n\n", initconfig.PlaceholderAMI)
		fmt.Printf("       billet ami build --config %s --base-image ami-<a dnf-based base>\n\n", pathArg)
		fmt.Printf("The orchestrator's own AWS role needs an IAM policy. `billet init iam` prints " +
			"exactly what this config exercises, scoped to this deployment:\n\n")
		fmt.Printf("       billet init iam --config %s\n", pathArg)
	}

	fmt.Printf("\nNext:\n\n")
	// THE CLAUSE, NOT THE PARAGRAPH. These are numbered steps an operator reads
	// as a list of commands, and the full rule four lines deep inside step 1
	// buries the steps either side of it; the command that is about to act says
	// the whole thing, before it acts.
	fmt.Printf("  1. Create the GitHub App and install it (%s):\n", configEditBrief)
	fmt.Printf("       billet github-app create %s --config %s\n", orgFlag, pathArg)
	fmt.Printf("  2. Confirm the config, its host prerequisites and any runner-group policy:\n")
	fmt.Printf("       billet check --config %s\n", pathArg)
	if profile == initconfig.ProfileLocalService {
		step := 3
		// The units' ExecStart reads exactly one path. A config generated
		// anywhere else must be installed there, or systemctl starts whatever
		// stale file that path holds — so the guidance says so instead of
		// pretending the staged file is live.
		service := initconfig.ServiceConfigPathFor(hostOS)
		account := initconfig.ServiceAccountFor(hostOS)

		if cfgPath != service {
			fmt.Printf("  3. Install the file where the services billet ships read it:\n")
			fmt.Printf("       cp %s %s\n", pathArg, shellArg(service))

			// ONLY WHERE THERE IS AN ACCOUNT TO HAND IT TO. On macOS the services
			// run as the operator, so a chown to a group that does not exist is
			// an instruction that fails and explains nothing.
			if account != "" {
				fmt.Printf("       chown root:%s %s && chmod 0640 %s\n",
					account, shellArg(service), shellArg(service))
			}

			step = 4
		}

		// `billet local up` ON BOTH, rather than the manager's own command. It
		// is the command that checks the credential before starting a control
		// plane on somebody's organization, proves each service held its process,
		// and enables only what it proved — none of which `systemctl enable
		// --now` or `launchctl bootstrap` does, and the second of which cannot
		// even be spelled on macOS without the override database.
		fmt.Printf("  %d. Start both roles as the services billet ships:\n", step)
		fmt.Printf("       billet local up\n")

		// THE KEY PATH IS THE ONE THIS CONFIG NAMES, read from the generator
		// rather than rebuilt here. It used to be a literal
		// /etc/billet/app-private-key.pem, which on a Mac is neither where the
		// config points nor a directory that exists.
		key := initconfig.ServiceKeyPathFor(hostOS)

		if account != "" {
			fmt.Printf("\nThe App key at %s must be readable by the service alone: "+
				"`chown %s:%s` it with mode 0600, or billet refuses a key other accounts can "+
				"read.\n", key, account, account)
		} else {
			fmt.Printf("\nThe App key at %s must be readable by you alone, mode 0600 — billet "+
				"refuses a key other accounts can read. The services run as you, so there is "+
				"nothing to chown it to.\n", key)
		}
	} else {
		fmt.Printf("  3. Run the control plane and a compute host, in two terminals:\n")
		fmt.Printf("       billet server --config %s\n", pathArg)
		fmt.Printf("       billet node   --config %s\n", pathArg)
	}

	// Untrusted is reachable on firecracker and ec2; docker always errors before
	// here without a policy, so a docker config that got printed is trusted.
	if !trusted && kind == config.ProviderFirecracker {
		fmt.Printf("\nThe tiers are `trust: untrusted`: a firecracker guest runs under its own " +
			"kernel on the untrusted bridge, so a fork's pull request is isolated from the host. " +
			"Add a trusted tier only with a runner group and workflow allowlist.\n")
		return
	}

	if !trusted && kind == config.ProviderTart {
		fmt.Printf("\nThe tiers are `trust: untrusted`: each job runs in its own VM with its " +
			"own kernel, and softnet confines that VM's network — a tart guest isolates the " +
			"kernel but not the bridge, and tart's default NAT reaches the host. Add a trusted " +
			"tier only with a runner group and workflow allowlist.\n")

		return
	}

	if !trusted && kind == config.ProviderEC2 {
		fmt.Printf("\nThe tiers are `trust: untrusted`: each job runs on its own EC2 instance in " +
			"the untrusted security group, so a fork's pull request is isolated by the instance " +
			"boundary and reaches only what that group allows. Add a trusted tier only with a " +
			"runner group and workflow allowlist.\n")
		return
	}

	fmt.Printf("\nThe tiers are `trust: trusted`: only the workflows you allowlisted, from " +
		"repositories your runner group permits, reach them. Launch authority is the tier's " +
		"static trust, not the job's event, so an allowlisted workflow must never check out or " +
		"run code you do not control — a fork's pull request included.\n")
}

// shellArg renders s so it survives being pasted into a POSIX shell as one word.
//
// A value that is already a single shell word — the common case, an ordinary
// path or org name — is returned unchanged so the printed command reads
// naturally. Anything else is wrapped in single quotes, with each embedded
// single quote written as the four-character break-out quote-backslash-quote-quote,
// because inside single quotes the shell treats every other byte literally. An
// empty string becomes an empty single-quoted pair so it stays a visible, valid
// argument rather than vanishing.
func shellArg(s string) string {
	if s == "" {
		return "''"
	}

	for _, r := range s {
		if !shellSafeRune(r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}

	return s
}

// shellSafeRune reports whether r can stand unquoted in a POSIX shell word. The
// set is deliberately conservative — alphanumerics and the punctuation a path,
// org or flag value ordinarily carries — so anything outside it is quoted rather
// than reasoned about.
func shellSafeRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		strings.ContainsRune("_-./@:", r)
}

// commitConfig replaces (or creates) a config file atomically: the bytes are
// staged beside the destination, synced, and renamed into place — a truncating
// write over the only record of where a deployment's data lives is a crash
// away from losing it.
//
// THE STAGED FILE IS ONE THIS CALL CREATED, NEVER <path>.tmp. That name is
// predictable and O_TRUNC takes whatever holds it — which is how the identical
// staging in `github-app create` destroyed an App private key an operator had
// pointed --key-path at exactly that name, a credential GitHub issues once and
// will not re-issue. `init` writes no key itself, so it reaches that state only
// through one an earlier run put there; the NAME is the hazard either way, and
// fixing one of two identical writers leaves a second entry point that does
// not enforce the rule. CreateTemp opens O_EXCL under a name nothing else can
// be holding.
//
// OWNERSHIP IS DELIBERATELY NOT PRESERVED HERE, unlike in the sibling writer.
// This one also creates files that did not exist, and where it does replace
// one, `serviceOwnership` is what hands the result to the group the packaged
// unit reads it with — adding a second answer would be two rules for one fact.
func commitConfig(path string, body []byte, mode os.FileMode) error {
	staged, err := os.CreateTemp(filepath.Dir(path), ".billet-config-*")
	if err != nil {
		return fmt.Errorf("stage a replacement for %s: %w", path, err)
	}

	name := staged.Name()

	committed := false

	// The descriptor is held to the end, so the mode is set through it and the
	// removal can ask os.SameFile first. Go unlinks by NAME, and this directory
	// is where an operator's App key may live; "not ours" is not "removed".
	defer func() {
		if !committed && verifyInstalled(staged, name) == identityMatches {
			_ = os.Remove(name)
		}

		_ = staged.Close()
	}()

	if _, err := staged.Write(body); err != nil {
		return fmt.Errorf("stage a replacement for %s: %w", path, err)
	}

	// THROUGH THE DESCRIPTOR, and before the sync. CreateTemp opens 0600, so the
	// caller's mode is applied here rather than at open — and the sync has to
	// follow it, or a crash can leave the file durable with the mode it was
	// created with. The call site chmods again after the rename for a different
	// reason: a creation mode is reduced by the umask and a chmod is not.
	//
	// Its mutant therefore SURVIVES — that second chmod repairs the final mode
	// whatever this one does. What it buys is that the file is already right when
	// the rename publishes it, rather than briefly 0600 and unreadable to the
	// service unit; that window is too small for a test to observe, which is why
	// this is said rather than covered. The removal below is the same: on the
	// path that succeeds the rename has already taken the file, and the failure
	// paths that would leave one are the filesystem errors no test can arrange.
	if err := staged.Chmod(mode); err != nil {
		return fmt.Errorf("set the mode of the replacement for %s: %w", path, err)
	}

	if err := staged.Sync(); err != nil {
		return fmt.Errorf("sync the replacement for %s: %w", path, err)
	}

	// Check-then-act, and it cannot be made atomic with the rename; it is here
	// for the reason the key install checks before its os.Link, and the residual
	// is the one recorded there.
	if verifyInstalled(staged, name) != identityMatches {
		return fmt.Errorf("%s is no longer the file this run staged, so it was NOT installed "+
			"as %s", name, path)
	}

	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	committed = true

	// AND THE RENAME IS MADE DURABLE. Syncing the file persists its contents and
	// its metadata, not the directory entry that publishes them — so a power loss
	// could lose a config this command reported writing, or restore the one it
	// replaced. That is precisely what this function's staging exists to prevent,
	// and syncDir says so where it is defined.
	//
	// A FAILURE HERE IS NOT THIS CALL'S FAILURE. The config is in place; telling
	// the caller the replace failed would be false, and would send an operator
	// looking for a file that is already there.
	if err := syncDir(filepath.Dir(path)); err != nil {
		fmt.Fprintf(os.Stderr, "\nWarning: %s was written, but that could not be flushed to "+
			"disk (%v). Confirm it is still there after a reboot.\n", path, err)
	}

	return nil
}

// refuseIdentityMove is the deployment-identity refusal, applied only to the
// paths that replace the existing config: a state directory holding a
// deployment-id is a LIVE deployment — its containers are labelled with that
// identity, its ledger accounts for them — and an init that points the config
// away from it (or at a different directory that holds one) silently orphans
// everything under the old identity. There is no override flag on purpose,
// and --force does not cross it: the honest path is `billet decommission` and
// `billet teardown`, which retire the compute before the pointer moves.
//
// FAIL CLOSED ON EVERY UNCERTAINTY: a file whose YAML cannot be read may
// still name live state, and an identity probe that errors is not an identity
// that is absent.
func refuseIdentityMove(cfgPath string, existingRaw []byte, newState string) error {
	oldState, ok := initconfig.ExistingServerStateDir(existingRaw)
	if !ok {
		return fmt.Errorf("%s exists but its YAML cannot be read, so billet cannot tell "+
			"whether it points at a live deployment — refusing to replace it. If it is "+
			"garbage, delete it yourself and re-run", cfgPath)
	}

	if oldState != "" && sameDir(oldState, newState) {
		return nil
	}

	// A node-only config has no old server state to protect, but the NEW
	// directory can still hold someone else's identity — the adoption probe
	// below runs regardless.
	if oldState == "" {
		return refuseIdentityAdoption(cfgPath, newState)
	}

	switch state.ProbeDeploymentID(oldState) {
	case state.IdentityAbsent:
	case state.IdentityPresent:
		return fmt.Errorf("%s points its server state at %s, which holds a live "+
			"deployment identity, and this init would point it at %s instead — "+
			"orphaning every container and lease under the old identity. Retire the "+
			"deployment first (`billet decommission`, then `billet teardown`), or keep "+
			"the same state directory", cfgPath, oldState, newState)
	case state.IdentityUnknown:
		return fmt.Errorf("%s points its server state at %s, and billet cannot read that "+
			"directory to rule out a live deployment identity — refusing to point the "+
			"config elsewhere. Fix the directory's permissions, or retire the deployment "+
			"(`billet decommission`, then `billet teardown`)", cfgPath, oldState)
	}

	return refuseIdentityAdoption(cfgPath, newState)
}

// refuseIdentityAdoption is the second half of the refusal: pointing a config
// at a directory that already holds a DIFFERENT deployment's identity mixes
// two deployments' compute under one file.
func refuseIdentityAdoption(cfgPath, newState string) error {
	switch state.ProbeDeploymentID(newState) {
	case state.IdentityAbsent:
	case state.IdentityPresent:
		return fmt.Errorf("this init would point %s at %s, which already holds a "+
			"different deployment's identity — adopting it silently would mix two "+
			"deployments' compute. Retire that deployment first (`billet decommission`, "+
			"then `billet teardown`), or choose a different state directory", cfgPath, newState)
	case state.IdentityUnknown:
		return fmt.Errorf("this init would point %s at %s, and billet cannot read that "+
			"directory to rule out an existing deployment identity — refusing. Fix the "+
			"directory's permissions first", cfgPath, newState)
	}

	return nil
}

// sameDir reports whether two paths name the same directory, resolving
// symlinks and lexical noise so a trailing slash cannot turn an in-place
// re-run into a hard refusal. Uncertainty (either resolution failing) reports
// NOT-same, which sends the caller to the identity probes — the fail-closed
// direction.
func sameDir(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}

	// Absolute BEFORE resolving: EvalSymlinks preserves relativeness, so a
	// relative spelling of the same directory would otherwise compare unequal
	// and turn an in-place re-run into a refusal.
	if abs, err := filepath.Abs(ca); err == nil {
		ca = abs
	}
	if abs, err := filepath.Abs(cb); err == nil {
		cb = abs
	}
	if ca == cb {
		return true
	}

	ra, errA := filepath.EvalSymlinks(ca)
	rb, errB := filepath.EvalSymlinks(cb)
	if errA == nil && errB == nil && ra == rb {
		return true
	}

	return false
}

// usable reports whether an existing App identity is complete enough to carry.
//
// A NONZERO APP ID WAS ONCE ENOUGH, and it is not: the carry writes every github
// field at once, so a blank org or a missing installation id overwrites good
// values with incomplete ones — and it happens AFTER Generate has run its
// config.Parse round trip, so nothing revalidates the result and the command
// reports success over a config that will not load.
func (b githubBlock) usable() bool {
	return strings.TrimSpace(b.scopePath()) != "" && b.AppID > 0 && b.InstallationID > 0
}

// wantsAnsibleEmission reports whether these raw args ask for the inventory
// emission, before the flag set has parsed them.
//
// Needed because the emission's whole contract is that stdout carries only the
// block, and flag.FlagSet decides where to write a parse error before any value
// is available to consult. Both spellings, and nothing else: this only chooses a
// stream, so a false negative prints a diagnostic where it always used to.
func wantsAnsibleEmission(args []string) bool {
	for i, a := range args {
		if a == "--emit="+string(emitAnsible) || a == "-emit="+string(emitAnsible) {
			return true
		}
		if (a == "--emit" || a == "-emit") && i+1 < len(args) && args[i+1] == string(emitAnsible) {
			return true
		}
	}

	return false
}

// shellArgs quotes a whole argument list for pasting into a shell.
func shellArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, shellArg(a))
	}

	return strings.Join(quoted, " ")
}

// generationInputs are the parsed flag values that DESCRIBE a deployment, as
// opposed to the ones that say where the generation goes.
type generationInputs struct {
	org, repository, provider, image, runnerGroup, listen string
	workflows                                             []string
	region, subnet, maxMemory                             string
	securityGroups, untrustedGroups                       []string
	instanceTypes, priceOverrides                         []string
	maxVCPU                                               int
	// state is where the ledger lives. It is NOT ec2 placement and not a tier
	// property — it describes the control plane — but it belongs here for the same
	// reason everything else does: the printed re-emit has to produce the same
	// file, and a dropped --state-backend produces a SQLite config for a
	// deployment whose database already exists.
	state *initconfig.StateParams
}

// generationFlags rebuilds the flags that describe this deployment, canonically.
//
// FROM THE PARSED VALUES, NOT FROM ARGV. The first version of this edited the raw
// arguments, dropping the two destination flags and whatever followed them — and
// a raw scan cannot tell a flag from a VALUE that looks like one. `--runner-group
// --config` is a legal invocation whose runner group is the string "--config";
// the scan removed it as a destination flag and swallowed the next argument as
// its value, printing two commands that do not run. Rebuilding from what the flag
// package actually parsed cannot make that mistake.
//
// --profile is deliberately absent. It selects PATHS, not the deployment, and the
// service shape puts the App key under /etc/billet — which `github-app create`
// then tries to create as an ordinary user, and cannot. The scratch config is for
// minting an identity; the emission that follows is service-shaped on its own.
func generationFlags(in generationInputs) []string {
	var out []string

	add := func(name, value string) {
		if value != "" {
			out = append(out, "--"+name, value)
		}
	}
	addEach := func(name string, values []string) {
		for _, v := range values {
			out = append(out, "--"+name, v)
		}
	}

	add("org", in.org)
	add("repository", in.repository)
	add("provider", in.provider)
	add("image", in.image)
	add("runner-group", in.runnerGroup)
	addEach("workflow", in.workflows)
	add("listen", in.listen)

	// WHERE THE LEDGER LIVES, or the re-emit silently produces a SQLite config
	// for a deployment whose database is already provisioned — and the two
	// spellings are mutually exclusive, so nothing downstream would flag it.
	if in.state != nil {
		add("state-backend", string(in.state.Backend))
		add("state-dsn-env", in.state.DSNEnv)
	}

	// The ec2 placement, which billet cannot detect and the bootstrap therefore
	// cannot infer either.
	add("region", in.region)
	add("subnet", in.subnet)
	addEach("security-group", in.securityGroups)
	addEach("untrusted-security-group", in.untrustedGroups)
	addEach("instance-type", in.instanceTypes)
	addEach("price", in.priceOverrides)
	if in.maxVCPU > 0 {
		out = append(out, "--max-vcpu", strconv.Itoa(in.maxVCPU))
	}
	add("max-memory", in.maxMemory)

	return out
}

// checkCarried proves the bytes an App identity was written into still load.
//
// Generate validates the config it RENDERED, and the identity is written into
// those bytes afterwards, so nothing between the two would notice an identity
// that renders a config config.Parse rejects. `usable` refuses every shape able
// to do that today — which is why this has no reachable failing case through the
// CLI, and why it is a function rather than four inline lines: a test can reach
// it, so the guard is covered rather than merely present.
//
// It exists for the field nobody has added yet. `installation_id` was carried
// without being checked until a review found it, and the next field added to
// renderGitHubBlock would arrive the same way.
func checkCarried(final []byte) error {
	if _, err := config.Parse("the config billet generated", final); err != nil {
		return fmt.Errorf("carrying the existing App identity produced a config that "+
			"does not load: %w", err)
	}

	return nil
}

// scopeFlag renders the flag that names a target for a pasted command: the
// organization, the repository, or the quoted placeholder an operator fills in.
//
// QUOTED, because a shell reads `<your-org>` as input redirection.
func scopeFlag(org, repository string) string {
	switch {
	case repository != "":
		return "--repository " + shellArg(repository)
	case org != "":
		return "--org " + shellArg(org)
	default:
		return "--org " + shellArg("<your-org>")
	}
}

// bootstrapIdentity is the file the printed bootstrap mints an App into. It
// holds only a github block, never a deployment.
const bootstrapIdentity = "~/billet-app.yaml"

// bootstrapSeed is what `github-app create --config` can write an identity into.
//
// It writes INTO an existing file — it does not create one — and what it needs is
// NARROWER than "any YAML mapping": a document whose root is a mapping and whose
// `github` key is a mapping or absent. A `github` key holding a sequence or a
// scalar is not converted, and duplicate keys re-encode into something the
// identity reader then rejects. So this exact seed is printed rather than a
// general claim about what would work.
//
// `github: {}` and not a bare `github:`, which parses as a null value that
// renderGitHubBlock silently declines to fill — no error, no block, and an
// operator told their identity did not carry with nothing to explain it.
const bootstrapSeed = "github: {}"

// noIdentityGuidance is what to do when the emitted block carries no App.
//
// A FUNCTION, because through the CLI this text is only reachable by generating
// for each backend — and ec2 generation performs a live AWS fetch, so the branch
// that used to exist here had no test at all and its mutation survived. Returned
// as a string so a test can read what an operator would read.
//
// IT TAKES NO PROVIDER, and that is the point rather than an omission: there is
// nothing backend-specific left for a future edit to get wrong.
//
// The role runs `billet check` on every converge and config refuses a zero
// app_id whether or not the server is enabled, so a block with no identity
// cannot converge as it stands.
//
// THE IDENTITY IS MINTED WITHOUT GENERATING ANYTHING. Three earlier versions of
// this printed `billet init --config <scratch>` first, and every one of them was
// wrong: it defaults to docker and refuses without a runner group and workflow,
// it dropped the flags the operator had used, and for ec2 it re-resolved
// --instance-type against live AWS — shape vCPU and memory are always fetched,
// even when every price is overridden — so the recipe needed credentials that
// can expire during onboarding and could resolve different shapes than the block
// it was bootstrapping. `github-app create` never needed a generated config: it
// writes a github block into any YAML mapping. So the App is minted into a file
// that holds nothing else, the emission re-runs against it, and the only AWS
// lookup is the one that emission was always going to perform. One recipe, every
// backend.
//
// The seed is written with the shell's noclobber flag set. After a successful run
// that file is the only local record of the App id, installation id and key path,
// and a plain `>` truncated it before reserveKeyFile noticed the key already
// existed and refused — the key survived, the identity record did not.
func noIdentityGuidance(cfgPath, org, repository string, flags []string) string {
	rest := shellArgs(flags)
	if rest != "" {
		rest = " " + rest
	}

	// QUOTED, because a shell reads `<your-org>` as input redirection: the
	// placeholder vanished from argv, or the shell failed before billet started,
	// and the App was never created.
	orgFlag := scopeFlag(org, repository)

	// AT CONFIG LOAD, NOT AT THE GITHUB PROBE. Measured on a real host by the
	// session that owns `billet local up`: a zero app_id is rejected by
	// config.Load before anything reaches GitHub, so the config cannot be LOADED
	// — which is worth saying exactly, because it tells an operator that no flag
	// and no environment variable gets them past it. BILLET_MAINTENANCE=1 skips
	// the GitHub verification and not this.
	return fmt.Sprintf("\nThe App ids are zero: no App identity was found at %s. A zero "+
		"app_id is refused when the config is LOADED, before anything reaches GitHub, so "+
		"the role cannot converge this block at all — not with the server disabled, and "+
		"not under BILLET_MAINTENANCE. Mint an App and re-emit against it:\n"+
		"  (set -C; printf '%%s\\n' %s > %s)\n"+
		"  billet github-app create %s --config %s\n"+
		"  billet init --emit ansible --config %s%s\n"+
		"then paste that block and pass the App key to the role as "+
		"BILLET_GITHUB_PRIVATE_KEY_PATH on the first converge.\n\n"+
		"That mints a NEW App. `set -C` is why the first command REFUSES an existing "+
		"%s rather than truncating it: after a successful run that file is the only "+
		"local record of the App id, installation id and key path, and the key "+
		"destination's own refusal comes too late to save it. If you already have an "+
		"App, point --config at the file holding it instead of running these; to mint "+
		"a second, use a fresh path and a fresh --key-path.\n",
		cfgPath, shellArg(bootstrapSeed), bootstrapIdentity,
		orgFlag, bootstrapIdentity, bootstrapIdentity, rest, bootstrapIdentity)
}

// chownAdvice is the command that gives a macOS service directory to whoever
// will run the launch agents.
func chownAdvice(dir string) string {
	// THE PROCESS'S OWN uid, AND NOTHING FROM THE ENVIRONMENT.
	//
	// Two review rounds found defects here, both of the same class, because two
	// versions of this tried to NAME the operator. SUDO_USER is an environment
	// variable that lands in a command pasted into a root shell — and shellArg
	// closes command injection but not option injection, since `-` is an
	// ordinary shell-safe character, so `--reference=/etc/shadow` was a valid
	// chown OPTION rather than an owner. Validating the shape fixed that one and
	// left the other: only the NAME `root` was refused, so a uid-0 account
	// called something else was handed back, and in a direct root shell every
	// source of an identity says root anyway.
	//
	// A uid needs no validation, cannot be an option, and cannot be wrong: this
	// process IS the operator whenever it is not root.
	//
	// `actor()` in drain.go DOES read SUDO_USER, and the difference is the one
	// its own comment draws: that is a LABEL written into the ledger so a person
	// can be found, and being wrong costs an unhelpful attribution. This is a
	// command run as root, where being wrong costs whatever the command does.
	// Do not unify them.
	if uid := effectiveUID(); uid != 0 {
		return fmt.Sprintf("  sudo mkdir -p %s && sudo chown %d %s", shellArg(dir), uid, shellArg(dir))
	}

	// AS ROOT THERE IS NOBODY TO NAME, and the command must not be pasted where
	// it is being read. `$(id -un)` is correct in the operator's OWN shell and
	// resolves to root in this one, so the sentence has to move them first —
	// which is the whole content of this branch, and why it is prose rather than
	// a bare command line.
	return fmt.Sprintf("Leave the root shell, then as the account that will run the agents:\n"+
		"  sudo mkdir -p %s && sudo chown \"$(id -un)\" %s", shellArg(dir), shellArg(dir))
}

// effectiveUID is os.Geteuid behind a seam, so the root refusal can be tested.
// Without one the refusal was unreachable: a test cannot become root, and
// changing its guard to `&& false` passed the whole suite.
var effectiveUID = os.Geteuid

// serviceConfigDir is the directory the services billet ships read their config
// from, behind a seam.
//
// BOTH CALL SITES OF THE REMEDY ARE ONLY TESTABLE THROUGH IT. The remedy is
// scoped to that directory deliberately, and a test cannot own /usr/local — so
// without this, deleting either call left every assertion green while restoring
// the exact bare-permission-error regression they were added for.
var serviceConfigDir = func() string {
	return filepath.Dir(initconfig.ServiceConfigPathFor(hostOS))
}

// dirWritable answers whether this process may create a file in dir.
//
// ACCESS RATHER THAN A WRITE PROBE. One caller is already on a failure path,
// where creating a file to find out would be a second thing that can fail; the
// other is deciding whether to let an irreversible App creation proceed, and a
// probe file left in an operator's config directory is litter it would then have
// to clean up. Both read it ONE-DIRECTIONALLY — only a definite no decides
// anything — because Access answers for the real uid and a mount can go
// read-only afterwards.
//
// Behind a seam for the same reason as effectiveUID: a test cannot arrange the
// real state of either directory it is asked about, the macOS service one least
// of all, since that depends on what /usr/local happens to hold on that machine.
var dirWritable = func(dir string) bool { return unix.Access(dir, unix.W_OK) == nil }

// macServiceDirRemedy turns a permission failure under the macOS service path
// into the command that resolves it, or nil when this is not that situation.
//
// SCOPED TO THE SERVICE DIRECTORY. The advice is "this is root-owned on a stock
// Mac", which is a claim about /usr/local and not about whatever an operator
// passed to --config — printing it for an arbitrary EACCES tells them something
// false about their own path. EROFS, ELOOP and ENOTDIR stay ordinary errors,
// because a chown does not fix any of them.
func macServiceDirRemedy(path string, profile initconfig.Profile, cause error) error {
	if profile != initconfig.ProfileLocalService ||
		initconfig.ServiceAccountFor(hostOS) != "" ||
		!errors.Is(cause, os.ErrPermission) {
		return nil
	}

	dir := filepath.Dir(path)
	if !sameDir(dir, serviceConfigDir()) {
		return nil
	}

	// AND THE DIRECTORY REALLY IS THE PROBLEM. commitConfig wraps staging,
	// writing, syncing, closing AND renaming, so translating any permission
	// error from it into "this directory is root-owned" claims something the
	// error does not say — a rename can return EPERM for an immutable
	// destination or an ACL, having already proved the directory writable.
	// Asked directly rather than inferred, which is also what makes the advice
	// safe to print: if we can write here, the remedy is not a chown.
	if dirWritable(dir) {
		return nil
	}

	// NOT "IS ROOT-OWNED": the common case is that the directory does not exist
	// at all, and cannot be created because /usr/local above it is root-owned on
	// a stock Mac. Claiming the ownership of something absent is a claim an
	// operator can check and find false.
	return fmt.Errorf("cannot write %s: %w\n\n"+
		"%s is under /usr/local, which is root-owned on a stock Mac. Do NOT re-run this under "+
		"sudo — the launch agents run as you, and a root-owned config is one they cannot read "+
		"while `billet local up` refuses to run as root. Make the directory yours instead:\n%s",
		path, cause, dir, chownAdvice(dir))
}
