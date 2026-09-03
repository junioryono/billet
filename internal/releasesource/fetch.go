package releasesource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/junioryono/billet/internal/imagesource"
)

// Policy is what a source demands before a manifest may be trusted.
//
// imagesource's, DELIBERATELY, and not a second one. Both consumers verify a
// signed document against sigstore's public-good roots using the SAME embedded
// trust root, and duplicating either the root or the verifier is the two-pins
// problem: two copies of a security-critical decision that agree today and drift
// on the next rotation. If a third consumer ever appears, the verifier should be
// lifted into a package of its own — with two, the import is cheaper than the
// move and carries the same guarantee.
type Policy = imagesource.Policy

// PublishWorkflow is the workflow allowed to sign billet's releases.
//
// NAMED, NOT JUST THE REPOSITORY, for the reason imagesource.PublishWorkflow
// gives: a certificate's SAN identifies the WORKFLOW that requested it, so
// pinning only the repository accepts a signature from any other workflow in it —
// including one a pull request adds, which is a far lower bar than compromising
// the release process.
const PublishWorkflow = ".github/workflows/release.yml"

// PublishRef is the git ref releases may be signed from.
//
// A RELEASE IS BUILT FROM A MAINTAINED BRANCH, not from main. cut-release.yml
// commits the generated release surface onto `release/vX.Y` and tags that, and
// release.yml runs against the tag — so pinning `refs/heads/main` would refuse
// every release billet actually publishes. What must stay excluded is
// `refs/pull/N/head`, which is the low bar the imagesource comment names.
var PublishRefPattern = `refs/(heads/release/v[0-9]+\.[0-9]+|tags/v[0-9]+\.[0-9]+\.[0-9]+)`

// DefaultSigningIdentity is the certificate SAN a billet release manifest must
// carry.
//
// ANCHORED AT BOTH ENDS AND ESCAPED EXCEPT WHERE THE PATTERN IS THE POINT. The
// repository and the workflow are literals and go through QuoteMeta; the ref is a
// deliberate alternation, because a release is cut from whichever minor branch
// carries it.
var DefaultSigningIdentity = `^https://github\.com/` +
	regexp.QuoteMeta(DefaultRepo) + `/` + regexp.QuoteMeta(PublishWorkflow) + `@` +
	PublishRefPattern + `$`

// ErrNotFound means the source has no such artifact.
//
// DISTINGUISHED FROM EVERY OTHER FAILURE because callers act on it differently: a
// channel that has never been published is a normal state with an instruction
// attached, while a network failure is a retry.
var ErrNotFound = errors.New("releasesource: no such artifact at this source")

// Client resolves a channel to one immutable release and proves what it fetches.
type Client struct {
	// HTTP is the transport. Nil means a bounded default.
	HTTP *http.Client

	// Repo is the repository releases come from.
	Repo string

	// Now is the clock the channel's expiry is judged against. Nil means
	// time.Now — a parameter because expiry is the behaviour worth testing and a
	// test that waits ten days is a test nobody runs.
	Now func() time.Time

	// verify is the signature check. Nil means the real one; a test replaces it
	// so it can exercise the surrounding refusals without minting certificates.
	verify func(document, signature []byte, p Policy) error
}

func (c *Client) repo() string {
	if c.Repo == "" {
		return DefaultRepo
	}

	return c.Repo
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}

	return c.Now()
}

func (c *Client) verifier() func([]byte, []byte, Policy) error {
	if c.verify == nil {
		return imagesource.VerifySignature
	}

	return c.verify
}

// defaultHTTP is used when a caller supplies none.
//
// NO CLIENT-LEVEL TIMEOUT, for the reason imagesource's default gives:
// http.Client.Timeout covers the whole exchange including the body read, so a
// value large enough for a release archive on a slow link is far too large to
// bound a channel fetch, and one sized for the channel severs every download
// partway. The per-request deadline is the caller's.
func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
	}
}

