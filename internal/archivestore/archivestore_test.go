package archivestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
)

// fakeS3 records what billet actually put on the wire.
//
// A REAL HTTP ROUND TRIP rather than a stubbed method, because what these tests
// are about is the REQUEST: the header that makes a write refuse to replace, the
// header that asks for encryption, and the fact that no method a delete could
// travel on is ever used. A stub of Put could not see any of that.
type fakeS3 struct {
	*httptest.Server

	mu       sync.Mutex
	requests []request
	respond  func(w http.ResponseWriter, r *http.Request) bool
}

type request struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()

	f := &fakeS3{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body) //nolint:errcheck // a recorded request body is diagnostic only

		f.mu.Lock()
		f.requests = append(f.requests, request{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			header: r.Header.Clone(), body: body.Bytes(),
		})
		respond := f.respond
		f.mu.Unlock()

		if respond != nil && respond(w, r) {
			return
		}

		w.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(f.Close)

	return f
}

func (f *fakeS3) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]request(nil), f.requests...)
}

func (f *fakeS3) answer(fn func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.respond = fn
}

// leakyCredentials is a DELIBERATELY UNREDACTED credential source.
//
// A CLOSURE CANNOT TEST REDACTION, and the first version of this file used one.
// Formatting a func value renders an address and never the strings it captured,
// so every assertion below stayed green with all five of S3's rendering methods
// deleted — the exact "test that could not have failed" this repository keeps a
// list of. A concrete struct with EXPORTED secret fields is what makes the
// question real: with it, anything that reflects over the store and does not go
// through a redactor prints these sentinels.
type leakyCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func (l leakyCredentials) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID:     l.AccessKeyID,
		SecretAccessKey: l.SecretAccessKey,
		SessionToken:    l.SessionToken,
	}, nil
}

// testCredentials are static and fake. Nothing here reaches AWS.
func testCredentials() CredentialSource {
	return leakyCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "billet-test-secret-access-key",
		SessionToken:    "billet-test-session-token",
	}
}

func newTestStore(t *testing.T, f *fakeS3, kmsKey string) *S3 {
	t.Helper()

	s, err := NewS3(config.BackupS3Config{
		Bucket:   "billet-backups",
		Region:   "us-east-1",
		Prefix:   "billet",
		Endpoint: f.URL,
		KMSKeyID: kmsKey,
	}, testCredentials())
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}

	return s
}

// A write REFUSES to replace what is already there, and asks for encryption.
//
// The no-clobber is the same shape every credential publication in billet uses.
// An archive holds two private keys; a PUT that silently replaced one would be
// the one operation in this package that can destroy a backup.
func TestAWriteRefusesToReplaceAndAsksForEncryption(t *testing.T) {
	f := newFakeS3(t)
	s := newTestStore(t, f, "")

	if err := s.Put(t.Context(), "billet/dep/2026/manifest.json", []byte("{}")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	seen := f.seen()
	if len(seen) != 1 {
		t.Fatalf("made %d requests, want one", len(seen))
	}

	if got := seen[0].header.Get("If-None-Match"); got != "*" {
		t.Errorf("If-None-Match is %q, want *; without it a write REPLACES an archive", got)
	}

	if got := seen[0].header.Get("X-Amz-Server-Side-Encryption"); got != "AES256" {
		t.Errorf("server-side encryption is %q, want AES256", got)
	}

	// SIGNED, and with the session token beside it — an unsigned request is one
	// the bucket refuses, and a missing token is one a role credential cannot
	// authenticate.
	if auth := seen[0].header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("the request carried %q, want a SigV4 signature", auth)
	}

	if seen[0].header.Get("X-Amz-Security-Token") == "" {
		t.Error("the request carried no session token, so a role credential cannot authenticate")
	}

	if !bytes.Equal(seen[0].body, []byte("{}")) {
		t.Errorf("the request body was %q, want the object", seen[0].body)
	}
}

