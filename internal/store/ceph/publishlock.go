package ceph

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LockImageName is the dedicated image the publish lock is taken on.
//
// A DEDICATED IMAGE RATHER THAN A LOCK ON THE GOLDEN IMAGE ITSELF, because
// mapping an image takes an automatic exclusive-lock on it — measured: the head
// carries an `auto <id>` locker while mapped — so locking the thing being written
// collides with the write.
//
// THE SAME NAME THE BUILD SCRIPT USES, and that is the whole point. A Go import
// and a hand-run `build-guest-image.sh` write the same head image, so a lock
// either side takes alone excludes nothing. This is the interop surface between
// them and it is why the name is a constant rather than a parameter.
const LockImageName = ".publish-lock"

// StaleLockAfter is when a held publish lock stops being believed.
//
// BECAUSE A LEAKED LOCK IS OTHERWISE PERMANENT. An `rbd lock` is NOT a lease: it
// outlives the process that took it, and nothing reclaims it. A killed build, a
// systemd timeout or a power loss therefore leaves it held by a process that no
// longer exists — and since a refusal never breaks a lock, every later publish on
// every node refuses too, forever. The fleet then stops being rebuilt, and thirty
// days after a runner release it stops being sent jobs, which is precisely the
// outage this whole mechanism exists to prevent.
//
// Six hours matches the script's bound, deliberately: the two must agree, or
// whichever is more patient would refuse a lock the other had already broken and
// retaken.
const StaleLockAfter = 6 * time.Hour

// cookieAge parses the timestamp the cookie ends with.
//
// THE SHAPE IS SHARED WITH THE BUILD SCRIPT, which reads it with the regex
// `-(?<t>[0-9]+)$`. Any cookie this package writes must end the same way or a
// script-side build could never age out a lock this side leaked — one direction
// of the interop would silently stop working, and only under the failure it
// exists to handle.
var cookieAge = regexp.MustCompile(`-(\d+)$`)

func knownLockCookie(cookie string) bool {
	return strings.HasPrefix(cookie, "billet-import-") ||
		strings.HasPrefix(cookie, "billet-build-")
}

var errLockContended = errors.New("ceph: advisory lock is contended")

// lockContendedError identifies safe exclusion separately from Ceph failures.
type lockContendedError struct {
	message string
}

func (e lockContendedError) Error() string { return e.message }
func (e lockContendedError) Unwrap() error { return errLockContended }

// lockContendedf records a human-readable holder while retaining retry identity.
func lockContendedf(format string, args ...any) error {
	return lockContendedError{message: fmt.Sprintf(format, args...)}
}

// publishCookie is the identity a publisher takes the lock under.
//
// A NONCE, BECAUSE HOST AND PID AND SECOND ARE NOT A UNIQUE IDENTITY. Release
// finds its own lock by matching this string, so two publishers that generate the
// same one -- the same hostname on cloned images, the same pid in two pid
// namespaces, inside the same second -- would each find the OTHER's locker id and
// be able to remove the other's lock. That is the one thing a lock must not
// permit, and it costs eight random bytes to make impossible.
//
// THE TRAILING UNIX TIME IS THE INTEROP SURFACE and must stay last.
// build-guest-image.sh ages out a stale lock by reading it with `-(?<t>[0-9]+)$`,
// so a nonce appended AFTER the timestamp would make every lock this side takes
// unreclaimable by a shell build -- and only under the failure that mechanism
// exists to handle, which is the worst possible time to discover it.
func publishCookie(now time.Time) (string, error) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown"
	}

	var nonce [8]byte

	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("ceph: make a unique publish-lock identity: %w", err)
	}

	return fmt.Sprintf("billet-import-%s-%d-%s-%d",
		host, os.Getpid(), hex.EncodeToString(nonce[:]), now.UTC().Unix()), nil
}

// HeartbeatInterval is how often a holder proves it is still alive.
//
// The counter is written to the lock image's metadata rather than into the lock
// itself, because an rbd lock's id IS the cookie and cannot be updated: refreshing
// it would mean removing and re-adding, which opens exactly the window this is
// meant to close.
const HeartbeatInterval = 30 * time.Second

