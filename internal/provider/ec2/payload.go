package ec2

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/runnerimages"
)

// THE PROVISIONING SCRIPT OUTGREW EC2's USER DATA, and this is where the rest of
// it travels.
//
// EC2 caps user data at 16384 bytes and billet gzips to fit. That held while the
// script was the runner plus a docker daemon; it stopped holding when the shared
// installers reached six toolcache runtimes and the compiler sections, at which
// point the installer file alone was 59% of the compressed budget and every
// addition pushed something else out. The bound on the CA bundle had already been
// cut three times to make room, and its own comment says a fourth cut is the wrong
// answer.
//
// SO THE BIG PAYLOAD IS STAGED IN S3 AND FETCHED BY A BOOTSTRAP. What remains in
// user data is a URL, a digest and four lines of shell, which does not grow when
// the installers do.
const (
	// payloadService is what the signature is scoped to.
	payloadService = "s3"

	// payloadLifetime is how long the presigned URL is good for.
	//
	// THE SHORTEST WINDOW THAT COVERS THE FETCH, because the URL is the credential:
	// anyone holding it can read the object until it lapses. cloud-init runs the
	// bootstrap within seconds of boot, and a builder that has not fetched inside
	// an hour has failed for a reason a longer window would not fix.
	payloadLifetime = time.Hour

	// payloadPath is where the bootstrap writes what it fetched.
	payloadPath = "/opt/billet-payload.tar.gz"
)

// bucketName is what billet will address, checked before it reaches a URL billet
// signs.
//
// NOT A GENERAL S3 NAME VALIDATOR, and NO DOTS. S3 itself permits them; billet
// cannot use them, because the endpoint below is virtual-hosted and a dotted name
// produces a multi-label host — `a.b.s3.us-west-2.amazonaws.com` — which the
// wildcard certificate for `*.s3.<region>.amazonaws.com` does not cover, a wildcard
// matching exactly one label. The build would fail TLS verification against a
// bucket that exists and is spelled correctly, which is a bad half-hour for whoever
// is holding it. Refusing here says which character was the problem instead.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// payloadStager puts a build's oversized payload where its builder can fetch it.
type payloadStager struct {
	bucket string
	region string
	creds  awssig.Credentials
	http   *http.Client
	now    func() time.Time
}

// endpoint is the virtual-hosted address of one key in the bucket.
//
// VIRTUAL-HOSTED, NOT PATH-STYLE. AWS retired path-style addressing for buckets
// created after September 2020, so a path-style URL works against some buckets and
// not others -- which is the worst kind of difference to discover from a build.
func (p payloadStager) endpoint(key string) (*url.URL, error) {
	if !bucketName.MatchString(p.bucket) {
		return nil, fmt.Errorf("ec2: %q is not a bucket name billet will sign a request "+
			"for; it must be 3 to 63 lowercase letters, digits and hyphens, starting and "+
			"ending with a letter or digit. Dots are legal in S3 and unusable here: the "+
			"virtual-hosted host they produce is not covered by S3's wildcard "+
			"certificate, so the fetch fails TLS verification", p.bucket)
	}

	// THE KEY IS BILLET'S OWN, and is checked anyway. It is interpolated into a
	// URL that a signature covers, and a key carrying a slash or a query character
	// would sign one path and fetch another.
	if key == "" || strings.ContainsAny(key, "/?#%") {
		return nil, fmt.Errorf("ec2: %q is not a payload key", key)
	}

	host := p.bucket + ".s3." + p.region + ".amazonaws.com"

	return &url.URL{Scheme: "https", Host: host, Path: "/" + key}, nil
}

// put uploads one object and returns a presigned URL for reading it back.
func (p payloadStager) put(ctx context.Context, key string, body []byte) (string, error) {
	u, err := p.endpoint(key)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ec2: build the payload upload: %w", err)
	}

	req.ContentLength = int64(len(body))

	// S3 REQUIRES THE PAYLOAD HASH AS A HEADER, unlike the EC2 Query API. Without
	// it the service answers with a signature error that says nothing about the
	// missing header.
	sum := sha256.Sum256(body)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	req.Header.Set("Content-Type", "application/octet-stream")

	if err := signService(req, body, p.creds, p.region, payloadService, p.now()); err != nil {
		return "", fmt.Errorf("ec2: sign the payload upload: %w", err)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ec2: upload the payload to s3://%s/%s: %w", p.bucket, key, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		snippet, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		if readErr != nil {
			snippet = []byte("(the error body could not be read: " + readErr.Error() + ")")
		}

		return "", fmt.Errorf("ec2: upload the payload to s3://%s/%s: %s: %s",
			p.bucket, key, resp.Status, strings.TrimSpace(string(snippet)))
	}

	get, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("ec2: build the payload url: %w", err)
	}

	signed, err := awssig.Presign(get, awssig.Credentials{
		AccessKeyID:     p.creds.AccessKeyID,
		SecretAccessKey: p.creds.SecretAccessKey,
		SessionToken:    p.creds.SessionToken,
	}, p.region, payloadService, payloadLifetime, p.now())
	if err != nil {
		return "", fmt.Errorf("ec2: presign the payload url: %w", err)
	}

	return signed, nil
}

