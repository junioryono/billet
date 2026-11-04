package tart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// probeWindow bounds a guest's answers after its boot, against the longest legitimate
// macOS first boot and Docker's start.
const probeWindow = 10 * time.Minute

// probeTeardownWindow bounds ending a probe once a verification is over, whatever
// ended it.
const probeTeardownWindow = 3 * time.Minute

// yieldPoll is how often a probe looks for a job's request.
const yieldPoll = 3 * time.Second

// deleteWindow bounds one `tart delete`, against the seconds removing a macOS VM's
// directory takes.
const deleteWindow = 5 * time.Minute

// lockSettle bounds the wait for a lock nothing should hold.
const lockSettle = 2 * time.Second

// answerLimit bounds what billet reads of a guest's answer. A version is short.
const answerLimit = 4096

// ErrYielded is a verification that gave its macOS slot to a job. The next refresh or
// the operator runs it again.
var ErrYielded = errors.New("the verification yielded its macOS slot to a job, so it recorded nothing")

// errNotBooted is a probe that never started, which says nothing about its image.
var errNotBooted = errors.New("take a place for the probe")

// errPull is a pull that did not finish, which is the network's fault until shown otherwise.
var errPull = errors.New("pull into a private store")

// failedRetry is how long the refresh leaves a digest that failed. The operator's own
// `billet images verify` is not held to it.
const failedRetry = 7 * 24 * time.Hour

// refreshInterest is how long a node keeps verifying an image its file names no tier for since
// a job last launched it or an operator last verified it. The tiers a launch uses are the control
// plane's, so nothing else tells a node an image stopped being wanted, and without this every
// image it ever verified would cost it tens of gigabytes for each move of its tag, forever. As
// long as GitHub's own window on a runner, so a tier used less often than that is one whose
// image would have gone stale regardless.
const refreshInterest = 30 * 24 * time.Hour

// lastUsedGrain is how stale LastUsed may be before a launch writes it, so that the record is
// not rewritten durably for every job.
const lastUsedGrain = time.Hour

// imageVerdict marks a failure that is the image's, the only kind that makes the refresh leave
// a digest alone. Everything else, a host's lock or disk or a guest that never came up, is
// tomorrow's to try again.
type imageVerdict struct{ error }

func (v imageVerdict) Unwrap() error { return v.error }

// isImageVerdict reports whether a verification's error is the image's.
func isImageVerdict(err error) bool {
	verdict, ok := errors.AsType[imageVerdict](err)

	return ok && verdict.error != nil
}

// runnerVersion is the one thing billet takes from a guest, and only once it matches.
var runnerVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// RunnerAnswer is GitHub's window for one runner release. A zero Deadline is no newer
// release yet, so no date.
type RunnerAnswer struct {
	Deadline time.Time
	Expired  bool
}

// RunnerWindow asks where a runner release sits. An error is could-not-tell, which
// never erases a stored deadline. A seam because the answer comes from GitHub, and a
// provider reaches no release history of its own.
type RunnerWindow func(ctx context.Context, runner string) (RunnerAnswer, error)

// VerifyOptions is one verification.
type VerifyOptions struct {
	// Image is the tier's reference, with or without `@verified`.
	Image string
	// Digest names the digest to pull rather than what the tag names now, for a
	// registry that does not answer anonymously and for clearing a withdrawal by hand.
	Digest string
	// Command is the tier's command, whose directory holds the runner.
	Command []string
	// GuestUntrusted boots the probe with the network flags of untrusted work.
	GuestUntrusted bool
	// MacOS says a tier knows this image's guest is macOS, so a limit or a slot it cannot
	// have refuses before tens of gigabytes are pulled. Its own config has the last word.
	MacOS bool
	// NoYield keeps a macOS probe going when a job asks for its slot.
	NoYield bool
	// Automatic is the refresh's own verification, which is no sign anyone wants the image:
	// it keeps its question and does not renew its interest.
	Automatic bool
	// Progress receives tart's pull output.
	Progress io.Writer
	// Window answers the runner's window. Nil answers could-not-tell.
	Window RunnerWindow
}

// Verify makes a base of an image, boots a clone of it and records it only once the
// guest answered and the clone is proved gone (ADR-021).
func (p *Provider) Verify(ctx context.Context, opts VerifyOptions) (*VerifiedRecord, error) {
	attempted := ""

	rec, err := p.verify(ctx, opts, &attempted)

	// A VERDICT ON THE IMAGE IS REMEMBERED, so the refresh does not pull it again tomorrow.
	// Only what is classified as one: this runs on a host, and a lock, a disk or a guest
	// that never came up says nothing about the bytes.
	if isImageVerdict(err) && attempted != "" && ctx.Err() == nil {
		p.recordFailure(ctx, config.TartImageKey(opts.Image), attempted)
	}

	return rec, err
}

// recordFailure notes a digest found wanting. A failure only costs a pull tomorrow.
func (p *Provider) recordFailure(ctx context.Context, key, digest string) {
	cleanCtx, cancel := cleanupContext(ctx)
	defer cancel()

	err := p.withRecords(cleanCtx, func(st *verifiedState) (bool, error) {
		st.verified.Failed[key] = FailedDigest{Digest: digest, At: p.now().UTC()}

		return true, nil
	})
	if err != nil {
		p.log.Warn("could not record a digest that failed its verification", "image", key, "error", err)
	}
}

// precheckGuest refuses what a macOS probe could not do, before the pull: a limit this node
// has no record of, or no slot to take. The slot is not held, since a pull takes an hour and
// a job may want it, so the claim is asked again at the boot. A guest not known to be macOS
// finds out from the base it pulled, which is kept for the next verification.
func (p *Provider) precheckGuest(ctx context.Context, key string, opts VerifyOptions) error {
	macOS := opts.MacOS

	if !config.TartImageIsRemote(key) {
		if guestOS, err := p.guestOSOf(key); err == nil {
			macOS = guestOS == guestDarwin
		}
	}

	if !macOS {
		return nil
	}

	limit, err := p.macOSLimitFor(true)
	if err != nil {
		return fmt.Errorf("%w: %w", errNotBooted, err)
	}

	f, _, err := p.admitMacOS(ctx, p.probeName(key), limit)
	if err != nil {
		return fmt.Errorf("%w: %w", errNotBooted, err)
	}

	_ = f.Close()

	return nil
}

