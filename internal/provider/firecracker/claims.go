package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// A microVM needs two things that are scarce on a host rather than derivable from
// its lease: a uid to run as, and a network device name. Both are allocated here,
// and both follow the same three rules — which is why they live together.
//
//  1. THE CLAIM IS THE CREATE. A "is it free" check followed by a "take it" step is
//     two operations, and two concurrent launches both pass the first. Every
//     allocation below is an os.Mkdir or an O_EXCL create, which the kernel makes
//     atomic: the winner is whoever the syscall says it is.
//  2. WHAT WAS ALLOCATED IS RECORDED IN THE JAIL. Teardown runs after a crash, with
//     nothing but a lease id, so a resource it cannot re-derive is one it cannot
//     release. Writing it down beside the microVM is what makes a NON-derivable
//     name safe to use at all.
//  3. THE JAIL IS THE LIFETIME. Removing it releases the claim, so there is one
//     thing to clean up rather than three, and a reaper that has removed a jail has
//     not leaked a uid.

// claimsDir is where host-wide allocations live, beside the jails they belong to.
//
// UNDER THE CHROOT BASE rather than in the node's state directory, because the thing
// being allocated is scarce on the MACHINE. Two billet deployments sharing a host
// share its uids and its network devices, so they have to contend in one place — the
// same reason the chroot base is shared, and the reason the owner marker exists to
// tell their jails apart afterwards.
func (p *Provider) claimsDir(kind string) string {
	return filepath.Join(p.cfg.ChrootBase, ".billet-"+kind)
}

// claimUID takes a uid for one microVM out of the configured range.
//
// A UID PER GUEST, NOT ONE FOR ALL OF THEM. The jailer drops the VMM to this
// account, so a single shared uid means every VMM on the host runs as the same user
// — and one that escapes its chroot can then reach every other jail's files, signal
// every other VMM, and read every other guest's root disk node, because the kernel
// sees one user doing all of it. The chroot is what separates them today; a uid of
// its own is what separates them when the chroot does not hold.
//
// THE RANGE IS HIGH AND UNNAMED ON PURPOSE. These uids have no passwd entry and are
// not meant to: an account a person could log into is a bigger thing than a number
// the kernel uses to keep two VMMs apart, and creating one per microVM would be a
// deployment step per job.
func (p *Provider) claimUID(j jail) (int, error) {
	dir := p.claimsDir("uids")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("firecracker: create the uid claims directory %s: %w", dir, err)
	}

	for uid := p.cfg.JailUIDMin; uid < p.cfg.JailUIDMin+p.cfg.JailUIDCount; uid++ {
		taken, err := p.take(filepath.Join(dir, strconv.Itoa(uid)), j.id)
		if err != nil {
			return 0, fmt.Errorf("firecracker: claim uid %d: %w", uid, err)
		}

		if taken {
			return uid, nil
		}
	}

	return 0, fmt.Errorf("firecracker: every uid in %d-%d is claimed by a running microVM, so this "+
		"host cannot start another; widen node.firecracker.jail_uid_count",
		p.cfg.JailUIDMin, p.cfg.JailUIDMin+p.cfg.JailUIDCount-1)
}