// HeartbeatObservation is how long a would-be breaker watches before concluding
// silence.
//
// SEVERAL INTERVALS, because one is a race: a holder that beat just before the
// first observation and is due to beat just after the second would look silent.
// This only runs on the break path, which by definition is already looking at a
// lock older than StaleLockAfter, so ninety seconds more costs nothing and buys
// certainty.
const HeartbeatObservation = 3 * HeartbeatInterval

// heartbeatObservation is how long this client watches before concluding silence.
//
// A METHOD SO A TEST CAN SHORTEN IT. Ninety seconds is right in production and
// unusable in a test suite, and a decision that is only ever exercised with the
// window stubbed out is one nobody has tested.
func (c *Client) heartbeatObservation() time.Duration {
	if c.observation > 0 {
		return c.observation
	}

	return HeartbeatObservation
}

// PublishLock is a held cluster-wide publish lock.
type PublishLock struct {
	client *Client
	cookie string
	image  string

	stop    context.CancelFunc
	stopped chan struct{}
}

// heartbeatKey is where a holder's liveness counter lives.
func heartbeatKey(cookie string) string { return "billet.heartbeat." + cookie }

// beat starts the goroutine that proves this holder is alive.
//
// ON A CONTEXT STRIPPED OF THE CALLER'S CANCELLATION, because the heartbeat has to
// outlive a deadline that expires mid-import: that is precisely the case where the
// holder is still writing and must not be declared dead. It stops when the lock is
// released, which is the only thing that should stop it.
func (l *PublishLock) beat(ctx context.Context) {
	beatCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	l.stop = cancel
	l.stopped = make(chan struct{})

	go func() {
		defer close(l.stopped)

		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()

		var counter uint64

		for {
			select {
			case <-beatCtx.Done():
				return
			case <-ticker.C:
				counter++

				// BEST EFFORT, AND SILENT. A failed beat is not a reason to abort an
				// import that is otherwise working; its only consequence is that a
				// breaker may fall back to the age bound, which is the behaviour that
				// existed before heartbeats at all.
				writeCtx, done := context.WithTimeout(beatCtx, l.client.wait)

				if _, beatErr := l.client.rbdCmd(writeCtx, false, "image-meta", "set", l.image,
					heartbeatKey(l.cookie), strconv.FormatUint(counter, 10)); beatErr != nil {
					// SILENT AND BEST EFFORT. A failed beat is not a reason to abort an
					// import that is otherwise working; its only consequence is that a
					// breaker falls back to the age bound, which is the behaviour that
					// existed before heartbeats at all.
					_ = beatErr
				}

				done()
			}
		}
	}()
}

// observeHeartbeat reads a holder's liveness counter.
func (c *Client) observeHeartbeat(ctx context.Context, image, cookie string) heartbeat {
	out, err := c.rbdCmd(ctx, false, "image-meta", "get", image, heartbeatKey(cookie))
	if err != nil {
		// ABSENT IS A VALID OBSERVATION, not an error: the build script writes no
		// counter at all, and its locks must still be judged.
		return heartbeat{}
	}

	value, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return heartbeat{}
	}

	return heartbeat{counter: value, present: true}
}

// TakePublishLock claims the right to write the golden image.
func (c *Client) TakePublishLock(ctx context.Context, now time.Time) (*PublishLock, error) {
	image := c.cfg.ImagePool + "/" + LockImageName
	cookie, err := publishCookie(now)
	if err != nil {
		return nil, err
	}

	return c.takeLock(ctx, image, cookie, now, StaleLockAfter)
}

