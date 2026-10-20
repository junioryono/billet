package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// maxReportLineBytes bounds one line of it: a component name and a version are
	// under a hundred bytes.
	maxReportLineBytes = 256
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

	openComponentsReader = func(cfg *config.Config) (componentsReader, error) {
		return ceph.New(*cfg.Node.Ceph)
	}
)

// completionStore is what a completion asks of the cluster: a generation to boot
// the completion microVM from, the kernel it is paired with, and the removal of the
// completion roots this node made.
type completionStore interface {
	NewestVerifiedForContract(ctx context.Context, image, contract string) (ceph.Generation, bool, error)
	Kernel(ctx context.Context, image, generation string) (string, bool, error)
	RemoveCompletionRoots(ctx context.Context, image, owner string) error
}

// importAsPublished imports the image being completed as it was published, as a
// completion root this node owns, and returns its snapshot. The pull supplies it,
// because the root names the kernel the pull installs, under the pull's kernel
// lock.
//
// A COMPLETION ROOT IS NOT A GENERATION, by its name: nothing that deals in
// generations sees it, no tier can boot it, and the pull removes it.
type importAsPublished func(ctx context.Context) (string, error)

// errCompletionLeftRunning is a completion whose microVM was not proved gone. The
// loop device and the file it backs are left in place, because releasing either
// under a guest that may still be writing is the failure the proof exists for.
var errCompletionLeftRunning = errors.New("the completion microVM was not proved gone")

// completeImage installs plan.Install into the root filesystem at raw, through a
// completion microVM, and returns the version installed of each. It fails unless
// every one was installed and the filesystem checked clean afterwards, and on
// failure raw must not be imported.
func completeImage(
	ctx context.Context, env cli.Env, cfg *config.Config,
	image, raw, kernelDir string, plan completionPlan, wait time.Duration,
	asPublished importAsPublished,
) (map[string]string, error) {
	if len(plan.Install) == 0 {
		return map[string]string{}, nil
	}

	fmt.Fprintf(env.Stdout, "completing %s with %s\n", image, strings.Join(plan.names(), ", "))

	store, err := openCompletionStore(cfg)
	if err != nil {
		return nil, err
	}

	owner := cfg.Node.Name

	// LEFTOVERS FIRST, and only this node's: another node's completion root may be
	// booting its completion right now.
	if err := store.RemoveCompletionRoots(ctx, image, owner); err != nil {
		return nil, err
	}

	// THE ROOT IS CHOSEN, AND ANY IMPORT OF THE IMAGE AS PUBLISHED MADE, BEFORE raw
	// IS GROWN OR WRITTEN. Imported from a file the completion has started to
	// change, it would be neither the published image nor the result.
	root, isCompletionRoot, err := completionRoot(ctx, env, store, image, kernelDir, asPublished)
	if err != nil {
		return nil, err
	}

	installed, completeErr := completeOnRoot(ctx, env, cfg, root, isCompletionRoot, raw, plan, wait)

	// REMOVED WHATEVER THE COMPLETION CAME TO, on a context of its own: it is owed
	// even to a cancelled pull, and is safe under a live clone, which clone v2
	// keeps working from the trash.
	if isCompletionRoot {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()

		completeErr = errors.Join(completeErr, store.RemoveCompletionRoots(removeCtx, image, owner))
	}

	if completeErr != nil {
		return nil, completeErr
	}

	return installed, nil
}

// completionRoot is the generation the completion microVM boots: the newest of
// this image verified under this binary's guest contract whose kernel is on this
// node, or else the image as published, imported as a generation of its own.
//
// THE KERNEL MUST BE HERE. A generation is paired with a kernel file, and a node
// that joined a site after it was verified has never installed that file, so the
// launch would refuse; the image as published carries its own, installed by this
// pull.
func completionRoot(
	ctx context.Context, env cli.Env, store completionStore,
	image, kernelDir string, asPublished importAsPublished,
) (string, bool, error) {
	newest, ok, err := store.NewestVerifiedForContract(ctx, image, firecracker.GuestContract)
	if err != nil {
		return "", false, err
	}

	if ok {
		kernel, recorded, err := store.Kernel(ctx, image, newest.Name)
		if err != nil {
			return "", false, err
		}

		if !recorded || regularFileExists(filepath.Join(kernelDir, kernel)) {
			return image + "@" + newest.Name, false, nil
		}

		fmt.Fprintf(env.Stdout, "the newest verified generation %s@%s is paired with a kernel this "+
			"node does not have, so the completion boots the image as published\n", image, newest.Name)
	}

	snapshot, err := asPublished(ctx)
	if err != nil {
		return "", false, err
	}

	fmt.Fprintf(env.Stdout, "imported the image as published as %s@%s, a completion root no tier "+
		"can boot, to boot the completion from\n", image, snapshot)

	return image + "@" + snapshot, true, nil
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular()
}