// take claims one name for a jail, reporting whether it got it.
//
// THE LINK IS THE ALLOCATION, AND THE CONTENT EXISTS BEFORE THE NAME DOES. The claim
// is staged as a file holding the jail id and then `link`ed into place: link is
// atomic and fails with EEXIST if the name is taken, so two launches racing for one
// name have exactly one winner, and the winner's claim is COMPLETE the instant it is
// visible.
//
// THAT SECOND HALF IS THE WHOLE FIX. This used to be `Mkdir` followed by a separate
// write of the jail file, which left a window where a claim existed with nothing
// recorded in it — and the reaper, correctly, frees claims in exactly that state. So
// two ordinary concurrent launches could both win: B created the directory, A found
// it empty and reaped it, B recreated it, A's write landed in B's directory, and both
// returned success holding one uid. Two VMMs then ran as one account, silently
// dissolving the isolation the per-guest uid exists to provide, with nothing anywhere
// able to detect it. Both reviewers found this independently.
//
// The lowest free name is always tried first, so concurrent launches contend on the
// SAME name systematically rather than occasionally — this was not a narrow race.
//
// A CLAIM WHOSE JAIL IS GONE IS REAPED AND THEN RETRIED — once, in that order, and
// the retry is the part a first version got wrong. Reaping and moving on frees the
// name for somebody else while this caller reports the host is full, so a range of
// one could never be reused at all. Retrying is bounded to a single attempt because
// losing the race twice means another launch genuinely holds it now.
func (p *Provider) take(claim, jailID string) (bool, error) {
	staged, err := os.CreateTemp(filepath.Dir(claim), ".staging-")
	if err != nil {
		return false, fmt.Errorf("firecracker: stage a claim beside %s: %w", claim, err)
	}

	// The staging name is never the claim, so removing it is unconditional: on the
	// winning path the link has already given the content a second name.
	defer os.Remove(staged.Name())

	if _, err := staged.WriteString(jailID + "\n"); err != nil {
		return false, errors.Join(fmt.Errorf("firecracker: write a staged claim: %w", err),
			staged.Close())
	}

	if err := staged.Close(); err != nil {
		return false, fmt.Errorf("firecracker: close a staged claim: %w", err)
	}

	for attempt := range 2 {
		err := os.Link(staged.Name(), claim)

		switch {
		case err == nil:
			return true, nil

		case !errors.Is(err, os.ErrExist):
			return false, err

		case attempt == 0 && p.reapClaim(claim):
			continue

		default:
			return false, nil
		}
	}

	return false, nil
}

// reapClaim frees an allocation whose microVM is gone, reporting whether it did.
//
// A CLAIM OUTLIVES ITS JAIL ONLY AFTER A CRASH. Teardown releases both, so a claim
// naming a jail that is not there is the residue of a node that died mid-launch —
// and without this the range would shrink by one on every such crash until a host
// that has been up for months could not start a microVM at all.
//
// It reads the jail's own existence rather than a timestamp: a claim is free exactly
// when the thing it was taken for is gone, which is a fact rather than a heuristic.
func (p *Provider) reapClaim(claim string) bool {
	raw, err := os.ReadFile(claim)
	if err != nil {
		// GONE ALREADY IS NOT REAPED BY THIS CALL. Reporting true here would say a
		// name was freed that somebody else may have just taken, and the caller acts
		// on that by trying to take it.
		//
		// There is no longer a "claim with nothing recorded in it" to handle: a claim
		// is linked into place with its content already written, so it is never
		// visible in a half-made state.
		return false
	}

	if _, found, err := p.findJail(strings.TrimSpace(string(raw))); err != nil || found {
		return false
	}

	return os.Remove(claim) == nil
}

// releaseClaim gives one name back, but only if it is still this jail's.
//
// COMPARE BEFORE DELETING, because a claim can legitimately belong to somebody else
// by the time teardown reaches it. Teardown removes the jail BEFORE releasing claims
// — deliberately, so that no claim is freed while a VMM might still be using it — and
// in that gap another launch may reap this very claim (its jail is gone, which is
// exactly the reaper's condition) and take the name for itself. A blind delete then
// removes a LIVE claim, and the next launch takes a uid a running microVM is using.
//
// So the name alone is not enough to authorise a delete; the claim has to still say
// it is ours.
func (p *Provider) releaseClaim(claim, jailID string) error {
	raw, err := os.ReadFile(claim)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("firecracker: read the claim %s: %w", claim, err)
	}

	if strings.TrimSpace(string(raw)) != jailID {
		// Somebody else's now. Leaving it is the only safe answer, and it is not an
		// error: the resource this teardown was responsible for is already released.
		return nil
	}

	if err := os.Remove(claim); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("firecracker: release the claim %s: %w", claim, err)
	}

	return nil
}

// releaseUID gives a microVM's uid back, if it is still that microVM's.
func (p *Provider) releaseUID(uid int, jailID string) error {
	if uid <= 0 {
		return nil
	}

	return p.releaseClaim(filepath.Join(p.claimsDir("uids"), strconv.Itoa(uid)), jailID)
}

