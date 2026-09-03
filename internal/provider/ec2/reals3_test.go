package ec2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
)

// THE PRESIGNED URL IS ASKED OF AWS, BECAUSE NOTHING ELSE CAN ANSWER IT.
//
// SigV4 presigning has a dozen ways to be subtly wrong that all produce a
// well-formed URL: a session token added after signing instead of before, a
// payload hash that is a digest rather than the literal UNSIGNED-PAYLOAD, a
// canonical query built before the X-Amz-* parameters were added, path-style
// addressing against a bucket that only answers virtual-hosted. Every one of them
// yields a URL that looks right and returns SignatureDoesNotMatch, and a unit test
// written from the same reading of the specification agrees with the mistake.
//
// This is the same argument sign_test.go makes for the header signer, where the
// vectors come from aws-sdk-go-v2 rather than from billet's understanding. There
// is no equivalent vector for a presigned URL that billet can carry, so the
// authority is the service.
//
// SKIPPED WITHOUT A BUCKET. It needs credentials and somewhere to write, which CI
// does not have; set BILLET_S3_TEST_BUCKET and BILLET_S3_TEST_REGION to run it.
func TestARealPresignedURLIsFetchable(t *testing.T) {
	bucket := os.Getenv("BILLET_S3_TEST_BUCKET")
	region := os.Getenv("BILLET_S3_TEST_REGION")

	if bucket == "" || region == "" {
		t.Skip("set BILLET_S3_TEST_BUCKET and BILLET_S3_TEST_REGION")
	}

	creds, err := awscreds.Env{}.Credentials(t.Context())
	if err != nil {
		t.Skipf("no credentials in the environment: %v", err)
	}

	stager := payloadStager{
		bucket: bucket,
		region: region,
		creds:  creds,
		http:   &http.Client{Timeout: 60 * time.Second},
		now:    time.Now,
	}

	// THE REAL ARCHIVE, not a fixture. Its size and content are what a build
	// actually stages, so a limit or an encoding problem shows up here rather than
	// on a builder.
	body, digest, err := payloadArchive()
	if err != nil {
		t.Fatalf("payloadArchive: %v", err)
	}

	key := "billet-test-" + digest[:16] + ".tar.gz"

	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		if err := stager.remove(clean, key); err != nil {
			t.Errorf("the test object could not be removed and is still billed: %v", err)
		}
	})

	url, err := stager.put(t.Context(), key, body)
	if err != nil {
		t.Fatalf("staging the payload: %v", err)
	}

	// FETCHED WITH NO CREDENTIALS OF ITS OWN, which is the whole point: a builder
	// has no AWS identity, so the URL has to be sufficient on its own.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build the fetch: %v", err)
	}

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("fetching the presigned url: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the object: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the presigned url answered %s:\n%s", resp.Status, got)
	}

	// AND THE BYTES ARE THE ONES BILLET SIGNED FOR. A URL that fetches something
	// else is worse than one that fetches nothing, because the bootstrap's digest
	// check is the only thing between it and a root shell.
	if !bytes.Equal(got, body) {
		t.Fatalf("the object is %d bytes and billet uploaded %d", len(got), len(body))
	}

	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != digest {
		t.Fatal("the digest the bootstrap would check does not match what was fetched")
	}
}