func (p *Provider) verify(ctx context.Context, opts VerifyOptions, attempted *string) (*VerifiedRecord, error) {
	if p.stateDir == "" {
		return nil, errors.New("tart: verify: this provider was built without node.state_dir")
	}

	key := config.TartImageKey(opts.Image)

	switch {
	case key == "":
		return nil, errors.New("tart: verify: no image given")
	case strings.HasPrefix(key, "-"):
		return nil, fmt.Errorf("tart: verify: %q begins with \"-\", which tart reads as a flag", key)
	case strings.Contains(key, "@"):
		return nil, fmt.Errorf("tart: verify: %q names a digest; name the tag the tier names and "+
			"pass the digest with --digest", key)
	case len(opts.Command) == 0:
		return nil, errors.New("tart: verify: no command names where the guest's runner is")
	case opts.Digest != "" && !config.TartImageIsRemote(key):
		return nil, fmt.Errorf("tart: verify: %q is a local VM, which has no digest", key)
	}

	hash := hashName(key)

	verification, err := openLockFile(p.verificationLockPath(hash))
	if err != nil {
		return nil, fmt.Errorf("tart: verify %s: %w", key, err)
	}

	defer func() { _ = verification.Close() }()

	// A SHORT WAIT, not a try: a descriptor a fork still holds until its exec keeps a lock
	// that was just released for an instant, and that is not another verification.
	if err := waitFlock(ctx, verification, syscall.LOCK_EX, lockSettle); err != nil {
		return nil, fmt.Errorf("tart: verify %s: another verification of this image is running: %w", key, err)
	}

	// BEFORE ANYTHING THAT CAN FAIL FOR A REASON OF ITS OWN, a registry that is down among them:
	// on a node whose file names no tier, this is what the refresh tries again from.
	// FROM THE FIRST READ, not from just before the pull: an unpromote during the cleanup or a
	// check below is one this verification started before, and must not undo.
	var (
		epoch     int64
		epochRead bool
	)

	keepIntent := func() error {
		return p.withRecords(ctx, func(st *verifiedState) (bool, error) {
			img := st.image(key)
			if !epochRead {
				epoch, epochRead = img.UnpromoteEpoch, true
			}

			img.Command, img.Untrusted = slices.Clone(opts.Command), opts.GuestUntrusted
			img.MacOS = img.MacOS || opts.MacOS

			if !opts.Automatic {
				img.LastUsed = p.now().UTC()
			}

			return true, nil
		})
	}

	intentErr := keepIntent()

	// A RECORD THAT COULD NOT EVEN BE READ LEAVES NO EPOCH TO BE HELD TO: reading one later would
	// count an unpromote since then as having come first. Only a write that failed goes on.
	if !epochRead {
		return nil, fmt.Errorf("tart: verify %s: %w", key, intentErr)
	}

	if p.beforeCleanup != nil {
		p.beforeCleanup()
	}

	cleanErr := p.cleanVerification(ctx, key)

	// AGAIN AFTER THE CLEANUP, even one that did not finish, whose reason to exist can be why the
	// first failed: a private store an interrupted pull left can fill the disk this record is
	// written to.
	if intentErr != nil {
		intentErr = keepIntent()
	}

	if err := errors.Join(intentErr, cleanErr); err != nil {
		return nil, fmt.Errorf("tart: verify %s: %w", key, err)
	}

	// AFTER WHAT WAS ASKED IS KEPT, since the refresh tries this again once it has been put right.
	netFlags, err := p.netFlags(probeTrust(opts))
	if err != nil {
		return nil, fmt.Errorf("tart: verify %s: %w", key, err)
	}

	digest := opts.Digest
	if digest == "" && config.TartImageIsRemote(key) {
		if digest, err = p.registry(ctx, key); err != nil {
			return nil, fmt.Errorf("tart: verify %s: ask the registry what this tag names: %w; for a "+
				"registry that needs credentials, name the digest with --digest", key, err)
		}
	}

	if err := p.precheckGuest(ctx, key, opts); err != nil {
		if errors.Is(err, ErrNoSlot) {
			p.countYield(ctx, key)
		}

		return nil, fmt.Errorf("tart: verify %s: %w", key, err)
	}

	var (
		generation int64
		pending    *VerifiedRecord
		stale      string
	)

	if err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		if opts.Digest == "" && st.isWithdrawn(key, digest) {
			return false, fmt.Errorf("%s was unpromoted and its tag still names it; to take it back, "+
				"run `billet images verify %s --digest %s`", digest, key, digest)
		}

		img := st.image(key)

		// THE SAME DIGEST WITH ITS FILES UNTOUCHED BOOTS WHAT WAS PULLED. Anything else is
		// not that base any more.
		if kept := st.verified.Pending[key]; kept != nil {
			files, err := fileIdentities(p.vmDir(kept.Base))
			if err == nil && digest != "" && kept.Digest == digest && slices.Equal(files, kept.Files) {
				pending = kept
			} else {
				stale = kept.Base
				delete(st.verified.Pending, key)
			}
		}

		if pending == nil {
			generation = img.NextGeneration
			img.NextGeneration++
		}

		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("tart: verify %s: %w", key, err)
	}

	if stale != "" {
		if err := p.deleteBase(ctx, stale); err != nil {
			p.log.Error("could not remove a base no record names; the next verification of its "+
				"image removes it", "vm", stale, "error", err)
		}
	}

	*attempted = digest

	rec := pending

	if rec == nil {
		var err error

		if rec, err = p.makeBase(ctx, key, digest, generation, opts.Progress); err != nil {
			return nil, fmt.Errorf("tart: verify %s: %w", key, err)
		}
	}

	committed, kept := false, false

	defer func() {
		if !committed && !kept {
			cleanCtx, cancel := cleanupContext(ctx)
			defer cancel()

			p.discardUnrecordedBase(cleanCtx, key, rec.Base)
		}
	}()

	runner, err := p.runProbe(ctx, *rec, opts, netFlags)
	if err != nil {
		if errors.Is(err, ErrYielded) || errors.Is(err, ErrNoSlot) {
			p.countYield(ctx, key)
		}

		if digest != "" && (errors.Is(err, ErrYielded) || errors.Is(err, errNotBooted)) {
			kept = p.keepPending(ctx, key, rec, epoch)
		}

		return nil, fmt.Errorf("tart: verify %s: %w", key, err)
	}

	rec.Runner = runner
	rec.VerifiedAt = p.now().UTC()

	answer, unknown := p.askWindow(ctx, opts.Window, runner)
	if answer.Expired {
		return nil, imageVerdict{fmt.Errorf("tart: verify %s: its runner %s is past GitHub's window, so "+
			"GitHub would send it no job", key, runner)}
	}

	if unknown != nil {
		p.log.Warn("could not tell where this runner sits in GitHub's window; the next refresh asks "+
			"again", "image", key, "runner", runner, "error", unknown)
	}

	var dropped []string

	if err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		img := st.image(key)
		if img.UnpromoteEpoch != epoch {
			return false, errors.New("this image was unpromoted while it was being verified, and a " +
				"probe already in flight never undoes that")
		}

		// THE EPOCH ABOVE SAYS NO UNPROMOTE RAN SINCE THIS BEGAN, in the same document as
		// what an unpromote withdraws, so what is withdrawn here was withdrawn before and
		// is the operator's to take back by naming it.
		if opts.Digest != "" {
			st.verified.Withdrawn[key] = slices.DeleteFunc(st.verified.Withdrawn[key],
				func(d string) bool { return d == digest })
			if len(st.verified.Withdrawn[key]) == 0 {
				delete(st.verified.Withdrawn, key)
			}
		}

		if !answer.Deadline.IsZero() {
			st.verified.Deadlines[runner] = answer.Deadline
		}

		// A DATE ALREADY HELD ANSWERS WHEN GITHUB DOES NOT: a launch would refuse this
		// record at once, so it is refused here, where the operator is looking.
		if deadline, ok := st.verified.Deadlines[runner]; ok && !p.now().Before(deadline) {
			return false, imageVerdict{fmt.Errorf("its runner %s went past GitHub's window on %s, so "+
				"GitHub would send it no job", runner, deadline.Format(time.DateOnly))}
		}

		img.Yields = 0
		img.MacOS = rec.GuestOS == guestDarwin
		delete(st.verified.Pending, key)
		delete(st.verified.Failed, key)
		st.verified.Records = append(st.verified.Records, *rec)
		dropped = retainBases(st, key)

		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("tart: verify %s: %w", key, err)
	}

	committed = true

	// AFTER THE RECORD LOCK: a base's lock can wait on a launch's clone. What this could
	// not remove has no record, so the image's next verification removes it.
	if err := p.deleteBases(ctx, dropped); err != nil {
		p.log.Error("could not remove a base no record names", "image", key, "error", err)
	}

	return rec, nil
}

