package ec2

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/guestassets"
	"github.com/junioryono/billet/internal/runnerrelease"
	"github.com/junioryono/billet/internal/version"
)

// AMIContract is what a runner AMI must satisfy for this billet to use it, and
// goes up whenever the image has to carry a newly required property.
//
// The analogue of firecracker's GuestContract, and it exists for the same reason:
// an image is built once and used for months, so "which billet made this" has to
// be a fact recorded ON the artifact rather than something inferred later. A
// creation date cannot answer it — an image made after a commit is not proof it
// was built from that commit — and an untagged image is the pre-contract case
// rather than a passing one.
//
// 1: /etc/docker/daemon.json selects the classic image store, so the Docker cache
// publishes with the images in it rather than an empty filesystem.
//
// 2: and the image carries the toolcache GitHub's declaration names, with the
// variables that make it findable.
//
// 3: and the runner is launched through billet-exec-env, so its registration and
// the cache bearer reach it in no process's argument list (#352).
//
// 4: and the image is GitHub's own template build for its architecture, run on
// the builder with billet's EC2 layer after it (ADR-022), carrying exactly the
// components images.accept named that the template offers, recorded in
// /etc/billet-image-components and in the components tag.
//
// THE TAG IS A PROMOTION, NOT A CREATE-TIME CLAIM, and that is the whole content
// of the number now. CreateImage stamps who owns the image and which billet made
// it; the contract tag is added afterwards, by CreateTags, once billet has BOOTED
// the image and proved the properties on the artifact. So an unstamped image is
// one nothing has verified — which both readers already treat as "no answer,
// rebuild" — rather than a claim nobody checked.
const AMIContract = 4

// contractFor is the contract a build of this architecture actually meets.
//
// BOTH ARCHITECTURES MEET IT: GitHub publishes a template for each, and each image
// is that architecture's template build. Parity on arm64 is parity with GitHub's
// arm64 image, which carries no Android SDK, CodeQL or PyPy.
//
// This function is kept rather than inlined because the stamp is what `billet
// check` reads INSTEAD of looking at the image: if a future architecture arrives
// that has no template, the lie has to be preventable here.
func contractFor(arch string) int {
	if arch == "x64" || arch == "arm64" {
		return AMIContract
	}

	return 1
}

const (
	amiContractTag   = "sh.billet.ami-contract"
	amiBuiltByTag    = "sh.billet.built-by"
	amiComponentsTag = "sh.billet.components"
)

// DefaultBuilderDiskGiB is the root volume a build gets when nobody says.
//
// Exported because `billet ami build` prints it in --builder-disk's help, and a
// default a flag describes must be the default the code uses.
//
// IT IS ALSO EVERY JOB'S ROOT. CreateImage snapshots this volume, and the job path
// clamps a job's root up to the image's, so what is left after the image is what a
// job has to build in.
//
// MEASURED ON REAL BUILDERS, 2026-10-25, running GitHub's whole template (ADR-022)
// on a 200 GiB root: the most used at any point was 42 GiB on x64 (c7i.4xlarge,
// before the template's reboot) and 35 GiB on arm64 (c7g.4xlarge), and the
// template's own cleanup left 40 and 33. GitHub's hosted runner leaves a job tens of
// gigabytes; 120 leaves about 75 on x64 after the image, and 80, the default for
// billet's own installers, would have left a job under 35.
//
// The figure this replaced was 80 for an image of 26.8 GiB, and its comment's
// lesson stands: the number that matters is the PEAK during the build, which the
// bootstrap's one free-space check cannot see, so the default is a size real
// builds completed on with room, not the image's size plus a guess.
const DefaultBuilderDiskGiB = 120

// minBuilderFreeGiB is what the bootstrap insists on before the build starts, and
// minBuilderDiskGiB is what a build must be given so that is reachable.
//
// TWO NUMBERS BECAUSE THEY MEASURE DIFFERENT THINGS. A volume's size is not its
// free space: the base image's own filesystem is already on it. So the bootstrap
// checks what it can actually use, and the flag is checked against a figure with
// the base image's footprint added back.
//
// 50 FREE BECAUSE THE TEMPLATE'S PEAK WAS 42 on x64 (measured 2026-10-25, with the
// base image's own two gigabytes in it), so a build admitted at the floor has
// about ten to spare at its high-water mark; 60 for the flag adds the base image
// and the filesystem's own share of a volume. It is a floor rather than a
// recommendation: the default is what leaves a job room.
const (
	minBuilderFreeGiB = 50
	minBuilderDiskGiB = 60
	// maxBuilderDiskGiB is a typo bound rather than a technical one. EBS goes far
	// higher; what makes this worth capping is that CreateImage snapshots the
	// builder's root, so the number is inherited by the AMI and, through the job
	// path's clamp, by every job launched from it.
	maxBuilderDiskGiB = 1000
)

// runnerVersionPattern is what may be interpolated into the release URL.
//
// strconv.Quote IS NOT SHELL QUOTING, AND THAT IS THE WHOLE REASON THIS EXISTS.
// It produces a GO double-quoted literal, and shell double quotes do not suppress
// command substitution -- so `--runner-version '$(poweroff)'` was emitted as
// "https://.../v$(poweroff)/..." and the substitution ran, as root, during
// provisioning. Measured, not reasoned about: the same construction with
// `$(id -u)` resolves to the uid before curl ever sees it.
//
// THE PAYLOAD THAT MATTERS IS `poweroff` ITSELF. It is billet's success signal --
// the build treats a self-stopped instance as a finished one -- so an injected
// shutdown does not merely run a command, it publishes a half-provisioned AMI as
// though it had passed every step.
//
// A RELEASE VERSION IS THE SHAPE GITHUB PUBLISHES, so nothing legitimate is
// refused: three numeric components with an optional prerelease or build suffix.
// Every character that makes injection possible -- $ ( ) ` ' " space, newline --
// is outside it.
var runnerVersionPattern = regexp.MustCompile(`^\d{1,6}\.\d{1,6}\.\d{1,6}([-+][0-9A-Za-z.-]{1,32})?$`)

// runnerDigestPattern is what may be written into the script as the runner's
// digest: exactly what sha256sum prints. Checked rather than quoted, for the
// reason runnerVersionPattern gives, and lowercase because `sha256sum -c`
// compares the text it is given.
var runnerDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsRunnerVersion says whether v is a version a build will write into its script.
func IsRunnerVersion(v string) bool { return runnerVersionPattern.MatchString(v) }

// IsRunnerSHA256 says whether s is a digest a build will write into its script.
func IsRunnerSHA256(s string) bool { return runnerDigestPattern.MatchString(s) }

// RunnerPlatform is actions/runner's own asset name for an image architecture.
//
// MAPPED, NOT ASSEMBLED. runnerrelease keys its checksums by actions/runner's
// spelling, and the rule there is that a caller maps its own concept onto it
// explicitly: an architecture nobody mapped is refused here rather than turned
// into a name no release publishes.
func RunnerPlatform(arch string) (string, error) {
	switch arch {
	case "x64":
		return "linux-x64", nil
	case "arm64":
		return "linux-arm64", nil
	default:
		return "", fmt.Errorf("ec2: runner architecture %q is not x64 or arm64", arch)
	}
}

// PinnedRunnerSHA256 is the digest billet pins for the pinned runner release's
// tarball on this architecture, and whether it pins one.
//
// READ FROM internal/runnerrelease's pinned.txt, the file the guest image build
// verifies the same release against, so both backends check one answer.
func PinnedRunnerSHA256(arch string) (string, bool) {
	platform, err := RunnerPlatform(arch)
	if err != nil {
		return "", false
	}

	return runnerrelease.PinnedSHA256For(platform)
}

