package ec2

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrPromotionUncertain accompanies a failure that happened AFTER CreateTags was
// accepted, so whether the image carries its contract tag is unknown.
//
// THE THREE-VALUED ANSWER AGAIN, and the third state is the one a caller collapses
// if you let it. "The verification failed" and "the tag may or may not be there"
// are different facts, and BuildImage's message told an operator the image was
// definitely unstamped in both cases — which sends them to re-run a verification
// against an image that may already be correct, or worse, to go looking for a tag
// they were told is absent. The same rule the credential paths follow: could-not-
// tell is never no.
var ErrPromotionUncertain = errors.New("ec2: whether the contract tag was written is unknown")

// VerifySpec describes an image to boot and assert on.
type VerifySpec struct {
	// Image is the AMI to verify. It has to be available: this launches from it.
	Image string
	// InstanceType is the shape the VERIFIER runs on, which has nothing to do with
	// the builder's or with any job's. Empty takes defaultVerifierType for the
	// image's own architecture.
	InstanceType string
	// Name prefixes the verifier instance's Name tag. Empty takes the image id,
	// which is what a standalone verification has.
	Name string
}

// defaultVerifierType is the shape a verification runs on when nobody says.
//
// SMALL, AND NITRO IN BOTH ARCHITECTURES. Small because the verifier runs a df, a
// docker info and a few version flags — nothing here is faster on a bigger
// machine, and this is a second instance on a path that already pays for one.
// Nitro because the live console read (`Latest`) is documented as supported only
// on Nitro-based instances, and the live read is what keeps a verification from
// waiting on a buffer AWS posts around state transitions.
func defaultVerifierType(arch string) string {
	if arch == "arm64" {
		return "c7g.large"
	}

	return "c7i.large"
}

// The bounds a verification runs under.
//
// verifyWindow is generous against a boot, and a live run says how generous: on
// 2026-08-28 a contract-2 x64 image reported a complete block 4 minutes 40 seconds
// after RunInstances, so fifteen minutes held with ten to spare. Most of that is
// the script working rather than the console lagging — about twenty-five tool
// invocations through the privilege drop, and a `du` over 5.1GiB of toolcache —
// which is also why a tighter bound would be a bad trade. It is a deadline rather
// than a wait, so a generous one costs nothing and a tight one reports a good
// image broken.
//
// verifyPoll is what it costs to be wrong about the console lag: every poll is
// one GetConsoleOutput, and the report is reprinted every verifyDwellSeconds, so
// polling much faster buys nothing.
const (
	verifyWindow = 15 * time.Minute
	verifyPoll   = 15 * time.Second
)

// maxConsoleOutput bounds the decoded console, and maxEncodedConsole the field it
// arrives in.
//
// EC2 keeps the last 64 KiB, so this is four times what is expected rather than a
// guess at what is possible: the size of an allocation must not be decided by
// whatever answered. The encoded bound is the decoded one plus base64's third,
// rounded to a quantum.
const (
	maxConsoleOutput  = 256 << 10
	maxEncodedConsole = 4 * ((maxConsoleOutput + 2) / 3)
)