// Resolve turns a channel name into the immutable release it currently points at.
//
// ONE ANSWER, RECORDED BY THE CALLER. A rollout persists what this returns and
// never asks again: a channel that advances mid-rollout must not retarget work
// already underway, which is why a channel is a pointer rather than a subscription.
func (c *Client) Resolve(ctx context.Context, channel string, p Policy,
) (*ChannelStatement, error) {
	if !KnownChannel(channel) {
		return nil, fmt.Errorf("releasesource: %q is not a channel billet publishes", channel)
	}

	verify := c.verifier()

	var lastErr error

	for attempt := 1; attempt <= channelPairAttempts; attempt++ {
		body, err := c.get(ctx, ChannelURL(c.repo(), channel), MaxChannelBytes)
		if err != nil {
			return nil, fmt.Errorf("releasesource: could not read the %s channel: %w",
				channel, err)
		}

		var signature []byte

		if p.Required {
			signature, err = c.get(ctx, ChannelBundleURL(c.repo(), channel),
				imagesource.MaxBundleBytes)
			if err != nil {
				return nil, fmt.Errorf("releasesource: could not authenticate the %s "+
					"channel: %w", channel, err)
			}
		}

		// THE PAIR IS REFETCHED ONCE ON A MISMATCH, because the statement and its
		// signature advance as one commit and arrive as two requests: a CDN can
		// serve them from different generations while the branch moves. Exactly
		// once — retrying forever turns a genuinely bad signature into a hang.
		if lastErr = verify(body, signature, p); lastErr == nil {
			return ParseChannel(body, channel, c.now())
		}
	}

	return nil, fmt.Errorf("releasesource: the %s channel is not authentic: %w",
		channel, lastErr)
}

// Manifest fetches one release's manifest, proves it, and validates it.
//
// THE SIGNATURE IS CHECKED OVER THE BYTES THAT ARRIVED, before anything is parsed
// out of them. Verifying a re-serialised manifest would verify a document this
// program produced rather than the one that was signed, and the two can differ in
// whitespace, key order, or any field a future reader drops.
//
// expectDigest COMES FROM THE CHANNEL and is checked here. A tag names a release;
// a digest names its contents. GitHub release immutability should make the two
// equivalent, which is a reason to check rather than a reason not to — an
// assumption billet can verify for free is one it should verify.
//
// THE DIGEST IS RETURNED because every caller needs to persist it, and computing
// it a second time from a re-serialised manifest would hash a document this
// program produced rather than the one that was published. An exact version pin
// has no channel to take it from, and a rollout still has to record WHICH bytes
// it decided on.
func (c *Client) Manifest(ctx context.Context, tag, expectDigest string, p Policy,
) (*Manifest, string, error) {
	body, err := c.get(ctx, ManifestURL(c.repo(), tag), MaxManifestBytes)
	if err != nil {
		return nil, "", err
	}

	got := hex.EncodeToString(digest(body))

	if expectDigest != "" && got != expectDigest {
		return nil, "", fmt.Errorf("releasesource: the manifest for %s hashes to %s and the "+
			"channel named %s. Either the release was republished, which immutability "+
			"is supposed to prevent, or these bytes are not the ones the channel "+
			"vouched for", tag, got, expectDigest)
	}

	if p.Required {
		signature, err := c.get(ctx, BundleURL(c.repo(), tag), imagesource.MaxBundleBytes)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, "", fmt.Errorf("releasesource: %s requires a signature and %s "+
					"publishes none: %w", c.repo(), tag, err)
			}

			return nil, "", err
		}

		if err := c.verifier()(body, signature, p); err != nil {
			return nil, "", err
		}
	}

	m, err := ParseManifest(body)
	if err != nil {
		return nil, "", err
	}

	// THE MANIFEST NAMES THE RELEASE IT IS IN. Without this, a correctly signed
	// manifest for one release served at another release's address is accepted
	// whole — every signature valid, every digest correct, and the wrong version
	// installed under the tag an operator asked for.
	if m.Version != tag {
		return nil, "", fmt.Errorf("releasesource: the manifest published under %s says it is "+
			"%s; refusing a release that does not agree with its own tag", tag, m.Version)
	}

	return m, got, nil
}

