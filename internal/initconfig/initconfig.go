// Package initconfig renders a runnable billet.yaml for `billet init`.
//
// IMPORTABLE, RATHER THAN LIVING IN package main, because two callers need it:
// the CLI, and the end-to-end test that proves a generated config actually
// launches a job. A generator that only the CLI can reach can only be tested by
// asserting the file loads, which is exactly the gap that let the docker trial
// ship refusing every job.
//
// WHAT IT WRITES HAS TO RUN. Copying billet.example.yaml did not: it describes a
// Firecracker deployment, so the provider, every tier's image, the state
// directories and the capacity ceiling all had to be edited before anything
// started. A generated config that also needs editing is the same trap with an
// extra step. The one thing it cannot know is the GitHub App, because that does
// not exist yet — it names the org and leaves the ids at zero for
// `billet github-app create` to fill.
package initconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

// Headroom is the NOMINAL reservation a generated ceiling leaves for the machine
// itself — the amount withheld whenever the machine can afford it.
//
// It is not a guaranteed minimum, and saying so was wrong once the cap below
// landed: CeilingMemory withholds at most half a machine, so a 2GiB host
// reserves 1GiB rather than this 4GiB. The cap is what keeps the rule coherent
// at the small end; see CeilingMemory.
//
// The ceiling is what the allocator escrows against, so setting it to everything
// the host has means billet fills the machine and leaves nothing for the kernel,
// the container runtime, or the operator's shell. A starting point to raise, not
// a measurement.
//
// A FLOOR RATHER THAN THE WHOLE RULE, because a fixed reservation means
// different things at different sizes. Two vCPU is a quarter of an eight-thread
// laptop and 1.5% of a 128-thread server — and the server is the machine
// carrying sixty guests, whose supervision, networking and storage clients are
// what the reservation exists for. A fixed number reserves proportionally least
// exactly where there is most to do.
const (
	HeadroomVCPU   = 2
	HeadroomMemory = 4 * config.GiB
)

// headroomDivisor is the share withheld once it exceeds the floor above.
//
// A sixteenth. On the reference host — 128 threads — that reserves eight and
// generates the 120 its operator had already chosen by hand, which is the only
// corroboration available for a number nothing can measure.
const headroomDivisor = 16

// DefaultRunnerImage is a container image that already contains the runner.
//
// The tier's image is handed straight to `docker run`, so a golden-image name
// like `ubuntu-2404-x64` — what the Firecracker example uses — is not pullable
// and every job fails to launch with a message about the image rather than the
// config.
const DefaultRunnerImage = "ghcr.io/actions/actions-runner:latest"

// Firecracker conventional values billet cannot detect but the host must
// provide. They are what `billet images pull` and the host Ansible role install,
// so a generated config names them and the operator prepares the host to match.
const (
	// DefaultFirecrackerImage is a published guest generation, resolved to a
	// verified one. Unlike a docker image it is not pulled from a registry — it
	// comes from `billet images pull`.
	DefaultFirecrackerImage = "ubuntu-2404-x64@verified"
	defaultBridge           = "billet0"
	defaultUntrustedBridge  = "billet1"
	defaultImagePool        = "billet-images"
	defaultCachePool        = "billet-cache"
)

// Profile selects the path shape a generated config is written for.
//
// TWO LOCAL SHAPES, NOT ONE WITH A FLAG PER PATH: the paths stand or fall
// together. A config whose state lives under $UserConfigDir cannot run under
// the packaged systemd units (ProtectHome=true makes the home directory
// unreadable, and the units pin StateDirectory=/var/lib/billet/*), and a
// config rooted in /var/lib/billet is the wrong shape for a two-terminal
// trial run as an ordinary user. Mixing them produces a config that starts in
// neither world.
type Profile string

const (
	// ProfileLocal is the user-session shape: everything under the user config
	// dir, run manually in two terminals. The default, and what `billet init`
	// always generated.
	ProfileLocal Profile = "local"
	// ProfileLocalService is the system-service shape the services billet ships
	// run, and its CONTENTS differ by platform. On Linux: config and App key
	// under /etc/billet, state under /var/lib/billet, the node's deployment lock
	// under /run/billet/locks — the units' own RuntimeDirectory. On macOS the
	// same shape lives under /usr/local, because launchd performs no variable
	// substitution and every path a launch agent uses is a literal in a file
	// billet ships. Written for `billet local up` on either.
	ProfileLocalService Profile = "local-service"
)

// DefaultListen is the loopback address a generated config binds and dials.
const DefaultListen = "127.0.0.1:7717"

// Paths a local-service config must use, matched to the packaged units.
// deploy/billet-server.service pins StateDirectory=billet/server and the node
// unit RuntimeDirectory=billet/locks; TestLocalServiceMatchesThePackagedUnits
// keeps these in step with the unit files.
const (
	serviceConfDir    = "/etc/billet"
	serviceStateBase  = "/var/lib/billet"
	serviceLockDir    = "/run/billet/locks"
	serviceKeyPath    = serviceConfDir + "/app-private-key.pem"
	serviceConfigPath = serviceConfDir + "/billet.yaml"
)

// The macOS family, pinned to what deploy/sh.billet.*.plist name.
//
// A DIFFERENT ROOT, AND NOT AS A PREFERENCE. launchd performs no variable
// substitution in a plist, so every path a launch agent uses is a literal in a
// file billet ships and compares against — which rules out anything under a
// home directory unless the plists stop being shipped constants. /usr/local is
// what the agents already name and what `scripts/install.sh` already installs
// the binary into.
//
// Only two of these are pinned BY the plists: the config the agents pass to
// `--config`, and the log directory. The state, key and lock paths have no
// plist to be pinned to — a launch agent declares no StateDirectory the way a
// systemd unit does — so they are chosen here and held together by
// TestGenerateLocalServiceMatchesTheLaunchAgents.
const (
	macServiceConfDir    = "/usr/local/etc/billet"
	macServiceStateBase  = "/usr/local/var/lib/billet"
	macServiceLockDir    = "/usr/local/var/run/billet/locks"
	macServiceKeyPath    = macServiceConfDir + "/app-private-key.pem"
	macServiceConfigPath = macServiceConfDir + "/billet.yaml"
)

// serviceOS is runtime.GOOS behind a seam, so the platform-derived service
// shape can be asserted from either side on one machine.
var serviceOS = runtime.GOOS

// ServiceConfigPath is where a local-service config lives — the path the
// packaged service definitions read.
//
// PLATFORM-DERIVED RATHER THAN A SECOND PROFILE NAME. "The system-service
// shape" means the same thing to an operator on either platform; what differs
// is where that shape puts things and which account it runs as. A fourth
// profile would make them choose between two words for one idea.
func ServiceConfigPath() string { return ServiceConfigPathFor(serviceOS) }

// ServiceConfigPathFor is the same answer for a NAMED platform.
//
// THE PLATFORM IS A PARAMETER because two packages need to ask about a platform
// that is not the one they are running on: cmd/billet already carries its own
// `hostOS` seam so its tests can assert both shapes, and a hidden global here
// would mean those tests asserted whichever machine happened to run them.
func ServiceConfigPathFor(goos string) string {
	if goos == "darwin" {
		return macServiceConfigPath
	}

	return serviceConfigPath
}

// ServiceKeyPathFor is where a local-service config on a NAMED platform points
// at the App private key.
//
// EXPORTED BECAUSE THE GUIDANCE MUST NAME THE SAME FILE THE CONFIG DOES.
// `billet init`'s closing note used to carry a hardcoded
// /etc/billet/app-private-key.pem, so on a Mac it named a path in a directory
// that does not exist while the config beside it pointed somewhere else — and
// the operator has no way to tell which of the two is the real one.
func ServiceKeyPathFor(goos string) string {
	if goos == "darwin" {
		return macServiceKeyPath
	}

	return serviceKeyPath
}

// ServiceAccount is the account the packaged services run as, or EMPTY where
// they run as the operator who installed them.
//
// Empty is macOS, and it is not an omission. billet ships launch AGENTS rather
// than daemons because Virtualization.framework needs an unlocked login
// keychain and tart's image store is per-user — so the services run in a
// person's own GUI session, own their own files, and there is no service
// account for an operator to chown anything to.
func ServiceAccount() string { return ServiceAccountFor(serviceOS) }