// takeLock claims an advisory lock on one dedicated image.
func (c *Client) takeLock(
	ctx context.Context,
	image, cookie string,
	now time.Time,
	staleAfter time.Duration,
) (*PublishLock, error) {
	// CREATED IF ABSENT, AND A FAILURE HERE IS NOT FATAL. The ordinary case is that
	// it already exists, which `rbd create` reports as an error; distinguishing
	// that from a real failure is what `lock add` does a line later, and far more
	// reliably than reading create's prose.
	//
	// `layering` ALONE, because Ceph documents the `exclusive-lock` feature as
	// incompatible with these advisory lock commands — and the default feature set
	// includes it.
	if _, createErr := c.rbdCmd(ctx, false, "create", image, "--size", "1",
		"--image-feature", "layering"); createErr != nil {
		// DELIBERATELY NOT RETURNED. The ordinary case is that the image already
		// exists, which rbd reports as a failure, and telling that apart from a real
		// one means reading its prose. The `lock add` on the next line answers the
		// same question definitively: if the image genuinely could not be created,
		// locking it fails too, and that failure is the one worth reporting.
		_ = createErr
	}

	_, addErr := c.rbdCmd(ctx, false, "lock", "add", image, cookie)
	if addErr == nil {
		lock := &PublishLock{client: c, cookie: cookie, image: image}
		lock.beat(ctx)

		return lock, nil
	}

	// SOMEBODY HOLDS IT. Whether that is a live build or a corpse is the only
	// question left, and it is answered from the cookie rather than from anything
	// about the process, because there is nothing here that can see the process.
	holder, age, ageKnown, found, err := c.heldLock(ctx, image, now)
	if err != nil {
		if !exitedWith(addErr, 16) {
			return nil, errors.Join(
				fmt.Errorf("ceph: could not take the advisory lock on %s: %w", image, addErr),
				err,
			)
		}

		return nil, err
	}

	if found && holder.ID == cookie {
		lock := &PublishLock{client: c, cookie: cookie, image: image}
		lock.beat(ctx)

		return lock, nil
	}
	if !exitedWith(addErr, 16) {
		return nil, fmt.Errorf("ceph: could not take the advisory lock on %s: %w", image, addErr)
	}
	if !found {
		// Taken and released between the two calls. Cache-index callers retry this
		// bounded race; golden-image publishers still report it and stand down.
		return nil, lockContendedf("ceph: the publish lock on %s could not be taken and is not held; "+
			"another publisher is contending for it right now", image)
	}
	if !ageKnown {
		return nil, fmt.Errorf("ceph: %s is held by %s and its age cannot be established; "+
			"this publisher is standing down rather than writing into the same image",
			image, holder.ID)
	}
	if age < staleAfter {
		return nil, lockContendedf("ceph: %s is held by %s and it is only %s old; "+
			"this publisher is standing down rather than writing into the same image",
			image, holder.ID, age.Round(time.Second))
	}

	// LIVENESS IS OBSERVED, NOT INFERRED FROM THE CLOCK.
	//
	// Age alone cannot tell an abandoned lock from a publish that is genuinely
	// taking a long time, or from an observer whose clock is ahead. The raw write
	// is deliberately unbounded and imports up to a terabyte, so a publish running
	// past the bound is not hypothetical -- and breaking a live holder's lock puts
	// two writers on one image, which is the one thing this exists to prevent.
	//
	// So the holder's counter is read twice, separated by a window. A counter that
	// MOVED is proof of life that needs no agreement about the time between the two
	// machines.
	before := c.observeHeartbeat(ctx, image, holder.ID)

	// A TIMER THAT IS STOPPED, not time.After, which holds its timer until it fires
	// however the select ends. Ninety seconds of leaked timer per contended publish
	// is small, and it is also exactly the kind of thing that is invisible until
	// something calls this in a loop.
	timer := time.NewTimer(c.heartbeatObservation())
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("ceph: gave up waiting to see whether %s is still alive: %w",
			holder.ID, ctx.Err())
	case <-timer.C:
	}

	after := c.observeHeartbeat(ctx, image, holder.ID)

	mayBreak, why := breakable(before, after, age, staleAfter)
	if !mayBreak {
		return nil, lockContendedf("ceph: %s is held by %s and %s; this publisher is standing down "+
			"rather than writing into the same image", image, holder.ID, why)
	}

	// OWNERSHIP IS REVALIDATED AFTER THE OBSERVATION WINDOW. A live holder can
	// finish during those ninety seconds; trying to remove its now-absent lock
	// turns successful serialization into a terminal error, while removing a
	// replacement would violate the exclusion this function exists to provide.
	current, currentAge, currentAgeKnown, currentFound, err := c.heldLock(ctx, image, now)
	if err != nil {
		return nil, err
	}
	if !currentFound {
		return nil, lockContendedf("ceph: %s was released while its holder was observed; "+
			"this publisher will retry rather than treating the race as a failure", image)
	}
	if current.ID != holder.ID || current.Locker != holder.Locker {
		if !currentAgeKnown {
			return nil, fmt.Errorf("ceph: %s changed holders while %s was observed and is now held by %s "+
				"whose age cannot be established; this publisher is standing down",
				image, holder.ID, current.ID)
		}

		return nil, lockContendedf("ceph: %s changed holders while %s was observed and is now held by %s "+
			"whose lock is %s old; this publisher is standing down",
			image, holder.ID, current.ID, currentAge.Round(time.Second))
	}

	if _, err := c.rbdCmd(ctx, false, "lock", "rm", image, holder.ID, holder.Locker); err != nil {
		if exitedWith(err, 2) {
			return nil, lockContendedf("ceph: %s was released while its holder was being removed; "+
				"this publisher will retry rather than treating the race as a failure", image)
		}

		return nil, fmt.Errorf("ceph: %s has been held by %s for %s and showed no sign of life, "+
			"and it could not be broken: %w", image, holder.ID, age.Round(time.Second), err)
	}

	if _, addErr := c.rbdCmd(ctx, false, "lock", "add", image, cookie); addErr != nil {
		replacement, replacementAge, replacementAgeKnown, replacementFound, readErr :=
			c.heldLock(ctx, image, now)
		if readErr != nil {
			return nil, errors.Join(
				fmt.Errorf("ceph: %s was broken after %s but could not then be taken: %w",
					image, age.Round(time.Second), addErr),
				readErr,
			)
		}
		// The command may have committed before its response was lost. This process
		// can continue only when the listing names its exact nonce-bearing cookie.
		ownsLock := replacementFound && replacement.ID == cookie
		if !ownsLock {
			switch {
			case !exitedWith(addErr, 16):
				return nil, fmt.Errorf("ceph: %s was broken after %s but could not then be taken: %w",
					image, age.Round(time.Second), addErr)
			case !replacementFound:
				return nil, lockContendedf("ceph: %s was broken after %s, but the publisher that won "+
					"the reacquisition race already released it; this publisher will retry",
					image, age.Round(time.Second))
			case !replacementAgeKnown:
				return nil, fmt.Errorf("ceph: %s was broken after %s, then was taken by %s "+
					"whose age cannot be established; this publisher is standing down",
					image, age.Round(time.Second), replacement.ID)
			default:
				return nil, lockContendedf("ceph: %s was broken after %s, then was taken by %s "+
					"whose lock is %s old; this publisher is standing down",
					image, age.Round(time.Second), replacement.ID, replacementAge.Round(time.Second))
			}
		}
	}

	// THE BROKEN HOLDER'S COUNTER IS REMOVED, so a future breaker does not read a
	// dead publisher's number and mistake it for this one's. Best effort: failing
	// to tidy it costs one fallback to the age bound, not correctness.
	if _, rmErr := c.rbdCmd(ctx, false, "image-meta", "remove", image,
		heartbeatKey(holder.ID)); rmErr != nil {
		_ = rmErr
	}

	lock := &PublishLock{client: c, cookie: cookie, image: image}
	lock.beat(ctx)

	return lock, nil
}