// A customer-managed key is asked for by name.
func TestAKMSKeyIsNamedOnEveryWrite(t *testing.T) {
	f := newFakeS3(t)
	s := newTestStore(t, f, "arn:aws:kms:us-east-1:123456789012:key/abc")

	if err := s.Put(t.Context(), "billet/dep/2026/manifest.json", []byte("{}")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	seen := f.seen()[0]

	if got := seen.header.Get("X-Amz-Server-Side-Encryption"); got != "aws:kms" {
		t.Errorf("server-side encryption is %q, want aws:kms", got)
	}

	if got := seen.header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"); got == "" {
		t.Error("the configured KMS key was not named, so the object lands under the default key")
	}
}

// An occupied key is its OWN answer, not a generic failure.
//
// A caller has to tell "this archive is already uploaded" — which is a retry of
// bytes that are already safe — from a transport that fell over, because only
// one of those is a reason to stop.
func TestAnOccupiedKeyIsDistinguishable(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusPreconditionFailed)

		return true
	})

	s := newTestStore(t, f, "")

	err := s.Put(t.Context(), "billet/dep/2026/manifest.json", []byte("{}"))
	if !errors.Is(err, ErrObjectExists) {
		t.Fatalf("Put returned %v, want ErrObjectExists", err)
	}
}

// An absent key is a FACT, not a failure: a caller that listed and then fetched
// races an operator's lifecycle rule.
//
// THE FAKE SERVES THE DOCUMENT S3 SERVES, and that is the point rather than
// decoration. This test used to answer a BODYLESS 404 — a shape S3 never sends —
// and it passed for a reader that looked only at the status, which is exactly the
// reader that could not tell a missing bucket from a missing object.
func TestAnAbsentKeyIsDistinguishable(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusNotFound)
		writeXML(t, w, noSuchKeyDocument)

		return true
	})

	s := newTestStore(t, f, "")

	_, err := s.Get(t.Context(), "billet/dep/2026/manifest.json")
	if !errors.Is(err, ErrNoSuchObject) {
		t.Fatalf("Get returned %v, want ErrNoSuchObject", err)
	}
}

// The two documents S3 sends for the two facts a 404 can carry, declaration and
// newline included.
const (
	noSuchKeyDocument = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Error><Code>NoSuchKey</Code>` +
		`<Message>The specified key does not exist.</Message>` +
		`<Key>billet/dep/2026/manifest.json</Key></Error>`
	noSuchBucketDocument = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Error><Code>NoSuchBucket</Code>` +
		`<Message>The specified bucket does not exist</Message>` +
		`<BucketName>billet-backups</BucketName></Error>`
)

func writeXML(t *testing.T, w http.ResponseWriter, document string) {
	t.Helper()

	if _, err := io.WriteString(w, document); err != nil {
		t.Errorf("write the refusal: %v", err)
	}
}

// A MISSING BUCKET IS NOT A MISSING OBJECT, and on the day this matters the
// difference is the whole recovery.
//
// A restore runs on a machine that holds the billet binary and nothing else. Told
// "no such object", an operator goes looking for backups that were never taken;
// told the bucket is not there, they fix one line of config. S3 says which it is
// and billet used to throw the answer away.
func TestAMissingBucketIsNotAMissingObject(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusNotFound)
		writeXML(t, w, noSuchBucketDocument)

		return true
	})

	s := newTestStore(t, f, "")

	_, err := s.Get(t.Context(), "billet/dep/2026/manifest.json")
	if err == nil {
		t.Fatal("a bucket that does not exist answered like a healthy fetch")
	}

	if errors.Is(err, ErrNoSuchObject) {
		t.Fatalf("a bucket that does not exist reached the caller as an absent archive: %v", err)
	}

	for _, must := range []string{"NoSuchBucket", "billet-backups", "does not exist"} {
		if !strings.Contains(err.Error(), must) {
			t.Errorf("the failure does not say %q: %v", must, err)
		}
	}
}

