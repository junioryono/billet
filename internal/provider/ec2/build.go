package ec2

import (
	"context"
	"encoding/base64"
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
// THE BUILDER IS ALWAYS TERMINATED, including on every failure path, because it is
// the one thing here that costs money for as long as it exists.
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

		if err := p.Destroy(stop, id); err != nil {
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
		return "", err
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
	params.Set("TagSpecification.1.ResourceType", "instance")
	params.Set("TagSpecification.1.Tag.1.Key", ownerTag)
	params.Set("TagSpecification.1.Tag.1.Value", p.owner)
	params.Set("TagSpecification.1.Tag.2.Key", "Name")
	params.Set("TagSpecification.1.Tag.2.Value", spec.Name+"-builder")

	var out runInstancesResponse

	if err := p.api.call(ctx, params, &out); err != nil {
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

		switch out {
		case "stopped":
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

// describeRaw reports one instance's state name.
func (p *Provider) describeRaw(ctx context.Context, id string) (string, error) {
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
			return "", nil
		}

		return "", fmt.Errorf("ec2: ask what the builder %s is doing: %w", id, err)
	}

	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if i.InstanceID == id {
				return i.State.Name, nil
			}
		}
	}

	return "", nil
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
		return "", fmt.Errorf("ec2: register an image from the builder %s: %w", instance, err)
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

	b.WriteString("exec setpriv --reuid=runner --regid=runner --init-groups \\\n")
	b.WriteString("  env ACTIONS_RUNNER_INPUT_JITCONFIG=\"$ACTIONS_RUNNER_INPUT_JITCONFIG\" \\\n")
	b.WriteString("  /opt/actions-runner/run.sh\n")
	b.WriteString("BILLETEOF\n")
	b.WriteString("chmod 0755 /usr/local/bin/billet-runner\n")

	// THE SUCCESS SIGNAL. Reaching this line is the only thing that tells billet
	// the image is worth making; set -e means a failure anywhere above never
	// arrives here.
	b.WriteString("poweroff\n")

	return b.String(), nil
}