// lockEntry is the half of `rbd lock ls --format json` this needs.
type lockEntry struct {
	ID     string `json:"id"`
	Locker string `json:"locker"`
}

// heldLock reports who holds the lock and for how long.
func (c *Client) heldLock(
	ctx context.Context,
	image string,
	now time.Time,
) (lockEntry, time.Duration, bool, bool, error) {
	out, err := c.rbdCmd(ctx, true, "lock", "ls", image)
	if err != nil {
		return lockEntry{}, 0, false, false, fmt.Errorf("ceph: could not read who holds %s: %w", image, err)
	}

	var held []lockEntry
	if err := json.Unmarshal(out, &held); err != nil {
		return lockEntry{}, 0, false, false, fmt.Errorf("ceph: %s did not list the lockers of %s as json; "+
			"is it the rbd command?", c.bin, image)
	}

	if len(held) == 0 {
		return lockEntry{}, 0, false, false, nil
	}

	if !knownLockCookie(held[0].ID) {
		return held[0], 0, false, true, nil
	}

	match := cookieAge.FindStringSubmatch(held[0].ID)
	if match == nil {
		// A COOKIE THIS DID NOT WRITE. Something took the lock that is not billet, or
		// is a billet old enough to predate the convention. Either way its age
		// cannot be established, and a lock of unknown age is never broken: the
		// alternative is breaking a live writer's lock because its name was
		// unfamiliar.
		return held[0], 0, false, true, nil
	}

	seconds, parseErr := strconv.ParseInt(match[1], 10, 64)
	if parseErr != nil {
		// AN AGE OF ZERO, NOT AN ERROR. A cookie whose trailing digits do not fit an
		// int64 was not written by anything this understands, and the caller treats
		// age zero as "too recent to break" -- which is the safe reading. Reporting
		// an error instead would refuse the publish outright rather than merely
		// declining to break somebody else's lock.
		return held[0], 0, false, true, nil //nolint:nilerr // an unreadable timestamp is an age, not a failure
	}

	age := now.UTC().Sub(time.Unix(seconds, 0).UTC())

	// A COOKIE FROM THE FUTURE IS NOT STALE. Clocks disagree across a cluster, and
	// a negative age passed through the comparison below unchanged would read as
	// "not yet stale" — which is the safe answer, but only by accident. Clamping
	// says so on purpose.
	if age < 0 {
		age = 0
	}

	return held[0], age, true, true, nil
}

