package imagesource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// MaxBundleBytes bounds a signature bundle.
//
// A real one is around ten kilobytes -- certificate, signature, and a transparency
// log entry. The bound is generous against that and exists for the same reason the
// manifest's does: it is fetched over the network before anything about the far end
// has been proven, and an unbounded read of an untrusted stream is how a fetch
// becomes a memory exhaustion.
const MaxBundleBytes = 512 << 10

// MaxChannelBytes bounds the first-party release pointer.
const MaxChannelBytes = 4 << 10

const (
	channelSchema       = 1
	channelPairAttempts = 2
	maxChannelLifetime  = 10 * 24 * time.Hour
	channelClockSkew    = 5 * time.Minute
)

// ErrNotFound is returned when the source has no such artifact.
//
// DISTINGUISHED FROM EVERY OTHER FAILURE because the callers act on it
// differently: a deployment that has never published an image is a normal state
// with an instruction attached, while a network failure is a retry.
var ErrNotFound = errors.New("imagesource: no such artifact at this source")

// Client fetches artifacts from a source and proves they are what was
// published before letting anything else see them.
type Client struct {
	// HTTP is the transport. Nil means a bounded default.
	HTTP *http.Client

	// Source is where artifacts are fetched from.
	Source Source

	verifyChannel func([]byte, []byte, Policy) error
}

// defaultHTTP is used when a caller supplies none.
//
// NO CLIENT-LEVEL TIMEOUT, DELIBERATELY. http.Client.Timeout covers the whole
// exchange INCLUDING the body read, so a value large enough for four hundred
// megabytes on a slow link is far too large to bound a manifest fetch, and a
// value sized for the manifest would sever every image download partway. The
// bound belongs on the context, which each caller sets to suit what it is
// fetching. What is bounded here is establishing the connection, which is the
// part that hangs when a host is blackholed.
func defaultHTTP() *http.Client {
	return &http.Client{
		// A REDIRECT MUST NOT DOWNGRADE THE TRANSPORT. Go's default client happily
		// follows https to http, so a source verified as https at configuration
		// time can be moved onto plaintext by whoever answers the first request --
		// and the whole reason ParseSource insists on https is that plaintext lets
		// somebody pin a node to an older-but-genuine manifest indefinitely, which
		// passes every digest check and walks the fleet into github's thirty-day
		// expiry.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("imagesource: stopped after 10 redirects")
			}

			if req.URL.Scheme != "https" {
				return fmt.Errorf("imagesource: %s redirected to %s, which is not https",
					via[len(via)-1].URL.Host, req.URL.Scheme)
			}

			return nil
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 5 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}

	return defaultHTTP()
}