// VerifyImage boots one instance from an AMI, makes it assert the contract on
// itself, and stamps the contract tag if it does.
//
// WHY THIS EXISTS AT ALL. Every other claim billet makes about a runner image is
// checked on the BUILDER, before CreateImage — on a machine that has been
// apt-installed, part-configured and never rebooted. That is not the machine the
// image produces, and the difference is not academic: the Docker gate asserted a
// storage driver against a daemon apt had already started, so it read the answer
// from before daemon.json was written and failed every build against an image
// that was correct. Anything a service reads at start, anything cloud-init does
// at first boot, and anything a job's own `env -i` can or cannot see are all
// invisible from there.
//
// HOW IT READS THE ANSWER, with no key pair, no agent and no inbound access: the
// verifier prints a bracketed report to the serial console and billet reads it
// back with GetConsoleOutput. That is the same shape of signalling BuildImage
// already relies on to know provisioning finished, and needs no IAM the builder
// does not already have besides the read itself.
//
// PROVEN ON A REAL BUILD, 2026-08-28, us-west-2. `billet ami build` produced
// ami-0af6ca1a9ff63a09a, booted it, and read back:
//
//	verdict=ok step=done
//	docker_driver=overlay2 docker_root=/var/lib/docker docker_server=29.1.3
//	root_free_kib=18576852 root_total_kib=29378688 root_used_kib=10785452
//	runner=2.336.0 toolcache_kib=5360256
//	tc_node=22.23.2 24.20.0
//	tc_go=1.24.13 1.25.14 1.26.7
//	tc_python=3.10.21 3.11.16 3.12.14 3.13.15 3.14.7
//	tc_java_temurin_hotspot_jdk=8.0.504-1 11.0.32-9 17.0.20-1 21.0.12-1 25.0.4-1
//
// then stamped the contract and terminated both machines. `docker_driver=overlay2`
// is the line the whole issue is about: the in-build gate read `overlayfs` off the
// builder's own daemon for exactly this image shape.
//
// THE VERIFIER IS TERMINATED ON EVERY FAILURE PATH THAT HAS AN ID, which is not
// the same as always — the same gap BuildImage documents for the builder, and
// worth restating because the second bound is weaker here than it looks. If
// RunInstances commits and its response is lost, launchVerifier returns an error
// and no id, so there is nothing to terminate. The script's own poweroff covers
// most of that, and NOT the case this command exists for: an image with broken
// cloud-init, or one that panics on boot, never runs the script at all. Such an
// instance carries the per-build owner tag, which is how it is found.
//
// WHAT IT DOES NOT PROVE, said here rather than discovered later: the nonce makes
// a block THIS run's, not TRUE. An image that prints `verdict=ok` without doing
// anything passes, exactly as a base image whose own policy powers the machine
// off would satisfy the build's success signal. This is a check against mistake —
// against the artifact differing from what the builder measured — and the trust
// it rests on is the operator's own image, which is the trust `billet ami build`
// already rests on.
func (p *Provider) VerifyImage(ctx context.Context, spec VerifySpec) error {
	if spec.Image == "" {
		return fmt.Errorf("ec2: a verification needs an image to boot")
	}

	// THE IMAGE IS ASKED WHAT IT IS, rather than a caller. The architecture decides
	// which shape can boot it and which spelling its toolcache entries carry, and
	// both are properties of the artifact — an operator's `--arch` typo would fail
	// every assertion on a correct image, which is the shape of failure that gets a
	// gate switched off.
	layout, err := p.imageLayout(ctx, spec.Image)
	if err != nil {
		return err
	}

	arch, err := billetArch(spec.Image, layout.arch)
	if err != nil {
		return err
	}

	contract := contractFor(arch)

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("ec2: invent a verification nonce: %w", err)
	}

	nonce := hex.EncodeToString(raw)

	script, schema, err := verifyScript(arch, contract, nonce)
	if err != nil {
		return err
	}

	id, err := p.launchVerifier(ctx, spec, layout, arch, script, nonce)
	if err != nil {
		return err
	}

	defer func() {
		// A FRESH CONTEXT, because the ordinary reason to be here is that ctx has
		// already expired, and that is exactly when the verifier must still be shot.
		// The same shape BuildImage uses for the builder, and for the same reason.
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()

		p.destroyVerifier(stop, id)
	}()

	p.log.Info("verifying the image on a machine booted from it",
		"image", spec.Image, "instance", id, "arch", arch, "contract", contract)

	report, err := p.awaitConsoleReport(ctx, id, nonce, schema)
	if err != nil {
		return fmt.Errorf("ec2: %s did not report on itself: %w", spec.Image, err)
	}

	if verdict := report[reportVerdictKey]; verdict != verdictOK {
		return fmt.Errorf("ec2: %s reported %s=%q at step %q, so it does not meet AMI contract "+
			"%d and has NOT been stamped\n\n  %s", spec.Image, reportVerdictKey, verdict,
			report[reportStepKey], contract, strings.Join(reportLines(report), "\n  "))
	}

	p.log.Info("the image proved itself",
		"image", spec.Image, "report", strings.Join(reportLines(report), " "))

	return p.promoteContract(ctx, spec.Image, contract)
}