// BuildSpec describes an AMI to build.
type BuildSpec struct {
	// BaseImage is the AMI the builder starts from. It must be EBS-backed and
	// Ubuntu 24.04.
	//
	// UBUNTU BECAUSE THE DECLARATION IS UBUNTU'S. GitHub publishes what a hosted
	// runner image contains as an apt package set for noble, and billet's
	// firecracker guest is built from exactly that. Provisioning this backend from
	// a dnf distribution meant hand-translating those names into a second,
	// unpublished list — which is the two-pins problem `runnerrelease` exists to
	// prevent, one backend drifting from the other with nothing to notice.
	BaseImage string
	// InstanceType is the shape of the BUILDER, which has nothing to do with the
	// shapes jobs will later run on. Bigger only makes the build faster.
	InstanceType string
	// BuilderDiskGiB is the root volume the BUILDER launches with. Zero takes
	// DefaultBuilderDiskGiB.
	//
	// THIS WAS UNSET AND THAT IS A DEFECT, not a simplification. Nothing here
	// wrote Ebs.VolumeSize at all, so every build inherited whatever the base
	// image declared -- 8GiB on Canonical's noble images. The declared package
	// set alone is most of that, and the toolcache does not fit in what is left,
	// so provisioning dies on ENOSPC. Under `set -e` that aborts before the
	// poweroff that signals success, so it produces no image rather than a broken
	// one; the cost is a paid builder and a failure that reads as an apt problem.
	//
	// It is the BUILDER's disk and not the runner's. CreateImage snapshots this
	// volume, so it also becomes the size of the root every job launched from the
	// image starts with -- which is why it is sized to what the image needs plus
	// working room, rather than to the largest build anybody might run.
	BuilderDiskGiB int64
	// Arch is the runner build to install: "x64" or "arm64". It must match
	// InstanceType, and nothing here can check that — a mismatch produces an image
	// whose runner will not execute.
	Arch string
	// RunnerVersion is the actions/runner release to install, without the "v".
	RunnerVersion string
	// RunnerSHA256 is the sha256 of that release's tarball for Arch, which the
	// builder checks before it unpacks anything. Required: the runner it installs
	// executes every job the image serves, so a download trusted on transport
	// alone is a mirror or a redirect deciding what runs. PinnedRunnerSHA256 is
	// the answer for the pinned release.
	RunnerSHA256 string
	// Name is the name given to the produced AMI. AWS requires it to be unique
	// within the account and region.
	Name string
	// Verify boots the produced AMI and asserts the contract on it before the
	// contract tag is written. It is the caller's default rather than this
	// package's: a zero BuildSpec that skipped verification would make the safe
	// behaviour the one you have to remember.
	Verify bool
	// VerifyInstanceType is the shape the VERIFIER runs on, which has nothing to do
	// with the builder's or with any job's. Empty takes defaultVerifierType for the
	// spec's architecture.
	VerifyInstanceType string
	// CACertPEM, when set, is a PEM bundle of one or more X.509 CAs baked into the
	// image's HOST trust store. The EC2 cache client speaks HTTPS to billet's
	// cache endpoint, whose certificate a private issuer signs; without that
	// issuer in the trust store every cache request fails its TLS handshake and
	// the job falls back to a cold fetch. It is validated before any paid builder
	// launches. The anchor lands in the host trust store only — job CONTAINERS do
	// not inherit it, which is correct: the cache client runs on the host.
	CACertPEM string

	// Accept is the components the image is completed with, from images.accept
	// (config.ImageComponents' names). The builder installs each one GitHub's
	// template for Arch offers and leaves out, and says so, each one it does not.
	Accept []string

	// PayloadBucket is where the build's payload is staged: GitHub's template, the
	// scripts that run it and billet's EC2 layer, far past EC2's 16384 bytes of
	// user data. Required.
	PayloadBucket string

	// payloadURL and payloadSHA256 are set by BuildImage after staging, and are
	// what the bootstrap in user data fetches and verifies; nonce is the build's
	// identity, which its verdict on the console carries.
	payloadURL    string
	payloadSHA256 string
	nonce         string
	// owed is the accepted components spec.Arch's template installs, which the
	// build's verdict must name exactly.
	owed []string
}

// BuildImage produces an AMI from GitHub's template for the spec's architecture,
// with billet's EC2 layer after it (ADR-022).
//
// WHY BILLET DRIVES THIS ITSELF rather than shipping a Packer template: GitHub's
// template is a Packer template, and the guest build already runs its steps in
// order from a plan billet derives from it (run-runner-images.sh). The builder
// runs the same runner against itself, so both backends' images are one build.
//
// HOW IT KNOWS THE BUILD FINISHED, without SSH, an agent, or any IAM the builder
// does not already have: the build prints a verdict carrying this build's nonce
// to the serial console, again and again, and on success then powers off. billet
// images the builder only when it has read the success verdict and the instance
// then stopped itself; a failure verdict ends the wait at once, naming the step.
//
// THE BUILDER IS TERMINATED ON EVERY FAILURE PATH THAT HAS AN ID, which is not the
// same as always, and the difference is worth stating because this is the one
// thing here that costs money for as long as it exists.
//
// If RunInstances commits and its response is lost — a transport failure, a
// context expiring mid-reply — launchBuilder returns an error and no id, so there
// is nothing to terminate and the builder runs until somebody notices. It carries
// the owner tag, which is how it is found.
//
// A CLIENT TOKEN NARROWS THAT TO A RECOVERY rather than a leak: the token is
// derived from the image name, which is already unique per account and region, so
// re-running the same build returns the SAME builder instead of buying a second
// one. That behaviour is not assumed — a live run measured EC2 refusing a reused
// token with IdempotentParameterMismatch, which is the same machinery.
func (p *Provider) BuildImage(ctx context.Context, spec BuildSpec) (string, error) {
	if spec.PayloadBucket == "" {
		return "", fmt.Errorf("ec2: a build needs a payload bucket: GitHub's template and the " +
			"scripts that run it are far past EC2's 16384 bytes of user data, so they are " +
			"staged in S3 and fetched by a bootstrap")
	}

	accept, err := checkAccept(spec.Accept)
	if err != nil {
		return "", err
	}

	spec.Accept = accept

	// WHAT THE BUILD OWES, decided before anything is bought: a component billet
	// cannot place in the template is refused now rather than after hours.
	owed, err := offeredComponents(spec.Arch, accept)
	if err != nil {
		return "", err
	}

	spec.owed = owed

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("ec2: invent a build nonce: %w", err)
	}

	spec.nonce = hex.EncodeToString(raw)

	// THE LAYER IS RENDERED BEFORE ANYTHING IS STAGED OR BOUGHT, so a spec it
	// refuses (a runner version, a digest, a CA bundle) costs nothing.
	layer, err := layerScript(spec)
	if err != nil {
		return "", err
	}

	// STAGED BEFORE THE BOOTSTRAP IS RENDERED, because the bootstrap names the URL
	// and the digest. Nothing is launched until the payload is readable, so a
	// builder never boots to fetch something that is not there.
	key, cleanup, err := p.stagePayload(ctx, &spec, layer)
	if err != nil {
		return "", err
	}

	// REMOVED WHETHER THE BUILD SUCCEEDS OR NOT. The object outlives its URL
	// otherwise, and it is billed and is one more copy of the payload.
	defer func() {
		if err := cleanup(); err != nil {
			p.log.ErrorContext(ctx, "the staged build payload could not be removed and "+
				"is still costing money; delete it by hand",
				"bucket", spec.PayloadBucket, "key", key, "error", err)
		}
	}()

	script, err := bootstrapScript(spec)
	if err != nil {
		return "", err
	}

	id, err := p.launchBuilder(ctx, spec, script)
	if err != nil {
		return "", err
	}

	defer func() {
		// A FRESH CONTEXT, because the ordinary reason to be here is that ctx has
		// already expired, and that is exactly when the builder must still be shot.
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()

		if _, err := p.Destroy(stop, id); err != nil {
			p.log.Error("the builder instance could not be terminated and is still costing "+
				"money; terminate it by hand",
				"instance", id, "error", err)
		}
	}()

	p.log.Info("building the image from GitHub's template", "instance", id,
		"arch", spec.Arch, "runner", spec.RunnerVersion, "accept", strings.Join(spec.Accept, ","))

	components, err := p.awaitBuild(ctx, id, spec)
	if err != nil {
		return "", err
	}

	image, err := p.createImage(ctx, id, spec.Name)
	if err != nil {
		return "", err
	}

	p.log.Info("image registered, waiting for it to become launchable", "image", image)

	if err := p.awaitImage(ctx, image); err != nil {
		// THE IMAGE OUTLIVES THIS FAILURE, and nothing here deletes it. billet
		// speaks no DeregisterImage and no DeleteSnapshot, so a registered AMI and
		// whatever snapshots AWS made for it persist — and because CreateImage
		// requires the name to be unique, re-running the same build fails on a
		// duplicate name pointing at nothing the operator can see. Naming it is the
		// difference between a puzzle and a chore.
		return "", fmt.Errorf("%w\n\nthe image %s was registered before this failed and still "+
			"exists, along with any snapshots behind it: deregister it before re-running a "+
			"build with the same name", err, image)
	}

	// AND THE IMAGE IS ASKED TO PROVE IT, on a machine booted from the image rather
	// than on the builder that made it. Every assertion above this line ran on a
	// host that had been apt-installed, part-configured and never rebooted, and the
	// two are not the same machine: the Docker gate asserted a driver on a daemon
	// apt had already started, read the pre-daemon.json answer, and failed every
	// build against an image that was correct. Anything a service reads at start,
	// anything cloud-init does at first boot, and anything a job's own `env -i`
	// can or cannot see are all invisible from the builder.
	//
	// A FAILURE LEAVES THE IMAGE UNSTAMPED AND SAYS SO. billet speaks no
	// DeregisterImage, so the AMI outlives this either way; what it does not do is
	// carry a contract tag claiming the property it just failed.
	if spec.Verify {
		if err := p.VerifyImage(ctx, VerifySpec{
			Image:        image,
			InstanceType: spec.VerifyInstanceType,
			Name:         spec.Name,
			Components:   &components,
		}); err != nil {
			// WHICH OF THE TWO THINGS HAPPENED, because they need different actions.
			// A verification that failed leaves an image with no contract tag; a
			// promotion whose read-back failed leaves one that may already carry it,
			// and telling an operator it is definitely unstamped sends them looking
			// for a tag they were told is absent. Both are re-run with the same
			// command, which is why only the sentence differs.
			if errors.Is(err, ErrPromotionUncertain) {
				return "", fmt.Errorf("%w\n\nthe image %s exists and PROVED itself; only the "+
					"stamp is in doubt, so check whether it carries %s before doing anything "+
					"else, and `billet ami verify %s` is safe to re-run either way",
					err, image, amiContractTag, image)
			}

			return "", fmt.Errorf("%w\n\nthe image %s exists and is NOT stamped with a contract, "+
				"so `billet check` will report it as needing a rebuild: fix the cause and run "+
				"`billet ami verify %s`, or deregister it", err, image, image)
		}

		return image, nil
	}

	// UNVERIFIED MEANS UNSTAMPED, and the caller is told rather than left to find
	// out from `billet check`. Skipping the verification skips the claim as well;
	// writing the contract tag anyway would give it two meanings, one of which is
	// "billet asserted this" and the other "billet did not look".
	p.log.Warn("this image was not verified, so it carries no contract tag; "+
		"`billet ami verify` boots it and stamps it",
		"image", image)

	return image, nil
}