// claimTap takes a network device name for one microVM.
//
// ALLOCATED RATHER THAN DERIVED, AND THAT IS THE WHOLE POINT. A name derived from
// the lease has to fit the kernel's 15-character limit while a lease id is 39, so
// the first version truncated — which turns a guarantee into a probability, and the
// failure it produces is two live guests contending for one device. Counting instead
// makes the name short by construction and unique by the same syscall that takes it.
//
// The number is small and reused, so a host's device list stays readable: `bt-7` is a
// name an operator can find in `ip link`, and the jail records which lease holds it.
func (p *Provider) claimTap(j jail) (string, error) {
	dir := p.claimsDir("taps")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("firecracker: create the tap claims directory %s: %w", dir, err)
	}

	for n := range maxTaps {
		name := tapPrefix + strconv.Itoa(n)

		taken, err := p.take(filepath.Join(dir, name), j.id)
		if err != nil {
			return "", fmt.Errorf("firecracker: claim the network device %s: %w", name, err)
		}

		if taken {
			return name, nil
		}
	}

	return "", fmt.Errorf("firecracker: every network device name up to %s%d is claimed by a "+
		"running microVM", tapPrefix, maxTaps-1)
}

// releaseTap gives a device name back, if it is still that microVM's.
func (p *Provider) releaseTap(name, jailID string) error {
	if name == "" {
		return nil
	}

	return p.releaseClaim(filepath.Join(p.claimsDir("taps"), name), jailID)
}

// maxTaps bounds how many microVMs one host may run at once, as far as naming goes.
//
// Far above what a machine can hold — the reference host is 64 cores and a tier is
// at least one vCPU — so this is a guard against a runaway loop rather than a
// capacity limit. The allocator's budget is the real one.
const maxTaps = 4096

// tapPrefix marks a device as billet's on a host that may have others, and leaves
// room for a number inside the kernel's 15-character limit — `bt-` plus four digits
// is seven, against a limit of fifteen.
//
// IT LIVES IN config BECAUSE THE CONFIG HAS TO KNOW IT: a bridge named `bt-3` is a
// name a launch will eventually try to create and be refused for, so validation
// rejects it — and that check is only as good as the two names being the same one.
const tapPrefix = config.TapPrefix

// resources are what one microVM holds that the HOST allocated, rather than what its
// lease implies. Recorded in the jail, because teardown after a crash has nothing
// else to go on.
type resources struct {
	UID int    `json:"uid"`
	GID int    `json:"gid"`
	Tap string `json:"tap"`
}

// claimResources takes everything the host allocates for one microVM and records it
// in the jail, in the order that leaves nothing unrecoverable.
//
// THE RECORD IS WRITTEN BEFORE THE LAST CLAIM IS RETURNED, so there is no moment
// where billet holds an allocation nothing on disk names. A crash between two claims
// leaves the first one recorded and reapable; a crash before any leaves nothing.
func (p *Provider) claimResources(j jail) (resources, error) {
	uid, err := p.claimUID(j)
	if err != nil {
		return resources{}, err
	}

	res := resources{UID: uid, GID: uid}

	// RECORDED AS SOON AS THERE IS SOMETHING TO RECORD. If the device claim below
	// fails, the uid is already written down and the unwind releases it; without this
	// the uid would be held by a claim directory whose jail exists but whose resource
	// file never mentioned it, and only the reaper would ever free it.
	if err := p.writeResources(j, res); err != nil {
		return resources{}, errors.Join(err, p.releaseUID(uid, j.id))
	}

	tap, err := p.claimTap(j)
	if err != nil {
		return resources{}, err
	}

	res.Tap = tap

	if err := p.writeResources(j, res); err != nil {
		return resources{}, err
	}

	return res, nil
}

// releaseResources gives back everything a microVM held.
//
// BOTH, WHATEVER EITHER DOES. Stopping at the first failure would leak the other,
// and these are the two things a host runs out of.
func (p *Provider) releaseResources(res resources, jailID string) error {
	return errors.Join(p.releaseUID(res.UID, jailID), p.releaseTap(res.Tap, jailID))
}

