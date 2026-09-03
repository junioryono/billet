package ec2

import (
	"context"
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
	"github.com/junioryono/billet/internal/runnerimages"
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
// THE TAG IS A PROMOTION, NOT A CREATE-TIME CLAIM, and that is the whole content
// of the number now. CreateImage stamps who owns the image and which billet made
// it; the contract tag is added afterwards, by CreateTags, once billet has BOOTED
// the image and proved the properties on the artifact. So an unstamped image is
// one nothing has verified — which both readers already treat as "no answer,
// rebuild" — rather than a claim nobody checked.
const AMIContract = 2

// contractFor is the contract a build of this architecture actually meets.
//
// BOTH ARCHITECTURES MEET IT NOW. The shared installers name every vendor's arch
// spelling from one variable -- node and Python use the tool-cache name, go and
// temurin use dpkg's, pypy calls arm64 `aarch64`, and ruby-builder suffixes arm64
// while leaving x64 bare -- so an arm64 image carries the same toolcache an x64
// one does, under `<version>/arm64` with the marker tool-cache stats.
//
// CodeQL IS THE ONE EXCEPTION AND IT IS RECORDED RATHER THAN SILENT. codeql-action
// publishes no arm64 bundle at all, so an arm64 build writes that line into the
// unpublished record, which both gates accept only for a line named there exactly.
// That is the same mechanism Ruby 4.0 uses on x64, and it is why this can return
// the constant without the stamp becoming a claim the image does not meet.
//
// This function is kept rather than inlined because the stamp is what `billet
// check` reads INSTEAD of looking at the image: if a future architecture arrives
// that the installers cannot serve, the lie has to be preventable here.
func contractFor(arch string) int {
	if arch == "x64" || arch == "arm64" {
		return AMIContract
	}

	return 1
}

const (
	amiContractTag = "sh.billet.ami-contract"
	amiBuiltByTag  = "sh.billet.built-by"
)

// DefaultBuilderDiskGiB is the root volume a build gets when nobody says.
//
// Exported because `billet ami build` prints it in --builder-disk's help, and a
// default a flag describes must be the default the code uses.
//
// NOT A ROUND NUMBER SOMEBODY LIKED. Canonical's noble images declare an 8GiB
// root and GitHub's declared package set is most of it, so the previous
// behaviour -- writing no Ebs.VolumeSize at all and inheriting whatever the base
// image said -- left provisioning to die on ENOSPC. Under `set -e` that aborts
// before the poweroff that signals success, so it produced no image rather than
// a broken one; what it cost was a paid builder and a failure that reads as an
// apt problem.
//
// MEASURED ON THE PRODUCED IMAGE, not derived, and re-measured when parity grew.
// Read by `billet ami verify` from a machine booted off the AMI:
//
//	26.8GiB used of 76.4GiB usable, 49.6GiB free   (root_used_kib=28106956)
//	/opt/hostedtoolcache: 5.2GiB                   (toolcache_kib=5435176)
//
// THE OLD FIGURE WAS 7GiB AND 30 WAS ENOUGH FOR IT. Parity took the content to
// 26.8GiB -- the six-runtime toolcache, five JDKs, three .NET SDKs, PowerShell and
// its four modules, and the Android SDK with three NDKs -- so a 30GiB volume now
// leaves about a gigabyte free against a floor the provisioning script itself sets
// at ten, and every build would be refused before it started.
//
// 80 BECAUSE 80 IS WHAT WAS MEASURED, AND NOTHING ELSE HAS BEEN.
//
// This was briefly 60, derived as "26.8 measured plus headroom", and that is the
// derivation this constant has now been wrong about twice. A review put the
// arithmetic plainly: 60GiB is about 57.3 usable, leaving 30.5 after the image --
// and the number that matters is not what the finished image occupies but the PEAK
// during the build, because the installers unpack archives onto this same
// filesystem and delete them as they go. That peak is unmeasured. The preflight
// free-space check cannot see it either: it runs once, before provisioning, so a
// transient high-water mark fails later with ENOSPC on a volume that passed.
//
// So the default is the size a real build actually completed on. Erring high costs
// a few cents of EBS for the life of one builder; erring low costs the build, and
// the failure arrives forty minutes in as an out-of-space error inside whichever
// install step happened to be running.
const DefaultBuilderDiskGiB = 80

// minBuilderFreeGiB is what the provisioning script insists on before it starts,
// and minBuilderDiskGiB is what a build must be given so that is reachable.
//
// TWO NUMBERS BECAUSE THEY MEASURE DIFFERENT THINGS. A volume's size is not its
// free space: the base image's own filesystem is already on it. So the script
// checks what it can actually use, and the flag is checked against a figure with
// the base image's footprint added back.
//
// Both are confirmed against a real build, and both moved when parity did. The
// finished image reports 26.8GiB used of 76.4GiB usable, so the 10GiB floor is met
// with room on a 60GiB volume and is unreachable on the 30GiB one that used to be
// the default.
//
// 50 IS A FLOOR ON THE FLAG, AND IT IS DELIBERATELY NOT THE ARITHMETIC MINIMUM.
//
// The arithmetic minimum is about 40: 26.8 measured, 10 required free, and roughly
// 4.5% of a volume lost to the filesystem. But a build admitted at exactly that
// passes the preflight check with about 1.4GiB to spare and then meets an
// unmeasured peak, which is a build that fails late for a reason its own guard
// said was fine. The floor sits above the arithmetic so that passing preflight
// means something.
//
// It is still a floor rather than a recommendation: 80 is the only size a build has
// actually completed on, and anything between these two numbers is an operator
// saying they know their own content is smaller.
const (
	minBuilderFreeGiB = 10
	minBuilderDiskGiB = 50
	// maxBuilderDiskGiB is a typo bound rather than a technical one. EBS goes far
	// higher; what makes this worth capping is that CreateImage snapshots the
	// builder's root, so the number is inherited by the AMI and, through the job
	// path's clamp, by every job launched from it.
	maxBuilderDiskGiB = 1000
)

// aptPackageName is what this build will pass to apt.
//
// CHECKED EVEN THOUGH THE NAMES COME FROM A DIGEST-PINNED FILE. The digest proves
// the declaration is the one GitHub published; it says nothing about whether its
// strings are safe to interpolate into a script that runs as root on a machine
// billet pays for. Debian policy already restricts a package name to lowercase
// alphanumerics with `+`, `-` and `.`, so nothing legitimate is refused by
// insisting on it -- and a name carrying a space, a semicolon or a glob would
// otherwise be word-split or expanded by the shell that installs it.
var aptPackageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)

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

	// PayloadBucket is where the shared installers are staged when they will not
	// fit in user data.
	//
	// EMPTY MEANS EMBED, WHICH IS WHAT EVERY BUILD DID UNTIL THE SCRIPT OUTGREW
	// 16384 BYTES. A build whose script still fits needs no bucket and no new
	// permission; one whose script does not is refused with the name of this
	// field rather than by EC2 with a parameter error.
	PayloadBucket string

	// payloadURL and payloadSHA256 are set by BuildImage after staging, and are
	// what the bootstrap in user data fetches and verifies.
	payloadURL    string
	payloadSHA256 string
}