// launchBuilder starts the one-shot machine an image is made from.
//
// DELIBERATELY NOT Launch. That path is job-shaped — it needs a JIT registration,
// it names the instance after a lease, and it applies the trusted/untrusted
// network split. A builder has no lease and no job; giving it a fake one would put
// something in the fleet that looks like work and is not.
//
// It DOES keep the two things that matter: the owner tag, so `billet ami build`
// cannot see or destroy another deployment's compute, and IMDSv2 with one hop.
func (p *Provider) launchBuilder(ctx context.Context, spec BuildSpec, script string) (string, error) {
	// THE BASE IMAGE IS INSPECTED FIRST, which also refuses one billet cannot use:
	// imageLayout is where an instance-store root is turned down, and a build from
	// such an image would fail at RunInstances with a parameter error instead.
	layout, err := p.imageLayout(ctx, spec.BaseImage)
	if err != nil {
		return "", err
	}

	rootDevice := layout.root

	userData, err := packUserData(script)
	if err != nil {
		return "", err
	}

	params := url.Values{}
	params.Set("Action", "RunInstances")
	params.Set("ImageId", spec.BaseImage)
	params.Set("InstanceType", spec.InstanceType)
	params.Set("MinCount", "1")
	params.Set("MaxCount", "1")
	params.Set("UserData", base64.StdEncoding.EncodeToString(userData))

	// STOPS ITSELF RATHER THAN TERMINATING, which is the whole signalling
	// mechanism: `poweroff` inside the guest has to leave a stopped instance for
	// CreateImage to snapshot, not a terminated one with no disk.
	params.Set("InstanceInitiatedShutdownBehavior", "stop")

	// THE TOKEN TURNS A LOST RESPONSE INTO A RECOVERY. AWS honours it and returns
	// the instance the first call created rather than starting a second, so
	// re-running a build whose response went missing costs nothing. The image name
	// is already required to be unique per account and region, which is exactly the
	// property a token needs.
	sum := sha256.Sum256([]byte("billet-ami-build:" + spec.Name))
	params.Set("ClientToken", hex.EncodeToString(sum[:])[:32])

	params.Set("MetadataOptions.HttpTokens", "required")
	params.Set("MetadataOptions.HttpPutResponseHopLimit", "1")

	if p.cfg.SubnetID != "" {
		params.Set("NetworkInterface.1.DeviceIndex", "0")
		params.Set("NetworkInterface.1.SubnetId", p.cfg.SubnetID)
		params.Set("NetworkInterface.1.DeleteOnTermination", "true")

		if p.cfg.AssignPublicIP {
			params.Set("NetworkInterface.1.AssociatePublicIpAddress", "true")
		}

		for i, sg := range p.cfg.SecurityGroupIDs {
			params.Set("NetworkInterface.1.SecurityGroupId."+strconv.Itoa(i+1), sg)
		}
	}

	// THE BUILDER CARRIES THE OWNER TAG TOO. Without it, a build in an account
	// running billet produces an instance this deployment cannot recognise as its
	// own — which is the same reason every launched job carries one.
	// THE BUILDER'S DISK GOES WITH IT. Terminating an instance does not delete a
	// volume the AMI asked to preserve, and this client cannot delete volumes at
	// all — so a base image with DeleteOnTermination=false would leave a disk
	// behind on every build, untagged and unfindable. Restating it is the same
	// thing the job path does for the same reason.
	params.Set("BlockDeviceMapping.1.DeviceName", rootDevice)
	params.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")

	// AND ITS SIZE, which nothing here used to state at all.
	//
	// The build inherited whatever the base image declared, which on Canonical's
	// noble images is 8GiB. GitHub's declared package set is most of that on its
	// own, so provisioning ran out of disk rather than out of anything to do.
	//
	// REFUSED, NOT CLAMPED, WHEN IT IS UNDER THE BASE IMAGE'S OWN ROOT. EBS will
	// not create a volume smaller than the snapshot behind it, so AWS rejects it
	// at RunInstances with a parameter error that names neither the base image nor
	// the number. Silently rounding up instead would spend an operator's money on
	// a disk they did not ask for, and the honest answer is cheap: say both
	// numbers and let them choose.
	// NEGATIVE IS REFUSED HERE AND NOT LEFT TO THE FLOOR CHECK BELOW, because that
	// check only fires when the base image reported a size billet could parse. An
	// image that reports none leaves rootGiB at zero, and without this a negative
	// would reach RunInstances as a VolumeSize AWS has to reject.
	// REFUSED BEFORE ANY API CALL, and not left to the floor check below: that one
	// only fires when the base image reported a size billet could parse, so an
	// image reporting none would let a nonsense value reach RunInstances.
	//
	// The lower bound is the image's own needs rather than EBS's minimum. A build
	// given less would launch, provision, and fail the script's own free-space
	// check on the instance -- true, correct, and paid for.
	// A CEILING TOO, because CreateImage snapshots this volume: a 3TiB builder
	// produces a 3TiB-root AMI, and the job path then clamps every job launched
	// from it up to the same size. One typo would be expensive on every job
	// rather than once.
	if spec.BuilderDiskGiB > maxBuilderDiskGiB {
		return "", fmt.Errorf("ec2: a %dGiB builder root is past the %dGiB this build will "+
			"ask for; CreateImage snapshots it, so every job launched from the image would "+
			"start with a volume that size too", spec.BuilderDiskGiB, maxBuilderDiskGiB)
	}

	if spec.BuilderDiskGiB != 0 && spec.BuilderDiskGiB < minBuilderDiskGiB {
		return "", fmt.Errorf("ec2: a builder root of %dGiB cannot hold this image; it needs at "+
			"least %dGiB free and the base image's own filesystem is on the same volume, so "+
			"--builder-disk must be at least %d",
			spec.BuilderDiskGiB, minBuilderFreeGiB, minBuilderDiskGiB)
	}

	disk := spec.BuilderDiskGiB
	if disk == 0 {
		disk = DefaultBuilderDiskGiB
	}

	if layout.rootGiB > 0 && disk < layout.rootGiB {
		// AN OPERATOR WHO TYPED A NUMBER READS THE ANSWER; ONE WHO DID NOT GETS A
		// BUILD.
		//
		// Refusing both was the first version and it broke a build that worked:
		// somebody whose base AMI declares a 40GiB root and who passes no flag was
		// told to "pass at least --builder-disk 40" for a flag they never used,
		// about a number DefaultBuilderDiskGiB chose for them. Before this branch
		// that build simply inherited 40GiB. The refusal's own justification --
		// not spending their money on a disk they did not ask for -- does not
		// apply when they did not ask for the default either, and DID choose the base
		// image.
		if spec.BuilderDiskGiB == 0 {
			p.log.Warn("this base image's root is larger than billet's default builder disk, "+
				"so the builder takes the image's size; EBS cannot create a volume smaller "+
				"than its snapshot",
				"image", spec.BaseImage, "image_gib", layout.rootGiB, "default_gib", disk)

			disk = layout.rootGiB
		} else {
			return "", fmt.Errorf("ec2: the builder was asked for a %dGiB root and %s declares "+
				"a %dGiB one; EBS cannot create a volume smaller than its snapshot, so pass "+
				"at least --builder-disk %d or start from a smaller base image",
				disk, spec.BaseImage, layout.rootGiB, layout.rootGiB)
		}
	}

	params.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.FormatInt(disk, 10))

	// AND EVERY OTHER DEVICE THE BASE IMAGE DECLARES. Restating only the root left
	// a worse hole than the one it closed: a base AMI with a non-root device marked
	// to survive leaks a volume on every build, AND CreateImage copies that mapping
	// into the produced image — so every JOB launched from it leaks one too. One
	// careless base image would have become a per-job leak in somebody's account.
	for i, d := range layout.devices {
		n := strconv.Itoa(i + 2)

		params.Set("BlockDeviceMapping."+n+".DeviceName", d.name)
		params.Set("BlockDeviceMapping."+n+".Ebs.DeleteOnTermination", "true")
	}

	// AND THE VOLUME CARRIES THE TAG TOO, so anything that does survive is
	// attributable rather than an anonymous disk in somebody's account.
	params.Set("TagSpecification.2.ResourceType", "volume")
	params.Set("TagSpecification.2.Tag.1.Key", ownerTag)
	params.Set("TagSpecification.2.Tag.1.Value", p.owner)

	params.Set("TagSpecification.1.ResourceType", "instance")
	params.Set("TagSpecification.1.Tag.1.Key", ownerTag)
	params.Set("TagSpecification.1.Tag.1.Value", p.owner)
	params.Set("TagSpecification.1.Tag.2.Key", "Name")
	params.Set("TagSpecification.1.Tag.2.Value", spec.Name+"-builder")

	var out runInstancesResponse

	if err = p.api.call(ctx, params, &out); err != nil {
		return "", fmt.Errorf("ec2: launch a builder from %s: %w", spec.BaseImage, err)
	}

	if len(out.Instances) == 0 || out.Instances[0].InstanceID == "" {
		return "", fmt.Errorf("ec2: launching a builder returned no instance id")
	}

	return out.Instances[0].InstanceID, nil
}