// keepPending notes a base the next verification of its digest may boot, and answers whether
// it did, since a base no document names goes with the verification that made it.
func (p *Provider) keepPending(ctx context.Context, key string, rec *VerifiedRecord, epoch int64) bool {
	cleanCtx, cancel := cleanupContext(ctx)
	defer cancel()

	err := p.withRecords(cleanCtx, func(st *verifiedState) (bool, error) {
		// AN UNPROMOTE SINCE THIS BEGAN SAYS THAT DIGEST IS NOT WANTED: holding its bytes for
		// a verification the refresh will never run keeps tens of gigabytes for nothing.
		if st.image(key).UnpromoteEpoch != epoch {
			return false, errors.New("the image was unpromoted while it was being verified")
		}

		// WHAT THE BASE SAYS, for the next verification's check before it pulls.
		st.image(key).MacOS = rec.GuestOS == guestDarwin
		st.verified.Pending[key] = rec

		return true, nil
	})
	if err != nil {
		p.log.Warn("could not keep a pulled base for the next verification", "vm", rec.Base, "error", err)

		return false
	}

	return true
}

// discardUnrecordedBase removes a failed verification's base, once the records positively
// do not name it. A commit that failed can still have installed them, since a durable write
// reports a directory sync that failed after its rename, and deleting then would leave a
// launch resolving a base that is gone. Kept when that cannot be told, for the next
// verification, which reads them again.
func (p *Provider) discardUnrecordedBase(ctx context.Context, key, base string) {
	named := false

	err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		named = slices.ContainsFunc(st.recordsOf(key), func(r VerifiedRecord) bool { return r.Base == base })
		if named {
			return false, nil
		}

		// A BASE THAT WAS WAITING AND THEN FAILED ITS PROBE IS WAITING NO LONGER.
		if kept := st.verified.Pending[key]; kept != nil && kept.Base == base {
			delete(st.verified.Pending, key)

			return true, nil
		}

		// AN ABSENCE THAT AUTHORISES A DELETE MUST BE DURABLE, as in cleanVerification.
		return false, p.installer.SyncDirectory(p.stateDir)
	})
	if err != nil {
		p.log.Error("could not read the records to tell whether a base is named, so it is kept; the "+
			"next verification of its image removes it if none names it", "vm", base, "error", err)

		return
	}

	if named {
		return
	}

	if err := p.deleteBase(ctx, base); err != nil {
		p.log.Error("could not remove a base no record names; the next verification of its image "+
			"removes it", "vm", base, "error", err)
	}
}

