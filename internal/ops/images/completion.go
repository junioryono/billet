package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/store/ceph"
)

// COMPLETION (#584). billet's published guest image leaves out components whose
// licenses forbid billet redistributing them, and a deployment that accepts a
// component in images.accept has it installed from its vendor before the image is
// imported. The install runs in a trusted microVM booted on this node: the image
// being completed is attached to it as its second disk, and inside the guest the
// image's own completer, /opt/billet/complete/run, is run in a chroot of that disk.
// Vendor code never runs on the node, and the node never mounts a filesystem it
// wrote; the guest kernel does all of that.
//
// THE CONTRACT, imagesource.CompletionContractV1, between this file and the
// completer the image carries:
//
//   - The completer is run as root, chrooted into the image, with /proc, /sys,
//     /dev, /dev/pts and /run bound from the guest, as
//     `/opt/billet/complete/run <component>...`, and BILLET_COMPLETION_REPORT naming
//     a file it writes.
//   - It installs each component it is given and runs GitHub's tests for it, and
//     writes one line per component to the report: `installed=<name> <version>` or
//     `failed=<name>`. Its exit status and its output decide nothing.
//
// The guest side of the contract is completionCommand, which also checks the
// filesystem after the completer has run and posts the report.

const (
	// completionDevice is where the guest sees the image being completed: the
	// first drive after the root, as the node's own volumes are (guestVolumeDevice).
	completionDevice = "/dev/vdb"

	completionVCPU   = 4
	completionMemory = 8 * config.GiB

	// defaultCompletionWait bounds the whole completion. The Android SDK is several
	// gigabytes, and GitHub's own tests run after each component.
	defaultCompletionWait = 2 * time.Hour

	// maxReportLines bounds what the guest may claim, at four lines per component.
	maxReportLines = 4 * imagesource.MaxComponents
)