// ServiceAccountFor is the same answer for a NAMED platform.
func ServiceAccountFor(goos string) string {
	if goos == "darwin" {
		return ""
	}

	return ServiceGroup
}

// ServiceGroup is the group the packaged server unit runs as, and therefore
// the group a local-service config must be readable by. ONE constant read by
// the generator, the ownership code and the units-pin test, so the unit files
// and this name cannot drift apart silently.
const ServiceGroup = "billet"

// CheckProfile refuses a profile value that is not one of the two shapes,
// naming the flag. Exported so the CLI can refuse it immediately after flag
// parsing — before capacity detection or a live AWS fetch — while Generate
// keeps the same guard for its other callers.
func CheckProfile(p Profile) error {
	if p != "" && p != ProfileLocal && p != ProfileLocalService {
		return fmt.Errorf("--profile: %q is not one of local, local-service", p)
	}

	return nil
}

// CheckListen refuses a listen value the local profiles cannot honor: anything
// that is not a well-formed loopback host:port. Same dual-caller contract as
// CheckProfile.
func CheckListen(listen string) error {
	if listen == "" {
		return nil
	}
	if err := config.CheckHostPort("--listen", listen); err != nil {
		return err
	}
	if !config.LoopbackAddr(listen) {
		return fmt.Errorf("--listen: %q is not a loopback address. A local profile "+
			"exposes nothing to the network — that is its guarantee. A control plane other "+
			"machines reach binds its published address in a hand-edited config, and then the "+
			"wire requires the client certificates `billet ca issue` mints", listen)
	}

	return nil
}

// PlaceholderAMI is the tier image an ec2 config is written with before an image
// exists. It PASSES config load, which checks only that an image is named, and
// FAILS at launch, where DescribeImages cannot resolve it — which is the staged
// flow on purpose: `billet init --provider ec2` writes this, `billet ami build`
// produces a real AMI id, and the operator pastes it in. A value that reads as an
// instruction rather than a plausible id, so a config that reached a launch
// unedited fails by naming what to do rather than with an opaque AWS error.
const PlaceholderAMI = "ami-REPLACE-run-billet-ami-build"

// Params is everything `billet init` decided or was told, for one machine.
type Params struct {
	// Org is the GitHub organization these runners serve. May be empty at
	// generation time; `github-app create` supplies it alongside the App ids.
	Org string
	// Provider is the compute backend. Docker, Firecracker and EC2 are all rendered.
	Provider config.ProviderKind
	// Image is the tier's image, handed verbatim to the backend: a container
	// reference for docker, a published guest generation for firecracker. Empty
	// selects the provider-appropriate default.
	Image string
	// VCPU and Memory are what this machine has, detected by the caller, and the
	// generated ceiling is these minus headroom — EXCEPT for ec2, where there is no
	// host to detect and these carry the operator's declared cloud budget, which
	// IS the ceiling (no headroom is withheld from a machine that does not exist).
	VCPU   int
	Memory config.ByteSize

	// RunnerGroup and Workflows are the trusted-pool policy. REQUIRED for Docker,
	// which shares the host kernel and so refuses any workload that is not trusted
	// — and a trusted pool needs a non-default runner group and an exact workflow
	// allowlist. Empty here is not "untrusted by default": for Docker that is a
	// config that loads and then refuses its first job, so it is refused up front.
	RunnerGroup string
	Workflows   []string

	// tartCatalogue is the tier catalogue Generate derived for a tart config.
	//
	// DERIVED IN Generate RATHER THAN IN THE RENDERER, because deriving it can
	// REFUSE — a Mac whose ceiling cannot afford Apple's 4GiB guest floor has no
	// macOS tier to write — and a renderer returns a string. Unexported so it can
	// only come from Generate; a caller-supplied catalogue would be a second
	// authority on what fits under the ceiling.
	tartCatalogue []tartTier

	// Tart carries the Apple-silicon inputs, set only when Provider is
	// ProviderTart: which guest kinds this Mac serves, its name in the deployment,
	// and the image each guest kind boots. There is nothing to fetch and nothing
	// to detect — a Mac's capacity is measured like any other host-run backend —
	// so unlike EC2Params this is entirely what the operator asked for.
	Tart *TartParams

	// CodeBuild carries the CodeBuild placement inputs, set only when Provider is
	// ProviderCodeBuild. Like EC2Params it is entirely declared: there is no
	// machine to measure and no API that reports what a compute type holds, so
	// the shapes arrive already carrying their vcpu, memory and audited price.
	CodeBuild *CodeBuildParams

	// EC2 carries the cloud-backend placement inputs, set only when Provider is
	// ProviderEC2. Its Shapes must arrive fully populated (type, vcpu, memory,
	// price) — resolved by the caller from a live fetch or explicit flags — because
	// billet ships no table of EC2 shapes and a shape smaller than the lease chosen
	// for it overcommits a host nobody can see.
	EC2 *EC2Params

	// State selects where the control-plane LEDGER lives. Nil writes the
	// `state_dir` shorthand, which is what almost every generation wants; set it
	// to render `identity_dir` plus an explicit `state:` block instead. The two
	// spellings are mutually exclusive at load, so this is a choice the generator
	// has to make rather than a value it can add beside the default.
	State *StateParams

	// Profile selects the path shape (see Profile). Empty means ProfileLocal.
	Profile Profile
	// GOOS is the platform whose SERVICE shape a local-service generation is
	// for. Empty means this machine's, which is what a real run wants; a caller
	// generating for another host — or a test asserting both shapes on one
	// machine — names it.
	//
	// THE SERVICE SHAPE ONLY. The user-session shape's state and key paths come
	// from os.UserConfigDir() of the process running this, so they describe THIS
	// machine whatever is named here — there is no target user directory to ask
	// about. tartTargetPlatform is where that distinction decides something.
	GOOS string
	// Host carries the values a host must provide that billet cannot detect. See
	// HostInputs; every one is optional and omitting all of them generates
	// exactly what this package generated before they existed.
	Host HostInputs

	// Listen is the loopback address the server binds and the node dials — ONE
	// value for both, because they must agree or the node dials a listener that
	// does not exist. Empty means DefaultListen. A non-loopback address is
	// refused: a local profile's whole guarantee is that nothing is exposed to
	// the network, and a control plane other machines reach is configured with
	// `billet ca issue` and a hand-written node section instead.
	Listen string
}

// EC2Params is the cloud placement an ec2 config needs and billet cannot detect.
type EC2Params struct {
	Region                  string
	SubnetID                string
	SecurityGroups          []string
	UntrustedSecurityGroups []string
	// Shapes are the instance types billet may buy, each already carrying what it
	// holds and its audited price. Tiers are derived so each fits a declared shape.
	Shapes []config.EC2InstanceType
}