// billetArch translates AWS's spelling of a processor into billet's.
//
// THE TWO VOCABULARIES ARE NOT THE SAME AND ONLY HALF OVERLAPS: AWS says
// "x86_64" where billet says "x64", and both say "arm64". Translating in one
// place is what keeps a future third spelling from being read as a passing one —
// an unknown architecture is refused rather than defaulted, because defaulting to
// x64 would assert x64 toolcache paths against an image that has none and report
// a correct image as broken.
func billetArch(image, aws string) (string, error) {
	switch aws {
	case "x86_64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	case "":
		return "", fmt.Errorf("ec2: image %s reports no architecture, so billet cannot say which "+
			"shape boots it or which toolcache spellings it should carry", image)
	default:
		// REFUSED, NOT DEFAULTED. Reading an unknown architecture as x64 would assert
		// x64 toolcache paths against an image that has none — every entry
		// structurally absent, and a correct image reported broken.
		return "", fmt.Errorf("ec2: image %s is for architecture %q, which billet builds no "+
			"runner image for", image, aws)
	}
}

// launchVerifier starts the one-shot machine that asserts on an image.
//
// DELIBERATELY NOT Launch, for the same reasons launchBuilder is not: that path
// needs a JIT registration, names the instance after a lease, and applies the
// trusted/untrusted network split. A verifier has no lease and no job.
func (p *Provider) launchVerifier(
	ctx context.Context, spec VerifySpec, layout imageLayout, arch, script, nonce string,
) (string, error) {
	userData, err := packUserData(script)
	if err != nil {
		return "", err
	}

	// THE ARCHITECTURE IS PASSED IN RATHER THAN RE-DERIVED. VerifyImage has already
	// refused one billet builds no image for, and a second reading here would be a
	// second place deciding what an unknown one means — which is how a refusal
	// becomes a default that boots an arm64 image on an x86 shape.
	shape := spec.InstanceType
	if shape == "" {
		shape = defaultVerifierType(arch)
	}

	params := url.Values{}
	params.Set("Action", "RunInstances")
	params.Set("ImageId", spec.Image)
	params.Set("InstanceType", shape)
	params.Set("MinCount", "1")
	params.Set("MaxCount", "1")
	params.Set("UserData", base64.StdEncoding.EncodeToString(userData))

	// TERMINATES ITSELF, WHICH IS THE OPPOSITE OF THE BUILDER AND IS THE LEAK BOUND.
	// The builder must leave a stopped instance for CreateImage to snapshot; a
	// verifier has nothing worth keeping, and its poweroff at the end of the dwell
	// is what ends an instance billet lost track of between RunInstances and its
	// terminate.
	params.Set("InstanceInitiatedShutdownBehavior", "terminate")

	// THE TOKEN IS THIS RUN'S, NOT THIS IMAGE'S, and that is the opposite choice
	// from the builder's on purpose. A token derived from the image would make a
	// second, legitimate `billet ami verify` of the same image collide with the
	// first: EC2 answers a reused token carrying different parameters with
	// IdempotentParameterMismatch, measured against a real account. What the
	// builder buys with a stable token — a lost response becoming a recovery — is
	// bought here by the verifier ending itself instead.
	sum := sha256.Sum256([]byte("billet-ami-verify:" + nonce))
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

	// EVERY DEVICE THE IMAGE DECLARES, RESTATED — the builder's rule, and it is not
	// redundant here just because the image came from a builder that applied it.
	// `billet ami verify` takes an image id from an operator, and this client can
	// delete no volumes at all, so a mapping that asks to survive would leave a disk
	// behind on a run whose entire purpose is to be transient.
	params.Set("BlockDeviceMapping.1.DeviceName", layout.root)
	params.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")

	for i, d := range layout.devices {
		n := strconv.Itoa(i + 2)

		params.Set("BlockDeviceMapping."+n+".DeviceName", d.name)
		params.Set("BlockDeviceMapping."+n+".Ebs.DeleteOnTermination", "true")
	}

	params.Set("TagSpecification.2.ResourceType", "volume")
	params.Set("TagSpecification.2.Tag.1.Key", ownerTag)
	params.Set("TagSpecification.2.Tag.1.Value", p.owner)

	// THE NAME IS ALSO THE ONLY WAY TO TELL A FINISHED BUILD FROM A FAILED ONE
	// AFTERWARDS. A successful build's operator usually deregisters the image, so
	// describe-images says the same nothing either way; the instances are the
	// discriminator. A `<name>-builder` alone means the build never reached
	// CreateImage, and a `<name>-builder` plus a `<name>-verify` means it did.
	params.Set("TagSpecification.1.ResourceType", "instance")
	params.Set("TagSpecification.1.Tag.1.Key", ownerTag)
	params.Set("TagSpecification.1.Tag.1.Value", p.owner)
	params.Set("TagSpecification.1.Tag.2.Key", "Name")
	params.Set("TagSpecification.1.Tag.2.Value", verifierName(spec))

	var out runInstancesResponse

	if err = p.api.call(ctx, params, &out); err != nil {
		return "", fmt.Errorf("ec2: launch a %s verifier from %s: %w", shape, spec.Image, err)
	}

	if len(out.Instances) == 0 || out.Instances[0].InstanceID == "" {
		return "", fmt.Errorf("ec2: launching a verifier returned no instance id")
	}

	return out.Instances[0].InstanceID, nil
}

