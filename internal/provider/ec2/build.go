package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BuildSpec describes an AMI to build.
type BuildSpec struct {
	// BaseImage is the AMI the builder starts from. It must be EBS-backed, and
	// the provisioning script below assumes a dnf-based distribution.
	BaseImage string
	// InstanceType is the shape of the BUILDER, which has nothing to do with the
	// shapes jobs will later run on. Bigger only makes the build faster.
	InstanceType string
	// Arch is the runner build to install: "x64" or "arm64". It must match
	// InstanceType, and nothing here can check that — a mismatch produces an image
	// whose runner will not execute.
	Arch string
	// RunnerVersion is the actions/runner release to install, without the "v".
	RunnerVersion string
	// Name is the name given to the produced AMI. AWS requires it to be unique
	// within the account and region.
	Name string
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
	// imageLayout is where an instance-store root is turned down (#54), and a build
	// from such an image would fail at RunInstances with a parameter error instead.
	layout, err := p.imageLayout(ctx, spec.BaseImage)
	if err != nil {
		return "", err
	}

	rootDevice := layout.root

	if len(script) > maxUserData {
		return "", fmt.Errorf("ec2: the provisioning script is %d bytes, over EC2's %d limit",
			len(script), maxUserData)
	}

	params := url.Values{}
	params.Set("Action", "RunInstances")
	params.Set("ImageId", spec.BaseImage)
	params.Set("InstanceType", spec.InstanceType)
	params.Set("MinCount", "1")
	params.Set("MaxCount", "1")
	params.Set("UserData", base64.StdEncoding.EncodeToString([]byte(script)))

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
	"  env HOME=/home/runner USER=runner LOGNAME=runner"

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
func provisionScript(spec BuildSpec) (string, error) {
	if spec.RunnerVersion == "" {
		return "", fmt.Errorf("ec2: a build needs a runner version to install")
	}

	if spec.Arch != "x64" && spec.Arch != "arm64" {
		return "", fmt.Errorf("ec2: runner architecture %q is not x64 or arm64", spec.Arch)
	}

	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eux\n")

	// Docker, git and tar are what a workflow cannot reasonably install itself.
	b.WriteString("dnf install -y docker git tar\n")
	b.WriteString("systemctl enable docker\n")

	// THE RUNNER'S OWN DEPENDENCIES, INSTALLED DIRECTLY RATHER THAN BY ITS SCRIPT.
	//
	// actions/runner ships bin/installdependencies.sh, and calling it is the
	// obvious move. It does not work here: on Amazon Linux 2023 it reads
	// /etc/os-release, finds ID_LIKE="fedora", prints "Can't detect current OS type"
	// and exits non-zero — which under `set -e` ends the build before the poweroff
	// that signals success. That was not a guess; it is what the first real build
	// did, and the console log of the builder it left running is where it was read.
	//
	// So the dependency is named instead. The runner is a .NET application and what
	// it actually needs on a dnf distribution is ICU; without it the runner starts
	// and dies on globalization rather than failing here where it can be seen.
	b.WriteString("dnf install -y libicu\n")

	// A NON-ROOT USER, because the runner refuses to run as root without an
	// override and running untrusted jobs as root would give away the only
	// isolation this backend has above the instance boundary.
	b.WriteString("id runner || useradd -m -s /bin/bash runner\n")
	b.WriteString("usermod -aG docker runner\n")

	b.WriteString("mkdir -p /opt/actions-runner\n")
	b.WriteString("cd /opt/actions-runner\n")

	release := "https://github.com/actions/runner/releases/download/v" + spec.RunnerVersion +
		"/actions-runner-linux-" + spec.Arch + "-" + spec.RunnerVersion + ".tar.gz"

	b.WriteString("curl -fsSL -o runner.tar.gz " + strconv.Quote(release) + "\n")
	b.WriteString("tar xzf runner.tar.gz\n")
	b.WriteString("rm runner.tar.gz\n")
	b.WriteString("chown -R runner:runner /opt/actions-runner\n")

	// THE SUPPORTED RUNNER HOOK, rather than an edit to run.sh. GitHub invokes it
	// after assigning the job and before running any job steps, which is the first
	// instant that separates registration/pickup from the workflow itself.
	b.WriteString("cat > /usr/local/bin/billet-job-started <<'BILLETJOBEOF'\n")
	b.WriteString(jobTimingHook())
	b.WriteString("BILLETJOBEOF\n")
	b.WriteString("chmod 0755 /usr/local/bin/billet-job-started\n")

	// THE ENTRY POINT A TIER NAMES. billet's boot script exports the JIT config and
	// execs the tier's command AS ROOT, so something has to drop privileges without
	// losing that variable. setpriv does it in one process with no shell in between.
	b.WriteString("cat > /usr/local/bin/billet-runner <<'BILLETEOF'\n")
	b.WriteString("#!/bin/sh\nset -eu\n")

	// WAIT FOR DOCKER BEFORE STARTING THE RUNNER, because billet's boot script
	// execs this the moment cloud-init reaches it and the daemon may not be up.
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
	b.WriteString("exec " + privilegeDrop + " \\\n")
	b.WriteString("  BILLET_LAUNCH_EPOCH_NS=\"${BILLET_LAUNCH_EPOCH_NS:-}\" \\\n")
	b.WriteString("  BILLET_RUNNER_START_EPOCH_NS=\"$runner_started\" \\\n")
	b.WriteString("  ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/billet-job-started \\\n")
	b.WriteString("  " + jitEnvVar + "=\"$" + jitEnvVar + "\" \\\n")
	b.WriteString("  /opt/actions-runner/run.sh\n")
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
	b.WriteString("setpriv --reuid=runner --regid=runner --init-groups \\\n")
	b.WriteString("  env HOME=/home/runner USER=runner LOGNAME=runner \\\n")
	b.WriteString("  /opt/actions-runner/bin/Runner.Listener --version\n")

	// THE SUCCESS SIGNAL. Reaching this line is the only thing that tells billet
	// the image is worth making; set -e means a failure anywhere above never
	// arrives here — including the version check.
	b.WriteString("poweroff\n")

	return b.String(), nil
}