// Generate renders a billet.yaml for these parameters and returns it together
// with whether it produced a trusted pool, having proved it validates. A
// parameter billet cannot turn into a RUNNABLE config — a Docker trial with no
// runner-group or workflow policy, a malformed workflow ref, a blank runner
// group, or an image with stray whitespace — is refused here by the name of the
// flag that carried it, rather than surfacing later as a config-load error
// blaming the generated tier (or, worse, loading and then failing to launch).
func Generate(p Params) (string, bool, error) {
	// Profile and listen defaults, then refusals, BEFORE any rendering: a bad
	// value here would otherwise surface as a config-load error blaming the
	// generated file rather than the flag that carried it.
	if err := CheckProfile(p.Profile); err != nil {
		return "", false, err
	}
	if p.Profile == "" {
		p.Profile = ProfileLocal
	}
	if err := CheckListen(p.Listen); err != nil {
		return "", false, err
	}
	if p.Listen == "" {
		p.Listen = DefaultListen
	}

	// AN ORG THAT NAMES A DIFFERENT ORGANIZATION IS REFUSED BY ITS FLAG. Empty is
	// left alone — that is the placeholder path, where renderConfig writes
	// <your-org> for the operator to fill in — but anything supplied goes through
	// the one rule config validation applies, so `--org "acme # prod"` is answered
	// here rather than as a load error about a file billet wrote itself.
	if p.Org != "" {
		if err := config.CheckOrg(p.Org); err != nil {
			return "", false, fmt.Errorf("--org: %w", err)
		}
	}
	// A BLANK-BUT-PRESENT VALUE IS NOT AN ABSENT ONE. Normalization trims, and a
	// runner group of only whitespace would trim to empty and read as "no policy"
	// — silently generating untrusted firecracker tiers, or a docker refusal that
	// names --runner-group as missing when it was in fact supplied blank. Caught
	// against the raw value, before the trim erases the distinction.
	if p.RunnerGroup != "" && strings.TrimSpace(p.RunnerGroup) == "" {
		return "", false, fmt.Errorf("--runner-group: a runner group cannot be only whitespace")
	}
	// An image is handed verbatim to the backend, so ANY whitespace or control
	// character in it is a reference that does not resolve — and config.Parse
	// checks only that the image is non-empty, so self-validation would pass it
	// through to a launch failure. Neither a docker reference nor a firecracker
	// generation name contains whitespace, so the whole class is rejected: not
	// just leading/trailing padding but an interior space, a tab, a non-breaking
	// space, or a control character. Rejected rather than silently trimmed — what
	// the operator meant by " x y " is not billet's to guess. Exact empty is left
	// alone; it is the per-provider default signal.
	if idx := strings.IndexFunc(p.Image, badImageRune); p.Image != "" && idx >= 0 {
		return "", false, fmt.Errorf("--image: %q contains whitespace or a control character", p.Image)
	}

	// NORMALIZE BEFORE BOTH VALIDATION AND RENDERING, or they check and emit
	// different strings: a runner group with trailing whitespace would pass the
	// flag check on its trimmed form and then be written raw, so the config
	// carries a group name GitHub does not have, and the lookup fails at runtime
	// with nothing pointing back here.
	p.RunnerGroup = strings.TrimSpace(p.RunnerGroup)
	// Cloned before trimming, so normalization does not reach back into the
	// caller's slice — Generate takes Params by value, but a slice field still
	// shares its backing array.
	p.Workflows = slices.Clone(p.Workflows)
	for i := range p.Workflows {
		p.Workflows[i] = strings.TrimSpace(p.Workflows[i])
	}

	if err := refuseForeignBackendInputs(p); err != nil {
		return "", false, err
	}

	if err := checkStateParams(p.State); err != nil {
		return "", false, err
	}

	if err := checkHostInputs(p.Host); err != nil {
		return "", false, err
	}

	if err := checkTargetPlatform(p.GOOS); err != nil {
		return "", false, err
	}

	trusted, err := p.trusted()
	if err != nil {
		return "", false, err
	}

	switch p.Provider {
	case config.ProviderDocker:
		// A container shares the host kernel, so docker refuses anything that is
		// not trusted — an untrusted docker trial is a config that loads and then
		// refuses its first job. The policy is required, not optional.
		if !trusted {
			return "", false, errDockerNeedsPolicy
		}
		if p.Image == "" {
			p.Image = DefaultRunnerImage
		}
	case config.ProviderFirecracker:
		// A microVM isolates the kernel, so firecracker runs untrusted work safely
		// on the untrusted bridge — the default. A trusted policy is accepted if
		// given (it was validated above), but not required.
		if p.Image == "" {
			p.Image = DefaultFirecrackerImage
		}
	case config.ProviderTart:
		// A tart guest is a real VM with its own kernel, so tart (like firecracker)
		// runs untrusted work — but the NETWORK is not a kernel boundary, and tart's
		// default is shared NAT, so untrusted here needs node.tart.untrusted_isolation.
		// billet writes that itself rather than taking a flag for it: softnet is the
		// only mechanism billet drives, so there is nothing for an operator to choose
		// between, unlike an ec2 security group that describes their own network.
		//
		// The image is per guest kind, so Params.Image is refused rather than
		// defaulted; normalizeTart fills each one.
		if err := refuseTartOffApplePlatform(p); err != nil {
			return "", false, err
		}

		if err := normalizeTart(&p); err != nil {
			return "", false, err
		}

		catalogue, err := tartTiers(p, CeilingVCPU(p.VCPU), CeilingMemory(p.Memory))
		if err != nil {
			return "", false, err
		}

		p.tartCatalogue = catalogue

	case config.ProviderEC2:
		// A whole instance is an isolation boundary, so ec2 (like firecracker) runs
		// untrusted work — but on a separately-described network, so untrusted here
		// requires an untrusted security group. The image is an AMI, which does not
		// exist until `billet ami build`, so it is a placeholder until then.
		if err := p.validateEC2(trusted); err != nil {
			return "", false, err
		}
		if p.Image == "" {
			p.Image = PlaceholderAMI
		}
	case config.ProviderCodeBuild:
		// UNTRUSTED IS REFUSED OUTRIGHT ON THIS BACKEND — not gated on a network
		// setting the way ec2's is, because the boundary AWS documents is not one a
		// security group can restore: a reserved-capacity instance stays alive
		// between builds and shares cached data with other projects in the account.
		// So a codebuild generation requires the trusted-pool policy for the same
		// reason docker does, and refusing here is the difference between a config
		// that will not be written and one that loads and then refuses its first
		// fork pull request.
		if !trusted {
			return "", false, errCodeBuildNeedsPolicy
		}

		if p.CodeBuild == nil {
			return "", false, errors.New("codebuild inputs are required for a codebuild " +
				"generation: there is no machine to measure and no API that reports what a " +
				"compute type holds, so the project, the environment, the parameter path and " +
				"the shapes billet may buy all have to be stated")
		}

		if err := normalizeCodeBuild(&p); err != nil {
			return "", false, err
		}

	default:
		return "", false, fmt.Errorf("initconfig: provider %q is not rendered here", p.Provider)
	}

	render := renderConfig

	switch p.Provider {
	case config.ProviderEC2:
		render = renderEC2Config
	case config.ProviderCodeBuild:
		render = renderCodeBuildConfig
	case config.ProviderDocker, config.ProviderFirecracker, config.ProviderTart:
		// renderConfig, above.
	}

	// The real file leaves the App ids at zero for `billet github-app create` to
	// fill; the App does not exist yet.
	body := render(p, trusted, 0, 0) + releaseBlock

	// PROVE IT before returning it. config.Parse is the single validation path a
	// file on disk takes, so a value that renders fine but does not VALIDATE — a
	// runner group the scale-set client cannot carry, a tier larger than the
	// ceiling — is caught now with billet's own diagnostic rather than by the
	// operator's next `billet check`. It is validated as a SEPARATE render with
	// non-zero App ids rather than by rewriting `body`: a `github.app_id is
	// required` error is not the config being generated, and an operator value
	// that happened to contain the text `app_id: 0` must not steer a substring
	// replacement or a rendered app id land in the returned file.
	validation := render(p, trusted, 1, 1) + releaseBlock
	if _, err := config.Parse("the config billet generated", []byte(validation)); err != nil {
		return "", false, fmt.Errorf("initconfig: the generated config is not valid: %w", err)
	}

	return body, trusted, nil
}

// releaseBlock says what a generated config does about new releases, which is
// everything, so the file states its own default rather than leaving the one
// decision most operators ask about to a page they have not read yet.
//
// COMMENTED OUT ENTIRELY, so it changes nothing about what the file means: the
// absent block IS the default. What it carries is the sentence that turns
// updates off and the window that bounds when they begin, beside each other.
const releaseBlock = `
# ── release ───────────────────────────────────────────────────────────────────
#
# This deployment updates itself. It follows the signed stable channel; when a
# new release is published the control plane starts a rollout, the scheduled
# updater on each host takes it up (draining every host for as long as its jobs
# take, verifying the candidate, and rolling back on failure), and each node
# refreshes the guest image it boots. The ledger refuses to be served by a
# release older than the newest that has served it, so this cannot go backwards
# without somebody saying so (` + "`billet host-upgrade --allow-downgrade`" + `).
#
# Turn it off with one line, or bound when a rollout may BEGIN (UTC; a window
# never stops one already running):
#
# release:
#   automatic: false
#   maintenance_window:
#     start: "02:00"
#     end: "04:00"
`

// checkTargetPlatform refuses a target billet ships no services for.
//
// EVERYTHING DOWNSTREAM READS THIS AS A BINARY. paths, serviceUnits and lockBlock
// all ask "is it darwin" and treat every other value as Linux, so
// `GOOS: "windows"` renders /etc/billet, /var/lib/billet and systemd prose for a
// platform billet has none of — a config that validates and describes a machine
// that cannot run it. The CLI refuses the same thing by flag, and this is the
// half an exported caller reaches.
func checkTargetPlatform(goos string) error {
	if goos == "" || goos == "linux" || goos == "darwin" {
		return nil
	}

	return fmt.Errorf("GOOS %q is not a platform billet ships services for: the service shape "+
		"is systemd units on linux and launch agents on darwin, and every other value would "+
		"render one of those two for a machine that has neither", goos)
}