// claimedBy finds what a microVM still holds when its jail is already gone.
//
// THE CLAIMS OUTLIVE THE JAIL, and that asymmetry is what this exists for. Teardown
// removes the jail before it releases anything, so a Destroy interrupted between the
// two leaves a uid and a device name held by a lease nothing on the host names any
// more. A later Destroy would find no jail, conclude there was nothing to do, and
// return success — while the tap stayed attached to the bridge, which nothing
// enumerates looking for orphans.
//
// The claim records the lease, so it can be read the other way round.
func (p *Provider) claimedBy(jailID string) (resources, error) {
	var (
		res      resources
		failures []error
	)

	for _, kind := range []string{"uids", "taps"} {
		entries, err := os.ReadDir(p.claimsDir(kind))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			failures = append(failures, fmt.Errorf("firecracker: list the %s claims: %w", kind, err))

			continue
		}

		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(p.claimsDir(kind), entry.Name()))
			if err != nil || strings.TrimSpace(string(raw)) != jailID {
				continue
			}

			if kind == "taps" {
				res.Tap = entry.Name()

				continue
			}

			uid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}

			res.UID, res.GID = uid, uid
		}
	}

	return res, errors.Join(failures...)
}

// releaseOrphaned takes back what a microVM held after its jail is gone.
func (p *Provider) releaseOrphaned(ctx context.Context, res resources, jailID string) error {
	if res == (resources{}) {
		return nil
	}

	// THE DEVICE FIRST, because releasing the NAME while the device still exists
	// would let another microVM claim a name the kernel already has — and `ip tuntap
	// add` would then refuse a launch for a reason that names nothing billet did.
	//
	// AND THE NAME IS KEPT WHEN THE DEVICE WOULD NOT GO. Releasing it anyway is how
	// a host wedges itself permanently: the device stays attached to the bridge,
	// nothing enumerates orphan devices, the next launch draws the same lowest-free
	// name, `ip tuntap add` refuses it, that launch fails and hands the name back —
	// and every launch after it fails the same way until an operator deletes the
	// device by hand. Keeping the claim is what lets a later teardown find the device
	// and finish the job.
	if err := p.deleteTap(ctx, res.Tap); err != nil {
		return errors.Join(err, p.releaseUID(res.UID, jailID))
	}

	return p.releaseResources(res, jailID)
}

const defaultProcMountsPath = "/proc/mounts"

func (p *Provider) cgroupExecDirs() (string, []string, error) {
	root, err := cgroup2Mount(p.procMountsPath)
	if err != nil {
		return "", nil, err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return "", nil, fmt.Errorf("firecracker: list cgroup-v2 hierarchy %s: %w", root, err)
	}

	var dirs []string
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return "", nil, fmt.Errorf("firecracker: inspect cgroup entry %s: %w", entry.Name(), err)
		}
		if info.IsDir() && strings.Contains(entry.Name(), "firecracker") {
			dirs = append(dirs, entry.Name())
		}
	}

	return root, dirs, nil
}

func cgroup2Mount(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("firecracker: read the mount table %s: %w", path, err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return decodeMountPath(fields[1]), nil
		}
	}

	return "", errors.New("firecracker: no cgroup-v2 hierarchy appears in the mount table")
}

func decodeMountPath(path string) string {
	path = strings.ReplaceAll(path, `\040`, " ")
	path = strings.ReplaceAll(path, `\011`, "\t")
	path = strings.ReplaceAll(path, `\012`, "\n")

	return strings.ReplaceAll(path, `\134`, `\`)
}

// removeCgroup takes away the cgroup the jailer made for a microVM.
//
// THE FIFTH THING THAT OUTLIVES A GUEST, and the one the teardown inventory missed.
// billet passes --cgroup on every launch precisely so the jailer creates one — it
// has to, since mixing the two forms on a host is refused outright — and nothing
// removed it, so a host accumulated one empty directory per job forever.
//
// An rmdir rather than a RemoveAll: a cgroup directory's contents are kernel files
// that cannot be deleted, and a non-empty one means processes are still in it, which
// is a refusal worth hearing rather than a state to force past.
func (p *Provider) removeCgroup(j jail) error {
	root, err := cgroup2Mount(p.procMountsPath)
	if err != nil {
		return err
	}

	return p.removeCgroupAtFn(root, j)
}

func (p *Provider) removeCgroupAt(root string, j jail) error {
	if err := os.Remove(filepath.Join(root, j.execName, j.id)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("firecracker: remove the cgroup of %s: %w", j.id, err)
	}

	return nil
}