// cleanupContext is what cleanup runs on once its caller has ended: a fresh bound, covering
// the tart commands as well as the locks. A delete that hangs on the caller's cancelled
// context would hold the store lock against every deployment on the Mac, so one that runs
// out leaves what it was removing for the next verification.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), probeTeardownWindow)
}

// probeTrust is the trust class a probe's network flags come from.
func probeTrust(opts VerifyOptions) provider.TrustClass {
	if opts.GuestUntrusted {
		return provider.TrustUntrusted
	}

	return provider.TrustTrusted
}

// askWindow asks once, answering could-not-tell as an error.
func (p *Provider) askWindow(ctx context.Context, window RunnerWindow, runner string) (RunnerAnswer, error) {
	if window == nil {
		return RunnerAnswer{}, errors.New("nothing asked GitHub")
	}

	return window(ctx, runner)
}

// retainBases keeps the two newest bases of an image, counted by distinct content, and
// answers the bases it dropped. Verifying the same bytes again replaces their base, so
// the previous image stays as a rollback target. An invalid base is never kept.
func retainBases(st *verifiedState, key string) []string {
	contents := map[string]bool{}
	keep := map[string]bool{}

	records := st.recordsOf(key)
	for i := range records {
		r := &records[i]
		if r.Invalid == "" && !st.isWithdrawn(key, r.Digest) && !contents[r.ContentKey] && len(contents) < 2 {
			contents[r.ContentKey] = true
			keep[r.Base] = true
		}
	}

	return st.removeRecords(key, func(r VerifiedRecord) bool { return !keep[r.Base] })
}

// countYield records a yield for `billet check`. A failure only loses the count.
func (p *Provider) countYield(ctx context.Context, key string) {
	cleanCtx, cancel := cleanupContext(ctx)
	defer cancel()

	err := p.withRecords(cleanCtx, func(st *verifiedState) (bool, error) {
		st.image(key).Yields++

		return true, nil
	})
	if err != nil {
		p.log.Warn("could not record that a verification yielded", "image", key, "error", err)
	}
}

// cleanVerification removes what an earlier verification of this image left: a probe, a
// base staging clone, a finished base no record names, and a private store. The caller
// holds the image's verification lock, so none of these is anyone's in-flight work.
func (p *Provider) cleanVerification(ctx context.Context, key string) error {
	named := map[string]bool{}

	// FIRST, because it refuses a state directory that is another deployment's before
	// anything of its could be removed.
	if err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		records := st.recordsOf(key)
		for i := range records {
			named[records[i].Base] = true
		}

		if kept := st.verified.Pending[key]; kept != nil {
			named[kept.Base] = true
		}

		// AN ABSENCE THAT AUTHORISES A DELETE MUST BE DURABLE: a commit can report a sync
		// that failed after its rename, and a crash could then bring back a document naming
		// a base this has removed.
		return false, p.installer.SyncDirectory(p.stateDir)
	}); err != nil {
		return err
	}

	if err := p.endProbe(ctx, p.probeName(key)); err != nil {
		return fmt.Errorf("remove an earlier probe: %w", err)
	}

	if err := os.RemoveAll(p.privateStore(key)); err != nil {
		return fmt.Errorf("remove an earlier private store: %w", err)
	}

	entries, err := os.ReadDir(filepath.Join(p.home, "vms"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("list tart's store: %w", err)
	}

	prefix := basePrefix + p.owner + "-" + hashName(key) + "-"

	for _, entry := range entries {
		name := entry.Name()

		switch {
		case !strings.HasPrefix(name, prefix) || named[name]:
		case strings.HasSuffix(name, stagingSuffix):
			if err := p.deleteOwnedVM(ctx, name); err != nil {
				return fmt.Errorf("remove an earlier staging clone: %w", err)
			}
		default:
			if err := p.deleteBase(ctx, name); err != nil {
				return fmt.Errorf("remove a base no record names: %w", err)
			}
		}
	}

	return nil
}

// runProbe boots a clone of the base with the network flags of its trust class, asks
// the guest what a job depends on, and answers the runner's version. The probe is
// destroyed and proved stopped before this returns, whichever way it ended, and a
// verification records nothing unless it was.
func (p *Provider) runProbe(
	ctx context.Context, rec VerifiedRecord, opts VerifyOptions, netFlags []string,
) (string, error) {
	probe := p.probeName(rec.Image)

	custody, err := openLockFile(p.custodyPath(probe))
	if err != nil {
		return "", fmt.Errorf("open the probe's custody file: %w", err)
	}

	defer func() { _ = custody.Close() }()

	if err := waitFlock(ctx, custody, syscall.LOCK_EX, lockSettle); err != nil {
		return "", fmt.Errorf("something still holds the probe's custody file, so its guest may "+
			"still be running: %w", err)
	}

	claim, err := p.claimGuest(ctx, probe, rec.GuestOS, true)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNotBooted, err)
	}

	defer claim.release()

	runner, askErr := p.bootProbe(ctx, rec, opts, netFlags, custody, claim)

	// BILLET'S COPIES GO FIRST, or its own descriptors would hold what is being proved
	// released. A FRESH BOUND: the caller or a yield may have ended the context this ran on,
	// and the probe is proved gone either way.
	_ = custody.Close()

	claim.release()

	endCtx, endCancel := cleanupContext(ctx)
	defer endCancel()

	if endErr := p.endProbe(endCtx, probe); endErr != nil {
		// NOT WRAPPING WHAT ENDED IT: a yield whose probe may still be alive is not the clean
		// yield a refresh reports as nothing wrong.
		if askErr != nil {
			return "", fmt.Errorf("the probe could not be proved gone: %w (the verification had "+
				"ended: %s)", endErr, askErr.Error())
		}

		return "", fmt.Errorf("the probe could not be proved gone: %w", endErr)
	}

	return runner, askErr
}