// refuseForeignBackendInputs rejects a backend block that belongs to a provider
// this generation is not for.
//
// SILENTLY DISCARDED IS THE FAILURE. Generate is exported and internal/e2e
// reaches it directly, so a caller passing Provider: docker with a Tart block —
// or an EC2 one — used to get a docker config with every one of those values
// thrown away and no error, while the CLI refuses the same combination by flag
// name. Two entry points enforcing different contracts is the shape the
// billet-config skill names, and config validation already refuses the mirror
// image of this on a loaded file ("node.tart is set but this node's provider is
// %s").
func refuseForeignBackendInputs(p Params) error {
	if p.Tart != nil && p.Provider != config.ProviderTart {
		return fmt.Errorf("tart inputs are set but this generation is for %s, and only a tart "+
			"config renders them — the guest kinds, the node name and the per-guest images "+
			"would all be discarded", p.Provider)
	}

	if p.EC2 != nil && p.Provider != config.ProviderEC2 {
		return fmt.Errorf("ec2 inputs are set but this generation is for %s, and only an ec2 "+
			"config renders them — the region, subnet, security groups and purchasable shapes "+
			"would all be discarded", p.Provider)
	}

	// THE HOST INPUTS BELONG TO THE ONE BACKEND THAT READS THEM. A pinned kernel,
	// a Ceph identity and a guest-reachable cache are node.firecracker, node.ceph
	// and node.cache on a microVM host — and config validation refuses node.ceph
	// outright on every other backend, so accepting them elsewhere would either
	// write a file that cannot load or discard them in silence, which is the
	// failure this function exists for.
	if !p.Host.empty() && p.Provider != config.ProviderFirecracker {
		return fmt.Errorf("host inputs are set but this generation is for %s, and only a "+
			"firecracker config renders them — the pinned kernel, the Ceph identity and the "+
			"guest cache endpoint would all be discarded", p.Provider)
	}

	if p.CodeBuild != nil && p.Provider != config.ProviderCodeBuild {
		return fmt.Errorf("codebuild inputs are set but this generation is for %s, and only a "+
			"codebuild config renders them — the project, the environment, the fleet, the "+
			"parameter path and the purchasable compute types would all be discarded",
			p.Provider)
	}

	return nil
}

// badImageRune reports whether r makes an image reference unresolvable.
//
// A SHARED PREDICATE rather than a repeated closure, because the tart fields
// replaced Params.Image for that backend and a second copy of the rule is how
// one of the two grows a case the other does not.
func badImageRune(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

// errDockerNeedsPolicy is what a docker trial with no trusted-pool policy gets.
var errDockerNeedsPolicy = errors.New(
	"a docker trial needs --runner-group and at least one --workflow: docker shares the " +
		"host kernel, so it refuses any workload that is not trusted, and a trusted pool is " +
		"defined by a non-default GitHub runner group and an exact workflow allowlist. Create a " +
		"runner group in your org (Settings → Actions → Runner groups), restrict it to the " +
		"repositories and workflows you trust, and pass its name as --runner-group with each " +
		"allowed workflow as --workflow owner/repo/.github/workflows/file.yml@refs/heads/main")

// trusted reports whether the tier policy makes a trusted pool, validating it
// when any part is present. Absent policy is a legal untrusted pool (which
// firecracker runs); a PARTIAL or malformed policy is refused, by the flag that
// carried it, rather than rendered into a config that fails to load. p is already
// normalized by Generate, so the value checked is the value rendered.
func (p Params) trusted() (bool, error) {
	if p.RunnerGroup == "" && len(p.Workflows) == 0 {
		return false, nil
	}

	// EqualFold, not ==: GitHub's built-in group is displayed as "Default" and
	// its name resolves case-insensitively, so `--runner-group Default` names the
	// all-repositories group a trusted pool must never use. config.PoolPolicyErrors
	// guards the same way; matching it here means the refusal names the flag rather
	// than surfacing as a config-load error.
	if p.RunnerGroup == "" || strings.EqualFold(p.RunnerGroup, "default") {
		return false, fmt.Errorf("--runner-group: a trusted pool needs a non-default runner group")
	}
	if err := config.CheckRunnerGroup(p.RunnerGroup); err != nil {
		return false, fmt.Errorf("--runner-group: %w", err)
	}

	if len(p.Workflows) == 0 {
		return false, fmt.Errorf("--workflow: a trusted pool needs at least one allowlisted workflow")
	}
	seen := make(map[string]struct{}, len(p.Workflows))
	for _, w := range p.Workflows {
		if err := config.CheckWorkflowRef(w); err != nil {
			return false, fmt.Errorf("--workflow %q: %w", w, err)
		}
		if _, dup := seen[w]; dup {
			return false, fmt.Errorf("--workflow %q is listed twice", w)
		}
		seen[w] = struct{}{}
	}

	return true, nil
}

// CeilingVCPU and CeilingMemory are what billet may spend, leaving the host
// enough to keep working.
// NOTHING DETECTED IS NOTHING TO SPEND, and both return zero for it rather than
// a floor. Subtracting a reservation from a non-positive reading used to yield 1
// vCPU — a ceiling ABOVE what was detected, which does not fail, it overcommits.
// Generate refuses a zero ceiling by name, so the caller gets the reading back
// rather than a config built on one that never happened.
func CeilingVCPU(detected int) int {
	if detected < 1 {
		return 0
	}

	return max(1, detected-max(HeadroomVCPU, detected/headroomDivisor))
}

func CeilingMemory(detected config.ByteSize) config.ByteSize {
	if detected <= 0 {
		return 0
	}

	// THE RESERVATION IS CAPPED AT HALF THE MACHINE, and that cap is what makes
	// the rule coherent at the small end. A flat 4GiB floor exceeds a small host
	// entirely, and the two ways of handling that were each wrong in their own
	// direction: returning the WHOLE amount withheld nothing from the kernel — the
	// overcommit the reservation exists to prevent — while refusing outright broke
	// billet init's commitment to write a config that loads on any size of
	// machine, down to the smallest thing that can boot. Between them the rule was
	// not even monotonic: 4GiB was accepted while a LARGER 4.5GiB host was refused,
	// because its remainder rounded to nothing.
	//
	// Half is the reservation a machine can always afford. Above ~8GiB the floor
	// and then the proportional share govern, so nothing about a real host moves.
	headroom := min(max(HeadroomMemory, detected/headroomDivisor), detected/2)

	return roundDownGiB(detected - headroom)
}

// roundDownGiB trims a ceiling to a whole GiB.
//
// IT IS A BUDGET, NOT A MEASUREMENT, and it is rendered into the file the
// operator reads and edits. Real memory is not a round number — a 512GiB host
// detects as 523505880KiB — and ByteSize renders only the unit that divides
// exactly, so an unrounded ceiling reaches the config as `519311576KiB`: a
// number nobody can compare to the tier sizes beneath it, in the one field whose
// purpose is to be reviewed and raised. DOWN, so the trim can only withhold more.
//
// ByteSize.String stays exact — MarshalYAML round-trips through it, so rendering
// approximately there would make every byte value in every config approximate.
func roundDownGiB(b config.ByteSize) config.ByteSize {
	// DOWN MEANS DOWN. Returning the remainder here instead abandoned both rules
	// at once: a host between the floor and one GiB above it generated a `512MiB`
	// ceiling — neither a whole GiB nor a refusal — and Generate then rendered a
	// config around a budget too small to place any tier.
	if b < config.GiB {
		return 0
	}

	return b - b%config.GiB
}

// tierMemoryPerVCPU is the proportion every generated ladder is shaped to.
//
// ONE NAME rather than the literal repeated per backend, because the tart
// catalogue has to RESERVE a rung it has not built yet — a reservation computed
// from a second copy of this number is a reservation that stops matching the
// ladder the moment either moves.
const tierMemoryPerVCPU = 4 * config.GiB

// tierLadder is the vCPU ladder a measured host's catalogue is drawn from,
// smallest first.
var tierLadder = []int{2, 4, 8}

// tier is one entry in the generated catalogue.
type tier struct {
	label  string
	vcpu   int
	memory config.ByteSize
}

// tiers is a catalogue that FITS UNDER THE CEILING it is generated with — every
// tier at once, not each one on its own.
//
// A tier larger than server.max_vcpu or server.max_memory is refused by
// validation — a job on it could never be placed — so a fixed catalogue makes a
// generated config load on some machines and not others. Shapes are 4GiB per
// vCPU; a machine too small for any of them still gets ONE tier sized to the
// ceiling, because a config with no tiers loads and then schedules nothing.
//
// THE BUDGET IS SHARED, AND EACH TIER SPENDS FROM IT BEFORE ANY JOB EXISTS.
// Every tier is its own scale set, and a listener escrows capacity BEFORE it
// advertises — one backed discovery slot per tier, because a scale set
// advertising zero receives no work and no statistics and so can never be
// discovered at all. So the catalogue's floor is one job of every tier
// simultaneously, and a candidate that fits the ceiling ALONE can still not fit
// beside the tiers already chosen.
//
// Checking each candidate against the bare ceiling generated exactly that: on a
// host measured at 10 vCPU and 19GiB, an 8GiB and a 16GiB tier were both
// individually legal and together needed 24GiB. The larger one's discovery slot
// took the memory, both tiers then advertised zero, and every job queued forever
// against a control plane reporting itself healthy. Nothing refused, because
// nothing compares a catalogue to its own budget.
//
// So the running total is what a candidate is tested against. Dropping a tier is
// the right loss: a label that can never be discovered is worse than an absent
// one, because it is in the config and in the operator's workflow.
func tiers(ceilVCPU int, ceilMemory config.ByteSize) []tier {
	var (
		fit        []tier
		usedVCPU   int
		usedMemory config.ByteSize
	)

	for _, vcpu := range tierLadder {
		memory := config.ByteSize(vcpu) * tierMemoryPerVCPU
		if usedVCPU+vcpu <= ceilVCPU && usedMemory+memory <= ceilMemory {
			fit = append(fit, tier{label: fmt.Sprintf("billet-%dvcpu", vcpu), vcpu: vcpu, memory: memory})
			usedVCPU += vcpu
			usedMemory += memory
		}
	}

	if len(fit) > 0 {
		return fit
	}

	return []tier{{
		label:  fmt.Sprintf("billet-%dvcpu", ceilVCPU),
		vcpu:   ceilVCPU,
		memory: min(ceilMemory, config.ByteSize(ceilVCPU)*tierMemoryPerVCPU),
	}}
}

// yamlScalar renders one value as a YAML scalar that survives a round trip.
//
// A runner group name or a workflow ref goes into the file verbatim, and either
// can contain a character YAML reads specially (a leading '*', a ':'): writing
// it raw produces a file that no longer parses, or parses to a different string.
// Marshalling one value is how the quoting is decided by the same library that
// will read it back.
//
// yaml.Marshal of a string does not error — it fails only on a Go type it cannot
// represent (a channel, a func, a cycle), never a string — so the error branch
// is unreachable and returns the input rather than pretending to escape it. The
// whole config is round-tripped through config.Parse before Generate returns, so
// a scalar that somehow did slip through unquoted fails there, not on disk.
func yamlScalar(v string) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return v
	}

	return strings.TrimRight(string(b), "\n")
}