// builderState is what an instance is doing, and who made it do that.
type builderState struct {
	state  string
	reason string
}

// describeRaw reports one instance's state and the reason for it.
func (p *Provider) describeRaw(ctx context.Context, id string) (builderState, error) {
	params := url.Values{}
	params.Set("Action", "DescribeInstances")
	params.Set("InstanceId.1", id)

	var out describeInstancesResponse

	if err := p.api.call(ctx, params, &out); err != nil {
		// NOT VISIBLE YET IS NOT AN ERROR, and this code walked into the hazard the
		// live smoke test had measured hours earlier: DescribeInstances is
		// EVENTUALLY CONSISTENT, so an instance RunInstances has already returned an
		// id for can answer InvalidInstanceID.NotFound on the very next call. The
		// first version of this treated that as fatal and shot a builder that was
		// starting normally.
		//
		// Reported as "unknown" rather than "gone", which the caller polls through.
		// The caller's own timeout is what bounds an id that never appears — an
		// absence here is never proof.
		if code, ok := codeOf(err); ok && code == "InvalidInstanceID.NotFound" {
			return builderState{}, nil
		}

		return builderState{}, fmt.Errorf("ec2: ask what the builder %s is doing: %w", id, err)
	}

	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if i.InstanceID == id {
				return builderState{state: i.State.Name, reason: i.StateReason.Code}, nil
			}
		}
	}

	return builderState{}, nil
}

// stampImage adds the image's create-time tags to a CreateImage request.
//
// Separate from createImage so the stamp can be asserted without an API round
// trip: a test that could only observe these through a fake client would be
// testing the fake.
//
// THE CONTRACT IS NOT HERE, AND ITS ABSENCE IS THE DESIGN. These two tags are
// facts about the BUILD — who owns this image, and which billet produced it — and
// both are true the instant CreateImage returns. Whether the image MEETS a
// contract is a fact about the artifact that nothing has looked at yet, so it is
// written by promoteContract after the image has been booted and asserted on.
// Stamping it here made a build's claim about itself indistinguishable from a
// verified one, and left a failed verification with an AMI already claiming the
// contract it had just failed.
func stampImage(params url.Values, owner string) {
	params.Set("TagSpecification.1.ResourceType", "image")
	params.Set("TagSpecification.1.Tag.1.Key", ownerTag)
	params.Set("TagSpecification.1.Tag.1.Value", owner)
	params.Set("TagSpecification.1.Tag.2.Key", amiBuiltByTag)
	params.Set("TagSpecification.1.Tag.2.Value", version.String())
}

// createImage snapshots the stopped builder.
func (p *Provider) createImage(ctx context.Context, instance, name string) (string, error) {
	params := url.Values{}
	params.Set("Action", "CreateImage")
	params.Set("InstanceId", instance)
	params.Set("Name", name)
	params.Set("Description", "billet runner image")

	// NO REBOOT, and it is safe here for a reason that would not hold generally:
	// the instance is already STOPPED, so there is nothing in flight for a reboot
	// to flush. On a running instance this flag risks an inconsistent snapshot.
	params.Set("NoReboot", "true")

	// STAMP WHAT BUILT THIS, because until now the image recorded nothing.
	//
	// The builder instance and its volume are tagged; the IMAGE was not, so the
	// only provenance an AMI carried was whatever name a person typed. That left
	// no way to answer "was this built by a billet that writes daemon.json?" —
	// which is exactly the question that went unanswered while a stale AMI lost
	// every cached image for nine days.
	//
	// The owner tag matches the builder's, so an image is as attributable as the
	// instance that made it — and it is also what a bundled IAM policy conditions
	// the contract promotion on, so this stamp is what makes that grant reach only
	// images this build made.
	stampImage(params, p.owner)

	var out createImageResponse

	if err := p.api.call(ctx, params, &out); err != nil {
		// AMBIGUOUS, AND SAID SO. CreateImage accepts no client token — it is in
		// neither AWS's idempotent-by-default list nor its token list — so a request
		// that commits and loses its response leaves an AMI and its snapshots behind
		// with billet holding no id. Naming what to search for is the only thing
		// available; inventing a mechanism the API does not offer is not.
		return "", fmt.Errorf("ec2: register an image from the builder %s: %w\n\nthis request "+
			"may have COMMITTED before failing, and CreateImage has no idempotency token: "+
			"search for an image named %q before retrying, or the retry will fail on a "+
			"duplicate name", instance, err, name)
	}

	if out.ImageID == "" {
		return "", fmt.Errorf("ec2: CreateImage for %s returned no image id", instance)
	}

	return out.ImageID, nil
}

// awaitImage waits until the new AMI can actually be launched from.
func (p *Provider) awaitImage(ctx context.Context, image string) error {
	for {
		params := url.Values{}
		params.Set("Action", "DescribeImages")
		params.Set("ImageId.1", image)

		var out describeImagesResponse

		if err := p.api.call(ctx, params, &out); err != nil {
			// NOT VISIBLE YET IS NOT FAILED. DescribeImages is eventually consistent
			// like the rest of them, so an AMI CreateImage has just returned an id
			// for can answer InvalidAMIID.NotFound on the next call. This package
			// has now made that mistake twice — once against instances, in code
			// written the same day it was measured — so it is worth naming as a
			// class rather than patching per call site.
			if code, ok := codeOf(err); ok && code == "InvalidAMIID.NotFound" {
				if err := sleepFor(ctx, 15*time.Second); err != nil {
					return fmt.Errorf("ec2: image %s never became visible: %w", image, err)
				}

				continue
			}

			return fmt.Errorf("ec2: ask whether %s is ready: %w", image, err)
		}

		if len(out.Images) > 0 {
			switch out.Images[0].State {
			case "available":
				return nil

			case "failed", "invalid", "error":
				return fmt.Errorf("ec2: image %s ended in state %q", image, out.Images[0].State)
			}
		}

		if err := sleepFor(ctx, 15*time.Second); err != nil {
			return fmt.Errorf("ec2: image %s never became available: %w", image, err)
		}
	}
}

// sleepFor waits, or reports why it stopped waiting.
//
// NOT time.After, which this repository forbids because it leaks its timer until
// it fires — harmless once, and this is inside a poll loop that can run for
// twenty minutes.
func sleepFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// toolcacheDir and imageEnvFile are the guest image's paths, deliberately.
//
// ONE CONTRACT ACROSS BOTH BACKENDS. `/opt/hostedtoolcache` is what
// @actions/tool-cache looks for and what scripts/build-guest-image.sh bakes;
// `/etc/billet-image-env` is the KEY=VALUE file the guest's runner service reads
// into the job's environment. Spelling either of them differently here would make
// a workflow's toolcache lookup a per-backend question, which is exactly the
// asymmetry this line of work exists to remove.
const (
	toolcacheDir = "/opt/hostedtoolcache"
	imageEnvFile = "/etc/billet-image-env"
)

// privilegeDrop is how the runner stops being root, written ONCE.
//
// The build validation and the job entry point both need this exact invocation,
// and writing it twice is how a change to one silently stops being true of the
// other. That is not hypothetical: a hand-written second copy meant deleting
// --init-groups from the entry point left the validation passing and every job
// failing.
//
// --init-groups is load-bearing rather than decorative — setpriv requires a
// supplementary-group option when it sets the primary GID, and without it the
// runner never gets the docker group either.
//
// THE ENVIRONMENT IS NOT ON THIS COMMAND LINE. `env -i` empties it and
// billet-exec-env builds it from the lines on descriptor 3, so the registration
// and the cache bearer appear in no argv (#352). Every use is followed by the
// command and then runnerEnvStream.
const privilegeDrop = "setpriv --reuid=runner --regid=runner --init-groups \\\n" +
	"  env -i " + execEnvPath