// bootProbe clones the base, boots the clone holding its slot and custody, and asks the guest.
func (p *Provider) bootProbe(
	ctx context.Context, rec VerifiedRecord, opts VerifyOptions, netFlags []string,
	custody *os.File, claim *guestClaim,
) (string, error) {
	probe := p.probeName(rec.Image)

	probeCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	if rec.GuestOS == guestDarwin && !opts.NoYield {
		stop := p.watchYield(probeCtx, cancel)
		defer stop()
	}

	if err := p.cloneBase(probeCtx, rec, probe); err != nil {
		return "", probeCause(probeCtx, err)
	}

	if err := p.writeOwner(probe); err != nil {
		return "", err
	}

	if err := p.startDetachedHolding(probe, slices.Concat(claim.files, []*os.File{custody}), netFlags...); err != nil {
		return "", err
	}

	// THE GUEST'S PROCESS NOW HOLDS THEM: billet's copies go, so the kernel releases the
	// slot when that process ends and not when this one does.
	claim.release()

	_ = custody.Close()

	if err := p.proveStarted(probeCtx, probe); err != nil {
		return "", probeCause(probeCtx, err)
	}

	if len(netFlags) > 0 && len(p.cfg.UntrustedDNS) > 0 {
		answerCtx, answerCancel := context.WithTimeout(probeCtx, p.probeWindow)
		defer answerCancel()

		// RETRIED, because `tart list` says running well before the guest agent listens.
		// The script exits nonzero unless the guest then resolves.
		if _, err := p.retryGuest(answerCtx, probe, probeCheck{
			name:   "resolving github.com through node.tart.untrusted_dns",
			script: resolverScript(p.cfg.UntrustedDNS, p.resolverWaits),
		}); err != nil {
			return "", probeCause(probeCtx, err)
		}
	}

	runner, err := p.askGuest(probeCtx, probe, rec.GuestOS, opts.Command)
	if err != nil {
		return "", probeCause(probeCtx, err)
	}

	return runner, nil
}

// probeCause reports a yield as a yield rather than as whichever step it cut short.
func probeCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrYielded) {
		return ErrYielded
	}

	return err
}

// watchYield ends a probe's context when a job asks for a slot. The returned func stops
// the watcher and waits for it.
func (p *Provider) watchYield(ctx context.Context, cancel context.CancelCauseFunc) func() {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		tick := time.NewTicker(p.yieldPoll)
		defer tick.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if p.yieldRequested() {
					cancel(ErrYielded)

					return
				}
			}
		}
	}()

	return func() {
		close(done)
		<-finished
	}
}

// probeCheck is one question put to a guest. Its output is never quoted, and name is
// billet's word for what failed.
type probeCheck struct {
	name   string
	script string
}

// askGuest asks what a job depends on: the runner answers --version on both operating
// systems, Docker with Buildx and Compose on Linux, xcodebuild on macOS. A guest that has
// not finished booting answers none of them yet, so each retries within the window.
func (p *Provider) askGuest(ctx context.Context, probe, guestOS string, command []string) (string, error) {
	askCtx, cancel := context.WithTimeout(ctx, p.probeWindow)
	defer cancel()

	// FIRST, ON ITS OWN: a guest whose agent never came up says nothing about what its image
	// holds, and may be this host's doing. One whose agent answers and whose checks do not is
	// the image's.
	if _, err := p.retryGuest(askCtx, probe, probeCheck{name: "its agent", script: "exit 0"}); err != nil {
		return "", err
	}

	listener := shellQuote(filepath.Join(filepath.Dir(command[0]), "bin", "Runner.Listener"))

	version, err := p.retryGuest(askCtx, probe, probeCheck{
		name:   "the runner answering --version",
		script: `cd "$HOME" && exec ` + listener + ` --version`,
	})
	if err != nil {
		return "", imageVerdict{err}
	}

	version = strings.TrimSpace(version)
	if !runnerVersion.MatchString(version) {
		return "", imageVerdict{errors.New("the guest's runner answered --version with something that " +
			"is not a version")}
	}

	checks := []probeCheck{
		{"Docker's daemon", `docker version --format '{{.Server.Version}}' >/dev/null`},
		{"Docker Buildx", `docker buildx version >/dev/null`},
		{"Docker Compose", `docker compose version >/dev/null`},
	}
	if guestOS == guestDarwin {
		checks = []probeCheck{{"xcodebuild", `xcodebuild -version >/dev/null`}}
	}

	for _, check := range checks {
		if _, err := p.retryGuest(askCtx, probe, check); err != nil {
			return "", imageVerdict{err}
		}
	}

	return version, nil
}

// retryGuest asks until the guest answers or the window ends.
func (p *Provider) retryGuest(ctx context.Context, probe string, check probeCheck) (string, error) {
	retry := time.NewTimer(p.execRetry)
	defer retry.Stop()

	for {
		out, err := p.guestAnswer(ctx, probe, check.script)
		if err == nil {
			return out, nil
		}

		retry.Reset(p.execRetry)

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the probe's guest did not answer for %s: %w", check.name, ctx.Err())
		case <-retry.C:
		}
	}
}

// guestAnswer runs a script through the guest agent and reads a bounded answer. stderr
// is discarded and a failure quotes nothing: a guest's output is never quoted into a
// billet error, and a third party built this image.
func (p *Provider) guestAnswer(ctx context.Context, name, script string) (string, error) {
	// #nosec G204 -- operator-configured binary, billet-built arguments, no shell on the
	// host side.
	cmd := exec.CommandContext(ctx, p.tart, "exec", name, "/bin/sh", "-c", script)
	cmd.WaitDelay = commandWaitDelay
	cmd.Env = append(os.Environ(), "TART_HOME="+p.home)
	cmd.Stderr = io.Discard

	out := &boundedBuffer{limit: answerLimit}
	cmd.Stdout = out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s exec in %s failed: %w", p.tart, name, err)
	}

	return string(out.buf), nil
}