// destroyVerifier terminates the verifier, having first made sure the API can see
// it.
//
// Destroy READS NotFound AS SUCCESS, AND FOR THE VERIFIER THAT IS NOT ENOUGH.
// DescribeInstances and TerminateInstances are eventually consistent, so an id
// RunInstances has just returned can answer InvalidInstanceID.NotFound on the next
// call — which is why Destroy refuses to treat it as a failure, and why it leaves
// the proof to the caller's own List. A node has that reconciliation; a one-shot
// verifier has nobody. So a terminate issued seconds after the launch — which is
// exactly what a cancelled context produces — could be answered NotFound, reported
// as done, and leave a machine that comes into existence a moment later.
//
// SO THE ID IS MADE VISIBLE FIRST. describeRaw already reports "not visible yet"
// as an empty state rather than as an error, so this waits for the instance to
// appear and only then asks for it to be terminated. It bounds itself and says so
// rather than blocking the caller's result: the verification's verdict is already
// decided by the time this runs, and what is left is money.
func (p *Provider) destroyVerifier(ctx context.Context, id string) {
	for attempt := range verifyTerminateAttempts {
		state, err := p.describeRaw(ctx, id)
		if err != nil {
			p.log.Error("billet could not ask what the verifier is doing, so it has not been "+
				"terminated; it powers itself off at the end of its dwell unless it never "+
				"booted, and it carries this build's owner tag",
				"instance", id, "owner", p.owner, "error", err)

			return
		}

		// AN EMPTY STATE IS "NOT VISIBLE YET", not "gone" — describeRaw is explicit
		// about that, and the distinction is the whole reason this loop exists.
		if state.state == "" {
			if attempt == verifyTerminateAttempts-1 {
				break
			}

			if err := p.sleep(ctx, verifyTerminateWait); err != nil {
				break
			}

			continue
		}

		if _, err := p.Destroy(ctx, id); err != nil {
			p.log.Error("the verifier instance could not be terminated and is still costing "+
				"money; it powers itself off at the end of its dwell, but terminate it by hand",
				"instance", id, "error", err)
		}

		return
	}

	p.log.Error("the verifier instance never became visible to the api, so billet cannot "+
		"confirm it was terminated; it powers itself off at the end of its dwell unless it "+
		"never booted, and it carries this build's owner tag",
		"instance", id, "owner", p.owner)
}

// How long destroyVerifier waits for an id to become visible.
//
// SHORT, BECAUSE THE WINDOW IT COVERS IS SHORT. AWS's own note on this is that an
// id may not be visible to a subsequent call for a "short time", and everything
// past that is a cleanup budget spent on an instance that will end itself.
const (
	verifyTerminateAttempts = 6
	verifyTerminateWait     = 5 * time.Second
)

// verifierName is what the verifier instance is called.
func verifierName(spec VerifySpec) string {
	base := spec.Name
	if base == "" {
		base = spec.Image
	}

	return base + "-verify"
}

