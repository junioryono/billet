package releasesource

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DefaultRepo is the repository billet's releases come from.
const DefaultRepo = "junioryono/billet"

// The named channels billet publishes.
//
// A CHANNEL IS A POINTER TO ONE IMMUTABLE MANIFEST, never a range or a policy. It
// is the only thing in the release contract that moves, which is exactly why a
// rollout resolves it once, records the digest it resolved to, and never consults
// it again — a channel advancing mid-rollout must not retarget work already
// underway.
const (
	ChannelStable    = "stable"
	ChannelCandidate = "candidate"
)

func KnownChannel(name string) bool {
	return name == ChannelStable || name == ChannelCandidate
}

// channelBranch carries the signed pointers, served through
// raw.githubusercontent.com.
//
// NOT THE RELEASES API, and the reason is measured rather than aesthetic: the
// anonymous REST limit is shared across every unauthenticated caller from one
// address, so a fleet checking a channel on a cadence spends a budget it does not
// control. The guest-image channel already resolves this way for the same reason;
// this follows it rather than inventing a second mechanism.
//
// THE BRANCH IS ONLY TRANSPORT. Its contents are signed, carry an expiry, and
// assert that the release they name is immutable — so an unsigned edit to the
// branch, or an old signed file replayed onto it, is refused by the reader rather
// than by the branch's protection.
const channelBranch = "release-channel"

const (
	// ChannelSchema is the layout of a channel statement.
	ChannelSchema = 1

	// maxChannelLifetime bounds how long one signed pointer stays valid.
	//
	// A REPLAY DEFENCE, not a freshness preference. Without an expiry, anybody who
	// can serve the branch's bytes — a stale CDN edge, a proxy, a mirror — can
	// hold a fleet on an old release indefinitely and there is nothing in the file
	// to notice. Ten days is long enough that an ordinary release cadence never
	// expires one in normal use, and short enough that a replay has to be
	// deliberate and recent.
	maxChannelLifetime = 10 * 24 * time.Hour

	// channelClockSkew tolerates a host whose clock is a little ahead.
	channelClockSkew = 5 * time.Minute

	// MaxChannelBytes bounds the pointer document.
	MaxChannelBytes = 4 << 10

	// channelPairAttempts is how many times a mismatched statement/signature pair
	// is refetched before it is called inauthentic.
	//
	// THE PAIR CAN LEGITIMATELY DISAGREE FOR A MOMENT. Two files advance on the
	// branch as one commit, but they are fetched as two requests and can come from
	// different CDN generations while it lands — so a signature mismatch is
	// refetched once before it is treated as an attack. Exactly once: retrying
	// forever would turn a genuinely bad signature into a hang.
	channelPairAttempts = 2
)

// ChannelStatement is one signed pointer from a channel to an immutable release.
type ChannelStatement struct {
	Schema  int    `json:"schema"`
	Channel string `json:"channel"`

	// Tag is the release this channel currently names.
	Tag string `json:"tag"`

	// ManifestSHA256 is the digest of that release's manifest.
	//
	// THE THING A ROLLOUT PERSISTS. A tag names a release and a digest names its
	// CONTENTS, and only the second survives somebody moving a tag. GitHub release
	// immutability means that should be impossible, which is a reason to record
	// the digest rather than a reason not to: an assumption billet can check for
	// free is one it should check.
	ManifestSHA256 string `json:"manifest_sha256"`

	PublishedAt time.Time `json:"published_at"`
	ExpiresAt   time.Time `json:"expires_at"`

	// ReleaseImmutable is the publisher asserting it PROVED the release immutable
	// before advancing the channel.
	//
	// CARRIED AND CHECKED rather than assumed from the repository setting. GitHub's
	// release immutability applies only to releases created after it was enabled,
	// so "the repository is protected now" says nothing about a given release. The
	// workflow proves it per release and signs the assertion; a reader that
	// accepted an absent or false value would be trusting a property nobody
	// checked.
	ReleaseImmutable bool `json:"release_immutable"`
}

// ChannelURL is where a channel's signed statement lives.
func ChannelURL(repo, channel string) string {
	return "https://raw.githubusercontent.com/" + repo + "/" + channelBranch + "/" +
		channel + ".json"
}

// ChannelBundleURL is where that statement's signature lives.
func ChannelBundleURL(repo, channel string) string {
	return "https://raw.githubusercontent.com/" + repo + "/" + channelBranch + "/" +
		channel + ".sigstore.json"
}

// ManifestURL is where one release's manifest lives.
func ManifestURL(repo, tag string) string {
	return "https://github.com/" + repo + "/releases/download/" + tag + "/" + ManifestName
}

// BundleURL is where that manifest's signature lives.
func BundleURL(repo, tag string) string {
	return "https://github.com/" + repo + "/releases/download/" + tag + "/" + BundleName
}

// ArtifactURL is where one published artifact lives.
func ArtifactURL(repo, tag, name string) string {
	return "https://github.com/" + repo + "/releases/download/" + tag + "/" + name
}

// The names the release publishes the manifest and its signature under.
const (
	ManifestName = "release-manifest.json"
	BundleName   = "release-manifest.sigstore.json"
)