// boundedBuffer keeps at most limit bytes and swallows the rest, so a guest writing
// forever cannot grow this process.
type boundedBuffer struct {
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, data[:min(room, len(data))]...)
	}

	return len(data), nil
}

// endProbe stops a probe, ends the processes holding its custody file and deletes it,
// and is what both a verification and a launch finding an orphan use.
//
// A PROBE'S NAME IS DELETED ONLY AFTER ITS CUSTODY LOCK CAN BE TAKEN, which proves its
// process has ended: a guest still starting can be waiting on tart's store lock, where
// `tart stop` reports it already stopped and deleting its directory ends nothing.
func (p *Provider) endProbe(ctx context.Context, probe string) error {
	path := p.custodyPath(probe)

	_, statErr := os.Lstat(p.vmDir(probe))

	_, custodyErr := os.Lstat(path)

	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		if errors.Is(custodyErr, fs.ErrNotExist) {
			return nil
		}
	case statErr != nil:
		return fmt.Errorf("look for the probe: %w", statErr)
	case errors.Is(custodyErr, fs.ErrNotExist):
		// NO CUSTODY FILE TO HOLD MEANS A LOCK ON ONE MADE HERE PROVES NOTHING, and someone
		// removed it, since a probe is cloned only after its file exists. Only tart's own
		// word that it is stopped lets this go on.
		if err := p.requireStopped(ctx, probe); err != nil {
			return err
		}
	}

	custody, err := openLockFile(path)
	if err != nil {
		return fmt.Errorf("open the probe's custody file: %w", err)
	}

	defer func() { _ = custody.Close() }()

	free, err := tryFlock(custody, syscall.LOCK_EX)
	if err != nil {
		return fmt.Errorf("try the probe's custody: %w", err)
	}

	if !free {
		if err := p.stopProbeProcesses(ctx, probe, custody); err != nil {
			return err
		}
	}

	if err := p.deleteOwnedVM(ctx, probe); err != nil {
		return err
	}

	// STILL HOLDING THE LOCK, so a verification reopening the name finds this inode gone
	// and makes a new one.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the probe's custody file: %w", err)
	}

	return nil
}

// requireStopped refuses unless tart lists the VM and says it is stopped.
func (p *Provider) requireStopped(ctx context.Context, name string) error {
	// THE ROWS ALONE, not the inventory: a lease-named directory that cannot be read says
	// nothing about this VM, and refusing on it would wedge every verification of an image
	// over a fault elsewhere.
	vms, err := p.listRows(ctx)
	if err != nil {
		return fmt.Errorf("ask tart about %s: %w", name, err)
	}

	// A GUEST STILL STARTING, waiting on tart's lock, is reported stopped, and only its
	// process says otherwise.
	guests, err := p.tartGuests(ctx)
	if err != nil {
		return fmt.Errorf("look for %s's process: %w", name, err)
	}

	for _, command := range guests {
		if guestNameIn(command) == name {
			return fmt.Errorf("%s has no custody file and its process is running, so nothing here "+
				"can end it; stop it with `tart stop %s`", name, name)
		}
	}

	for _, vm := range vms {
		if vm.Name == name && !vm.Running && strings.EqualFold(vm.State, "stopped") {
			return nil
		}
	}

	return fmt.Errorf("%s has no custody file and tart does not say it is stopped, so whether its "+
		"process has ended cannot be proved; stop it with `tart stop %s`", name, name)
}