// completionVersionPattern is what a recorded version may be, the same shape the
// store accepts.
var completionVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+:~_-]{0,63}$`)

// completionPlan is what a pull installs into one image.
type completionPlan struct {
	// LeftOut is every component the image leaves out, sorted.
	LeftOut []string
	// Install is the components this deployment accepts, in the manifest's order.
	Install []imagesource.Component
}

// planCompletion intersects what the image leaves out with what this deployment
// accepts. A component this build does not know is left out and never installed:
// nothing could have accepted it.
func planCompletion(m *imagesource.Manifest, accepted []config.ImageComponent) completionPlan {
	var plan completionPlan
	for _, c := range m.Components {
		plan.LeftOut = append(plan.LeftOut, c.Name)
		if config.IsImageComponent(c.Name) && slices.Contains(accepted, config.ImageComponent(c.Name)) {
			plan.Install = append(plan.Install, c)
		}
	}
	slices.Sort(plan.LeftOut)

	return plan
}

// names is the plan's components to install, in order.
func (p completionPlan) names() []string {
	out := make([]string, 0, len(p.Install))
	for _, c := range p.Install {
		out = append(out, c.Name)
	}

	return out
}

// The seams a completion runs through, so a test can drive it without a loop
// device, a cluster or a microVM.
var (
	newCompletionProvider = app.NewProvider

	// attachLoop exposes a file as a block device, and returns it with the detach.
	attachLoop = attachLoopDevice

	openCompletionStore = func(cfg *config.Config) (completionStore, error) {
		return ceph.New(*cfg.Node.Ceph)
	}
)

// completionStore is what a completion asks of the cluster: a generation to boot
// the completion microVM from, and the removal of a staged one.
type completionStore interface {
	NewestVerifiedForContract(ctx context.Context, image, contract string) (ceph.Generation, bool, error)
	RemoveCompletionRoot(ctx context.Context, name string) error
}

// stageRoot imports the image as published, under the completion root's name,
// and returns the generation. The pull supplies it, because a staged generation
// names the kernel the pull installs, under the pull's kernel lock.
type stageRoot func(ctx context.Context, name string) (string, error)

// completeImage installs plan.Install into the root filesystem at raw, through a
// completion microVM, and returns the version installed of each. It fails unless
// every one was installed and the filesystem checked clean afterwards, and on
// failure raw must not be imported.
func completeImage(
	ctx context.Context, env cli.Env, cfg *config.Config,
	image, raw string, plan completionPlan, wait time.Duration, stage stageRoot,
) (map[string]string, error) {
	if len(plan.Install) == 0 {
		return map[string]string{}, nil
	}

	fmt.Fprintf(env.Stdout, "completing %s with %s\n", image, strings.Join(plan.names(), ", "))

	store, err := openCompletionStore(cfg)
	if err != nil {
		return nil, err
	}

	// THE ROOT IS CHOSEN, AND ANY STAGING IMPORTED, BEFORE raw IS GROWN OR WRITTEN.
	// A staging root is the image as published, and imported from a file the
	// completion has started to change it would be neither that nor the result.
	root, removeRoot, err := completionRoot(ctx, env, store, image, stage)
	if err != nil {
		return nil, err
	}

	installed, completeErr := completeOnRoot(ctx, env, cfg, root, raw, plan, wait)

	return installed, errors.Join(completeErr, removeRoot())
}

// completionRoot is the generation the completion microVM boots: the newest of
// this image proved to boot under this binary's guest contract, or, on a site
// that has none, a staging import of the image being completed. remove takes the
// staging away again and does nothing for a real generation.
func completionRoot(
	ctx context.Context, env cli.Env, store completionStore, image string, stage stageRoot,
) (string, func() error, error) {
	nothing := func() error { return nil }

	newest, ok, err := store.NewestVerifiedForContract(ctx, image, firecracker.GuestContract)
	if err != nil {
		return "", nil, err
	}
	if ok {
		return image + "@" + newest.Name, nothing, nil
	}

	staging := ceph.CompletionRoot(image)

	// LEFTOVERS FIRST. A completion that died left its root behind, and a second
	// generation of it would only be more to remove.
	if err := store.RemoveCompletionRoot(ctx, staging); err != nil {
		return "", nil, err
	}

	fmt.Fprintf(env.Stdout, "no generation of %s is verified yet, so the completion boots a "+
		"staging copy of it, %s\n", image, staging)

	generation, err := stage(ctx, staging)
	if err != nil {
		return "", nil, errors.Join(err, store.RemoveCompletionRoot(context.WithoutCancel(ctx), staging))
	}

	return staging + "@" + generation, func() error {
		// ON A CONTEXT OF ITS OWN, because removal is owed even when the
		// completion was cancelled, and is what a later pull would otherwise have
		// to find.
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()

		return store.RemoveCompletionRoot(removeCtx, staging)
	}, nil
}

// completeOnRoot grows raw by what plan installs, attaches it as a block device and
// runs the completion microVM on root.
func completeOnRoot(
	ctx context.Context, env cli.Env, cfg *config.Config,
	root, raw string, plan completionPlan, wait time.Duration,
) (map[string]string, error) {
	if err := growForCompletion(raw, plan); err != nil {
		return nil, err
	}

	device, detach, err := attachLoop(ctx, raw)
	if err != nil {
		return nil, err
	}

	installed, runErr := runCompletion(ctx, env, cfg, root, device, plan, wait)

	// DETACHED AFTER THE MICROVM IS GONE, which runCompletion's destroy proves;
	// detaching under a running guest would cut off a write it had not finished.
	detachCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()

	return installed, errors.Join(runErr, detach(detachCtx))
}

// growForCompletion extends raw by the space each component it installs declares.
// Only the file grows: the guest grows the filesystem, so the node parses none.
func growForCompletion(raw string, plan completionPlan) error {
	info, err := os.Stat(raw)
	if err != nil {
		return fmt.Errorf("billet images pull: cannot grow %s for its components: %w", raw, err)
	}

	size := info.Size()
	for _, c := range plan.Install {
		size += c.Space
	}

	if err := os.Truncate(raw, size); err != nil {
		return fmt.Errorf("billet images pull: cannot grow %s for its components: %w", raw, err)
	}

	return nil
}

// runCompletion boots the completion microVM and judges what it reports.
func runCompletion(
	ctx context.Context, env cli.Env, cfg *config.Config,
	root, device string, plan completionPlan, wait time.Duration,
) (map[string]string, error) {
	deployment, err := verifyDeploymentID(cfg)
	if err != nil {
		return nil, err
	}

	// THE PROBE'S IDENTITY AND ITS LOCK, so the node's sweep leaves the microVM
	// alone and no verification on this host is the same microVM at once.
	prov, err := newCompletionProvider(cfg, probeDeployment(deployment))
	if err != nil {
		return nil, err
	}

	lease, err := probeLeaseID(deployment)
	if err != nil {
		return nil, err
	}

	lock, err := takeProbeLock(cfg.Node.LockDir, deployment)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err := lock.release(); err != nil {
			fmt.Fprintf(env.Stdout, "warning: the verification lock was not released: %v\n", err)
		}
	}()

	if err := lock.hold(); err != nil {
		return nil, err
	}

	if err := destroyProbe(ctx, prov, provider.InstanceName(lease)); err != nil {
		return nil, err
	}

	var installed map[string]string

	probe := guestProbe{
		command: "billet images pull",
		image:   root,
		lease:   lease,
		wait:    wait,
		spec: func(name, addr, secret string) provider.Spec {
			return provider.Spec{
				Name:      name,
				Image:     root,
				VCPU:      completionVCPU,
				Memory:    completionMemory,
				Disk:      verifyDisk(cfg, 0),
				Volumes:   []provider.VolumeMount{{Device: device}},
				Command:   []string{"/bin/sh", "-c", completionCommand(addr, plan.names())},
				Trust:     provider.TrustTrusted,
				JITConfig: secret,
			}
		},
		check: func(body, secret string) error {
			got, err := checkCompletionReport(body, secret, plan)
			installed = got

			return err
		},
		silent: "so nothing was installed into the image and it has not been imported",
	}

	if err := runGuestProbe(ctx, env, prov, cfg.Node.Firecracker.Bridge,
		cfg.Node.Firecracker.ImageVerifyPort, probe, lock.hold); err != nil {
		return nil, err
	}

	return installed, nil
}

// completionCommand is the guest's half of completion contract 1: prepare the
// image at completionDevice, run its completer, check its filesystem, and post
// what happened to addr. Each step's status is reported rather than acted on,
// because the node, not the guest, decides whether the image is imported.
//
// components are config.ImageComponent names, which are lowercase letters, digits
// and hyphens, and so need no quoting.
func completionCommand(addr string, components []string) string {
	return completionScript(addr, components, completionDevice, "/mnt/billet-completion",
		"/run/billet-completion.report")
}

// completionScript is completionCommand with its paths named, so a test can run it
// against a directory of its own.
func completionScript(addr string, components []string, dev, mnt, report string) string {
	return `set -u