// BuildImage produces an AMI containing the GitHub Actions runner and Docker.
//
// WHY BILLET DRIVES THIS ITSELF rather than shipping a Packer template: the
// contract between billet and an image is six lines of shell, and a build tool
// that has to be kept in sync with those six lines is a second place for them to
// be wrong. billet already speaks four EC2 actions; this adds one.
//
// HOW IT KNOWS PROVISIONING FINISHED, without SSH, an agent, or any IAM the
// builder does not already have: the provisioning script ends in `poweroff`. A
// successful build is an instance that STOPPED ITSELF, which billet can see with
// the DescribeInstances it already makes. A failed one never reaches that line, so
// it stays running and the wait below times out against a machine still holding
// its console log — which is the state you want to debug from, rather than an
// image made from a half-provisioned disk.
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
	// STAGED BEFORE THE SCRIPT IS RENDERED, because the script is a bootstrap that
	// names the URL and the digest. Nothing is launched until the payload is
	// readable, so a builder never boots to fetch something that is not there.
	if spec.PayloadBucket != "" {
		key, cleanup, err := p.stagePayload(ctx, &spec)
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
	}

	script, err := provisionScript(spec)
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

	p.log.Info("provisioning the image", "instance", id, "runner", spec.RunnerVersion)

	if err := p.awaitStopped(ctx, id); err != nil {
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

// awaitStopped waits for the builder to shut itself down, which is how the
// provisioning script reports success.
func (p *Provider) awaitStopped(ctx context.Context, id string) error {
	for {
		out, err := p.describeRaw(ctx, id)
		if err != nil {
			return err
		}

		switch out.state {
		case "stopped":
			// STOPPED BY WHOM. The signal this build relies on is the guest powering
			// ITSELF off at the end of provisioning — and "stopped" alone does not
			// say that. A cost scheduler calling StopInstances on a tag, or a host
			// failure, stops a half-provisioned builder just as convincingly, and
			// imaging that is the exact outcome this design exists to prevent.
			// EXACT EQUALITY, INCLUDING REJECTING EMPTY. AWS marks the reason code
			// optional, so "" is not "the guest did it" — it is "nobody said". The
			// first version of this accepted it, which left the whole door open that
			// the check was added to close.
			if out.reason != "Client.InstanceInitiatedShutdown" {
				return fmt.Errorf("ec2: the builder %s stopped for reason %q rather than "+
					"Client.InstanceInitiatedShutdown, so this is not its provisioning script "+
					"finishing and whatever is on its disk is not a finished image",
					id, out.reason)
			}

			return nil

		case "terminated", "shutting-down":
			// SOMETHING ELSE KILLED IT. Imaging is impossible and the reason is not
			// in the console log of a machine that no longer exists.
			return fmt.Errorf("ec2: the builder %s was terminated before it finished "+
				"provisioning, so no image was made", id)
		}

		if err := sleepFor(ctx, 10*time.Second); err != nil {
			return fmt.Errorf("ec2: the builder %s never stopped itself, which is how the "+
				"provisioning script reports success — it is still running, and its console "+
				"output is where the failure is: %w", id, err)
		}
	}
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
// The environment belongs to the same contract: setpriv does not reset it, so
// without HOME the runner inherits cloud-init's HOME=/root, registers fine, and
// then fails every job step that writes to $HOME.
const privilegeDrop = "setpriv --reuid=runner --regid=runner --init-groups \\\n" +
	"  env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin " +
	"HOME=/home/runner USER=runner LOGNAME=runner"

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

// provisionScript is what the builder runs, and it is the whole definition of
// what a billet runner image contains.
//
// MINIMAL ON PURPOSE. The runner, Docker, and the two things a workflow cannot
// install for itself. Language toolchains are NOT here: actions/setup-go,
// setup-node and setup-python download what they need at runtime and work on any
// runner, costing a few seconds rather than failing. A workflow that assumes a
// toolchain is already on PATH will fail loudly, which is the right failure — and
// the sticky-disk work is what makes those downloads free later.
// canonicalizeCACert validates a --ca-cert bundle and returns billet's own
// re-encoding of it, or an error. Empty in, empty out: the CA is optional.
//
// IT NEVER RETURNS THE OPERATOR'S BYTES. Each certificate is parsed and
// re-emitted from its parsed DER (cert.Raw), so the string that reaches the
// provisioning script is standard PEM billet produced — not attacker text. That
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
// maxCACertPEM bounds the trust bundle a build will bake into an image.
//
// BOUNDED ON ITS OWN TERMS, NOT BY THE TRANSPORT. This used to be caught only
// because an absurd bundle pushed the provisioning script past EC2's user-data
// limit — a side effect, and one that stopped firing the moment the script began
// to be compressed, since a bundle of one certificate repeated sixty times is
// exactly what gzip erases. A trust store is worth bounding because it is a trust
// store: every certificate in it is an authority every job on that machine will
// believe.
//
// AND IT HAS TO BE A BOUND THE BUILD CAN HONOUR, which 32 KiB stopped being.
// That figure was chosen when the script was 11.6KB; with the toolcache it is
// past 40KB plain, and measured, a 32652-byte bundle took the payload to 24239
// compressed against a 16384 limit -- so the bound promised something every such
// build would refuse. A bound that cannot be delivered is not a bound.
//
// 2 KiB, AND THE TREND MATTERS MORE THAN THE NUMBER. This is the second time the
// bound has moved because the script grew -- 32 KiB when the script was 11.6KB,
// 4 KiB when the toolcache landed, 2 KiB now that PyPy, Ruby and CodeQL joined it.
// The CA and the toolcache compete for ONE 16384-byte budget and the toolcache
// keeps winning, because it is what the image is for.
//
// WHAT 2 KiB ADMITS, MEASURED WITH openssl RATHER THAN ASSERTED. A PEM
// RSA-2048 self-signed root is 1115 bytes and an RSA-4096 root is 1822, so either
// fits. An RSA-2048 root AND intermediate concatenated is 2181 -- REFUSED, by 133
// bytes. An earlier version of this comment claimed a chain "still fits with
// room", which was written from the shape of the problem rather than from a
// certificate; the deliverability test could not contradict it because it mints
// ECDSA P-256, at 558 bytes the smallest certificate that exists.
//
// REFUSING THE CHAIN IS CORRECT, WHICH IS WHY THE ANSWER IS NOT A BIGGER BOUND.
// A trust store needs the ROOT: an intermediate is presented by the endpoint
// during the handshake and a client that has the root will build the path. Baking
// the intermediate in adds an authority every job on the machine believes, for
// nothing. So the bound admits what a private cache CA actually requires and
// refuses what it does not.
//
// If it has to move again the answer is probably not a smaller bound: it is that
// the script has outgrown user data and wants a different delivery.
//
// THE MARGIN IS THE POINT. Measured with the toolcache present, a
// certificate costs about 575 bytes of the compressed budget: none leaves 5662
// spare, one leaves 4518, two leave 3943. 8 KiB was the first answer and it is a
// knife edge -- roughly a kilobyte of headroom, so the next line added to this
// script turns the size test red and reads as "the CA bound is wrong" when the
// cause is elsewhere. 4 KiB is three or four certificates, still far past a
// private cache CA (one self-signed root, sometimes a root and an intermediate),
// and leaves the script room to grow.
//
// packUserData REMAINS THE REAL GATE. Deliverability is not knowable from the CA
// alone -- it depends on everything else in the script -- so this is the
// trust-store bound and that is the size bound, and the two are not the same
// question.
const maxCACertPEM = 2 << 10

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

func provisionScript(spec BuildSpec) (string, error) {
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

	if spec.Arch != "x64" && spec.Arch != "arm64" {
		return "", fmt.Errorf("ec2: runner architecture %q is not x64 or arm64", spec.Arch)
	}

	// VALIDATED AND RE-ENCODED BEFORE A PAID BUILDER EXISTS. provisionScript runs
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

	// THE DISK IS THERE BEFORE ANYTHING TRIES TO USE IT.
	//
	// billet asks for a larger root than the base image declares, and growing an
	// EBS volume does not grow the filesystem on it. On Ubuntu cloud images
	// cloud-init's growpart and resizefs modules do that in the `init` stage,
	// before user data runs in `final` -- but that is an ordering somebody else
	// owns, and this script cannot see whether it happened except by looking.
	//
	// WITHOUT THIS THE FAILURE IS AN apt ERROR. Provisioning would run until some
	// package or tarball hit ENOSPC, which aborts the build under `set -e` and so
	// produces no image -- correct, and unreadable: the operator sees dpkg
	// complaining about disk space and nothing naming the volume or growpart.
	//
	// df -P FOR THE POSIX FORMAT, because the default wraps a long device name
	// onto its own line and `NR==2` would then read the wrong record. An `if`
	// with an explicit exit rather than a negated pipeline, because `set -e` is
	// ignored for one preceded by `!`.
	b.WriteString("billet_free_kib=$(df -Pk / | awk 'NR == 2 { print $4 }')\n")

	// A VALUE THAT IS NOT A NUMBER MUST FAIL, NOT PASS. `[ "$x" -lt N ]` exits 2
	// on garbage, and `set -e` is suppressed for an `if` CONDITION -- so the arm
	// is not taken and the build proceeds onto a disk it never measured. Measured,
	// not read: with billet_free_kib=not-a-number the script exits 0. Same family
	// as the `!`-pipeline rule, and the reason a `case` comes first.
	b.WriteString("case \"${billet_free_kib:-}\" in\n")
	b.WriteString("  ''|*[!0-9]*)\n")
	b.WriteString("    echo \"df reported '${billet_free_kib:-}' free on the builder root, " +
		"which is not a number; billet cannot tell whether this image fits\" >&2\n")
	b.WriteString("    exit 1 ;;\n")
	b.WriteString("esac\n")

	b.WriteString("if [ \"$billet_free_kib\" -lt " +
		strconv.Itoa(minBuilderFreeGiB*1024*1024) + " ]; then\n")
	b.WriteString("  echo \"the builder root has ${billet_free_kib}KiB free and this image " +
		"needs " + strconv.Itoa(minBuilderFreeGiB) + "GiB; the volume was requested larger, so " +
		"either cloud-init did not grow the filesystem onto it (growpart, resizefs) or " +
		"--builder-disk is too small\" >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")

	// APT, NOT dnf, AND THE PACKAGE SET COMES FROM GITHUB'S OWN DECLARATION.
	//
	// Two lists, and the split is the same one the guest image makes. The first is
	// what the BACKEND needs: Docker, git, and the tools billet's own scripts run.
	// The second is what a WORKFLOW is entitled to find, read from the pinned
	// toolset that `internal/runnerimages` vendors -- so a package added for the
	// microVM backend cannot silently miss this one, which is the whole reason
	// that file is read by both.
	//
	// noninteractive AND A LOCK WAIT. cloud-init's own apt work can still hold the
	// dpkg lock when this runs, and without the wait the first apt-get here dies
	// on a lock rather than on anything about the packages.
	b.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	b.WriteString("apt-get -o DPkg::Lock::Timeout=600 update\n")

	ts, err := runnerimages.Load()
	if err != nil {
		return "", fmt.Errorf("ec2: %w", err)
	}

	backendPackages := []string{
		"ca-certificates", "curl", "git", "jq", "tar",
		"docker.io", "docker-buildx", "docker-compose-v2",
		"e2fsprogs", "util-linux", "libicu74",
		// mawk BY NAME, NOT `awk`. The anchor proof below is an awk program, and
		// nothing in either list named an awk -- it was relying on the base image
		// having one. It always does (mawk is Priority: required on Ubuntu), and
		// the failure if it did not would be loud rather than silent, since a
		// missing command exits 127 and `if !` turns that into a refused build.
		// Naming it costs one package and removes the assumption.
		//
		// `awk` IS A VIRTUAL PACKAGE WITH THREE PROVIDERS, and apt refuses a
		// virtual package that more than one real package provides -- the same
		// thing that killed a guest-image build on `netcat`. So the provider is
		// named, and mawk is the one Ubuntu ships by default.
		"mawk",
	}

	declared := ts.AptPackages()
	if len(declared) == 0 {
		return "", fmt.Errorf("ec2: the pinned toolset declares no apt packages; refusing to " +
			"build an image that would carry only billet's own dependencies")
	}

	// DEDUPLICATED, KEEPING FIRST-SEEN ORDER. curl, jq and tar are things billet
	// needs AND things github declares, and asking apt to install a package twice
	// makes a long command longer while making a diff of it read as though
	// something changed.
	//
	// EVERY NAME IS CHECKED BEFORE IT REACHES A SHELL. These come from a vendored
	// file rather than from a caller, but they are interpolated into a script that
	// runs as root on a machine billet pays for -- and the digest that protects the
	// file proves provenance, not that its strings are safe shell syntax.
	seen := make(map[string]struct{}, len(backendPackages)+len(declared))
	all := make([]string, 0, len(backendPackages)+len(declared))

	for _, pkg := range append(append([]string{}, backendPackages...), declared...) {
		if !aptPackageName.MatchString(pkg) {
			return "", fmt.Errorf("ec2: %q is not a package name this build will pass to apt", pkg)
		}

		if _, dup := seen[pkg]; dup {
			continue
		}

		seen[pkg] = struct{}{}

		all = append(all, pkg)
	}

	b.WriteString("apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends \\\n")
	b.WriteString("  " + strings.Join(all, " ") + "\n")

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
		// and lets the build run on to poweroff. Measured on /bin/sh.
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
	// Docker 29 defaults fresh installations to image content under
	// /var/lib/containerd. The cache attaches one fenced filesystem at
	// /var/lib/docker, so select the supported classic store before the daemon is
	// enabled. Otherwise the cache publishes successfully without the images.
	b.WriteString("install -d /etc/docker\n")
	b.WriteString("cat > /etc/docker/daemon.json <<'BILLETDOCKERDAEMON'\n")
	b.WriteString("{\n")
	b.WriteString("  \"features\": {\n")
	b.WriteString("    \"containerd-snapshotter\": false\n")
	b.WriteString("  },\n")
	b.WriteString("  \"storage-driver\": \"overlay2\"\n")
	b.WriteString("}\n")
	b.WriteString("BILLETDOCKERDAEMON\n")
	// COMPOSE AND BUILDX COME FROM THE ARCHIVE NOW. Amazon Linux packaged neither,
	// so this downloaded a pinned release and checked its digest by hand; Ubuntu
	// ships docker-compose-v2 and docker-buildx, signed by the same archive key as
	// everything else installed above and pulled in the apt transaction. A
	// hand-pinned binary is a second thing to bump and a second digest to keep
	// true, for a plugin the distribution already maintains.
	b.WriteString("docker buildx version\n")
	b.WriteString("docker compose version\n")
	b.WriteString("systemctl enable docker\n")

	// THE RUNNER'S OWN DEPENDENCY IS STILL NAMED RATHER THAN SCRIPTED, and the
	// reason changed with the distribution.
	//
	// On Amazon Linux, actions/runner's bin/installdependencies.sh could not be
	// used at all: it read /etc/os-release, found ID_LIKE="fedora", printed "Can't
	// detect current OS type" and exited non-zero, ending the build before the
	// poweroff that signals success. That was measured on a real build, from the
	// console log of the builder it left running.
	//
	// On Ubuntu that script DOES work -- and it is still not called. It runs its
	// own apt-get update and install for a set it decides, which would sit beside
	// the pinned declaration above as a second, unversioned source of packages for
	// the same image. What the runner actually needs is ICU, which is in the
	// backend list as libicu74 and installed in the one transaction with everything
	// else. Without it the runner starts and dies on globalization; the version
	// check below is where that surfaces, rather than at registration.

	// A NON-ROOT USER, because the runner refuses to run as root without an
	// override and running untrusted jobs as root would give away the only
	// isolation this backend has above the instance boundary.
	b.WriteString("id runner || useradd -m -s /bin/bash runner\n")
	b.WriteString("usermod -aG docker runner\n")

	// THE TOOLCACHE DIRECTORY AND ITS VARIABLES LAND TOGETHER, and that is not
	// tidiness.
	//
	// Exporting RUNNER_TOOL_CACHE without creating the directory is WORSE than
	// exporting neither: it points every setup-* action at a path under root-owned
	// /opt that the unprivileged runner cannot create, so an action that would
	// otherwise have fallen back to its own location now fails outright. A review
	// of the plan caught that, and it was going to be a separate commit.
	//
	// 0777 IS THE GUEST'S VALUE, for the guest's reason: this is read AND WRITTEN
	// by every job, which runs as the unprivileged runner account, and a setup
	// action's whole job is to add versions to it.
	b.WriteString("install -d -m 0777 " + toolcacheDir + "\n")

	// WHAT THE IMAGE SAYS IT IS, in the same file and the same format the
	// firecracker guest uses (/etc/billet-image-env, KEY=VALUE per line). One
	// contract, so the runner entry points on both backends read the same thing
	// and the toolcache install can append to it from either.
	//
	// ImageOS IS SET BECAUSE THIRD-PARTY ACTIONS BRANCH ON IT, and NOT because of
	// the setup-python folklore: that action shells out to `lsb_release -i -r -s`
	// and falls back to /etc/os-release, so it never reads this.
	//
	// AGENT_TOOLSDIRECTORY BESIDE RUNNER_TOOL_CACHE because the actions read only
	// the latter while the runner itself also honours the former, and an image
	// that sets one is an image where half the tooling looks somewhere else.
	b.WriteString("cat > " + imageEnvFile + " <<'BILLETIMAGEENV'\n")
	b.WriteString("ImageOS=ubuntu24\n")
	b.WriteString("ImageVersion=billet\n")
	b.WriteString("RUNNER_TOOL_CACHE=" + toolcacheDir + "\n")
	b.WriteString("AGENT_TOOLSDIRECTORY=" + toolcacheDir + "\n")
	b.WriteString("BILLETIMAGEENV\n")
	b.WriteString("chmod 0644 " + imageEnvFile + "\n")

	if err := writeToolcacheInstall(&b, spec.Arch, spec.payloadURL,
		spec.payloadSHA256); err != nil {
		return "", err
	}

	b.WriteString("mkdir -p /opt/actions-runner\n")
	b.WriteString("cd /opt/actions-runner\n")

	release := "https://github.com/actions/runner/releases/download/v" + spec.RunnerVersion +
		"/actions-runner-linux-" + spec.Arch + "-" + spec.RunnerVersion + ".tar.gz"

	b.WriteString("curl -fsSL -o runner.tar.gz " + strconv.Quote(release) + "\n")
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

	// THE ENTRY POINT A TIER NAMES. billet's boot script exports the JIT config and
	// execs the tier's command AS ROOT, so something has to drop privileges without
	// losing that variable. setpriv does it in one process with no shell in between.
	b.WriteString("cat > /usr/local/bin/billet-runner <<'BILLETEOF'\n")
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
	// THE IMAGE'S OWN VARIABLES, READ AT JOB TIME, exactly as the guest's runner
	// service reads them.
	//
	// `env -i` MEANS A VARIABLE NOT NAMED HERE DOES NOT EXIST FOR THE JOB, whatever
	// /etc/environment says — so the toolcache would be on disk and invisible.
	// Reading the FILE rather than baking the values in is what lets the toolcache
	// install append JAVA_HOME and the JAVA_HOME_*_X64 set after the JDKs exist,
	// which is the only point at which those names are known.
	//
	// THE POSITIONAL PARAMETERS ARE THE ARRAY THIS SHELL DOES NOT HAVE. The entry
	// point is /bin/sh and takes no arguments of its own, so `set --` is free. The
	// alternative — expanding the file unquoted into the command line — word-splits
	// any value containing a space.
	//
	// THE SAME `[A-Za-z_]*=*` FILTER AS THE GUEST, so a comment or a blank line in
	// that file is skipped rather than handed to env as a malformed assignment.
	b.WriteString("set --\n")
	b.WriteString("if [ -r " + imageEnvFile + " ]; then\n")
	b.WriteString("  while IFS= read -r billet_line; do\n")
	b.WriteString("    case \"$billet_line\" in\n")
	b.WriteString("      [A-Za-z_]*=*) set -- \"$@\" \"$billet_line\" ;;\n")
	b.WriteString("    esac\n")
	b.WriteString("  done <" + imageEnvFile + "\n")
	b.WriteString("fi\n")

	b.WriteString("set +e\n")
	b.WriteString(privilegeDrop + " \\\n")
	b.WriteString("  \"$@\" \\\n")
	b.WriteString("  BILLET_LAUNCH_EPOCH_NS=\"${BILLET_LAUNCH_EPOCH_NS:-}\" \\\n")
	b.WriteString("  BILLET_RUNNER_START_EPOCH_NS=\"$runner_started\" \\\n")
	b.WriteString("  ACTIONS_RUNNER_HOOK_JOB_STARTED=" + jobTimingHookPath + " \\\n")
	b.WriteString("  ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true \\\n")
	b.WriteString("  ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=\"${ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE:-}\" \\\n")
	b.WriteString("  " + jitEnvVar + "=\"$" + jitEnvVar + "\" \\\n")
	b.WriteString("  BILLET_CACHE_ENDPOINT=\"${BILLET_CACHE_ENDPOINT:-}\" \\\n")
	b.WriteString("  BILLET_CACHE_TOKEN=\"${BILLET_CACHE_TOKEN:-}\" \\\n")
	b.WriteString("  BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES=\"${BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES:-}\" \\\n")
	b.WriteString("  /opt/actions-runner/billet-runner-service\n")
	b.WriteString("job_status=$?\n")
	b.WriteString("set -e\n")
	b.WriteString("/usr/local/bin/billet-docker-cache complete \"$job_status\"\n")
	b.WriteString("service_status=$(/usr/local/bin/billet-docker-cache service-status \"$job_status\") || service_status=$job_status\n")
	b.WriteString("exit \"$service_status\"\n")
	b.WriteString("BILLETEOF\n")
	b.WriteString("chmod 0755 /usr/local/bin/billet-runner\n")

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
	b.WriteString(privilegeDrop + " \\\n")
	b.WriteString("  /opt/actions-runner/bin/Runner.Listener --version\n")

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

	// THE SUCCESS SIGNAL. Reaching this line is the only thing that tells billet
	// the image is worth making; set -e means a failure anywhere above never
	// arrives here — including the version check.
	//
	// What awaitStopped can prove from it is that the GUEST powered itself off,
	// which is not quite "the script finished": a base image whose own policy
	// powers the machine off after a failure produces the same stop reason. billet
	// takes the base image from the caller, so this rests on that image being
	// trusted — stated rather than papered over. Closing it needs a positive signal
	// out of the guest, and the only channel is the serial console, which would add
	// an ec2:GetConsoleOutput grant to every operator and a poll against output
	// that lags minutes. Not worth it against a hostile base image the operator
	// chose; worth revisiting if billet ever reads the console for another reason.
	b.WriteString("poweroff\n")

	return b.String(), nil
}
