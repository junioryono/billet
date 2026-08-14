package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// THE MKDIR IS THE ALLOCATION. Two launches racing for the same name both call it and
// exactly one succeeds, which is the property a scan-then-take cannot have.
//
// A CLAIM WHOSE JAIL IS GONE IS REAPED AND THEN RETRIED — once, in that order, and
// the retry is the part a first version got wrong. Reaping and moving on frees the
// name for somebody else while this caller reports the host is full, so a range of
// one could never be reused at all. Retrying is bounded to a single attempt because
// losing the race twice means another launch genuinely holds it now.
func (p *Provider) take(claim, jailID string) (bool, error) {
	for attempt := range 2 {
		err := os.Mkdir(claim, 0o700)

		switch {
		case err == nil:
			if err := os.WriteFile(filepath.Join(claim, "jail"), []byte(jailID+"\n"), 0o600); err != nil {
				// THE CLAIM IS ABANDONED, and its own removal failing adds nothing a
				// caller could act on — the reaper frees a claim with no jail
				// recorded in it, which is exactly the state this would leave.
				return false, errors.Join(err, os.RemoveAll(claim))
			}

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
	raw, err := os.ReadFile(filepath.Join(claim, "jail"))
	if err != nil {
		// A claim with no jail recorded in it was interrupted between the mkdir and
		// the write. Nothing is using it, and leaving it would leak a uid forever.
		if errors.Is(err, os.ErrNotExist) {
			return os.RemoveAll(claim) == nil
		}

		return false
	}

	if _, found, err := p.findJail(strings.TrimSpace(string(raw))); err != nil || found {
		return false
	}

	return os.RemoveAll(claim) == nil
}

// releaseUID gives a microVM's uid back.
func (p *Provider) releaseUID(uid int) error {
	if uid <= 0 {
		return nil
	}

	if err := os.RemoveAll(filepath.Join(p.claimsDir("uids"), strconv.Itoa(uid))); err != nil {
		return fmt.Errorf("firecracker: release uid %d: %w", uid, err)
	}

	return nil
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

// releaseTap gives a device name back.
func (p *Provider) releaseTap(name string) error {
	if name == "" {
		return nil
	}

	if err := os.RemoveAll(filepath.Join(p.claimsDir("taps"), name)); err != nil {
		return fmt.Errorf("firecracker: release the network device name %s: %w", name, err)
	}

	return nil
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
const tapPrefix = "bt-"

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
		return resources{}, errors.Join(err, p.releaseUID(uid))
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
func (p *Provider) releaseResources(res resources) error {
	return errors.Join(p.releaseUID(res.UID), p.releaseTap(res.Tap))
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
			raw, err := os.ReadFile(filepath.Join(p.claimsDir(kind), entry.Name(), "jail"))
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
func (p *Provider) releaseOrphaned(ctx context.Context, res resources) error {
	if res == (resources{}) {
		return nil
	}

	// THE DEVICE FIRST, because releasing the NAME while the device still exists
	// would let another microVM claim a name the kernel already has — and `ip tuntap
	// add` would then refuse a launch for a reason that names nothing billet did.
	return errors.Join(p.deleteTap(ctx, res.Tap), p.releaseResources(res))
}

// cgroupDir is where the jailer put this microVM's cgroup.
//
// DERIVED THE SAME WAY THE CHROOT IS, from the resolved binary's name and the jail
// id, because the jailer builds both from the same two values.
func (p *Provider) cgroupDir(j jail) string {
	return filepath.Join(cgroupRoot, p.execName, j.id)
}

// cgroupRoot is where cgroup v2 is mounted on every distribution billet targets.
const cgroupRoot = "/sys/fs/cgroup"

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
	if err := os.Remove(p.cgroupDir(j)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("firecracker: remove the cgroup of %s: %w", j.id, err)
	}

	return nil
}