// awaitConsoleReport polls the serial console until the verifier's own block
// appears, and returns what it said.
func (p *Provider) awaitConsoleReport(
	ctx context.Context, id, nonce string, schema reportSchema,
) (map[string]string, error) {
	deadline := time.Now().Add(verifyWindow)

	// THE LIVE READ FIRST, AND A FALL BACK RATHER THAN A REFUSAL. `Latest` returns
	// the console as it stands, which is what makes this bounded at all: the default
	// is a buffer AWS posts around a state transition, so without it a report can be
	// written and not readable for minutes. It is documented as Nitro-only, and the
	// shapes billet picks are Nitro — but an operator may name any shape with
	// --verify-instance-type, and refusing their older one would be a worse answer
	// than waiting for the buffer.
	latest := true

	for {
		out, err := p.consoleOutput(ctx, id, latest)

		switch {
		case err == nil:

		case latest && refusedLatestConsole(err):
			p.log.Warn("this shape does not serve the live serial console, so the verification "+
				"falls back to the buffered one, which AWS posts around a state transition "+
				"rather than continuously",
				"instance", id)

			latest = false

			continue

		// NOT VISIBLE YET IS NOT GONE. This API is eventually consistent like the rest
		// of them: an instance RunInstances has just returned an id for can answer
		// NotFound on the very next call, and this package has already made that
		// mistake twice against instances alone. The caller's deadline is what bounds
		// an id that never appears; an absence here is never proof.
		case isInstanceNotFound(err):

		default:
			return nil, err
		}

		if report, ok := parseReport(out, nonce, schema); ok {
			return report, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the verifier %s printed no complete report to its console "+
				"within %s; boot the image by hand and read its console to see how far it got",
				id, verifyWindow)
		}

		if err := p.sleep(ctx, verifyPoll); err != nil {
			return nil, err
		}
	}
}

// consoleOutput asks for one instance's console, decoded.
func (p *Provider) consoleOutput(ctx context.Context, id string, latest bool) (string, error) {
	params := url.Values{}
	params.Set("Action", "GetConsoleOutput")
	params.Set("InstanceId", id)

	if latest {
		params.Set("Latest", "true")
	}

	var out getConsoleOutputResponse

	if err := p.api.call(ctx, params, &out); err != nil {
		return "", fmt.Errorf("ec2: read the console of %s: %w", id, err)
	}

	if out.Output == "" {
		// NOTHING POSTED YET IS NOT AN ERROR AND NOT AN ANSWER. The caller polls
		// through it and its own deadline is what bounds a machine that never speaks.
		return "", nil
	}

	// WHITESPACE IS STRIPPED BECAUSE THE FIELD IS WRAPPED. The query API returns
	// base64 with line breaks in it, which the standard decoder refuses.
	encoded := stripSpace(out.Output)

	// BOUNDED BEFORE THE DECODE, because a decode allocates whatever the encoded
	// length implies and checking afterwards is checking after the allocation that
	// was the risk. AWS keeps 64 KiB, so this should never fire; it is here so the
	// size of an allocation is never decided by whatever answered.
	//
	// AND IT KEEPS THE TAIL, WHICH IS NOT INTERCHANGEABLE WITH THE HEAD. The report
	// is the last thing the machine printed, so dropping the end of a console would
	// throw away the only part billet is looking for and report a working image as
	// silent.
	//
	// AND THE CUT LANDS ON A QUANTUM WITHOUT ANY ARITHMETIC, which is worth stating
	// because the obvious alternative is a realignment step that can never fire.
	// Four characters encode three bytes and padding appears only at the very end,
	// so base64 always has a length that is a multiple of four and maxEncodedConsole
	// is one too — the offset is therefore already aligned, and a tail starting
	// there decodes exactly as it did in place. Cutting anywhere else would leave
	// bytes that are no longer base64, which reads as a console that printed
	// nothing rather than as a truncation.
	if len(encoded) > maxEncodedConsole {
		encoded = encoded[len(encoded)-maxEncodedConsole:]
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// NOT "NOTHING YET". Every truncation billet performs is quantum-aligned, so
		// what is left is base64 or the field never was — and swallowing that would
		// make an endpoint answering something else look exactly like a machine that
		// has not printed, which is the distinction this whole poll turns on.
		return "", fmt.Errorf("ec2: the console of %s is not base64: %w", id, err)
	}

	if len(decoded) > maxConsoleOutput {
		decoded = decoded[len(decoded)-maxConsoleOutput:]
	}

	return string(decoded), nil
}

// stripSpace removes every ASCII space and line break, which is all the query
// API's base64 wrapping consists of.
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, s)
}

// The bounds on what billet will read out of a console block.
//
// A CONSOLE IS WHATEVER THE MACHINE PRINTED, so the block is data and not
// billet's own output however plausible it looks. These bounds sit UNDER the
// schema rather than in place of it: the schema decides which keys exist at all
// and what the two fields billet acts on may say, and these cap how much of the
// rest can arrive and in which bytes.
//
// A KEY SHAPE IS NOT AN ALLOWLIST, which is the mistake the first version of this
// made. `^[a-z][a-z0-9_]{0,31}$` accepts `secret` as readily as `runner`, so a
// machine could put a line of its own choosing into billet's log through a check
// whose comment claimed only emitted keys were rendered.
const (
	maxReportLines = 64
	maxReportValue = 200
)

