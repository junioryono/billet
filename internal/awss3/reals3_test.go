package awss3

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssig"
)

// THE TWO CODES ARE ASKED OF S3, BECAUSE NOTHING ELSE CAN ANSWER THEM.
//
// The whole package rests on one claim about somebody else's API: that a missing
// OBJECT and a missing BUCKET are both 404 and are told apart only by the <Code>
// element. A fixture written from the documentation agrees with whatever billet
// believes, which is exactly the shape of the CreateSnapshot ClientToken defect —
// every fake had pinned a parameter the real service refuses.
//
// TWO SIGNED GETs AND NOTHING ELSE. Nothing is created, nothing is deleted and
// nothing is billed; the second bucket name is random precisely so it cannot
// exist, and S3's wildcard DNS record answers for it.
//
// SKIPPED WITHOUT A BUCKET, the same two variables internal/provider/ec2's
// reals3_test.go uses, so there is one answer to how a real-S3 test is gated.
func TestRealS3TellsAMissingBucketFromAMissingKey(t *testing.T) {
	bucket := os.Getenv("BILLET_S3_TEST_BUCKET")
	region := os.Getenv("BILLET_S3_TEST_REGION")

	if bucket == "" || region == "" {
		t.Skip("set BILLET_S3_TEST_BUCKET and BILLET_S3_TEST_REGION")
	}

	creds, err := awscreds.Env{}.Credentials(t.Context())
	if err != nil {
		t.Skipf("no credentials in the environment: %v", err)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("make a name no bucket can have: %v", err)
	}

	absent := "billet-probe-" + hex.EncodeToString(nonce[:])

	// A KEY UNDER A PREFIX NOTHING WRITES, so a bucket that happens to hold a
	// billet cache or a billet archive still answers that this object is not
	// there.
	key := "billet-probe-does-not-exist/" + hex.EncodeToString(nonce[:])

	t.Run("a missing key in a bucket that exists", func(t *testing.T) {
		refusal := probe(t, s3Get{bucket: bucket, region: region, key: key, creds: creds})

		// A TEST ROLE THAT CANNOT LIST IS A SETUP PROBLEM, NOT A MODEL PROBLEM.
		// S3 answers 403 rather than NoSuchKey for a miss when the caller holds
		// no s3:ListBucket for the prefix — that is the very behaviour
		// internal/store/ebss3's CheckAccess is written around — so failing here
		// would report billet's model of S3 as wrong when what is wrong is the
		// credential this run was given.
		if refusal.Status == http.StatusForbidden {
			t.Skipf("this identity was refused (%s) rather than told the key is missing; "+
				"S3 answers NoSuchKey only to a caller holding s3:ListBucket on %q for "+
				"this prefix", refusal, bucket)
		}

		if refusal.Status != http.StatusNotFound || refusal.Code != CodeNoSuchKey {
			t.Fatalf("a missing object answered %s; billet reads only %s as absence",
				refusal, CodeNoSuchKey)
		}

		if !refusal.Absent() {
			t.Error("S3 said the object is not there and billet did not read it as absence")
		}
	})

	t.Run("a bucket that does not exist", func(t *testing.T) {
		refusal := probe(t, s3Get{bucket: absent, region: region, key: key, creds: creds})

		if refusal.Status != http.StatusNotFound || refusal.Code != CodeNoSuchBucket {
			t.Fatalf("a bucket that does not exist answered %s, not %s at HTTP 404 — the "+
				"whole classification rests on this", refusal, CodeNoSuchBucket)
		}

		// THE POINT OF THE ISSUE. Before this, the same 404 read as "the object
		// is not there" and every job fetched cold with nothing reporting a
		// fault.
		if refusal.Absent() {
			t.Error("a bucket that does not exist read as an absent object")
		}
	})
}

// s3Get is one request the probe makes. A struct rather than four parameters
// because only the bucket differs between the two, and the pair is the whole
// measurement.
type s3Get struct {
	bucket string
	region string
	key    string
	creds  awssig.Credentials
}

// probe makes one signed GET, the way billet's own S3 clients do, and reports
// what came back.
func probe(t *testing.T, get s3Get) *Refusal {
	t.Helper()

	bucket, region, key := get.bucket, get.region, get.key
	url := "https://" + bucket + ".s3." + region + ".amazonaws.com/" + key

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build the probe request: %v", err)
	}

	req.Header.Set("X-Amz-Content-Sha256", awssig.SHA256Hex(nil))

	if err := awssig.Sign(req, nil, get.creds, region, "s3", time.Now()); err != nil {
		t.Fatalf("sign the probe request: %v", err)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		// The same refusal billet's clients make: a redirect is S3's wrong-region
		// answer, and following it would send a signature to a host it does not
		// cover.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("call S3: %v", err)
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close the probe response: %v", err)
		}
	}()

	refusal := ReadRefusal(response)
	t.Logf("GET s3://%s/%s answered %s", bucket, key, refusal)

	return refusal
}