// completeOnRoot grows raw by what plan installs, attaches it as a block device and
// runs the completion microVM on root.
func completeOnRoot(
	ctx context.Context, env cli.Env, cfg *config.Config,
	root string, isCompletionRoot bool, raw string, plan completionPlan, wait time.Duration,
) (map[string]string, error) {
	if err := growForCompletion(raw, plan); err != nil {
		return nil, err
	}

	device, detach, err := attachLoop(ctx, raw)
	if err != nil {
		return nil, err
	}

	installed, gone, runErr := runCompletion(ctx, env, cfg, root, isCompletionRoot, device, plan, wait)

	// DETACHED ONLY ONCE THE MICROVM IS PROVED GONE. A guest that may still be
	// running may still be writing, and a detach under it cuts off the write.
	if !gone {
		return nil, errors.Join(runErr, fmt.Errorf("billet images pull: %w; %s and %s, which it "+
			"may still be writing, are left in place", errCompletionLeftRunning, device, raw))
	}

	detachCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()

	if err := errors.Join(runErr, detach(detachCtx)); err != nil {
		return nil, err
	}

	return installed, nil
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

// runCompletion boots the completion microVM and judges what it reports. gone is
// whether no microVM holding device remains: nothing was launched with it, or the
// one launched was destroyed and the backend confirmed it.
func runCompletion(
	ctx context.Context, env cli.Env, cfg *config.Config,
	root string, isCompletionRoot bool, device string, plan completionPlan, wait time.Duration,
) (map[string]string, bool, error) {
	deployment, err := verifyDeploymentID(cfg)
	if err != nil {
		return nil, true, err
	}

	// THE PROBE'S IDENTITY AND ITS LOCK, so the node's sweep leaves the microVM
	// alone and no verification on this host is the same microVM at once.
	prov, err := newCompletionProvider(cfg, probeDeployment(deployment))
	if err != nil {
		return nil, true, err
	}

	lease, err := probeLeaseID(deployment)
	if err != nil {
		return nil, true, err
	}

	lock, err := takeProbeLock(cfg.Node.LockDir, deployment)
	if err != nil {
		return nil, true, err
	}

	defer func() {
		if err := lock.release(); err != nil {
			fmt.Fprintf(env.Stdout, "warning: the verification lock was not released: %v\n", err)
		}
	}()

	if err := lock.hold(); err != nil {
		return nil, true, err
	}

	// A PROBE AN EARLIER RUN LEFT is not one holding this device, so failing to
	// remove it leaves nothing of this run's behind.
	if err := destroyProbe(ctx, prov, provider.InstanceName(lease)); err != nil {
		return nil, true, err
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

				CompletionRoot: isCompletionRoot,
			}
		},
		check: func(body, secret string) error {
			got, err := checkCompletionReport(body, secret, plan)
			installed = got

			return err
		},
		silent: "so nothing was installed into the image and it has not been imported",
	}

	result := runGuestProbe(ctx, env, prov, cfg.Node.Firecracker.Bridge,
		cfg.Node.Firecracker.ImageVerifyPort, probe, lock.hold)
	if err := errors.Join(result.verdict, result.cleanup); err != nil {
		return nil, result.gone, err
	}

	return installed, result.gone, nil
}

// componentsReader reads what a generation's image left out and what was installed.
type componentsReader interface {
	Components(ctx context.Context, image, generation string) (ceph.GenerationComponents, bool, error)
}

// completionGap compares a generation's components record with what this
// deployment accepts: missing is what the image left out, the deployment accepts
// and was not installed; unaccepted is what was installed and the deployment no
// longer accepts. A generation with no record left nothing out.
func completionGap(record ceph.GenerationComponents, found bool, accepted []config.ImageComponent,
) ([]string, []string) {
	if !found {
		return nil, nil
	}

	var missing, unaccepted []string

	for _, name := range record.LeftOut {
		_, installed := record.Installed[name]
		isAccepted := slices.Contains(accepted, config.ImageComponent(name))
		switch {
		case isAccepted && !installed:
			missing = append(missing, name)
		case !isAccepted && installed:
			unaccepted = append(unaccepted, name)
		}
	}

	return missing, unaccepted
}

// refuseIncomplete refuses to let image@generation be verified when it lacks a
// component this deployment accepts, which is the image as published that a
// completion boots from, or one whose completion failed. Verified, @verified
// would move every tier onto it.
func refuseIncomplete(ctx context.Context, store componentsReader, command, image, generation string,
	accepted []config.ImageComponent,
) error {
	record, found, err := store.Components(ctx, image, generation)
	if err != nil {
		return err
	}

	if missing, _ := completionGap(record, found, accepted); len(missing) > 0 {
		return fmt.Errorf("%s: %s@%s lacks %s, which this deployment accepts, so it is not marked "+
			"verified; `billet images pull` completes a new generation of the image",
			command, image, generation, strings.Join(missing, ", "))
	}

	return nil
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
		if len(line) > maxReportLineBytes {
			return nil, fmt.Errorf("billet images pull: the completion reported a line of %d bytes, "+
				"past the %d one may be", len(line), maxReportLineBytes)
		}

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