// parseReport finds the verifier's last complete block and reads the lines it
// recognises.
//
// THE LAST ONE, AND THAT IS NOT A PREFERENCE. EC2 keeps the last 64 KiB of
// console output, so a long boot log truncates from the FRONT — which can cut a
// block's opening marker off and leave its closing one behind. Taking the last
// complete pair is what makes reading a truncated console safe, and it is also
// why the verifier reprints rather than announcing once.
func parseReport(console, nonce string, schema reportSchema) (map[string]string, bool) {
	begin := reportBegin + " " + nonce
	end := reportEnd + " " + nonce

	last := strings.LastIndex(console, end)
	if last < 0 {
		return nil, false
	}

	// THE OPENING MARKER MUST PRECEDE THE CLOSING ONE THIS RUN'S. Searching the
	// whole console for the begin marker would pair the FIRST opening with the LAST
	// closing and read every line between two separate reports as one.
	start := strings.LastIndex(console[:last], begin)
	if start < 0 {
		return nil, false
	}

	body := console[start+len(begin) : last]

	report := make(map[string]string, maxReportLines)

	for i, line := range strings.Split(body, "\n") {
		if i >= maxReportLines {
			break
		}

		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok || !printableASCII(value) || len(value) > maxReportValue {
			continue
		}

		// THE SCHEMA IS THE ALLOWLIST, and the shape check stays in front of it as a
		// cheap rejection rather than as the decision.
		if !reportKey.MatchString(key) || !schema.allows(key, value) {
			continue
		}

		report[key] = value
	}

	// A BLOCK WITH NO VERDICT IN IT IS NOT AN ANSWER. The marker pair alone says
	// only that something printed; the field billet acts on has to be there, or a
	// console that carried a truncated block would be read as a verdict of "".
	if _, ok := report[reportVerdictKey]; !ok {
		return nil, false
	}

	return report, true
}

// isInstanceNotFound reports whether the API said it has never heard of this
// instance, which so soon after a launch means "not yet" rather than "gone".
func isInstanceNotFound(err error) bool {
	code, ok := codeOf(err)

	return ok && code == "InvalidInstanceID.NotFound"
}

// refusedLatestConsole reports whether the API turned down the LIVE console read
// specifically, which is the one refusal worth retrying differently.
//
// A NAMED SET, NOT "ANY ERROR". Falling back on anything at all would turn a
// permission billet does not hold, or a throttle, into a silent switch to the
// buffered console — and then into a verification that times out for a reason
// nobody is told. These are the codes AWS uses for a parameter this shape or this
// endpoint does not serve.
func refusedLatestConsole(err error) bool {
	code, ok := codeOf(err)
	if !ok {
		return false
	}

	switch code {
	case "UnsupportedOperation", "InvalidParameterCombination", "InvalidParameterValue":
		return true
	}

	return false
}

// printableASCII reports whether every byte is one a terminal renders as itself.
func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}

	return true
}

// reportLines is the validated fields in one fixed order.
//
// ORDERED RATHER THAN RANGED, because Go randomises map iteration: a diagnostic
// whose field order changes per run is one nobody can diff against the last
// build. The verdict and the step lead because they are what an operator reads
// first; everything else is alphabetical.
//
// ONE ORDERING AND TWO JOINS, so the line a log carries and the block an error
// carries cannot drift into disagreeing about what was reported.
func reportLines(report map[string]string) []string {
	rest := make([]string, 0, len(report))

	for k := range report {
		if k != reportVerdictKey && k != reportStepKey {
			rest = append(rest, k)
		}
	}

	slices.Sort(rest)

	out := make([]string, 0, len(report))

	for _, k := range append([]string{reportVerdictKey, reportStepKey}, rest...) {
		if v, ok := report[k]; ok {
			out = append(out, k+"="+v)
		}
	}

	return out
}

