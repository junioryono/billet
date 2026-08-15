package ceph

import (
	"context"
	"encoding/json"
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

// PublishLock is a held cluster-wide publish lock.
type PublishLock struct {
	client *Client
	cookie string
	image  string
}

// TakePublishLock claims the right to write the golden image.
func (c *Client) TakePublishLock(ctx context.Context, now time.Time) (*PublishLock, error) {
	image := c.cfg.ImagePool + "/" + LockImageName

	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown"
	}

	// THE TRAILING UNIX TIME IS LOAD-BEARING, not decoration: it is how both this
	// package and the build script decide whether a held lock is stale.
	cookie := fmt.Sprintf("billet-import-%s-%d-%d", host, os.Getpid(), now.UTC().Unix())

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

	if _, err := c.rbdCmd(ctx, false, "lock", "add", image, cookie); err == nil {
		return &PublishLock{client: c, cookie: cookie, image: image}, nil
	}

	// SOMEBODY HOLDS IT. Whether that is a live build or a corpse is the only
	// question left, and it is answered from the cookie rather than from anything
	// about the process, because there is nothing here that can see the process.
	holder, age, found, err := c.heldLock(ctx, image, now)
	if err != nil {
		return nil, err
	}

	if !found {
		// Taken and released between the two calls. Reporting the race honestly
		// beats retrying in a loop nobody bounded.
		return nil, fmt.Errorf("ceph: the publish lock on %s could not be taken and is not held; "+
			"another publisher is contending for it right now", image)
	}

	if age < StaleLockAfter {
		return nil, fmt.Errorf("ceph: %s is held by %s and has been for %s; another publisher is "+
			"writing the golden image, so this one is standing down rather than writing into "+
			"the same image", image, holder.ID, age.Round(time.Second))
	}

	// BROKEN ONLY PAST THE BOUND. Breaking a lock whose holder is alive puts two
	// writers on one image, which is the corruption this exists to prevent — so
	// the bound is deliberately far past any real run rather than tuned close to
	// one.
	if _, err := c.rbdCmd(ctx, false, "lock", "rm", image, holder.ID, holder.Locker); err != nil {
		return nil, fmt.Errorf("ceph: %s has been held by %s for %s, longer than any publish can "+
			"run, and it could not be broken: %w", image, holder.ID, age.Round(time.Second), err)
	}

	if _, err := c.rbdCmd(ctx, false, "lock", "add", image, cookie); err != nil {
		return nil, fmt.Errorf("ceph: %s was broken after %s but could not then be taken; "+
			"another publisher took it first: %w", image, age.Round(time.Second), err)
	}

	return &PublishLock{client: c, cookie: cookie, image: image}, nil
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
) (lockEntry, time.Duration, bool, error) {
	out, err := c.rbdCmd(ctx, true, "lock", "ls", image)
	if err != nil {
		return lockEntry{}, 0, false, fmt.Errorf("ceph: could not read who holds %s: %w", image, err)
	}

	var held []lockEntry
	if err := json.Unmarshal(out, &held); err != nil {
		return lockEntry{}, 0, false, fmt.Errorf("ceph: %s did not list the lockers of %s as json; "+
			"is it the rbd command?", c.bin, image)
	}

	if len(held) == 0 {
		return lockEntry{}, 0, false, nil
	}

	match := cookieAge.FindStringSubmatch(held[0].ID)
	if match == nil {
		// A COOKIE THIS DID NOT WRITE. Something took the lock that is not billet, or
		// is a billet old enough to predate the convention. Either way its age
		// cannot be established, and a lock of unknown age is never broken: the
		// alternative is breaking a live writer's lock because its name was
		// unfamiliar.
		return held[0], 0, true, nil
	}

	seconds, parseErr := strconv.ParseInt(match[1], 10, 64)
	if parseErr != nil {
		// AN AGE OF ZERO, NOT AN ERROR. A cookie whose trailing digits do not fit an
		// int64 was not written by anything this understands, and the caller treats
		// age zero as "too recent to break" -- which is the safe reading. Reporting
		// an error instead would refuse the publish outright rather than merely
		// declining to break somebody else's lock.
		return held[0], 0, true, nil //nolint:nilerr // an unreadable timestamp is an age, not a failure
	}

	age := now.UTC().Sub(time.Unix(seconds, 0).UTC())

	// A COOKIE FROM THE FUTURE IS NOT STALE. Clocks disagree across a cluster, and
	// a negative age passed through the comparison below unchanged would read as
	// "not yet stale" — which is the safe answer, but only by accident. Clamping
	// says so on purpose.
	if age < 0 {
		age = 0
	}

	return held[0], age, true, nil
}

// Release gives the publish lock back.
//
// NOT AUTOMATIC, AND NOT A LEASE. Nothing reclaims this if the process dies; that
// is what StaleLockAfter is for. Callers defer it.
func (l *PublishLock) Release(ctx context.Context) error {
	if l == nil {
		return nil
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