// execEnvPath is where the image carries guestassets' exec-env.sh; the guest
// image installs it at the same path.
const execEnvPath = "/usr/local/bin/billet-exec-env"

// entryPointPath is the entry point a tier names in its command.
const entryPointPath = "/usr/local/bin/billet-runner"

// runnerBaseEnv is what every runner-side command's environment starts from,
// before the image's own variables. setpriv does not reset the environment, so
// without HOME the runner inherits cloud-init's HOME=/root, registers fine, and
// then fails every job step that writes to $HOME. LANG is GitHub's Ubuntu
// runners' C.UTF-8: without it every job runs in the C locale, where tools that
// normalise or print Unicode fail or mangle it.
var runnerBaseEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/home/runner",
	"USER=runner",
	"LOGNAME=runner",
	"LANG=C.UTF-8",
}

// runnerEnvStream hands billet_env, as writeRunnerEnv built it, to
// billet-exec-env on descriptor 3. A here-document reaches it through a pipe or
// an unlinked file, never through an argument list.
const runnerEnvStream = " 3<<BILLETENVEOF\n${billet_env}end\nBILLETENVEOF\n"

// writeRunnerEnv emits shell that sets billet_env to runnerBaseEnv followed by
// the image's own variables from imageEnvFile, one NAME=VALUE line each, every
// line ending in a newline. indent prefixes each emitted line.
//
// THE IMAGE'S OWN VARIABLES, READ AT JOB TIME. `env -i` means a variable not
// listed does not exist for the job, whatever /etc/environment says, so the
// toolcache would be on disk and invisible. Reading the FILE rather than baking
// the values in is what lets the toolcache install append JAVA_HOME and the
// JAVA_HOME_*_X64 set after the JDKs exist, which is the only point at which
// those names are known.
//
// ONLY A VARIABLE'S NAME IS TAKEN, as the guest takes it, so a comment, a blank
// line or a name billet-exec-env would refuse is skipped rather than ending the
// launch: an image whose file is damaged should run jobs that download their own
// toolchains, not refuse to run jobs at all.
func writeRunnerEnv(b *strings.Builder, indent string) {
	b.WriteString(indent + "billet_env='" + strings.Join(runnerBaseEnv, "\n") + "\n'\n")
	b.WriteString(indent + "if [ -r " + imageEnvFile + " ]; then\n")
	b.WriteString(indent + "  while IFS= read -r billet_line; do\n")
	b.WriteString(indent + "    case \"$billet_line\" in\n")
	b.WriteString(indent + "      OPTIND=*) continue ;;\n")
	b.WriteString(indent + "      [A-Za-z_]*=*) ;;\n")
	b.WriteString(indent + "      *) continue ;;\n")
	b.WriteString(indent + "    esac\n")
	b.WriteString(indent + "    case \"${billet_line%%=*}\" in\n")
	b.WriteString(indent + "      *[!A-Za-z0-9_]*) continue ;;\n")
	b.WriteString(indent + "    esac\n")
	b.WriteString(indent + "    billet_env=\"$billet_env$billet_line\n\"\n")
	b.WriteString(indent + "  done <" + imageEnvFile + "\n")
	b.WriteString(indent + "fi\n")
}

// jobTimingHookPath ends in the extension GitHub uses to select the hook's
// interpreter. A shebang and executable mode are not sufficient.
const jobTimingHookPath = "/usr/local/bin/billet-job-started.sh"

// jobTimingHook is invoked by the runner after it accepts a job and before it
// starts any job steps. It deliberately has no `set -e`: timing is observation,
// and a missing or malformed timestamp must never turn into a failed workflow.
func jobTimingHook() string {
	return `#!/bin/sh
launch=${BILLET_LAUNCH_EPOCH_NS:-}
runner=${BILLET_RUNNER_START_EPOCH_NS:-}

case "$launch:$runner" in
  *[!0-9:]*|:*|*:) exit 0 ;;
esac

now=$(date +%s%N 2>/dev/null) || exit 0
case "$now" in
  ''|*[!0-9]*) exit 0 ;;
esac

[ "$launch" -le "$runner" ] 2>/dev/null || exit 0
[ "$runner" -le "$now" ] 2>/dev/null || exit 0

printf 'billet timing: launch_to_job_start_ms=%s launch_to_runner_ms=%s runner_to_job_start_ms=%s\n' \
  "$(((now-launch)/1000000))" \
  "$(((runner-launch)/1000000))" \
  "$(((now-runner)/1000000))"
exit 0
`
}

// maxCACertPEM bounds the trust bundle a build will bake into an image.
//
// BOUNDED ON ITS OWN TERMS: a trust store is worth bounding because it is a trust
// store, and every certificate in it is an authority every job on that machine will
// believe. The bound was once set by the provisioning script's share of EC2's 16384
// bytes of user data; the layer travels in the staged payload now, so the
// transport no longer decides it.
//
// WHAT 2 KiB ADMITS, MEASURED WITH openssl RATHER THAN ASSERTED. A PEM
// RSA-2048 self-signed root is 1115 bytes and an RSA-4096 root is 1822, so either
// fits. An RSA-2048 root AND intermediate concatenated is 2181, and is refused.
//
// REFUSING THE CHAIN IS CORRECT. A trust store needs the ROOT: an intermediate is
// presented by the endpoint during the handshake and a client that has the root
// will build the path. Baking the intermediate in adds an authority every job on
// the machine believes, for nothing.
const maxCACertPEM = 2 << 10

// canonicalizeCACert validates a --ca-cert bundle and returns billet's own
// re-encoding of it, or an error. Empty in, empty out: the CA is optional.
//
// IT NEVER RETURNS THE OPERATOR'S BYTES. Each certificate is parsed and
// re-emitted from its parsed DER (cert.Raw), so the string that reaches the
// layer is standard PEM billet produced — not attacker text. That
// is what closes the injection: the raw input could carry a heredoc delimiter, a
// `poweroff`, or a secret in bytes that pem.Decode skips, and none of it survives
// canonicalization. Trailing bytes that are not a certificate are refused rather
// than silently dropped, so a file that is half certificate and half something
// else is a stop, not a guess.
//
// A non-empty bundle must be one or more PEM CERTIFICATE blocks, each a parseable
// X.509 certificate, at least one a CA — so a private key pasted by mistake (which
// must never enter a machine image), a bundle with no CA at all, or non-PEM bytes
// are refused before a paid builder launches. A CA accompanied by leaf
// certificates is allowed: what matters is that at least one anchor is a CA.
func canonicalizeCACert(pemData string) (string, error) {
	if pemData == "" {
		return "", nil
	}

	if len(pemData) > maxCACertPEM {
		return "", fmt.Errorf("ec2: --ca-cert is %d bytes and the limit is %d; a trust bundle "+
			"this large is not a chain, and every certificate in it becomes an authority "+
			"every job on the built image believes", len(pemData), maxCACertPEM)
	}

	rest := []byte(pemData)
	certs, cas := 0, 0

	var out strings.Builder

	for {
		var block *pem.Block

		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}

		if block.Type != "CERTIFICATE" {
			return "", fmt.Errorf("ec2: --ca-cert holds a %q PEM block; it must contain only CA "+
				"certificates, never a private key, which would be baked into the image", block.Type)
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("ec2: --ca-cert has a CERTIFICATE block that is not a valid "+
				"X.509 certificate: %w", err)
		}

		certs++
		if cert.IsCA {
			cas++
		}

		out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	}

	// Checked in this order so the message fits the failure: input with no block at
	// all is "no certificate", not "trailing data" (the trailing data IS the whole
	// input). pem.Decode skips text before and between blocks — a bundle may carry
	// comments — but trailing bytes AFTER a real certificate are refused: that is
	// where injected shell or a stray secret would sit in a file that looks like a
	// cert.
	// pem.Decode SILENTLY SKIPS a block whose base64 body is corrupt as long as a
	// valid block follows, so a bundle of good+mangled+good canonicalizes to
	// good+good with no error — the operator pays for a builder and bakes an AMI
	// missing an anchor it thought it installed. A CERTIFICATE header appears only
	// in PEM armor (base64 has no '-'), so counting them against the number parsed
	// turns that silent drop into a stop. A key block is caught by the type check
	// in the loop before this, so a mismatch here means a malformed certificate.

	if headers := strings.Count(pemData, "-----BEGIN CERTIFICATE-----"); headers != certs {
		return "", fmt.Errorf("ec2: --ca-cert has %d CERTIFICATE blocks but only %d parsed as "+
			"valid X.509 certificates; a block is malformed", headers, certs)
	}

	if certs == 0 {
		return "", fmt.Errorf("ec2: --ca-cert contains no PEM CERTIFICATE block")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return "", fmt.Errorf("ec2: --ca-cert has trailing data after the last certificate that " +
			"is not itself a PEM certificate")
	}
	if cas == 0 {
		return "", fmt.Errorf("ec2: --ca-cert holds %d certificate(s) but none is a CA; the cache "+
			"endpoint's certificate is signed by a CA, and that CA is what belongs here", certs)
	}

	return out.String(), nil
}