// ParseChannel decodes and validates a channel statement.
//
// EVERY REFUSAL HERE FAILS CLOSED — an unreadable, expired, replayed, or
// unattested pointer resolves to nothing rather than to a default, because the
// only thing worse than not knowing which release is current is guessing.
//
// `now` IS A PARAMETER because expiry is the interesting behaviour and a test
// that has to wait ten days is a test nobody runs.
func ParseChannel(body []byte, want string, now time.Time) (*ChannelStatement, error) {
	if len(body) > MaxChannelBytes {
		return nil, fmt.Errorf("releasesource: the %s channel statement is %d bytes, above "+
			"the %d-byte bound", want, len(body), MaxChannelBytes)
	}

	// STRICT, AND CLOSED AT BOTH ENDS. json.Unmarshal ignores unknown fields, so a
	// statement carrying a constraint added after this build shipped was followed
	// as though the constraint were absent — and it also accepts trailing content
	// after the first object, so a second document appended to a signed one was
	// silently discarded. The signature covers the whole body either way, which is
	// exactly why the reader must not disagree with the signer about where the
	// document ends.
	var c ChannelStatement
	if err := decodeExactly(body, &c); err != nil {
		return nil, fmt.Errorf("releasesource: the %s channel statement is invalid: %w",
			want, err)
	}

	if c.Schema != ChannelSchema {
		return nil, fmt.Errorf("releasesource: the %s channel uses schema %d, want %d",
			want, c.Schema, ChannelSchema)
	}

	// THE STATEMENT NAMES ITS OWN CHANNEL, and it is checked against the one that
	// was asked for. Without this, serving candidate.json at stable.json's URL
	// moves a whole fleet onto the candidate channel with a signature that
	// verifies perfectly — every byte authentic, and the wrong document.
	if c.Channel != want {
		return nil, fmt.Errorf("releasesource: a statement for the %q channel was served at "+
			"the %q channel's address; refusing to follow a pointer that names another "+
			"channel", c.Channel, want)
	}

	if !versionPattern.MatchString(c.Tag) {
		return nil, fmt.Errorf("releasesource: the %s channel names %q, which is not a billet "+
			"release tag", want, c.Tag)
	}

	if !digestPattern.MatchString(c.ManifestSHA256) {
		return nil, fmt.Errorf("releasesource: the %s channel carries no usable manifest "+
			"digest (%q)", want, c.ManifestSHA256)
	}

	if !c.ReleaseImmutable {
		return nil, fmt.Errorf("releasesource: the %s channel does not attest that %s is "+
			"immutable. Immutability applies only to releases created after it was enabled "+
			"on the repository, so this has to be proved per release rather than assumed",
			want, c.Tag)
	}

	if c.PublishedAt.IsZero() || c.ExpiresAt.IsZero() ||
		!c.ExpiresAt.After(c.PublishedAt) ||
		c.ExpiresAt.Sub(c.PublishedAt) > maxChannelLifetime {
		return nil, fmt.Errorf("releasesource: the %s channel has an invalid validity window "+
			"(%s to %s); a pointer with no bounded lifetime cannot be told from a replay",
			want, c.PublishedAt.Format(time.RFC3339), c.ExpiresAt.Format(time.RFC3339))
	}

	if c.PublishedAt.After(now.Add(channelClockSkew)) {
		return nil, fmt.Errorf("releasesource: the %s channel claims a future publication "+
			"time %s", want, c.PublishedAt.Format(time.RFC3339))
	}

	if !now.Before(c.ExpiresAt) {
		return nil, fmt.Errorf("releasesource: the %s channel expired at %s; refusing a "+
			"replayed pointer. If this deployment's clock is correct, the channel has not "+
			"been republished and billet will not guess which release is current",
			want, c.ExpiresAt.Format(time.RFC3339))
	}

	return &c, nil
}

// Marshal renders a channel statement the way the publisher writes it.
//
// HERE RATHER THAN IN THE WORKFLOW, so the writer and the reader agree by
// construction. A statement assembled by a shell heredoc is a second
// implementation of this schema, and the two drift on the first field anybody
// adds — which is the failure the vendored toolset declaration exists to prevent
// one directory over.
func (c *ChannelStatement) Marshal() ([]byte, error) {
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("releasesource: render the %s channel statement: %w",
			c.Channel, err)
	}

	return append(body, '\n'), nil
}

// NewChannelStatement builds one pointer for a publisher to sign.
func NewChannelStatement(channel, tag, manifestDigest string, now time.Time,
) (*ChannelStatement, error) {
	if !KnownChannel(channel) {
		return nil, fmt.Errorf("releasesource: %q is not a channel billet publishes", channel)
	}

	c := &ChannelStatement{
		Schema:           ChannelSchema,
		Channel:          channel,
		Tag:              strings.TrimSpace(tag),
		ManifestSHA256:   strings.TrimSpace(manifestDigest),
		PublishedAt:      now.UTC(),
		ExpiresAt:        now.UTC().Add(maxChannelLifetime),
		ReleaseImmutable: true,
	}

	// VALIDATED THROUGH THE READER'S OWN RULES, so a publisher cannot emit a
	// statement its own fleet will refuse. The alternative is discovering it when
	// every node stops resolving the channel.
	body, err := c.Marshal()
	if err != nil {
		return nil, err
	}

	if _, err := ParseChannel(body, channel, now); err != nil {
		return nil, err
	}

	return c, nil
}