// omittedNameComment is what the node block says where billet writes no
// node.name — every generation but a tart one carrying a host-pinned macOS tier.
const omittedNameComment = "  # name is omitted: with a certificate the name comes from it, and on this\n" +
	"  # machine there is no certificate to disagree with.\n"

// firecrackerNodeBlocks is the node.firecracker and node.ceph configuration a
// microVM host needs, with conventional values billet cannot detect but the host
// must provide — commented so an operator sees exactly what to prepare.
// untrustedBridgeComment explains node.firecracker.untrusted_bridge for the
// tiers this generation actually wrote.
//
// The bridge is what lets a host run untrusted work; whether anything here NEEDS
// it depends on the trust the same run chose. Said one way for both, a
// trusted-only config claimed that removing the bridge would refuse every job it
// schedules, which is the reverse — nothing in that file attaches to it.
func untrustedBridgeComment(trusted bool) string {
	shared := "    # Fork pull-request guests attach here instead. Its presence is what lets this\n" +
		"    # host run untrusted work at all — a microVM isolates the kernel, not the\n" +
		"    # network, so untrusted jobs need a bridge that reaches only what a stranger's\n" +
		"    # code should.\n"

	if trusted {
		return shared +
			"    # No tier below is `trust: untrusted`, so nothing here attaches to it yet.\n" +
			"    # Keep it and create the bridge if you will add one; otherwise remove both.\n"
	}

	return shared +
		"    # The tiers below are all `trust: untrusted`, so removing it refuses every job\n" +
		"    # this file schedules — add a trusted tier first.\n"
}

// HostInputs are values a HOST must provide that billet cannot detect and will
// not guess.
//
// THE MEASURED LIST FROM A REAL FIRECRACKER DEPLOYMENT (2026-08-26). A
// generated block was diffed against an inventory written by hand months
// earlier, and everything the generator could know it reproduced exactly:
// listen, both state directories, the App identity, the provider, the lock
// directory, both bridges, both Ceph pools, and a 2/4/8 tier ladder unprompted.
// What it could not know is this — values the operator had set, none of which
// has a right default:
//
//   - a PINNED kernel, because `<kernel_dir>/vmlinux` is a fallback and a real
//     host names a version;
//   - the Ceph identity and its keyring, because `billet` is a default and
//     `admin` — which the rbd command picks for itself — can delete a pool;
//   - the cache endpoint, which is a fact about the operator's network.
//
// EVERY ONE IS OPTIONAL. Omitted, the generation writes exactly what it wrote
// before. Supplied, it is written OUT rather than left as a comment — which is
// the point: an operator who has answered the question should not have to answer
// it again by editing the file afterwards.
type HostInputs struct {
	// KernelImage pins node.firecracker.kernel_image.
	KernelImage string
	// CephUser and CephKeyringPath name the RADOS identity, WITHOUT the `client.`
	// prefix. Empty leaves Ceph's own search path, which finds
	// /etc/ceph/ceph.<user>.keyring.
	CephUser        string
	CephKeyringPath string
	// CacheListen and CacheGuestEndpoint are node.cache, and are given together
	// or not at all.
	CacheListen        string
	CacheGuestEndpoint string
}

// empty reports whether the operator supplied nothing.
func (h HostInputs) empty() bool { return h == HostInputs{} }

// checkHostInputs refuses a half-declared pair before anything is rendered.
func checkHostInputs(h HostInputs) error {
	if (h.CacheListen == "") != (h.CacheGuestEndpoint == "") {
		return errors.New("--cache-listen and --cache-guest-endpoint are given together or " +
			"not at all: the guest endpoint is the origin placed in a guest's metadata and it " +
			"must name the same address the cache listens on, so half of the pair is a cache " +
			"that is configured and unreachable")
	}

	return nil
}

// cephIdentityYAML renders the identity lines a host supplied.
//
// ABSENT RATHER THAN DEFAULTED, because each key means something specific when
// unset: no user is `billet` — deliberately, since `admin` is what the rbd
// command picks on its own and an admin key can delete a pool — and no keyring
// is Ceph's own search path.
func cephIdentityYAML(h HostInputs) string {
	var b strings.Builder

	if h.CephUser != "" {
		b.WriteString("    # The RADOS identity billet authenticates as, WITHOUT the " +
			"`client.`\n")
		b.WriteString("    # prefix. `admin` is refused: it is what the rbd command picks for\n")
		b.WriteString("    # itself, and an admin key can delete a pool.\n")
		fmt.Fprintf(&b, "    user: %s\n", yamlScalar(h.CephUser))
	}

	if h.CephKeyringPath != "" {
		fmt.Fprintf(&b, "    keyring_path: %s\n", yamlScalar(h.CephKeyringPath))
	}

	return b.String()
}