dev=` + dev + `
mnt=` + mnt + `
report=` + report + `
prepare() {
	sudo mkdir -p "$mnt" || return 1
	sudo mount "$dev" "$mnt" || return 1
	sudo resize2fs "$dev" >/dev/null 2>&1 || return 1
	for d in proc sys dev dev/pts run; do
		sudo mount --bind "/$d" "$mnt/$d" || return 1
	done
	sudo rm -f "$report" || return 1
}
prepared=1
if prepare; then
	sudo env BILLET_COMPLETION_REPORT="$report" chroot "$mnt" /opt/billet/complete/run ` +
		strings.Join(components, " ") + ` >/dev/null 2>&1
	prepared=0
fi
sudo umount -R "$mnt" >/dev/null 2>&1
unmounted=$?
fsck=skipped
if [ "$unmounted" -eq 0 ]; then
	sudo e2fsck -f -y "$dev" >/dev/null 2>&1
	fsck=$?
fi
{
	echo "jit=$ACTIONS_RUNNER_INPUT_JITCONFIG"
	echo "prepared=$prepared"
	echo "unmounted=$unmounted"
	echo "fsck=$fsck"
	sudo cat "$report" 2>/dev/null | grep -E '^(installed|failed)=' | head -n ` +
		fmt.Sprint(maxReportLines) + `
} | curl -sf --max-time 30 --data-binary @- http://` + addr + `/report
`
}

// checkCompletionReport turns the guest's report into the versions installed, or
// into the reason the image must not be imported.
//
// NOTHING THE GUEST WROTE IS QUOTED. A component is named only once it matches one
// this pull asked for, and a version only once it matches the store's pattern.
func checkCompletionReport(body, secret string, plan completionPlan) (map[string]string, error) {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) > maxReportLines+4 {
		return nil, fmt.Errorf("billet images pull: the completion reported %d lines, past the "+
			"%d it may", len(lines), maxReportLines+4)
	}

	fields := map[string][]string{}
	for _, line := range lines {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			return nil, errors.New("billet images pull: the completion reported a line that is " +
				"not key=value")
		}
		fields[key] = append(fields[key], value)
	}

	one := func(key string) (string, bool) {
		v := fields[key]

		return firstOf(v), len(v) == 1
	}

	if v, ok := one("jit"); !ok || v != secret {
		return nil, errors.New("billet images pull: the completion's report did not carry this " +
			"run's secret, so it is not this run's guest")
	}

	for _, step := range []struct{ key, failed string }{
		{"prepared", "the image could not be mounted and prepared for its completer"},
		{"unmounted", "the image could not be unmounted after its completer ran"},
		{"fsck", "the image's filesystem did not check clean after its completer ran"},
	} {
		if v, ok := one(step.key); !ok || v != "0" {
			return nil, fmt.Errorf("billet images pull: %s, so it has not been imported", step.failed)
		}
	}

	asked := plan.names()
	installed := map[string]string{}
	for _, value := range fields["installed"] {
		name, version, _ := strings.Cut(value, " ")
		if !slices.Contains(asked, name) {
			return nil, errors.New("billet images pull: the completion reported installing a " +
				"component this pull did not ask for")
		}
		if _, dup := installed[name]; dup {
			return nil, fmt.Errorf("billet images pull: the completion reported %s installed twice",
				name)
		}
		if !completionVersionPattern.MatchString(version) {
			return nil, fmt.Errorf("billet images pull: the completion reported %s installed with "+
				"no version billet can record", name)
		}
		installed[name] = version
	}

	var failed []string
	for _, value := range fields["failed"] {
		if !slices.Contains(asked, value) {
			return nil, errors.New("billet images pull: the completion reported a failure of a " +
				"component this pull did not ask for")
		}
		failed = append(failed, value)
	}

	for _, name := range asked {
		if _, ok := installed[name]; !ok && !slices.Contains(failed, name) {
			failed = append(failed, name)
		}
	}

	for key := range fields {
		if !slices.Contains([]string{"jit", "prepared", "unmounted", "fsck", "installed", "failed"}, key) {
			return nil, errors.New("billet images pull: the completion reported a field the contract " +
				"does not name")
		}
	}

	if len(failed) > 0 {
		slices.Sort(failed)
		failed = slices.Compact(failed)

		return nil, fmt.Errorf("billet images pull: %s did not install, so the image has not been "+
			"imported; the generation in use is unchanged", strings.Join(failed, ", "))
	}

	return installed, nil
}

func firstOf(values []string) string {
	if len(values) == 0 {
		return ""
	}

	return values[0]
}

// attachLoopDevice exposes path as a loop device and returns it with its detach.
func attachLoopDevice(ctx context.Context, path string) (string, func(context.Context) error, error) {
	out, err := exec.CommandContext(ctx, "losetup", "--find", "--show", path).Output()
	if err != nil {
		return "", nil, fmt.Errorf("billet images pull: cannot attach %s as a loop device: %w", path, err)
	}

	device := strings.TrimSpace(string(out))
	if !strings.HasPrefix(device, "/dev/loop") {
		return "", nil, fmt.Errorf("billet images pull: losetup attached %s as %q, which is not a "+
			"loop device", path, device)
	}

	return device, func(ctx context.Context) error {
		if err := exec.CommandContext(ctx, "losetup", "--detach", device).Run(); err != nil {
			return fmt.Errorf("billet images pull: cannot detach %s from %s: %w", device, path, err)
		}

		return nil
	}, nil
}