// Resolve discovers the immutable dated release behind the default channel.
// Custom and already-resolved sources are unchanged.
func (c *Client) Resolve(ctx context.Context) error {
	if c.Source.channelURL == "" {
		return nil
	}

	policy := Policy{}
	var err error

	if c.Source.channelPolicy != nil {
		policy = *c.Source.channelPolicy
	} else {
		policy, err = PolicyFor(c.Source, "", "", false)
		if err != nil {
			return err
		}
	}

	var body []byte
	verify := VerifySignature

	if c.verifyChannel != nil {
		verify = c.verifyChannel
	}

	for attempt := 1; attempt <= channelPairAttempts; attempt++ {
		body, err = c.get(ctx, c.Source.channelURL, MaxChannelBytes)
		if err != nil {
			return fmt.Errorf("imagesource: could not discover the current guest image: %w", err)
		}

		var signature []byte

		if policy.Required {
			signature, err = c.get(ctx, c.Source.channelBundleURL, MaxBundleBytes)
			if err != nil {
				return fmt.Errorf("imagesource: could not authenticate the guest-image channel: %w", err)
			}
		}
		if err = verify(body, signature, policy); err == nil {
			break
		}
		if attempt == channelPairAttempts {
			return fmt.Errorf("imagesource: the guest-image channel is not authentic: %w", err)
		}
	}

	var channel struct {
		Schema           int       `json:"schema"`
		Tag              string    `json:"tag"`
		PublishedAt      time.Time `json:"published_at"`
		ExpiresAt        time.Time `json:"expires_at"`
		ReleaseImmutable bool      `json:"release_immutable"`
	}
	if err := json.Unmarshal(body, &channel); err != nil {
		return fmt.Errorf("imagesource: the guest-image channel is invalid: %w", err)
	}
	if channel.Schema != channelSchema {
		return fmt.Errorf("imagesource: the guest-image channel uses schema %d, want %d",
			channel.Schema, channelSchema)
	}
	if !guestReleasePattern.MatchString(channel.Tag) {
		return fmt.Errorf("imagesource: the guest-image channel names invalid release %q", channel.Tag)
	}
	if !channel.ReleaseImmutable {
		return fmt.Errorf("imagesource: the guest-image channel does not attest that release %s is immutable", channel.Tag)
	}
	if channel.PublishedAt.IsZero() || channel.ExpiresAt.IsZero() ||
		!channel.ExpiresAt.After(channel.PublishedAt) ||
		channel.ExpiresAt.Sub(channel.PublishedAt) > maxChannelLifetime {
		return fmt.Errorf("imagesource: the guest-image channel has an invalid validity window")
	}
	now := time.Now()
	if channel.PublishedAt.After(now.Add(channelClockSkew)) {
		return fmt.Errorf("imagesource: the guest-image channel claims a future publication time %s",
			channel.PublishedAt.Format(time.RFC3339))
	}
	if !now.Before(channel.ExpiresAt) {
		return fmt.Errorf("imagesource: the guest-image channel expired at %s; refusing a replayed pointer",
			channel.ExpiresAt.Format(time.RFC3339))
	}

	c.Source = Source{
		BaseURL:  defaultDownloadPrefix + channel.Tag,
		official: true,
	}

	return nil
}

// Manifest fetches the index for the current release, proves it, and validates it.
//
// THE ONLY WAY A Manifest IS PRODUCED FROM THE NETWORK, and it takes a policy for
// that reason: a caller cannot obtain one without having said what would make it
// trustworthy. ParseManifest validates before returning, so no caller can hold an
// unvalidated one either — which is what lets the download path treat the digests
// and names as constrained.
//
// THE SIGNATURE IS CHECKED OVER THE BYTES THAT ARRIVED, before anything is parsed
// out of them. Verifying a re-serialised manifest would verify a document this
// program produced rather than the one that was signed, and the two can differ in
// whitespace, key order, or any field a future reader drops.
func (c *Client) Manifest(ctx context.Context, p Policy) (*Manifest, error) {
	if err := c.Resolve(ctx); err != nil {
		return nil, err
	}

	body, err := c.get(ctx, c.Source.ManifestURL(), MaxManifestBytes)
	if err != nil {
		return nil, err
	}

	var signature []byte

	if p.Required {
		signature, err = c.get(ctx, c.Source.BaseURL+"/"+BundleName, MaxBundleBytes)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("imagesource: this source requires a signature and %s "+
					"publishes none: %w", c.Source.BaseURL, err)
			}

			return nil, err
		}
	}

	if err := VerifySignature(body, signature, p); err != nil {
		return nil, err
	}

	return ParseManifest(body)
}

// get retrieves a small document whole, bounded.
func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("imagesource: could not build a request for %s: %w", url, err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("imagesource: could not reach %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, url)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imagesource: %s answered %s", url, resp.Status)
	}

	// LIMIT+1 SO THAT "EXACTLY AT THE LIMIT" AND "OVER IT" ARE DISTINGUISHABLE.
	// Reading exactly limit bytes cannot tell a document that just fits from one
	// that was truncated, and silently truncating a manifest produces a parse
	// error that blames the publisher for something the reader did.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("imagesource: could not read %s: %w", url, err)
	}

	if int64(len(body)) > limit {
		return nil, fmt.Errorf("imagesource: %s returned more than %d bytes", url, limit)
	}

	return body, nil
}