// cacheBlockYAML renders node.cache when a host supplied one.
func cacheBlockYAML(h HostInputs) string {
	if h.CacheListen == "" {
		return ""
	}

	return fmt.Sprintf(`
  # THE CACHE GUESTS REACH. One literal, non-loopback address — a wildcard is
  # refused, because it would expose the bearer-token API on another interface —
  # and the origin placed in each guest's metadata, which must name the same
  # address. Every call is authorised by that guest's own per-instance bearer.
  cache:
    listen: %s
    guest_endpoint: %s
`, yamlScalar(h.CacheListen), yamlScalar(h.CacheGuestEndpoint))
}

func firecrackerNodeBlocks(trusted bool, h HostInputs) string {
	kernel := h.KernelImage
	if kernel == "" {
		kernel = filepath.Join(config.DefaultKernelDir, "vmlinux")
	}

	return fmt.Sprintf(`
  # THE MICROVM BACKEND. These name what the HOST must provide; billet does not
  # create them. The junioryono.billet.host Ansible role installs the bridges and
  # bootstraps Ceph; the guest kernel below is yours to place. `+"`billet check`"+`
  # reports whichever is missing.
  firecracker:
    # An uncompressed guest kernel YOU place at this path — neither the Ansible
    # role nor `+"`billet images pull`"+` creates it, and `+"`billet check`"+` requires it to
    # exist. A pulled generation boots its OWN recorded kernel, so this file is the
    # fallback for a hand-built generation; build one per docs/reference/reference-hardware.md
    # or copy a pulled kernel out of kernel_dir.
    kernel_image: %s
    # The host bridge trusted guests attach to.
    bridge: %s
%s    untrusted_bridge: %s

  # THE SITE'S STORAGE. Golden images and every cache are RBD images in one Ceph
  # cluster on this host's NVMe. Two pools, because a cache is disposable and a
  # golden image is not.
  ceph:
    image_pool: %s
    cache_pool: %s
%s`, yamlScalar(kernel),
		defaultBridge, untrustedBridgeComment(trusted), defaultUntrustedBridge,
		defaultImagePool, defaultCachePool, cephIdentityYAML(h)) + cacheBlockYAML(h)
}

// renderConfig writes the config as commented text.
//
// Text rather than a marshalled struct, because the comments are most of the
// value: an operator reading this file should see which numbers billet measured,
// which it chose, and what changing one costs.
func renderConfig(p Params, trusted bool, appID, installationID int) string {
	org := p.Org
	if org == "" {
		org = "<your-org>"
	}

	paths := p.paths()

	ceilVCPU := CeilingVCPU(p.VCPU)
	ceilMemory := CeilingMemory(p.Memory)

	runIntro := "# One file, both roles: `billet server` is the control plane and\n" +
		"# `billet node` is a compute host. On this machine you run both, as two\n" +
		"# processes talking over the loopback address below — nothing is exposed to the\n" +
		"# network and no certificates are involved."
	lockBlock := ""
	if p.Profile == ProfileLocalService {
		runIntro = "# One file, both roles: `billet server` is the control plane and\n" +
			"# `billet node` is a compute host, run as " + p.serviceUnits() + "\n" +
			"# (`billet local up`) talking over the loopback address below — nothing is\n" +
			"# exposed to the network and no certificates are involved."

		lockBlock = p.lockBlock(paths.lockDir)
	}

	nodeBlocks := ""
	nameBlock := omittedNameComment
	tierIntro := "# Docker shares the host kernel, so every tier is `trust: trusted` and bound\n" +
		"# to your runner group and workflow allowlist below. That is what makes a job\n" +
		"# safe to run in a container: only the workflows you named, from the\n" +
		"# repositories your runner group permits, ever reach it."

	var tierBlock string
	if p.Provider == config.ProviderTart {
		// THE CATALOGUE CAME FROM Generate, which is where refusing one was
		// possible. Recomputing it here would be a second answer to "what fits",
		// and the two would differ the first time either changed.
		nodeBlocks = tartNodeBlocks(trusted)
		nameBlock = tartNameBlock(p, p.tartCatalogue)
		tierBlock = renderTartTiers(p.tartCatalogue, p, trusted)
		tierIntro = tartTierIntro(trusted)
	} else {
		tierBlock = renderTiers(tiers(ceilVCPU, ceilMemory), p, trusted)
	}

	if p.Provider == config.ProviderFirecracker {
		nodeBlocks = firecrackerNodeBlocks(trusted, p.Host)
		tierIntro = "# A microVM isolates the guest kernel, so these tiers are `trust: untrusted`\n" +
			"# and run on the untrusted bridge above — firecracker can run code billet\n" +
			"# cannot vouch for. The image is a published guest generation from\n" +
			"# `billet images pull`, resolved to a verified one, not a container image."
		if trusted {
			tierIntro = "# A microVM isolates the guest kernel. These tiers are `trust: trusted` and\n" +
				"# bound to your runner group and workflow allowlist below. The image is a\n" +
				"# published guest generation from `billet images pull`, not a container image."
		}
	}

	return fmt.Sprintf(`# billet — written by `+"`billet init`"+` for this machine.
#
%s
#
# Add a second machine by running `+"`billet ca issue <name>`"+` here, copying the
# bundle to that host, and giving it a config with a node: section pointing at
# this one. Its node.name comes from the certificate.

server:
  # Loopback only, so billet opens nothing the network can reach. A control
  # plane that must serve other machines binds an address they can reach, and
  # then the wire requires the client certificates `+"`billet ca issue`"+` mints.
  listen: %s

%s
  # THE CEILING BILLET ESCROWS AGAINST, and the one number worth reviewing.
  #
  # This machine has %d vCPU and %s. Left here is %d vCPU and %s, keeping
  # %d vCPU and %s for the kernel, the container runtime and your shell.
  # Raising it lets billet fill the machine; there is no error when it does,
  # only a host that is busier than you expected.
  max_vcpu: %d
  max_memory: %s

github:
  org: %s

  # Filled in by `+"`billet github-app create --config <this file>`"+`.
  app_id: %d
  installation_id: %d
  private_key_path: %s

# This machine, as a compute host. Delete this section on a control plane that
# should not run jobs itself.
node:
%s  server_addr: %s
  provider: %s
  state_dir: %s
%s
  # What this host CONTRIBUTES, which need not be everything it has. Unset means
  # everything billet can detect, bounded by the ceiling above.
  #
  # max_vcpu: %d
  # max_memory: %s
%s
# Tiers are yours to define. The label is what users put in `+"`runs-on`"+`, and the
# server is the only role that reads them — a node is told the shape of what it
# is launching with each job.
#
# These are sized to fit the ceiling above, so they are what this machine can
# actually place. A tier larger than the ceiling is refused at load rather than
# quietly never scheduled.
#
%s
tiers:
%s`,
		runIntro,
		yamlScalar(p.Listen),
		serverStateYAML(p.State, paths.serverState),
		p.VCPU, p.Memory, ceilVCPU, ceilMemory,
		p.VCPU-ceilVCPU, p.Memory-ceilMemory,
		ceilVCPU, ceilMemory,
		yamlScalar(org),
		appID, installationID,
		yamlScalar(paths.keyPath),
		nameBlock,
		yamlScalar(p.Listen),
		p.Provider,
		yamlScalar(paths.nodeState),
		lockBlock,
		ceilVCPU, ceilMemory,
		nodeBlocks,
		tierIntro,
		tierBlock,
	)
}

// renderTierPolicy writes the trust suffix every generated tier carries.
//
// ONE COPY FOR THREE RENDERERS. It was two identical copies before tart made it
// three, and the failure a third would invite is not a cosmetic one: a trusted
// tier that lost its runner_group or an allowlist entry is a pool GitHub hands
// work to from repositories the operator never named.
// A trusted tier carries its whole pool policy, rendered YAML-safe because the
// group name and the refs came from the operator. An untrusted one states its
// trust rather than leaving it implied.
func renderTierPolicy(b *strings.Builder, p Params, trusted bool) {
	if !trusted {
		// Stated rather than left to the migration default, so a reader sees it
		// was chosen.
		b.WriteString("    trust: untrusted\n")

		return
	}

	fmt.Fprintf(b, "    trust: trusted\n    runner_group: %s\n    workflows:\n",
		yamlScalar(p.RunnerGroup))
	for _, w := range p.Workflows {
		fmt.Fprintf(b, "      - %s\n", yamlScalar(w))
	}
}