// promoteContract records that this image was booted and proved.
//
// A STANDALONE CreateTags, WHICH IS THE ONLY ONE BILLET MAKES, and it has to be:
// the fact being recorded — that billet ran this image and asserted on it — is
// not knowable at the instant CreateImage creates the image. A bundled IAM policy
// scopes this to images already carrying the per-build owner tag, so the grant
// reaches only what this builder made.
//
// AND IT IS READ BACK. CreateTags answers with nothing but a request id, so the
// only way to know the tag landed on the image billet meant is to ask — the same
// rule the in-build anchor check follows, where an `update-ca-certificates` that
// exits 0 having added nothing is exactly the failure that matters.
func (p *Provider) promoteContract(ctx context.Context, image string, contract int) error {
	params := url.Values{}
	params.Set("Action", "CreateTags")
	params.Set("ResourceId.1", image)

	stampContract(params, contract)

	// ONLY THE API SAYING NO IS CONCLUSIVE, and "the api said no" means it named a
	// CODE. Everything else — a dropped connection, a body that would not parse, a
	// context that ended, a gateway answering 502 with prose — may have committed,
	// and treating it as a refusal is the collapsed answer this sentinel exists to
	// remove. So an ambiguous failure falls through to the read-back, which is the
	// only thing that can actually settle it.
	//
	// A CODE, NOT MERELY AN apiError. parseAPIError builds one carrying the status
	// alone when a proxy or a load balancer answered instead of EC2 — which is the
	// ambiguous case wearing the shape of a verdict, and `codeOf` reports it as
	// present because the TYPE is there. Asking for the code is what tells the two
	// apart.
	tagErr := p.api.call(ctx, params, nil)
	if tagErr != nil {
		if code, ok := codeOf(tagErr); ok && code != "" {
			return fmt.Errorf("ec2: %s passed verification but its contract tag could not be "+
				"written, so `billet check` will report it as needing a rebuild; re-run "+
				"`billet ami verify %s`: %w", image, image, tagErr)
		}
	}

	if err := p.confirmContract(ctx, image, contract); err != nil {
		if tagErr != nil {
			return fmt.Errorf("%w: %s passed verification and its CreateTags neither succeeded "+
				"nor was refused (%v), and the tag is not readable back: %w",
				ErrPromotionUncertain, image, tagErr, err)
		}

		return fmt.Errorf("%w: %s passed verification and CreateTags reported success: %w",
			ErrPromotionUncertain, image, err)
	}

	p.log.Info("stamped the image with the contract it proved",
		"image", image, "contract", contract)

	return nil
}

// confirmContract reads the tag back until it is there, or says it is not.
//
// POLLED, BECAUSE THIS API IS EVENTUALLY CONSISTENT LIKE THE REST OF THEM. A
// single describe that misses a tag AWS has accepted turns a correct promotion
// into a failed build and an error accusing something else of writing the image's
// tags. This package has already made the eventually-consistent mistake twice
// against instances and once against images, which is enough times to stop
// treating a first answer as the answer.
//
// THE CACHE IS NOT THE ANSWER EITHER. imageLayout memoises a describe per image
// id, and this needs the tag set as it is NOW rather than as it was before the
// write.
func (p *Provider) confirmContract(ctx context.Context, image string, contract int) error {
	want := strconv.Itoa(contract)

	var last error

	for attempt := range confirmAttempts {
		if attempt > 0 {
			if err := p.sleep(ctx, confirmWait); err != nil {
				return err
			}
		}

		params := url.Values{}
		params.Set("Action", "DescribeImages")
		params.Set("ImageId.1", image)

		var out describeImagesResponse

		if err := p.api.call(ctx, params, &out); err != nil {
			last = fmt.Errorf("reading it back failed: %w", err)

			continue
		}

		if len(out.Images) > 0 {
			for _, tag := range out.Images[0].Tags {
				if tag.Key == amiContractTag && tag.Value == want {
					return nil
				}
			}
		}

		last = fmt.Errorf("the image does not carry %s=%s", amiContractTag, want)
	}

	return last
}

// How long confirmContract keeps asking.
const (
	confirmAttempts = 4
	confirmWait     = 5 * time.Second
)

// stampContract adds the contract tag to a CreateTags request.
//
// Separate from promoteContract for the same reason stampImage is separate from
// createImage: what this tag says is the whole basis of the contract check, and a
// test that could only observe it through a fake client would be testing the fake.
func stampContract(params url.Values, contract int) {
	params.Set("Tag.1.Key", amiContractTag)
	params.Set("Tag.1.Value", strconv.Itoa(contract))
}