// layerScript is billet's EC2 layer: what build-ami.sh runs on the builder after
// GitHub's template and the components, as the last thing before the image is
// cleaned and the builder powers off.
//
// THE TEMPLATE IS THE IMAGE; THIS IS WHAT MAKES IT A BILLET RUNNER. The runner
// release billet pins, checked against its digest, the cache issuer in the host
// trust store, the entry point a tier names and the helpers it runs, and proof
// that the runner executes the way a job's will and that Docker keeps its images
// where the cache attaches. Everything a workflow finds otherwise is GitHub's.
//
// IT RUNS UNDER bash FROM build-ami.sh, whose verdict carries its failure: a
// failure here is `step=layer`, under `set -e`.
func layerScript(spec BuildSpec) (string, error) {
	if spec.RunnerVersion == "" {
		return "", fmt.Errorf("ec2: a build needs a runner version to install")
	}

	// CHECKED BEFORE IT REACHES THE SCRIPT, not quoted on the way in. See
	// runnerVersionPattern: the quoting that was there is Go's, not the shell's.
	if !runnerVersionPattern.MatchString(spec.RunnerVersion) {
		return "", fmt.Errorf("ec2: %q is not an actions/runner release version; it is "+
			"interpolated into a URL in a script that runs as root, so only a release-shaped "+
			"version is accepted", spec.RunnerVersion)
	}

	platform, err := RunnerPlatform(spec.Arch)
	if err != nil {
		return "", err
	}

	// REFUSED BEFORE A BUILDER EXISTS. An empty digest would render a check that
	// sha256sum rejects as malformed, so the build would fail closed, hours and one
	// paid builder later, naming nothing an operator set.
	if !runnerDigestPattern.MatchString(spec.RunnerSHA256) {
		return "", fmt.Errorf("ec2: the runner's sha256 %q is not 64 lowercase hex digits; the "+
			"build checks the actions/runner tarball against it before unpacking, and "+
			"refuses to install a runner it cannot check", spec.RunnerSHA256)
	}

	// VALIDATED AND RE-ENCODED BEFORE A PAID BUILDER EXISTS. layerScript runs
	// before launchBuilder, so a bundle that is not a CA — a private key pasted by
	// mistake, a leaf certificate, PEM that does not parse, or trailing shell after
	// a real certificate — is refused here rather than baked into an image that
	// cannot reach the cache, or worse carries a secret. The returned PEM is
	// billet's own re-encoding, never the operator's bytes.
	caCert, err := canonicalizeCACert(spec.CACertPEM)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eux\n")

	// A HOME, BECAUSE A SYSTEM UNIT GIVES ITS SERVICE NONE, and the runner's own
	// tooling, run below as root, assumes one.
	b.WriteString("export HOME=/root\n")

	// THE CACHE ISSUER IN THE HOST TRUST STORE, if one was given. The cache client
	// runs on the host and speaks HTTPS to a privately-signed endpoint, so its
	// issuer has to be an anchor or every request fails the handshake. The
	// canonical PEM is base64-encoded and decoded into the anchor in the guest, and
	// installed before the runner so a first job's cache request already trusts it.
	// Job CONTAINERS do not inherit this store — that is intended; the cache client
	// is host-side.
	if caCert != "" {
		// A BASE64 BLOB DECODED IN THE GUEST, not a heredoc. The canonical PEM is
		// billet's own bytes, but there is no need for a delimiter that operator
		// input might one day contain: base64 of the whole bundle is a single line
		// of [A-Za-z0-9+/=] with no quote and no newline, so single-quoting it is
		// injection-proof by construction. base64 is coreutils, present before
		// ca-certificates is installed.
		// DEBIAN'S TRUST STORE, NOT RED HAT'S. The anchor directory and the refresh
		// command are both distribution-specific: /etc/pki/ca-trust with
		// `update-ca-trust` is the dnf shape, and writing there on Ubuntu leaves a
		// file nothing reads and a trust store nothing added to -- so every cache
		// request fails its handshake and the job silently falls back to a cold
		// fetch, which is the failure this anchor exists to prevent.
		//
		// THE .crt EXTENSION IS LOAD-BEARING HERE. update-ca-certificates only
		// considers files ending in .crt under /usr/local/share/ca-certificates;
		// any other name is ignored without complaint.
		blob := base64.StdEncoding.EncodeToString([]byte(caCert))
		anchor := "/usr/local/share/ca-certificates/billet-cache-ca.crt"

		b.WriteString("install -d -m 0755 /usr/local/share/ca-certificates\n")
		b.WriteString("printf '%s' '" + blob + "' | base64 -d > " + anchor + "\n")
		b.WriteString("chmod 0644 " + anchor + "\n")
		b.WriteString("update-ca-certificates\n")

		// PROVED, NOT ASSUMED. update-ca-certificates exits 0 whether or not it
		// added anything, so the only way to know the anchor took is to look for
		// it in the bundle it maintains. A trust store that silently did not
		// update produces a cache that fails every handshake at job time.
		//
		// WHOLE CERTIFICATES, ONE COMMAND, FAIL-CLOSED. Three spellings of this
		// check were wrong before this one, each in a way that made it accept a
		// trust store that had not been updated -- so the shape matters more than
		// the comparison:
		//
		// `grep -qFf anchor.crt bundle` is VACUOUS. -f reads one pattern per LINE
		// and succeeds if ANY matches, and every certificate shares the line
		// `-----BEGIN CERTIFICATE-----`. Measured against a bundle holding a
		// different certificate entirely, it matched.
		//
		// `! grep ... | grep -q .` NEVER ABORTS. POSIX says the -e setting is
		// IGNORED for a pipeline preceded by `!`, so it computes the right answer
		// and lets the build run on to its success verdict. Measured on /bin/sh.
		//
		// `if grep ... | grep -q .` FAILS OPEN ON A READ ERROR. Without pipefail
		// the pipeline's status is the LAST command's, so an unreadable or missing
		// bundle makes the first grep produce nothing, the second exit 1, the
		// condition read false, and provisioning continue. Measured on both GNU
		// grep 3.12 and BSD grep.
		//
		// Hence: one awk invocation whose own exit status IS the answer, inside
		// `if !`, so a parse error, an unreadable file and a missing anchor all
		// land on the same explicit failure. It compares complete BEGIN/END blocks
		// rather than loose lines, and requires EVERY certificate in the anchor to
		// be present -- an operator may legitimately pass a bundle. No temporary
		// file, so there is no fixed path under /tmp for the root shell to follow
		// into a symlink.
		b.WriteString("if ! awk -v A=" + anchor + " '\n")
		// EXACT ARMOR, NOT A PREFIX MATCH. `/^-----BEGIN CERTIFICATE-----/`
		// also matches `-----BEGIN CERTIFICATE-----ANYTHING`, so a bundle
		// carrying the anchor's body between two lines that are not PEM at all
		// satisfied the check. Measured: it was accepted. Comparing the whole
		// line costs nothing and removes the class.
		//
		// AND THE STRUCTURE IS VALIDATED, not assumed: a BEGIN inside a block, an
		// END with no BEGIN, and a block still open at end of input are each a
		// bundle this program cannot reason about, and the only safe answer about
		// a file it cannot parse is to refuse the build.
		b.WriteString("function arm(s) { sub(/\\r$/, \"\", s); return s }\n")
		b.WriteString("BEGIN {\n")
		b.WriteString("  n = 0; inb = 0; cur = \"\"\n")
		b.WriteString("  while ((r = (getline l < A)) > 0) {\n")
		b.WriteString("    l = arm(l)\n")
		b.WriteString("    if (l == \"-----BEGIN CERTIFICATE-----\") {\n")
		b.WriteString("      if (inb) { bad = 1; exit 1 }\n")
		b.WriteString("      inb = 1; cur = \"\"; continue\n")
		b.WriteString("    }\n")
		b.WriteString("    if (l == \"-----END CERTIFICATE-----\") {\n")
		b.WriteString("      if (!inb || cur == \"\") { bad = 1; exit 1 }\n")
		b.WriteString("      n++; want[n] = cur; inb = 0; continue\n")
		b.WriteString("    }\n")
		// CONCATENATED, SO LINE WRAPPING DOES NOT DECIDE THE ANSWER. What must
		// match is the certificate, and a bundle generator is free to wrap the
		// base64 differently from billet's canonical 64 columns. A line-wise
		// comparison would call that a missing anchor and fail every build.
		//
		// \r IS STRIPPED for the same reason: a CRLF bundle would otherwise be a
		// false rejection, and that is the direction that breaks a working fleet
		// rather than the direction that publishes a broken image.
		b.WriteString("    if (inb) cur = cur l\n")
		b.WriteString("  }\n")
		b.WriteString("  close(A)\n")
		// r < 0 is a read error on the anchor, n == 0 an anchor with no
		// certificate in it, inb an anchor whose last block never closed. All
		// three are failures, and all three must survive END.
		b.WriteString("  if (r < 0 || n == 0 || inb) { bad = 1; exit 1 }\n")
		b.WriteString("}\n")
		b.WriteString("{\n")
		b.WriteString("  line = arm($0)\n")
		b.WriteString("  if (line == \"-----BEGIN CERTIFICATE-----\") {\n")
		b.WriteString("    if (binb) { bad = 1; exit 1 }\n")
		b.WriteString("    binb = 1; bcur = \"\"; next\n")
		b.WriteString("  }\n")
		b.WriteString("  if (line == \"-----END CERTIFICATE-----\") {\n")
		b.WriteString("    if (!binb) { bad = 1; exit 1 }\n")
		// BOTH SIDES ARE BUILT BY CONCATENATION, AND THAT IS WHAT MAKES `==` A
		// STRING COMPARISON. awk compares numerically when both operands carry the
		// strnum attribute, which values from `getline` and `$0` do -- measured,
		// `getline`ing "1e2" and "100" into two variables makes them EQUAL.
		// Concatenation always yields a string, so accumulating into `cur`/`bcur`
		// keeps this a comparison of certificates. Comparing a raw `$0` or a bare
		// `getline` value here would reintroduce it.
		b.WriteString("    for (i = 1; i <= n; i++) if (want[i] == bcur) seen[i] = 1\n")
		b.WriteString("    binb = 0; next\n")
		b.WriteString("  }\n")
		b.WriteString("  if (binb) bcur = bcur line\n")
		b.WriteString("}\n")
		// `exit` in BEGIN still runs END, so the bad flag has to be re-checked
		// here or END's own exit would overwrite the failure with a success.
		// binb is the same rule for the bundle: a block left open at end of input
		// is a file this cannot parse.
		b.WriteString("END {\n")
		b.WriteString("  if (bad || binb) exit 1\n")
		b.WriteString("  for (i = 1; i <= n; i++) if (!(i in seen)) exit 1\n")
		b.WriteString("  exit 0\n")
		b.WriteString("}\n")
		b.WriteString("' /etc/ssl/certs/ca-certificates.crt; then\n")
		b.WriteString("  echo 'the cache CA anchor is not in " +
			"/etc/ssl/certs/ca-certificates.crt, so every cache request would fail " +
			"its handshake' >&2\n")
		b.WriteString("  exit 1\n")
		b.WriteString("fi\n")
	}
	// GITHUB'S DOCKER, WITH BILLET'S daemon.json. The template's install-docker.sh
	// installs the engine, buildx and compose, and the docker-daemon difference
	// writes the classic image store's configuration before the script first starts
	// the daemon. The two plugins are executed so an image without them stops here,
	// because the cache's own actions drive both.
	b.WriteString("docker buildx version\n")
	b.WriteString("docker compose version\n")
	b.WriteString("systemctl enable docker\n")

	// A NON-ROOT USER, because the runner refuses to run as root without an
	// override and running untrusted jobs as root would give away the only
	// isolation this backend has above the instance boundary. The template's
	// build-user difference has already made it, so its home holds what GitHub's
	// steps put in /etc/skel after it; this only states the property.
	b.WriteString("id runner || useradd -m -s /bin/bash runner\n")
	b.WriteString("usermod -aG docker runner\n")

	b.WriteString("mkdir -p /opt/actions-runner\n")
	b.WriteString("cd /opt/actions-runner\n")

	release := "https://github.com/actions/runner/releases/download/v" + spec.RunnerVersion +
		"/actions-runner-" + platform + "-" + spec.RunnerVersion + ".tar.gz"

	b.WriteString("curl -fsSL -o runner.tar.gz " + strconv.Quote(release) + "\n")

	// VERIFIED BEFORE IT IS UNPACKED, against the digest the guest build checks
	// the same release with: this runner executes every job the image serves.
	//
	// AN `if` WITH ITS OWN `exit 1`, NOT AN `&&` CHAIN. Under `set -e` a failing
	// `&&` list does not stop the script, so `curl && check && tar` would fail
	// its check and carry on to the next line and eventually to the success verdict,
	// which billet reads as a finished build. This holds with or without `set -e`, and
	// a download that failed has no file for the check to accept.
	b.WriteString("if ! printf '%s  %s\\n' '" + spec.RunnerSHA256 + "' runner.tar.gz | " +
		"sha256sum -c -; then\n")
	b.WriteString("  echo 'the actions/runner tarball for " + platform + " " + spec.RunnerVersion +
		" does not hash to " + spec.RunnerSHA256 + "; refusing to unpack it' >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	b.WriteString("tar xzf runner.tar.gz\n")
	b.WriteString("rm runner.tar.gz\n")
	b.WriteString("cat > /opt/actions-runner/billet-runner-service <<'BILLETRUNNEREOF'\n")
	b.WriteString(guestassets.RunnerServiceScript)
	if !strings.HasSuffix(guestassets.RunnerServiceScript, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("BILLETRUNNEREOF\n")
	b.WriteString("chmod 0755 /opt/actions-runner/billet-runner-service\n")
	b.WriteString("chown -R runner:runner /opt/actions-runner\n")

	// THE SUPPORTED RUNNER HOOK, rather than an edit to run.sh. GitHub invokes it
	// after assigning the job and before running any job steps, which is the first
	// instant that separates registration/pickup from the workflow itself.
	b.WriteString("cat > " + jobTimingHookPath + " <<'BILLETJOBEOF'\n")
	b.WriteString(jobTimingHook())
	b.WriteString("BILLETJOBEOF\n")
	b.WriteString("chmod 0755 " + jobTimingHookPath + "\n")
	b.WriteString("cat > /usr/local/bin/billet-docker-cache <<'BILLETDOCKEREOF'\n")
	b.WriteString(guestassets.DockerCacheScript)
	if !strings.HasSuffix(guestassets.DockerCacheScript, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("BILLETDOCKEREOF\n")
	b.WriteString("chmod 0755 /usr/local/bin/billet-docker-cache\n")
	b.WriteString("cat > " + execEnvPath + " <<'BILLETEXECENVEOF'\n")
	b.WriteString(guestassets.ExecEnvScript)
	if !strings.HasSuffix(guestassets.ExecEnvScript, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("BILLETEXECENVEOF\n")
	b.WriteString("chmod 0755 " + execEnvPath + "\n")

	// THE ENTRY POINT A TIER NAMES. billet's boot script exports the JIT config and
	// execs the tier's command AS ROOT, so something has to drop privileges without
	// losing that variable. setpriv does it in one process with no shell in between.
	b.WriteString("cat > " + entryPointPath + " <<'BILLETEOF'\n")
	b.WriteString(entryPointScript())
	b.WriteString("BILLETEOF\n")
	b.WriteString("chmod 0755 " + entryPointPath + "\n")

	// THE RUNNER HAS TO RUN BEFORE THIS COUNTS AS SUCCESS.
	//
	// An architecture mismatch is otherwise INVISIBLE to the build: an arm64
	// tarball extracts perfectly well on x64, every command above succeeds, the
	// script reaches poweroff, and billet registers an image whose runner cannot
	// exec. The failure then surfaces on somebody's first job as a machine that
	// booted, registered nothing, and left the job queued — the exact silent class
	// this whole issue exists to close. An earlier version of this comment claimed
	// nothing here could check that. The guest can, by running it.
	// RUN IT THE WAY A JOB WILL, which is the difference between validating the
	// binary and validating the image.
	//
	// An earlier version used `sudo -u runner`, which proved the runner executes and
	// proved nothing about the path a job actually takes. A dnf base image carrying
	// sudo but NOT setpriv would have passed that check, powered off, been imaged —
	// and then failed on every job before the runner ever started. So the check goes
	// through the same setpriv invocation the entry point uses, which proves in one
	// command that setpriv exists, that the user switch works, that the runner user
	// can reach and execute the binary, and that .NET starts (a missing libicu dies
	// here rather than at registration).
	writeRunnerEnv(&b, "")
	b.WriteString(privilegeDrop + " /opt/actions-runner/bin/Runner.Listener --version" +
		runnerEnvStream)

	// THE IMAGE STORE HAS TO BE THE CLASSIC ONE BEFORE THIS COUNTS AS SUCCESS.
	//
	// Docker 29 defaults a fresh installation to the containerd image store under
	// /var/lib/containerd, and the cache attaches its fenced filesystem at
	// /var/lib/docker. Get that wrong and the cache publishes perfectly and holds
	// no images: every job re-pulls, nothing errors, and the only symptom is a
	// permanently cold cache. The daemon.json written above selects the classic
	// store; this proves it took, the way the firecracker gate proves it against a
	// mounted guest artifact rather than against the script that wrote it.
	//
	// NOT HYPOTHETICAL. An AMI built roughly eighteen hours before that daemon.json
	// block existed lost every cached image for nine days without reporting
	// anything, and is why this check is here.
	//
	// RESTART, NOT START, AND THE DIFFERENCE IS THE WHOLE CHECK.
	//
	// Two runs measured two different worlds, and only restart is correct in both.
	// One found docker INACTIVE at check time and usable seven seconds later,
	// which is why the wait below exists. Another found apt's postinst had left
	// the daemon RUNNING -- and `systemctl start` on an active unit returns
	// success and does nothing, so the daemon never re-reads the daemon.json
	// written above and keeps Docker 29's default containerd snapshotter.
	//
	// That is not a broken image. A machine booted FROM the image starts docker
	// with daemon.json already on disk and gets the classic store. It is the
	// BUILDER's daemon that is stale, so asserting against it measured a true
	// fact about the wrong process -- and failed every build on a correct image.
	//
	// restart re-reads the file whether or not the unit was up, so the builder's
	// daemon matches what the image does on first boot, which is the only daemon
	// these assertions are about. The wait stays necessary: a unit going active
	// does not mean dockerd is answering yet.
	b.WriteString("systemctl restart docker\n")
	b.WriteString("i=0\n")
	b.WriteString("while [ $i -lt 60 ] && ! docker info >/dev/null 2>&1; do\n")
	b.WriteString("  i=$((i+1)); sleep 1\n")
	b.WriteString("done\n")
	b.WriteString("docker info >/dev/null\n")

	// BOTH HALVES, because they answer different questions. The file is what the
	// snapshot preserves and therefore what the AMI carries; `docker info` is that
	// file having taken effect. A daemon started before the file was written would
	// satisfy one and not the other.
	b.WriteString("jq -e '.features[\"containerd-snapshotter\"] == false and " +
		".[\"storage-driver\"] == \"overlay2\"' /etc/docker/daemon.json >/dev/null\n")

	// A PATH BOUNDARY, NOT A PREFIX, AND RESOLVED BEFORE IT IS COMPARED.
	//
	// Matching the directory and its subtree as separate arms is what stops
	// /var/lib/docker-elsewhere passing, which a string prefix or a bare glob would
	// accept. That much is necessary and is not sufficient: a LEXICAL comparison
	// also accepts /var/lib/docker/../containerd, which resolves to exactly the
	// directory this check exists to keep Docker out of, and accepts any symlink
	// under the root pointing off the cache filesystem. Measured, not reasoned:
	// `case /var/lib/docker/../containerd in /var/lib/docker/*)` matches.
	//
	// So both sides are canonicalised first. -m resolves a path whose tail does not
	// exist yet, which the cache root legitimately may not at image-build time.
	b.WriteString("billet_docker_root=$(realpath -m " +
		"\"$(docker info -f '{{.DockerRootDir}}')\")\n")
	b.WriteString("billet_cache_root=$(realpath -m /var/lib/docker)\n")
	b.WriteString("case \"$billet_docker_root\" in\n")
	b.WriteString("  \"$billet_cache_root\"|\"$billet_cache_root\"/*) ;;\n")
	b.WriteString("  *) echo \"docker data root $billet_docker_root resolves outside " +
		"$billet_cache_root, so the cache would publish without the images\" >&2; " +
		"exit 1 ;;\n")
	b.WriteString("esac\n")

	// THE DRIVER NAME LAST, BECAUSE IT IS A PROXY AND THE DATA ROOT IS THE
	// PROPERTY. The cache attaches its fenced filesystem at /var/lib/docker, so
	// where the bytes land is what matters; the driver string is Docker's to
	// rename. Asserting it FIRST aborted under set -e before the data root was
	// read, so a failing build could not say whether the store was wrong or
	// merely spelled differently. In this order a bad image fails on the root,
	// with the root in the message.
	b.WriteString("test \"$(docker info -f '{{.Driver}}')\" = overlay2\n")

	// NO poweroff: build-ami.sh cleans the image and announces the verdict after
	// this, and only then powers the builder off.
	return b.String(), nil
}

// entryPointScript is the entry point a tier names, entryPointPath, written by
// the build and compared byte for byte by the verifier, so the image is proved to
// carry this launch and not merely a helper that could run one.
func entryPointScript() string {
	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n")

	// ATTACH THE IMAGE STORE BEFORE STARTING THE RUNNER. The helper stops Docker if
	// cloud-init already started it, mounts the cache, and starts it again before
	// service containers can be pulled. Every cache failure falls back to the root
	// disk and a cold pull.
	b.WriteString("/usr/local/bin/billet-docker-cache prepare\n")

	// WAIT FOR DOCKER BEFORE STARTING THE RUNNER, because billet's boot script
	// invokes this the moment cloud-init reaches it and the daemon may not be up.
	//
	// A verification run of a finished image measured exactly that: at the instant
	// the check ran, systemctl reported docker inactive, and a container ran fine
	// SEVEN SECONDS LATER. Without this the first job on a fresh instance can pick
	// up a workflow whose first step is `docker build` and fail it, on a machine
	// that is about to be perfectly healthy — the worst kind of flake, because the
	// next run works.
	//
	// Bounded, and it proceeds anyway on timeout: a runner that starts without
	// Docker fails the jobs that need it, which is better than an instance that
	// silently never registers and leaves the job queued until GitHub gives up.
	b.WriteString("i=0\n")
	b.WriteString("while [ $i -lt 60 ] && ! docker info >/dev/null 2>&1; do\n")
	b.WriteString("  i=$((i+1)); sleep 1\n")
	b.WriteString("done\n")
	b.WriteString("runner_started=$(date +%s%N 2>/dev/null || true)\n")

	// ONE INVOCATION, SHARED WITH THE VALIDATION BELOW, so a change here cannot
	// leave the check passing while every job fails. See privilegeDrop.
	//
	// THE JIT VARIABLE IS THE CONSTANT, NOT A STRING. The boot script that exports
	// it uses jitEnvVar; spelled out here, a rename would leave this file and its
	// tests green while producing the failure this whole issue exists to prevent —
	// a runner that starts, finds no registration, exits, and leaves a machine
	// looking perfectly healthy.
	writeRunnerEnv(&b, "")

	// A VALUE HOLDING A NEWLINE CANNOT TRAVEL ONE LINE PER VARIABLE, so it refuses
	// the launch, naming the variable and never the value, and the steps after the
	// launch run as for a failed job. A shell function's arguments are no
	// process's argv.
	b.WriteString("billet_refused=\"\"\n")
	b.WriteString("billet_env_add() {\n")
	b.WriteString("  case \"$2\" in\n")
	b.WriteString("    *'\n'*)\n")
	b.WriteString("      echo \"billet-runner: $1 holds a newline, which cannot reach the runner; " +
		"refusing the launch\" >&2\n")
	b.WriteString("      billet_refused=1\n")
	b.WriteString("      return ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("  billet_env=\"$billet_env$1=$2\n\"\n")
	b.WriteString("}\n")
	b.WriteString("billet_env_add BILLET_LAUNCH_EPOCH_NS \"${BILLET_LAUNCH_EPOCH_NS:-}\"\n")
	b.WriteString("billet_env_add BILLET_RUNNER_START_EPOCH_NS \"$runner_started\"\n")
	b.WriteString("billet_env_add ACTIONS_RUNNER_HOOK_JOB_STARTED " + jobTimingHookPath + "\n")
	b.WriteString("billet_env_add ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED true\n")
	b.WriteString("billet_env_add ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE " +
		"\"${ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE:-}\"\n")
	b.WriteString("billet_env_add " + jitEnvVar + " \"$" + jitEnvVar + "\"\n")
	b.WriteString("billet_env_add BILLET_CACHE_ENDPOINT \"${BILLET_CACHE_ENDPOINT:-}\"\n")
	b.WriteString("billet_env_add BILLET_CACHE_TOKEN \"${BILLET_CACHE_TOKEN:-}\"\n")
	b.WriteString("billet_env_add BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES " +
		"\"${BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES:-}\"\n")

	b.WriteString("set +e\n")
	b.WriteString("if [ -n \"$billet_refused\" ]; then\n")
	b.WriteString("job_status=1\n")
	b.WriteString("else\n")
	b.WriteString(privilegeDrop + " /opt/actions-runner/billet-runner-service" + runnerEnvStream)
	b.WriteString("job_status=$?\n")
	b.WriteString("fi\n")
	b.WriteString("set -e\n")
	b.WriteString("/usr/local/bin/billet-docker-cache complete \"$job_status\"\n")
	b.WriteString("service_status=$(/usr/local/bin/billet-docker-cache service-status \"$job_status\") || service_status=$job_status\n")
	b.WriteString("exit \"$service_status\"\n")

	return b.String()
}