// renderTiers writes the catalogue as YAML entries, one blank line apart. The
// trust suffix each carries is renderTierPolicy's, shared with the other two
// renderers.
func renderTiers(ts []tier, p Params, trusted bool) string {
	var b strings.Builder

	for i, t := range ts {
		if i > 0 {
			b.WriteString("\n")
		}

		fmt.Fprintf(&b, "  - label: %s\n    provider: %s\n    vcpu: %d\n    memory: %s\n    image: %s\n",
			t.label, p.Provider, t.vcpu, t.memory, yamlScalar(p.Image))

		renderTierPolicy(&b, p, trusted)
	}

	return b.String()
}

// errEC2UntrustedNeedsGroup is what an ec2 trial with no trusted-pool policy gets
// when it names no untrusted security group.
var errEC2UntrustedNeedsGroup = errors.New(
	"an ec2 trial with no trusted-pool policy runs untrusted work, which needs an untrusted " +
		"security group: a whole instance isolates the kernel but not the network, so a fork's " +
		"job must run in a group that reaches only what a stranger's code should. Pass " +
		"--untrusted-security-group sg-..., or define a trusted pool with --runner-group and " +
		"--workflow")

// validateEC2 refuses ec2 inputs that would render a config that does not run.
// trusted is Generate's decision about the tier policy, which decides whether an
// untrusted security group is required.
func (p Params) validateEC2(trusted bool) error {
	if p.EC2 == nil {
		return errors.New("initconfig: an ec2 config needs node.ec2 inputs " +
			"(region, subnet, security group, shapes)")
	}

	// There is no host to detect a budget from, and billet will not choose how much
	// to buy on someone's account — so the budget is required, and it is the ceiling
	// directly rather than a detected number minus headroom.
	if p.VCPU <= 0 || p.Memory <= 0 {
		return errors.New("--max-vcpu and --max-memory are required for ec2: the compute this node " +
			"launches runs in a region rather than on this host, so there is nothing to detect a " +
			"budget from")
	}

	if len(p.EC2.Shapes) == 0 {
		return errors.New("--instance-type is required at least once for ec2: billet ships no table " +
			"of EC2 shapes, so it must be told which ones it may buy")
	}

	if !trusted && len(p.EC2.UntrustedSecurityGroups) == 0 {
		return errEC2UntrustedNeedsGroup
	}

	// A CEILING SMALLER THAN EVERY SHAPE places nothing. Every declared shape is
	// larger than the budget, so no tier derived from a shape fits the ceiling and
	// the catalogue would be empty — a config that loads and schedules nothing.
	if len(ec2Tiers(p.EC2.Shapes, p.VCPU, p.Memory)) == 0 {
		return fmt.Errorf("no declared shape fits the budget of %d vCPU and %s: every shape is "+
			"larger, so no tier could be placed — raise --max-vcpu/--max-memory or name a smaller "+
			"--instance-type", p.VCPU, p.Memory)
	}

	return nil
}

// ec2Tiers derives one tier per declared shape that fits the budget, sized to the
// shape so it fits at least that shape (placement charges the smallest fitting
// shape). A shape larger than the ceiling is skipped rather than made into a tier
// that validation would refuse. Shapes that share a vcpu count collapse to one
// tier, because a tier label must be unique and the size is what a job asks for.
func ec2Tiers(shapes []config.EC2InstanceType, ceilVCPU int, ceilMemory config.ByteSize) []tier {
	return remoteTiers(shapes, ceilVCPU, ceilMemory, func(s config.RemoteShape) string {
		return fmt.Sprintf("billet-ec2-%dvcpu", s.VCPU)
	})
}

// remoteTiers derives one tier per declared shape that the deployment can
// actually afford to advertise, for any backend whose catalogue is an ordered,
// priced list.
//
// TWO RULES, AND BOTH WERE MISSING FROM ec2Tiers. Neither is a tidiness matter:
// each produces a catalogue that loads, starts, and then advertises zero.
//
// THE BUDGET IS SHARED. Every tier is its own scale set and every listener
// escrows one backed discovery slot BEFORE it advertises — a scale set
// advertising zero receives no work and no statistics, so it can never be
// discovered at all. The catalogue's floor is therefore one instance of every
// tier simultaneously, and a candidate that fits the ceiling ALONE can still not
// fit beside the tiers already chosen. Checking each shape against the bare
// ceiling produced exactly that on a measured host, and the same arithmetic was
// still here for the declared budget: shapes of 8 and 16 vCPU against a 20 vCPU
// budget both pass individually and together need 24.
//
// AND THE CHARGE IS THE FIRST FITTING SHAPE, NOT THE TIER'S OWN. Placement walks
// the operator's ordered catalogue and charges the first entry that fits the
// tier, which is the point of the order — so a tier derived from a small shape
// listed AFTER a large one is charged the large one. A floor summed from the tier
// requests therefore understates what the deployment will actually hold, and the
// tier it lets through has no node that can afford it.
func remoteTiers(
	shapes []config.RemoteShape, ceilVCPU int, ceilMemory config.ByteSize,
	label func(config.RemoteShape) string,
) []tier {
	var (
		fit        []tier
		usedVCPU   int
		usedMemory config.ByteSize
	)

	seen := make(map[string]bool, len(shapes))

	for i := range shapes {
		s := shapes[i]

		name := label(s)
		if seen[name] {
			continue
		}

		// WHAT PLACEMENT WOULD ACTUALLY BUY for a tier of this size, which is what
		// the budget has to be tested against.
		charged := firstFittingShape(shapes, s)

		if usedVCPU+charged.VCPU > ceilVCPU || usedMemory+charged.Memory > ceilMemory {
			continue
		}

		seen[name] = true
		fit = append(fit, tier{label: name, vcpu: s.VCPU, memory: s.Memory})
		usedVCPU += charged.VCPU
		usedMemory += charged.Memory
	}

	return fit
}

// firstFittingShape is what placement charges a tier of this size: the first
// declared shape that can hold it, in the operator's own order.
//
// It always finds one, because `want` is itself a member of the catalogue — but
// an EARLIER, larger entry wins, and that is the case this exists for.
func firstFittingShape(shapes []config.RemoteShape, want config.RemoteShape) config.RemoteShape {
	for i := range shapes {
		if shapes[i].VCPU >= want.VCPU && shapes[i].Memory >= want.Memory {
			return shapes[i]
		}
	}

	return want
}

// stateDirBase is where a generated config puts server and node state, preferring
// the user config dir and falling back to the packaged /var/lib/billet.
func stateDirBase() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "billet")
	}

	return "/var/lib/billet"
}

// lockBlock renders the service shape's lock_dir with the reason it is pinned
// on THIS platform.
//
// SHARED BY BOTH RENDERERS, because it was not. The ec2 renderer carried its own
// copy and kept explaining a macOS path as a systemd RuntimeDirectory — and an
// ec2 node is an orchestrator, so it is the one backend a Mac can run without
// any of the hardware the others need.
func (p Params) lockBlock(dir string) string {
	note := "in the tmpfs directory the packaged node\n" +
		"  # unit creates per boot (RuntimeDirectory=billet/locks). The per-user\n" +
		"  # default is wrong for a system service sharing a rootful runtime socket."

	if p.platform() == "darwin" {
		// LAUNCHD CREATES NOTHING, and the reason for setting this at all is a
		// different one. There is no RuntimeDirectory, so the directory is one
		// `billet local up` plans and makes; and the agent runs as the operator,
		// so billet's per-user default would resolve — but it would resolve to a
		// DIFFERENT directory for anyone else administering the same host, and
		// the lock's whole job is that two processes find it.
		note = "in the directory `billet local up`\n" +
			"  # creates. The per-user default resolves per ACCOUNT, so two people\n" +
			"  # managing one host would take two different locks and neither would hold."
	}

	return fmt.Sprintf("\n  # The host-wide deployment lock, %s\n  lock_dir: %s\n",
		note, yamlScalar(dir))
}

// serviceUnits names the things `billet local up` converges on this platform, in
// the middle of a sentence.
//
// THE GENERATED FILE IS READ ON THE MACHINE IT DESCRIBES, so naming systemd on a
// Mac sends its operator looking for a unit that is not there and a package that
// does not exist — the same defect as the systemd-only next-steps output this
// command prints beside it.
func (p Params) serviceUnits() string {
	if p.platform() == "darwin" {
		return "the launch agents billet ships"
	}

	return "the packaged systemd units"
}

