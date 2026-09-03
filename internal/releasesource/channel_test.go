package releasesource

import (
	"strings"
	"testing"
	"time"
)

var channelNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func goodChannel() *ChannelStatement {
	return &ChannelStatement{
		Schema:           ChannelSchema,
		Channel:          ChannelStable,
		Tag:              "v0.4.0",
		ManifestSHA256:   strings.Repeat("b", 64),
		PublishedAt:      channelNow.Add(-time.Hour),
		ExpiresAt:        channelNow.Add(9 * 24 * time.Hour),
		ReleaseImmutable: true,
	}
}

func render(t *testing.T, c *ChannelStatement) []byte {
	t.Helper()

	body, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	return body
}

func TestAGoodChannelStatementParses(t *testing.T) {
	t.Parallel()

	c, err := ParseChannel(render(t, goodChannel()), ChannelStable, channelNow)
	if err != nil {
		t.Fatalf("ParseChannel: %v", err)
	}

	if c.Tag != "v0.4.0" {
		t.Errorf("the channel resolved to %q", c.Tag)
	}
}

func TestChannelRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*ChannelStatement)
		asking string
		at     time.Time
		want   string
	}{{
		name:   "an unknown schema",
		mutate: func(c *ChannelStatement) { c.Schema = ChannelSchema + 1 },
		want:   "uses schema",
	}, {
		// SERVING candidate.json AT stable.json's ADDRESS moves a whole fleet onto
		// the candidate channel with a signature that verifies perfectly: every
		// byte authentic, and the wrong document.
		name:   "a statement for another channel",
		mutate: func(c *ChannelStatement) { c.Channel = ChannelCandidate },
		want:   "names another channel",
	}, {
		name:   "a tag that is not a release",
		mutate: func(c *ChannelStatement) { c.Tag = "latest" },
		want:   "not a billet release tag",
	}, {
		name:   "no manifest digest",
		mutate: func(c *ChannelStatement) { c.ManifestSHA256 = "" },
		want:   "no usable manifest digest",
	}, {
		// IMMUTABILITY APPLIES ONLY TO RELEASES CREATED AFTER IT WAS ENABLED, so
		// "the repository is protected now" says nothing about a given release.
		name:   "no immutability assertion",
		mutate: func(c *ChannelStatement) { c.ReleaseImmutable = false },
		want:   "does not attest",
	}, {
		// AN UNBOUNDED POINTER CANNOT BE TOLD FROM A REPLAY. Anybody who can serve
		// the branch's bytes could otherwise hold a fleet on an old release
		// indefinitely with nothing in the file to notice.
		name:   "no expiry",
		mutate: func(c *ChannelStatement) { c.ExpiresAt = time.Time{} },
		want:   "invalid validity window",
	}, {
		name:   "a lifetime beyond the bound",
		mutate: func(c *ChannelStatement) { c.ExpiresAt = c.PublishedAt.Add(90 * 24 * time.Hour) },
		want:   "invalid validity window",
	}, {
		name:   "an expiry before its publication",
		mutate: func(c *ChannelStatement) { c.ExpiresAt = c.PublishedAt.Add(-time.Hour) },
		want:   "invalid validity window",
	}, {
		name:   "a future publication time",
		mutate: func(c *ChannelStatement) { c.PublishedAt = channelNow.Add(time.Hour) },
		want:   "future publication time",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := goodChannel()
			tc.mutate(c)

			// Marshal directly rather than through NewChannelStatement, which
			// validates: these are documents a publisher could not produce and a
			// reader must still refuse.
			body := render(t, c)

			asking := tc.asking
			if asking == "" {
				asking = ChannelStable
			}

			at := tc.at
			if at.IsZero() {
				at = channelNow
			}

			if _, err := ParseChannel(body, asking, at); err == nil {
				t.Fatalf("this statement was accepted: %+v", c)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q:\n%v", tc.want, err)
			}
		})
	}
}

// AN EXPIRED POINTER IS REFUSED RATHER THAN FOLLOWED.
//
// The refusal has to say that billet will not guess, because the operator's next
// question is why their fleet stopped updating and the answer is that nobody
// republished the channel — not that anything is broken.
func TestAnExpiredChannelIsRefusedAsAReplay(t *testing.T) {
	t.Parallel()

	body := render(t, goodChannel())

	_, err := ParseChannel(body, ChannelStable, channelNow.Add(30*24*time.Hour))
	if err == nil {
		t.Fatal("an expired channel statement was followed")
	}

	if !strings.Contains(err.Error(), "expired") ||
		!strings.Contains(err.Error(), "replayed") {
		t.Errorf("the refusal does not name the replay it prevents: %v", err)
	}
}

// A PUBLISHER CANNOT EMIT A STATEMENT ITS OWN FLEET WOULD REFUSE. The
// alternative is discovering it when every node stops resolving the channel.
func TestANewStatementSatisfiesTheReader(t *testing.T) {
	t.Parallel()

	c, err := NewChannelStatement(ChannelStable, "v0.4.0", strings.Repeat("c", 64), channelNow)
	if err != nil {
		t.Fatalf("NewChannelStatement: %v", err)
	}

	if _, err := ParseChannel(render(t, c), ChannelStable, channelNow); err != nil {
		t.Errorf("a freshly published statement was refused by the reader: %v", err)
	}

	if !c.ExpiresAt.After(channelNow) {
		t.Errorf("a fresh statement expires at %s, which is not in the future", c.ExpiresAt)
	}
}

func TestOnlyPublishedChannelsAreAccepted(t *testing.T) {
	t.Parallel()

	if _, err := NewChannelStatement("nightly", "v0.4.0", strings.Repeat("c", 64),
		channelNow); err == nil {
		t.Error("a channel billet does not publish was accepted")
	}

	for _, name := range []string{ChannelStable, ChannelCandidate} {
		if !KnownChannel(name) {
			t.Errorf("%s is not recognised as a channel", name)
		}
	}
}

// A SIGNED DOCUMENT IS CLOSED AT BOTH ENDS.
//
// json.Unmarshal ignores unknown fields, so a statement carrying a constraint
// added after this build shipped was followed as though the constraint were
// absent. And it accepts trailing content after the first object, so a second
// document appended to a signed one was silently discarded — while the signature
// covered all of it. A reader that stopped at the first object would be verifying
// bytes it never looked at.
func TestAChannelStatementRefusesUnknownAndTrailingContent(t *testing.T) {
	t.Parallel()

	body := render(t, goodChannel())

	augmented := strings.Replace(string(body), "{",
		`{"requires_attestation":true,`, 1)
	if _, err := ParseChannel([]byte(augmented), ChannelStable, channelNow); err == nil {
		t.Error("a channel statement carrying an unknown constraint was followed as " +
			"though the constraint were absent")
	}

	trailing := append(append([]byte{}, body...), []byte(`{"schema":1}`)...)
	if _, err := ParseChannel(trailing, ChannelStable, channelNow); err == nil {
		t.Error("a channel statement with a second document appended was accepted; the " +
			"signature covers those bytes and the reader did not")
	}
}