// A 404 BILLET CANNOT READ IS A FAILURE, NOT AN ABSENCE.
//
// S3 always names a code. Something that answers 404 without one — a proxy, a
// captive network, a gateway in front of the bucket — is not S3 answering about
// this object, and reading it as "that archive is gone" would be billet asserting
// something it was never told.
func TestAnUnreadableFourOhFourIsNotAnAbsentObject(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusNotFound)
		writeXML(t, w, "<html><head><title>404 Not Found</title></head></html>")

		return true
	})

	s := newTestStore(t, f, "")

	_, err := s.Get(t.Context(), "billet/dep/2026/manifest.json")
	if err == nil || errors.Is(err, ErrNoSuchObject) {
		t.Fatalf("a 404 with no error code answered %v, want a failure that is not "+
			"ErrNoSuchObject", err)
	}

	if !strings.Contains(err.Error(), "named no error code billet recognises") {
		t.Errorf("the failure does not say why it is not an absent object: %v", err)
	}
}

// A wrong region is NAMED, because a bare 301 gives an operator nothing.
func TestAWrongRegionIsNamed(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("X-Amz-Bucket-Region", "eu-west-1")
		w.WriteHeader(http.StatusMovedPermanently)

		return true
	})

	s := newTestStore(t, f, "")

	_, err := s.Get(t.Context(), "billet/dep/2026/manifest.json")
	if err == nil || !strings.Contains(err.Error(), "eu-west-1") {
		t.Fatalf("Get returned %v, want the bucket's real region named", err)
	}
}

// AND A HEADER BILLET WILL NOT REPEAT IS DROPPED RATHER THAN PRINTED.
//
// PROVING THE HELPER IS NOT PROVING IT IS USED: awss3's own test stays green with
// this client reading the header directly. It matters more here than anywhere,
// because backup.s3.endpoint means the far side is not always AWS — and the
// operator reading this line is recovering a deployment.
func TestARegionHintFromTheFarSideIsNotRepeatedWhole(t *testing.T) {
	hostile := "the bucket moved; run this command instead"

	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("X-Amz-Bucket-Region", hostile)
		w.WriteHeader(http.StatusMovedPermanently)

		return true
	})

	s := newTestStore(t, f, "")

	_, err := s.Get(t.Context(), "billet/dep/2026/manifest.json")
	if err == nil {
		t.Fatal("a redirect passed as a healthy fetch")
	}

	if strings.Contains(err.Error(), hostile) {
		t.Errorf("a header the far side chose was repeated whole to an operator: %v", err)
	}
}