// Download retrieves one asset into dir and returns the path it landed at.
//
// THE ORDER IS THE POINT. Bytes are streamed to a temporary name while being
// hashed, the length and digest are checked against what the manifest promised,
// and ONLY THEN does the file get the name the caller will use. A verifier that
// runs after the final rename has already published unverified bytes under a
// trusted name, and anything reading the directory concurrently — or after a
// crash between the two steps — cannot tell the difference.
//
// The alternative shape, streaming straight into the cluster and verifying
// afterwards, is worse in the same way and harder to undo: it imports unverified
// bytes into shared storage. Staging costs one disk write and makes the failure
// a deleted temporary file.
func (c *Client) Download(ctx context.Context, asset Asset, dir string) (string, error) {
	if err := c.Resolve(ctx); err != nil {
		return "", err
	}

	if err := asset.validate("asset"); err != nil {
		return "", err
	}

	url, err := c.Source.AssetURL(asset.Name)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("imagesource: could not build a request for %s: %w", url, err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("imagesource: could not reach %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: %s", ErrNotFound, url)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imagesource: %s answered %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(dir, ".billet-image-*")
	if err != nil {
		return "", fmt.Errorf("imagesource: could not stage a download in %s: %w", dir, err)
	}

	staged := tmp.Name()

	// REMOVED ON EVERY FAILURE PATH. A staged file that outlives its failure is
	// several hundred megabytes of garbage that nothing will ever collect, and
	// on a node that retries weekly it is the disk-filling kind.
	committed := false

	defer func() {
		_ = tmp.Close()

		if !committed {
			_ = os.Remove(staged)
		}
	}()

	sum := sha256.New()

	// SIZE+1 AGAIN, so a body longer than promised is an error rather than a
	// silent truncation to the promised length -- which would otherwise let a
	// publisher's digest match a prefix of something larger.
	written, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(resp.Body, asset.Size+1))
	if err != nil {
		return "", fmt.Errorf("imagesource: could not download %s: %w", asset.Name, err)
	}

	if written != asset.Size {
		return "", fmt.Errorf("imagesource: %s was published as %d bytes and %d arrived; "+
			"refusing rather than importing a partial image", asset.Name, asset.Size, written)
	}

	// FSYNC BEFORE THE RENAME. The digest was computed over what passed through
	// memory; without a sync, what the rename publishes is whatever reached the
	// disk, and a crash here would leave a trusted name over a partial file that
	// nothing will check again.
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("imagesource: could not flush %s: %w", asset.Name, err)
	}

	got := hex.EncodeToString(sum.Sum(nil))
	if got != asset.SHA256 {
		return "", fmt.Errorf("imagesource: %s hashes to %s and the manifest published %s; "+
			"the bytes that arrived are not the bytes that were signed",
			asset.Name, got, asset.SHA256)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("imagesource: could not close %s: %w", asset.Name, err)
	}

	final := filepath.Join(dir, asset.Name)

	if err := os.Rename(staged, final); err != nil {
		return "", fmt.Errorf("imagesource: could not place %s: %w", asset.Name, err)
	}

	committed = true

	return final, nil
}

// VerifyFile checks a file on disk against a published digest.
//
// THE SIDELOAD PATH'S EQUIVALENT OF WHAT Download DOES INLINE. A file that arrived
// on a USB stick is no more trustworthy than one that arrived over http — less,
// arguably, since nothing about its journey is even in principle observable — so
// it is checked against the same manifest with the same digest before anything
// imports it.
func VerifyFile(path, want string) error {
	if !digestPattern.MatchString(want) {
		return fmt.Errorf("imagesource: %q is not a sha256 digest", want)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("imagesource: cannot read %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	sum := sha256.New()

	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Errorf("imagesource: could not read %s: %w", path, err)
	}

	got := hex.EncodeToString(sum.Sum(nil))
	if got != want {
		return fmt.Errorf("imagesource: %s hashes to %s and the manifest published %s; it is "+
			"not the file that was signed", path, got, want)
	}

	return nil
}