// Release gives the publish lock back.
//
// NOT AUTOMATIC, AND NOT A LEASE. Nothing reclaims this if the process dies; that
// is what StaleLockAfter is for. Callers defer it.
func (l *PublishLock) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}

	// THE HEARTBEAT STOPS FIRST AND IS WAITED FOR. Releasing while a beat is in
	// flight would leave the counter behind after the key was removed, and the next
	// holder's breaker would then read a number belonging to nobody.
	if l.stop != nil {
		l.stop()
		<-l.stopped
		l.stop = nil
	}

	// Best effort, for the reason above: a counter left behind makes the next
	// breaker fall back to age, which is what it did before heartbeats existed.
	if _, rmErr := l.client.rbdCmd(ctx, false, "image-meta", "remove", l.image,
		heartbeatKey(l.cookie)); rmErr != nil {
		_ = rmErr
	}

	// THE LOCKER IS READ BACK RATHER THAN REMEMBERED. `rbd lock rm` takes both the
	// cookie AND the locker's client id, and this process does not otherwise know
	// its own client id — it is assigned by the cluster per connection.
	out, err := l.client.rbdCmd(ctx, true, "lock", "ls", l.image)
	if err != nil {
		return fmt.Errorf("ceph: could not read %s to release it: %w", l.image, err)
	}

	var held []lockEntry
	if err := json.Unmarshal(out, &held); err != nil {
		return fmt.Errorf("ceph: %s did not list the lockers of %s as json", l.client.bin, l.image)
	}

	for _, entry := range held {
		if entry.ID != l.cookie {
			continue
		}

		if _, err := l.client.rbdCmd(ctx, false, "lock", "rm", l.image,
			entry.ID, entry.Locker); err != nil {
			return fmt.Errorf("ceph: could not release %s: %w", l.image, err)
		}

		return nil
	}

	// SOMEBODY ELSE'S LOCK IS NOT RELEASED. If this cookie is no longer there, the
	// lock was broken as stale and retaken by another publisher — and removing
	// whatever is there now would hand a second writer the image while the first
	// is still writing, which is the exact corruption this whole mechanism exists
	// to prevent. Reported rather than silently accepted, because it means this
	// publisher was running for longer than anybody believed.
	return fmt.Errorf("ceph: %s is no longer held as %s; it was broken as stale and retaken "+
		"while this publish was still running", l.image, l.cookie)
}