// listing XML the fake serves, in S3's shape.
func listingXML(truncated bool, next string, keys ...string) string {
	var b strings.Builder

	b.WriteString(`<ListBucketResult>`)

	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key>`+
			`<LastModified>2026-08-30T07:00:00.000Z</LastModified></Contents>`, k)
	}

	fmt.Fprintf(&b, `<IsTruncated>%t</IsTruncated>`, truncated)

	if next != "" {
		fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, next)
	}

	b.WriteString(`</ListBucketResult>`)

	return b.String()
}

// A listing offers only COMPLETE archives, and follows continuations.
//
// THE MANIFEST IS WHAT MAKES A PREFIX AN ARCHIVE. An upload interrupted before
// it leaves entries behind, and offering that prefix to an operator on the worst
// day of a deployment's life is offering them half a deployment.
func TestAListingOffersOnlyCompleteArchives(t *testing.T) {
	f := newFakeS3(t)

	var page int

	f.answer(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("list-type") != "2" {
			return false
		}

		page++

		if page == 1 {
			_, _ = w.Write([]byte(listingXML(true, "more", //nolint:errcheck // writing to an httptest recorder cannot fail
				"billet/dep/first/manifest.json",
				"billet/dep/first/ledger/billet.db",
				"billet/dep/interrupted/ledger/billet.db")))

			return true
		}

		if r.URL.Query().Get("continuation-token") != "more" {
			t.Errorf("the second page was fetched without the continuation token")
		}

		_, _ = w.Write([]byte(listingXML(false, "", //nolint:errcheck // writing to an httptest recorder cannot fail
			"billet/dep/second/manifest.json",
			"billet/dep/second/identity/deployment-id")))

		return true
	})

	s := newTestStore(t, f, "")

	archives, err := s.List(t.Context(), "dep")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var names []string
	for _, a := range archives {
		names = append(names, a.Deployment+"/"+a.Name)
	}

	if strings.Join(names, ",") != "dep/first,dep/second" {
		t.Errorf("List reported %v, want the two archives whose manifest landed", names)
	}

	if len(archives) > 0 && archives[0].Modified.IsZero() {
		t.Error("an archive came back with no timestamp, so nothing can say which is newest")
	}
}

// A listing with no deployment reports every one in the bucket.
//
// THE MACHINE DOING A RESTORE IS NEW AND HOLDS NO IDENTITY, so an operator
// cannot name the deployment they are looking for. Filtering by an identity the
// host does not have would report nothing and read as an empty bucket, on the
// one day that answer is acted on.
func TestAListingWithNoDeploymentReportsEveryOne(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("prefix") != "billet/" {
			t.Errorf("listed prefix %q, want the whole store", r.URL.Query().Get("prefix"))
		}

		_, _ = w.Write([]byte(listingXML(false, "", //nolint:errcheck // writing to an httptest recorder cannot fail
			"billet/alpha/first/manifest.json",
			"billet/beta/first/manifest.json")))

		return true
	})

	s := newTestStore(t, f, "")

	archives, err := s.List(t.Context(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var names []string
	for _, a := range archives {
		names = append(names, a.Deployment+"/"+a.Name)
	}

	if strings.Join(names, ",") != "alpha/first,beta/first" {
		t.Errorf("List reported %v, want both deployments", names)
	}
}

// A truncated listing with no new token is refused rather than looped on.
func TestATruncatedListingWithNoTokenIsRefused(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) bool {
		_, _ = w.Write([]byte(listingXML(true, "", "billet/dep/first/manifest.json"))) //nolint:errcheck // writing to an httptest recorder cannot fail

		return true
	})

	s := newTestStore(t, f, "")

	if _, err := s.List(t.Context(), "dep"); err == nil {
		t.Fatal("a truncated listing with no continuation token was accepted, which loops forever")
	}
}

// NOTHING IN THIS PACKAGE CAN DELETE.
//
// The absence of a delete is the refusal: the credential doing this sits on the
// one host that also holds the App key and the node-wire CA, and an off-box copy
// that host can destroy is not an off-box copy. This drives every exported
// operation and asserts on the METHODS that reached the wire, so a delete added
// anywhere below fails here rather than being noticed by a reader.
func TestNoOperationCanEverDelete(t *testing.T) {
	f := newFakeS3(t)
	f.answer(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("list-type") == "2" {
			_, _ = w.Write([]byte(listingXML(false, "", "billet/dep/first/manifest.json"))) //nolint:errcheck // writing to an httptest recorder cannot fail

			return true
		}

		return false
	})

	s := newTestStore(t, f, "")

	if err := s.Put(t.Context(), "billet/dep/first/manifest.json", []byte("{}")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := s.Get(t.Context(), "billet/dep/first/manifest.json"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if _, err := s.List(t.Context(), "dep"); err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, r := range f.seen() {
		if r.method != http.MethodPut && r.method != http.MethodGet {
			t.Errorf("this package issued a %s request; it must only ever PUT and GET",
				r.method)
		}
	}
}

// A configured endpoint is addressed PATH style, because virtual-host style is
// what MinIO does not do by default — and Ceph RGW, which billet's own reference
// hardware runs, is the case this exists for.
func TestACustomEndpointIsAddressedPathStyle(t *testing.T) {
	f := newFakeS3(t)
	s := newTestStore(t, f, "")

	if err := s.Put(t.Context(), "billet/dep/2026/manifest.json", []byte("{}")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := "/billet-backups/billet/dep/2026/manifest.json"
	if got := f.seen()[0].path; got != want {
		t.Errorf("the request path was %q, want %q", got, want)
	}
}

// The store re-validates its own configuration, because it is exported.
//
// A caller whose config did not come through config.Load must not be able to
// build one pointed somewhere the file could never have named — the rule
// alloc.New follows, for the same reason.
func TestTheStoreRefusesAConfigurationConfigWouldHaveRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.BackupS3Config
	}{
		{"plaintext endpoint", config.BackupS3Config{
			Bucket: "billet-backups", Region: "us-east-1", Endpoint: "http://backups.example",
		}},
		{"a bucket name that breaks TLS", config.BackupS3Config{
			Bucket: "billet.backups", Region: "us-east-1",
		}},
		{"a wildcard prefix, which widens every IAM grant", config.BackupS3Config{
			Bucket: "billet-backups", Region: "us-east-1", Prefix: "billet/*",
		}},
		{"no region at all", config.BackupS3Config{Bucket: "billet-backups"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewS3(tc.cfg, testCredentials()); err == nil {
				t.Fatal("the store accepted a configuration config.Load refuses")
			}
		})
	}
}

// A TYPED NIL satisfies the interface, passes a plain == nil, and panics at the
// first signed request.
func TestATypedNilCredentialSourceIsRefused(t *testing.T) {
	var missing *staticCredentials

	if _, err := NewS3(config.BackupS3Config{
		Bucket: "billet-backups", Region: "us-east-1",
	}, missing); err == nil {
		t.Fatal("the store accepted a typed-nil credential source")
	}
}

type staticCredentials struct{}

func (s *staticCredentials) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{}, nil
}

// The store never renders its credential source, on any path.
//
// EVERY RENDERING PATH IGNORES THE OTHERS: fmt does not consult String once
// Format exists, slog's JSON handler does not consult fmt at all, and a pointer
// receiver is not consulted when a VALUE is formatted. Four types had to learn
// that one at a time in internal/provider/ec2; this asserts all of them at once,
// through the container rather than through the credential.
func TestTheStoreNeverRendersItsCredentialSource(t *testing.T) {
	f := newFakeS3(t)
	s := newTestStore(t, f, "")

	// THROUGH `any`, so the linter cannot rewrite a verb into a direct String()
	// call: what is under test IS fmt's dispatch, and a check that called the
	// method directly would prove only the method.
	var value any = *s

	var pointer any = s

	rendered := []string{
		fmt.Sprintf("%v", pointer),
		fmt.Sprintf("%v", value),
		fmt.Sprintf("%+v", value),
		fmt.Sprintf("%#v", value),
		fmt.Sprintf("%d", value),
		// AND THE METHODS DIRECTLY, because a caller holding a fmt.Stringer never
		// goes through a verb — the gap that let a mutated String survive a test
		// which only used fmt.
		s.String(),
		s.GoString(),
	}

	body, err := json.Marshal(*s)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	rendered = append(rendered, string(body), s.LogValue().String())

	// AND slog, WHICH CONSULTS NONE OF THE ABOVE — its JSON handler never asks
	// fmt anything.
	var log bytes.Buffer

	slog.New(slog.NewJSONHandler(&log, nil)).Info("store", "s", s)

	rendered = append(rendered, log.String())

	// THE SENTINELS ARE THE CREDENTIAL SOURCE'S OWN EXPORTED FIELDS, so a
	// rendering path that reflects over the store instead of going through a
	// redactor prints one of them. That is what makes this fail when any of the
	// five methods is deleted.
	for _, out := range rendered {
		for _, secret := range []string{
			"billet-test-secret-access-key", "billet-test-session-token", "AKIDEXAMPLE",
		} {
			if strings.Contains(out, secret) {
				t.Errorf("a rendering of the store carried %q: %s", secret, out)
			}
		}
	}
}