// remove deletes a staged object.
//
// BEST EFFORT, AND THE CALLER IS TOLD. The object expires with its URL either way,
// but an object left behind is billed and is one more place a payload exists —
// so a failure to remove it is reported rather than swallowed, the way a leaked
// builder instance is.
func (p payloadStager) remove(ctx context.Context, key string) error {
	u, err := p.endpoint(key)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("ec2: build the payload delete: %w", err)
	}

	sum := sha256.Sum256(nil)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))

	if err := signService(req, nil, p.creds, p.region, payloadService, p.now()); err != nil {
		return fmt.Errorf("ec2: sign the payload delete: %w", err)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("ec2: delete s3://%s/%s: %w", p.bucket, key, err)
	}

	defer func() { _ = resp.Body.Close() }()

	// 404 IS SUCCESS HERE. A build that failed before uploading still runs this,
	// and reporting "the object you asked me to delete is not there" as an error
	// would bury the failure that actually mattered.
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("ec2: delete s3://%s/%s: %s", p.bucket, key, resp.Status)
	}

	return nil
}

// payloadArchive is the shared installers and the declaration, as one tarball.
//
// A TARBALL RATHER THAN TWO OBJECTS, so the bootstrap is one fetch and one digest
// check. Two would mean two URLs, two checks, and a window where a builder holds
// one half of a matched pair.
func payloadArchive() ([]byte, string, error) {
	projected, err := runnerimages.InstallerToolset()
	if err != nil {
		return nil, "", err
	}

	files := []struct {
		name string
		body []byte
	}{
		// STRIPPED THE SAME WAY IT WAS WHEN EMBEDDED. The comments are the reason
		// the file is readable and no reason for a builder to download them.
		{toolcacheAssetPath, []byte(stripShellComments(runnerimages.InstallToolcacheScript))},
		{toolsetPath, projected},
	}

	var raw bytes.Buffer

	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)

	for _, f := range files {
		hdr := &tar.Header{
			Name: strings.TrimPrefix(f.name, "/"),
			Mode: 0o644,
			Size: int64(len(f.body)),
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", fmt.Errorf("ec2: build the payload archive: %w", err)
		}

		if _, err := tw.Write(f.body); err != nil {
			return nil, "", fmt.Errorf("ec2: build the payload archive: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, "", fmt.Errorf("ec2: build the payload archive: %w", err)
	}

	if err := zw.Close(); err != nil {
		return nil, "", fmt.Errorf("ec2: build the payload archive: %w", err)
	}

	sum := sha256.Sum256(raw.Bytes())

	return raw.Bytes(), hex.EncodeToString(sum[:]), nil
}

// stagePayload uploads the archive and records where the builder can fetch it.
//
// THE KEY CARRIES THE DIGEST AND A NONCE, and the nonce is the load-bearing half.
//
// A digest-ALONE key was content-addressed and self-describing, which is pleasant,
// and it made two concurrent builds share one object, which is a bug: each build
// deletes that key when it finishes, so a build that ends while a second builder is
// still fetching deletes the archive out from under it and the second build fails
// on something it did nothing wrong to cause. Two copies of a hundred-kilobyte
// object cost nothing next to that. The digest stays because a leftover object
// should still say what it is rather than being a mystery with a timestamp; the key
// is not a secret either way, since the presigned URL is what grants access.
func (p *Provider) stagePayload(ctx context.Context, spec *BuildSpec) (string, func() error, error) {
	body, digest, err := payloadArchive()
	if err != nil {
		return "", nil, err
	}

	// RESOLVED HERE, NOT HELD. The provider carries a awscreds.Source rather than
	// credentials, because an IMDSv2 role rotates and a set captured at
	// construction expires mid-build -- which is exactly how an arm64 build failed
	// after registering its image.
	creds, err := p.api.creds.Credentials(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("ec2: credentials for staging the payload: %w", err)
	}

	// `now` IS NIL UNLESS A TEST PINNED IT, which is the api client's own idiom one
	// file over -- and reading it straight into a struct that CALLS it turned a
	// replaceable clock into a nil dereference on the first real build. The panic
	// was inside the signer, which says nothing about a missing default.
	now := time.Now
	if p.api.now != nil {
		now = p.api.now
	}

	stager := payloadStager{
		bucket: spec.PayloadBucket,
		region: p.api.region,
		creds:  creds,
		http:   p.api.http,
		now:    now,
	}

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, fmt.Errorf("ec2: name the payload object: %w", err)
	}

	key := "billet-payload-" + digest + "-" + hex.EncodeToString(nonce[:]) + ".tar.gz"

	remove := func() error {
		// A CONTEXT OF ITS OWN, because this runs from a deferred call on the way
		// out of a build whose context may already be cancelled -- and a cleanup
		// that inherits the cancellation it is cleaning up after never runs.
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		return stager.remove(clean, key)
	}

	// THE CLEANUP EXISTS BEFORE THE PUT, because a PUT whose response is lost may
	// still have committed. Returning that error without ever naming the key left an
	// object nobody could attribute and nobody would delete; the caller is handed a
	// cleanup only on success, so this is the one place that can try.
	signed, err := stager.put(ctx, key, body)
	if err != nil {
		if rmErr := remove(); rmErr != nil {
			return "", nil, fmt.Errorf("%w (the object may have been written; removing "+
				"%s failed too: %w)", err, key, rmErr)
		}

		return "", nil, err
	}

	spec.payloadURL = signed
	spec.payloadSHA256 = digest

	return key, remove, nil
}