// Download fetches one artifact into dir and proves it against the manifest.
//
// THE DIGEST IS CHECKED AS THE BYTES ARRIVE and the file is only named once it
// passes, so nothing downstream can ever open a partially-written or unverified
// artifact. The size is enforced too: a digest is only checkable after the last
// byte, so it cannot stop a response that never ends.
func (c *Client) Download(ctx context.Context, tag string, a *Artifact, dir string,
) (string, error) {
	body, err := c.open(ctx, ArtifactURL(c.repo(), tag, a.Name))
	if err != nil {
		return "", err
	}

	defer func() { _ = body.Close() }()

	// STAGED UNDER A TEMPORARY NAME. A reader that finds the final name finds a
	// verified file or nothing; an interrupted download that had already claimed
	// the real name would be indistinguishable from a complete one.
	staged, err := os.CreateTemp(dir, "."+a.Name+".*")
	if err != nil {
		return "", fmt.Errorf("releasesource: stage %s: %w", a.Name, err)
	}

	staging := staged.Name()

	defer func() {
		_ = staged.Close()
		_ = os.Remove(staging)
	}()

	hash := sha256.New()

	// BOUNDED AT ONE BYTE PAST THE DECLARED SIZE, so a response longer than the
	// manifest promised is caught as a mismatch rather than written to the disk in
	// full and only then rejected.
	written, err := io.Copy(io.MultiWriter(staged, hash), io.LimitReader(body, a.Size+1))
	if err != nil {
		return "", fmt.Errorf("releasesource: download %s: %w", a.Name, err)
	}

	if written != a.Size {
		return "", fmt.Errorf("releasesource: %s is %d bytes and the manifest says %d",
			a.Name, written, a.Size)
	}

	if got := hex.EncodeToString(hash.Sum(nil)); got != a.SHA256 {
		return "", fmt.Errorf("releasesource: %s hashes to %s and the manifest says %s; "+
			"these are not the bytes that were published", a.Name, got, a.SHA256)
	}

	if err := staged.Sync(); err != nil {
		return "", fmt.Errorf("releasesource: flush %s: %w", a.Name, err)
	}

	if err := staged.Close(); err != nil {
		return "", fmt.Errorf("releasesource: close %s: %w", a.Name, err)
	}

	final := filepath.Join(dir, a.Name)
	if err := os.Rename(staging, final); err != nil {
		return "", fmt.Errorf("releasesource: publish %s: %w", a.Name, err)
	}

	return final, nil
}

// get retrieves a small document whole, bounded.
func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, err := c.open(ctx, url)
	if err != nil {
		return nil, err
	}

	defer func() { _ = body.Close() }()

	// LIMIT+1 SO AN OVERSIZE DOCUMENT IS AN ERROR rather than a silent truncation.
	// A manifest cut short at the bound would fail to parse, which reads as a
	// corrupt release rather than as a document somebody made too large.
	read, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("releasesource: read %s: %w", url, err)
	}

	if int64(len(read)) > limit {
		return nil, fmt.Errorf("releasesource: %s is larger than the %d-byte bound", url, limit)
	}

	return read, nil
}

func (c *Client) open(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("releasesource: request %s: %w", url, err)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("releasesource: fetch %s: %w", url, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("%w: %s", ErrNotFound, url)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("releasesource: %s returned %s", url, resp.Status)
	}

	return resp.Body, nil
}

func digest(body []byte) []byte {
	sum := sha256.Sum256(body)

	return sum[:]
}

// PolicyForRelease decides what verification billet's own releases demand.
//
// A MISSING POLICY IS AN ERROR RATHER THAN A SKIP, which is imagesource's rule and
// applies here with more force. The manifest is the only thing that makes every
// digest in it mean anything: an attacker who can serve one names digests of bytes
// they chose, and every check downstream then passes against those bytes. Here the
// bytes in question replace a running control plane.
//
// THE WAIVER IS EXPLICIT AND IT WINS. An air-gapped deployment mirroring its own
// releases has a real reason; the point is that skipping verification is an act
// somebody performed rather than what happens when nothing is configured.
func PolicyForRelease(skip bool) (Policy, error) {
	if skip {
		return Policy{Required: false}, nil
	}

	return Policy{
		Required: true,
		Identity: DefaultSigningIdentity,
		Issuer:   imagesource.GitHubOIDCIssuer,
	}, nil
}