// stopProbeProcesses asks the VM to stop and then, by signal, ends what holds its
// custody file, until the lock can be taken.
func (p *Provider) stopProbeProcesses(ctx context.Context, probe string, custody *os.File) error {
	if _, err := p.run(ctx, "stop", probe); err != nil && !isNotRunning(err) && !isMissingVM(err) {
		p.log.Warn("tart could not stop a probe; ending its processes", "vm", probe, "error", err)
	}

	for _, sig := range []syscall.Signal{0, syscall.SIGTERM, syscall.SIGKILL} {
		if sig != 0 {
			if err := p.signalCustodyHolders(ctx, custody.Name(), sig); err != nil {
				return err
			}
		}

		if err := waitFlock(ctx, custody, syscall.LOCK_EX, p.stopWindow); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return fmt.Errorf("the probe %s's process still holds its custody file", probe)
}

// signalCustodyHolders signals only what it positively identifies as the probe's: a
// process whose executable is tart's or softnet's and which holds the custody file.
// Neither ever opens a file of billet's, so such a process can hold it only by
// inheriting it from the probe's own launch, softnet reparented after a crashed `tart
// run` included. Any other opener, a billet trying the lock or anything else, is left
// alone. Holding the file and the start time are read again around the executable, so a
// pid reused in between is not signalled.
func (p *Provider) signalCustodyHolders(ctx context.Context, path string, sig syscall.Signal) error {
	pids, err := p.openers(ctx, path)
	if err != nil {
		return fmt.Errorf("see who holds the probe's custody file: %w", err)
	}

	for _, pid := range pids {
		start, err := p.processStart(pid)
		if err != nil {
			continue
		}

		exe, err := p.processPath(pid)
		if err != nil {
			continue
		}

		if name := filepath.Base(exe); name != "tart" && name != "softnet" {
			continue
		}

		still, err := p.openers(ctx, path)
		if err != nil {
			return fmt.Errorf("see who holds the probe's custody file: %w", err)
		}

		again, err := p.processStart(pid)
		if err != nil || again != start || !slices.Contains(still, pid) {
			continue
		}

		if err := p.signal(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			p.log.Warn("could not signal a probe's process", "pid", pid, "error", err)
		}
	}

	return nil
}

// removeOrphanProbe removes a probe holding a slot that no verifier owns, and answers
// false when it cannot say so and has done nothing: the image's verification lock is
// held, which asks it to yield instead, or the probe is another deployment's, which only
// that deployment may destroy.
func (p *Provider) removeOrphanProbe(ctx context.Context, probe string) (bool, error) {
	hash, ours := strings.CutPrefix(probe, probePrefix+p.owner+"-")
	if !ours {
		return false, nil
	}

	lock, err := openLockFile(p.verificationLockPath(hash))
	if err != nil {
		return false, err
	}

	defer func() { _ = lock.Close() }()

	free, err := tryFlock(lock, syscall.LOCK_EX)
	if err != nil || !free {
		return false, err
	}

	if err := p.endProbe(ctx, probe); err != nil {
		return false, err
	}

	return true, nil
}

// resolveVerified is the record a launch of `<image>@verified` clones: the newest whose
// base is valid and whose digest is not withdrawn. It refuses, naming the command that
// would fix it, when there is none or the runner's deadline has passed. It reads the
// clock against the stored deadline, so it needs neither the network nor a refresh.
func (p *Provider) resolveVerified(ctx context.Context, key string) (VerifiedRecord, heldFile, error) {
	var (
		found VerifiedRecord
		held  heldFile
	)

	err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		records := st.recordsOf(key)
		for i := range records {
			r := &records[i]
			if r.Invalid != "" || st.isWithdrawn(key, r.Digest) {
				continue
			}

			if deadline, ok := st.verified.Deadlines[r.Runner]; ok && !p.now().Before(deadline) {
				return false, fmt.Errorf("the runner %s in the newest verified base of %s went past "+
					"GitHub's window on %s, so GitHub would send it no job; once the image carries a "+
					"newer one, run `billet images verify %s`", r.Runner, key,
					deadline.Format(time.DateOnly), key)
			}

			// UNDER THE RECORD LOCK, and without waiting: only a delete holds this lock
			// exclusively, a delete comes after the record is gone, and a record naming a
			// base still being deleted is one a crash left. So what removes a record cannot
			// slip between this answer and this lock.
			f, err := openLockFile(p.baseLockPath(r.Base))
			if err != nil {
				return false, fmt.Errorf("lock base %s: %w", r.Base, err)
			}

			free, err := tryFlock(f, syscall.LOCK_SH)
			if err != nil || !free {
				_ = f.Close()

				if err == nil {
					err = errors.New("it is being deleted")
				}

				return false, fmt.Errorf("lock base %s: %w", r.Base, err)
			}

			found, held = *r, heldFile{f: f}

			return false, nil
		}

		return false, fmt.Errorf("no verified base of %s on this node; run `billet images verify %s`",
			key, key)
	})

	if err != nil {
		return found, held, err
	}

	// AFTER, AND NOT A CONDITION OF THE LAUNCH: this is what keeps the refresh interested in an
	// image, and a job is not refused over it.
	if touchErr := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		img := st.image(key)
		if p.now().Sub(img.LastUsed) < lastUsedGrain {
			return false, nil
		}

		img.LastUsed = p.now().UTC()

		return true, nil
	}); touchErr != nil {
		p.log.Warn("could not record that a verified image was launched", "image", key, "error", touchErr)
	}

	return found, held, nil
}

// Unpromote removes a record and its base, withdraws the digest it was pulled as and
// removes every other record of that digest, so no older base of the same bytes keeps
// serving and the refresh never verifies it again. A launch that resolved the base first
// still completes its clone, under the base's lock.
func (p *Provider) Unpromote(ctx context.Context, image, base string) ([]string, error) {
	key := config.TartImageKey(image)

	var dropped []string

	err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		var target *VerifiedRecord

		records := st.recordsOf(key)
		for i := range records {
			if records[i].Base == base {
				target = &records[i]

				break
			}
		}

		if target == nil {
			return false, fmt.Errorf("%s records no base named %q", key, base)
		}

		st.image(key).UnpromoteEpoch++

		if target.Digest != "" && !st.isWithdrawn(key, target.Digest) {
			st.verified.Withdrawn[key] = append(st.verified.Withdrawn[key], target.Digest)
		}

		dropped = st.removeRecords(key, func(r VerifiedRecord) bool {
			return r.Base == base || (target.Digest != "" && r.Digest == target.Digest)
		})

		if kept := st.verified.Pending[key]; kept != nil && target.Digest != "" && kept.Digest == target.Digest {
			dropped = append(dropped, kept.Base)
			delete(st.verified.Pending, key)
		}

		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("tart: unpromote: %w", err)
	}

	if err := p.deleteBases(ctx, dropped); err != nil {
		return dropped, fmt.Errorf("tart: unpromote: the record is gone and no launch resolves it, and "+
			"its base is still on the Mac until the image's next verification removes it: %w", err)
	}

	return dropped, nil
}

// VerifiedDue is what a refresh decided about one reference.
type VerifiedDue struct {
	// Verify says the tag names a digest with no record and no withdrawal.
	Verify bool
	// Digest is what the tag names, empty for a local VM.
	Digest string
	// Why says what was decided, in billet's words.
	Why string
}

// ImageDue answers whether a refresh verifies a `@verified` reference. A local VM never
// is, since nothing tells billet it changed. A registry that cannot answer is an error,
// never a no.
func (p *Provider) ImageDue(ctx context.Context, image string) (VerifiedDue, error) {
	key := config.TartImageKey(image)
	if !config.TartImageIsRemote(key) {
		return VerifiedDue{Why: "a local VM is verified again only when an operator asks"}, nil
	}

	digest, err := p.registry(ctx, key)
	if err != nil {
		return VerifiedDue{}, fmt.Errorf("ask the registry what %s names: %w", key, err)
	}

	due := VerifiedDue{Digest: digest}

	err = p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		switch {
		case st.isWithdrawn(key, digest):
			due.Why = digest + " is withdrawn, and a tag that still names it does not bring it back"
		case slices.ContainsFunc(st.recordsOf(key), func(r VerifiedRecord) bool {
			return r.Digest == digest && r.Invalid == ""
		}):
			due.Why = digest + " is already verified"
		case st.verified.Failed[key].Digest == digest && p.now().Sub(st.verified.Failed[key].At) < failedRetry:
			due.Why = digest + " failed its verification on " + st.verified.Failed[key].At.Format(time.DateOnly) +
				", so the refresh leaves it for a week; `billet images verify` tries it now"
		default:
			due.Verify = true
			due.Why = "the tag names " + digest + ", which is not verified"
		}

		return false, nil
	})

	return due, err
}