// platform is the OS whose service shape this generation is for.
func (p Params) platform() string {
	if p.GOOS != "" {
		return p.GOOS
	}

	return serviceOS
}

// profilePaths is where a generated config points, resolved by profile.
type profilePaths struct {
	serverState, nodeState, keyPath string
	// lockDir is set only for the service shape: the packaged node unit's
	// RuntimeDirectory creates it per boot, while the user-session shape takes
	// billet's per-user default.
	lockDir string
}

func (p Params) paths() profilePaths {
	if p.Profile == ProfileLocalService {
		if p.platform() == "darwin" {
			return profilePaths{
				serverState: macServiceStateBase + "/server",
				nodeState:   macServiceStateBase + "/node",
				// THROUGH THE ACCESSOR, not the constant beside it. The printed
				// guidance names the key through ServiceKeyPathFor, so a second
				// platform branch here is a way for the note and the config to
				// name different files for the credential GitHub issues once.
				keyPath: ServiceKeyPathFor(p.platform()),
				lockDir: macServiceLockDir,
			}
		}

		return profilePaths{
			serverState: serviceStateBase + "/server",
			nodeState:   serviceStateBase + "/node",
			keyPath:     ServiceKeyPathFor(p.platform()),
			lockDir:     serviceLockDir,
		}
	}

	base := stateDirBase()

	return profilePaths{
		serverState: filepath.Join(base, "server"),
		nodeState:   filepath.Join(base, "node"),
		keyPath:     filepath.Join(base, "app-private-key.pem"),
	}
}

// ec2GroupsYAML renders one security-group list field.
func ec2GroupsYAML(field string, groups []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "    %s:\n", field)
	for _, g := range groups {
		fmt.Fprintf(&b, "      - %s\n", yamlScalar(g))
	}

	return b.String()
}

// ec2InstanceTypesYAML renders the shapes billet may buy, each declaring what it
// holds and its audited price.
func ec2InstanceTypesYAML(shapes []config.EC2InstanceType) string {
	var b strings.Builder

	for _, s := range shapes {
		price := s.PriceUSDPerHour
		fmt.Fprintf(&b, "      - type: %s\n        vcpu: %d\n        memory: %s\n"+
			"        price_usd_per_hour: %s\n",
			yamlScalar(s.Type), s.VCPU, s.Memory, price.Decimal())
	}

	return b.String()
}

// renderEC2Config writes an ec2 config as commented text. It differs from
// renderConfig in shape rather than intent: there is no host to detect, so the
// ceiling is the declared cloud budget rather than a measurement minus headroom,
// and the node carries an ec2 block instead of firecracker and ceph.
func renderEC2Config(p Params, trusted bool, appID, installationID int) string {
	org := p.Org
	if org == "" {
		org = "<your-org>"
	}

	paths := p.paths()
	e := p.EC2

	lockBlock := ""
	if p.Profile == ProfileLocalService {
		lockBlock = p.lockBlock(paths.lockDir)
	}

	untrustedBlock := ""
	if len(e.UntrustedSecurityGroups) > 0 {
		untrustedBlock = ec2GroupsYAML("untrusted_security_group_ids", e.UntrustedSecurityGroups)
	}

	tierIntro := "# A whole EC2 instance is an isolation boundary, so these tiers are\n" +
		"# `trust: untrusted` and launch in the untrusted security group above —\n" +
		"# ec2 can run code billet cannot vouch for. Each tier is derived from a\n" +
		"# shape below, and its image is an AMI you build with `billet ami build`."
	if trusted {
		tierIntro = "# A whole EC2 instance is an isolation boundary. These tiers are\n" +
			"# `trust: trusted` and bound to your runner group and workflow allowlist\n" +
			"# below. Each tier is derived from a shape, and its image is an AMI you\n" +
			"# build with `billet ami build`."
	}

	runIntro := "# One file, both roles: `billet server` is the control plane and\n" +
		"# `billet node` is a compute host that launches ONE EC2 instance per job.\n" +
		"# On this machine you run both, as two processes talking over the loopback\n" +
		"# address below; the jobs themselves run on instances in %s."
	if p.Profile == ProfileLocalService {
		runIntro = "# One file, both roles: `billet server` is the control plane and\n" +
			"# `billet node` is a compute host that launches ONE EC2 instance per job,\n" +
			"# run as " + p.serviceUnits() + " (`billet local up`) over the loopback\n" +
			"# address below; the jobs themselves run on instances in %s."
	}

	// The intro is rendered on its own and passed as an argument: splicing it
	// into the template text would make any % in an operator value corrupt the
	// argument list.
	intro := fmt.Sprintf(runIntro, e.Region)

	return fmt.Sprintf(`# billet — written by `+"`billet init --provider ec2`"+` for a cloud fleet.
#
%s
#
# Add a second orchestrator by running `+"`billet ca issue <name>`"+` here, copying
# the bundle to that host, and giving it a config with a node: section pointing
# at this one. Its node.name comes from the certificate.

server:
  # Loopback only, so billet opens nothing the network can reach.
  listen: %s

%s
  # THE CEILING BILLET ESCROWS AGAINST — for ec2 this is your CLOUD BUDGET, not a
  # measurement, because the compute runs in a region rather than on this host.
  # Placement charges the shape it will buy, so these are hard resource budgets:
  # billet never has more than this much vCPU or memory running at once.
  max_vcpu: %d
  max_memory: %s

github:
  org: %s

  # Filled in by `+"`billet github-app create --config <this file>`"+`.
  app_id: %d
  installation_id: %d
  private_key_path: %s

# This machine, as the ec2 orchestrator. It runs no jobs itself — it calls the
# EC2 API and the compute appears in the region.
node:
  server_addr: %s
  provider: ec2
  state_dir: %s
%s
  # REQUIRED for ec2 and equal to the ceiling above: there is no host to detect a
  # contribution from, so billet will not guess how much to buy on your account.
  max_vcpu: %d
  max_memory: %s

  ec2:
    region: %s
    # Where instances launch. Its route to GitHub is yours to arrange: a private
    # subnet needs a NAT gateway, a public one needs assign_public_ip.
    subnet_id: %s
    # Applied to trusted work.
%s%s
    # THE IAM POLICY THIS NODE NEEDS IS GENERATED FROM THIS FILE. Run
    # `+"`billet init iam --config <this file>`"+` and it prints exactly the policy
    # this node exercises. Its resources are tagged with this deployment's
    # identity and the policy is scoped to that, isolating this deployment from
    # any other in the same account; pass --account-wide only if it is the only one.
    #
    # The shapes billet may buy, each DECLARING what it holds — billet ships no
    # table of EC2 types. The prices were fetched at init time; VERIFY them, since
    # a stale price only understates the exposure billet reports and never gates a
    # job.
    instance_types:
%s
# Tiers are yours to define. The label is what users put in `+"`runs-on`"+`. Each is
# sized to a shape above and fits the budget, so a job on it can actually be
# placed and bought.
#
%s
tiers:
%s`,
		intro,
		yamlScalar(p.Listen),
		serverStateYAML(p.State, paths.serverState),
		p.VCPU, p.Memory,
		yamlScalar(org),
		appID, installationID,
		yamlScalar(paths.keyPath),
		yamlScalar(p.Listen),
		yamlScalar(paths.nodeState),
		lockBlock,
		p.VCPU, p.Memory,
		yamlScalar(e.Region),
		yamlScalar(e.SubnetID),
		ec2GroupsYAML("security_group_ids", e.SecurityGroups),
		untrustedBlock,
		ec2InstanceTypesYAML(e.Shapes),
		tierIntro,
		renderEC2Tiers(ec2Tiers(e.Shapes, p.VCPU, p.Memory), p, trusted),
	)
}

// renderEC2Tiers writes the derived catalogue, one entry per fitting shape.
func renderEC2Tiers(ts []tier, p Params, trusted bool) string {
	var b strings.Builder

	for i, t := range ts {
		if i > 0 {
			b.WriteString("\n")
		}

		fmt.Fprintf(&b, "  - label: %s\n    provider: ec2\n    vcpu: %d\n    memory: %s\n    image: %s\n",
			t.label, t.vcpu, t.memory, yamlScalar(p.Image))

		renderTierPolicy(&b, p, trusted)
	}

	return b.String()
}
