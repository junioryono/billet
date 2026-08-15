package ceph

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var cookieAt = time.Date(2026, 8, 15, 4, 17, 9, 0, time.UTC)

// A COOKIE IS AN IDENTITY, AND HOST+PID+SECOND IS NOT A UNIQUE ONE.
//
// Release finds its own lock by matching the cookie. Two publishers that generate
// the same string -- same hostname on cloned images, the same pid in two pid
// namespaces, within the same second -- would each find the OTHER's locker id and
// be able to remove the other's lock, which is the one thing a lock must not
// permit.
func TestPublishCookieIsUniquePerCall(t *testing.T) {
	seen := map[string]bool{}

	for i := range 200 {
		// THE SAME INSTANT EVERY TIME, so the timestamp cannot be what distinguishes
		// them. On one host, in one process, this is the worst case: everything except
		// the nonce is identical.
		cookie := publishCookie(cookieAt)

		if seen[cookie] {
			t.Fatalf("publishCookie produced %q twice in %d calls at one instant; two "+
				"publishers could each remove the other's lock", cookie, i+1)
		}

		seen[cookie] = true
	}
}

// THE TRAILING UNIX TIME IS THE INTEROP SURFACE. build-guest-image.sh ages out a
// stale lock by reading it with `-(?<t>[0-9]+)$`, so a cookie this writes must end
// the same way or a lock leaked by the Go side could never be reclaimed by the
// shell side -- and only under the failure that mechanism exists to handle.
func TestPublishCookieEndsWithTheTimestampTheShellParses(t *testing.T) {
	cookie := publishCookie(cookieAt)

	// COPIED CHARACTER FOR CHARACTER from build-guest-image.sh's jq expression,
	// `capture("-(?<t>[0-9]+)$")`. Written as `\d` it would be an equivalent regex
	// and a worse test: the point is that this is the shell's pattern, not one that
	// happens to agree with it today.
	shellRegex := regexp.MustCompile(`-([0-9]+)$`) //nolint:gocritic // mirrors the shell verbatim

	match := shellRegex.FindStringSubmatch(cookie)
	if match == nil {
		t.Fatalf("the shell's stale-lock regex does not match %q, so a lock leaked by "+
			"this side could never be aged out by a shell build", cookie)
	}

	// DERIVED, NOT HARDCODED. The first version of this pinned a literal that was
	// simply the wrong number, so it failed against correct code and read as a bug
	// in the cookie.
	want := strconv.FormatInt(cookieAt.Unix(), 10)

	if match[1] != want {
		t.Errorf("the trailing timestamp is %q, want %q", match[1], want)
	}

	// AND THIS PACKAGE'S OWN READER AGREES WITH THE SHELL'S.
	if cookieAge.FindStringSubmatch(cookie) == nil {
		t.Error("this package cannot parse the cookie it just wrote")
	}
}

// THE COOKIE IS ALSO A DIAGNOSTIC. An operator staring at a held lock needs to know
// which machine to go and look at.
func TestPublishCookieNamesTheHostAndTheTool(t *testing.T) {
	cookie := publishCookie(cookieAt)

	if !strings.HasPrefix(cookie, "billet-import-") {
		t.Errorf("%q does not say what took it; the shell's cookies say billet-build-", cookie)
	}
}