// VerifiedImage is what a node holds of one image, for a report.
type VerifiedImage struct {
	Key       string
	Records   []VerifiedRecord
	Withdrawn []string
	Yields    int
	// Deadlines holds the stored date of each record's runner that has one.
	Deadlines map[string]time.Time
	// Pending is a base waiting for its probe, and Failed the digest that last failed.
	Pending *VerifiedRecord
	Failed  *FailedDigest
	// BaseProblems says, by base, what no longer holds of a record's files: a job's launch
	// refuses on it, and only reading them says so.
	BaseProblems map[string]string
	// Wanted says a job launched it or an operator verified it within refreshInterest.
	Wanted bool
	// Command, Untrusted and MacOS are what its last verification was asked with.
	Command   []string
	Untrusted bool
	MacOS     bool
}

// VerifiedImages reads what this node has recorded.
func (p *Provider) VerifiedImages(ctx context.Context) ([]VerifiedImage, error) {
	var images []VerifiedImage

	err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		keys := make([]string, 0, len(st.verified.Images))
		for key := range st.verified.Images {
			keys = append(keys, key)
		}

		slices.Sort(keys)

		for _, key := range keys {
			image := VerifiedImage{
				Key:       key,
				Records:   st.recordsOf(key),
				Withdrawn: slices.Clone(st.verified.Withdrawn[key]),
				Yields:    st.verified.Images[key].Yields,
				Deadlines: map[string]time.Time{},
			}

			progress := st.verified.Images[key]
			image.Pending = st.verified.Pending[key]
			image.BaseProblems = map[string]string{}
			image.Wanted = !progress.LastUsed.IsZero() && p.now().Sub(progress.LastUsed) < refreshInterest
			image.Command, image.Untrusted, image.MacOS = progress.Command, progress.Untrusted, progress.MacOS

			if failed, ok := st.verified.Failed[key]; ok {
				image.Failed = &failed
			}

			for i := range image.Records {
				runner := image.Records[i].Runner
				if deadline, ok := st.verified.Deadlines[runner]; ok {
					image.Deadlines[runner] = deadline
				}

				if problem := p.baseProblem(&image.Records[i]); problem != "" {
					image.BaseProblems[image.Records[i].Base] = problem
				}
			}

			images = append(images, image)
		}

		return false, nil
	})

	return images, err
}

// baseProblem says what no longer holds of a record's files, without marking anything: only a
// launch that is about to boot a copy does that.
func (p *Provider) baseProblem(rec *VerifiedRecord) string {
	files, err := fileIdentities(p.vmDir(rec.Base))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "it is gone from tart's store"
	case errors.Is(err, errNotRegular):
		return "its files are not the ones billet probed"
	case err != nil:
		return "its files could not be read: " + err.Error()
	case !slices.Equal(files, rec.Files):
		return "its files are not the ones billet probed"
	default:
		return ""
	}
}

// OrphanProbes names this deployment's probes no verification owns, which nothing removes
// until the image's next verification or a job needing a slot. A local VM's may never see
// either, and a Linux one takes no slot and is reserved no CPU or memory.
func (p *Provider) OrphanProbes(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(p.home, "vms"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("list tart's store: %w", err)
	}

	var orphans []string

	for _, entry := range entries {
		hash, ours := strings.CutPrefix(entry.Name(), probePrefix+p.owner+"-")
		if !ours {
			continue
		}

		lock, err := openLockFile(p.verificationLockPath(hash))
		if err != nil {
			return nil, err
		}

		free := waitFlock(ctx, lock, syscall.LOCK_SH, lockSettle) == nil

		_ = lock.Close()

		if free {
			orphans = append(orphans, entry.Name())
		}
	}

	return orphans, nil
}

// RefreshDeadlines asks GitHub again for every recorded runner and stores each date it
// answers, answering the versions it could not tell about. It never erases one: an
// answer of could-not-tell is not an answer that the window closed or opened.
func (p *Provider) RefreshDeadlines(ctx context.Context, window RunnerWindow) ([]string, error) {
	var runners []string

	if err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		for i := range st.verified.Records {
			if runner := st.verified.Records[i].Runner; !slices.Contains(runners, runner) {
				runners = append(runners, runner)
			}
		}

		return false, nil
	}); err != nil {
		return nil, err
	}

	slices.Sort(runners)

	var unknown []string

	answers := map[string]time.Time{}

	for _, runner := range runners {
		answer, askErr := p.askWindow(ctx, window, runner)

		switch {
		case askErr != nil:
			unknown = append(unknown, runner)
		case !answer.Deadline.IsZero():
			answers[runner] = answer.Deadline
		}
	}

	if len(answers) == 0 {
		return unknown, nil
	}

	return unknown, p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		for runner, deadline := range answers {
			st.verified.Deadlines[runner] = deadline
		}

		return true, nil
	})
}

// StoreSpace is the room on tart's store and the size of the newest base's disk, which
// a verification needs once more while its private store exists.
func (p *Provider) StoreSpace(rec VerifiedRecord) (uint64, int64, error) {
	var st unix.Statfs_t

	if err := unix.Statfs(p.home, &st); err != nil {
		return 0, 0, fmt.Errorf("read the room on %s: %w", p.home, err)
	}

	info, err := os.Lstat(filepath.Join(p.vmDir(rec.Base), baseDisk))
	if err != nil {
		return 0, 0, fmt.Errorf("read the size of %s: %w", rec.Base, err)
	}

	return freeBytes(&st), info.Size(), nil
}

// defaultRegistry is the registry seam's real answer.
func defaultRegistry(ctx context.Context, key string) (string, error) {
	return registryDigest(ctx, registryClient(), key)
}
