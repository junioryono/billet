package ebss3

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awss3"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
)

type staticCredentials struct{}

func (staticCredentials) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "session",
	}, nil
}

func TestS3StateUsesConditionalSignedEncryptedWrites(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/state/key.json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/s3/aws4_request") ||
			r.Header.Get("X-Amz-Security-Token") != "session" {
			t.Errorf("request is not signed with the session credential: %v", r.Header)
		}
		switch requests {
		case 1:
			if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" ||
				r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" ||
				r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "" {
				t.Errorf("create headers = %v", r.Header)
			}
			w.Header().Set("ETag", `"one"`)
			w.WriteHeader(http.StatusOK)
		case 2:
			if r.Method != http.MethodPut || r.Header.Get("If-Match") != `"one"` {
				t.Errorf("replace headers = %v", r.Header)
			}
			w.WriteHeader(http.StatusPreconditionFailed)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	api := newS3API(config.EBSS3Config{
		Region: "us-west-2", Bucket: "billet-cache-example", KMSKeyID: "alias/billet",
	}, staticCredentials{}, server.Client(), server.URL, func() time.Time {
		return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	})
	if etag, err := api.Put(t.Context(), "state/key.json", []byte(`{"one":1}`), ""); err != nil || etag != `"one"` {
		t.Fatalf("initial Put = %q, %v", etag, err)
	}
	if _, err := api.Put(t.Context(), "state/key.json", []byte(`{"two":2}`), `"one"`); !errors.Is(err, errObjectConflict) {
		t.Fatalf("conditional Put error = %v", err)
	}
}

func TestIndeterminateS3WritesAreAmbiguous(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "accepted without etag", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
				staticCredentials{}, server.Client(), server.URL, time.Now)
			if _, err := api.Put(t.Context(), "state/key.json", []byte(`{"state":1}`), ""); !errors.Is(err, errObjectAmbiguous) {
				t.Fatalf("Put error = %v, want an ambiguous outcome", err)
			}
		})
	}
}

// ONLY NoSuchKey READS AS AN ABSENT STATE OBJECT.
//
// The state read is the cache's whole map of itself, and `found=false` puts an
// empty one in its place. So a 404 that is really "this bucket does not exist"
// used to be indistinguishable from a cold cache: every job fetched cold, the
// node's cache path fails open by design, and the one diagnostic that looks —
// `billet check`'s bucket probe — printed that the bucket answered.
func TestOnlyNoSuchKeyReadsAsAnAbsentStateObject(t *testing.T) {
	t.Parallel()

	// The shape S3 answers in, declaration and newline included.
	const (
		noSuchKey = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
			`<Error><Code>NoSuchKey</Code>` +
			`<Message>The specified key does not exist.</Message></Error>`
		noSuchBucket = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
			`<Error><Code>NoSuchBucket</Code>` +
			`<Message>The specified bucket does not exist</Message>` +
			`<BucketName>billet-cache-example</BucketName></Error>`
		accessDenied = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
			`<Error><Code>AccessDenied</Code>` +
			`<Message>Access Denied</Message></Error>`
	)

	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
		// says is a clause the operator has to be able to read in the failure.
		says string
	}{
		{
			name: "a missing object", status: http.StatusNotFound, body: noSuchKey,
		},
		{
			name: "a bucket that does not exist", status: http.StatusNotFound, body: noSuchBucket,
			wantErr: true, says: `node.ebs_s3.bucket names "billet-cache-example"`,
		},
		{
			// NOT A SHAPE S3 SENDS, which is exactly why it must not be the case
			// that means absence: whatever answered is not S3 talking about an
			// object.
			name: "a 404 with no error document", status: http.StatusNotFound,
			wantErr: true, says: "named no error code billet recognises",
		},
		{
			name: "a refused read", status: http.StatusForbidden, body: accessDenied,
			wantErr: true, says: "AccessDenied (HTTP 403)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				if _, err := io.WriteString(w, tc.body); err != nil {
					t.Errorf("write the refusal: %v", err)
				}
			}))
			defer server.Close()

			api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
				staticCredentials{}, server.Client(), server.URL, time.Now)

			body, etag, found, err := api.Get(t.Context(), "state/key.json")

			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Get answered found=%v with no error; a refusal billet cannot read as "+
					"NoSuchKey must not read as an empty cache", found)
			case !tc.wantErr && err != nil:
				t.Fatalf("Get: %v", err)
			}

			// NOTHING HERE IS A STATE OBJECT. found=true is what makes the caller
			// parse a body, so a refusal must never claim one — and the absent
			// case is the pair (found=false, err=nil), which is the only way
			// loadObject reaches its empty state.
			if found {
				t.Errorf("Get claimed to have found a state object in a %d answer", tc.status)
			}

			if body != nil || etag != "" {
				t.Errorf("Get returned body %q and etag %q on a refusal", body, etag)
			}

			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the failure does not say %q: %v", tc.says, err)
			}
		})
	}
}

// THE REGION HINT REACHES AN OPERATOR THROUGH awss3.RegionHint, NOT RAW.
//
// PROVING THE HELPER IS NOT PROVING IT IS USED: awss3's own test would stay green
// with this client reading the header directly, and the header is bytes the far
// side chose, on a path that ends in a terminal. Two things at once — a hint
// billet will not repeat is dropped, and one it will is quoted.
func TestTheRegionHintIsFilteredBeforeItReachesAnOperator(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		hint   string
		reject bool
	}{
		// A SENTENCE AND AN OVERLONG VALUE, not an escape sequence: Go's own
		// server will not put a control character on the wire, so a test built
		// around one would pass for a reason that has nothing to do with this
		// client. awss3's table covers the control characters directly.
		{name: "a sentence", hint: "your region is wrong, run rm -rf /", reject: true},
		{name: "longer than billet will repeat", hint: strings.Repeat("z", 65), reject: true},
		{name: "a region", hint: "eu-west-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Amz-Bucket-Region", tc.hint)
				w.WriteHeader(http.StatusMovedPermanently)
			}))
			defer server.Close()

			api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
				staticCredentials{}, server.Client(), server.URL, time.Now)

			_, _, _, err := api.Get(t.Context(), "state/key.json")
			if err == nil {
				t.Fatal("a redirect passed as a healthy read")
			}

			if tc.reject && strings.Contains(err.Error(), tc.hint) {
				t.Errorf("a header billet will not repeat was repeated whole: %v", err)
			}

			if !tc.reject && !strings.Contains(err.Error(), `"`+tc.hint+`"`) {
				t.Errorf("the bucket's real region was not named, or not quoted: %v", err)
			}
		})
	}
}

// THE PROBE'S 403 BRANCH ASKS THE ANSWER, NOT THE WORDS.
//
// `billet check` reports a 403 from the cache probe as INCONCLUSIVE, because
// billet's own minimal grant can answer one for a healthy miss. It reaches that
// verdict through the refusal S3 sent, so this asserts the store's error carries
// it — a message reworded above this line must not be able to change a verdict.
func TestARefusedReadCarriesItsStatusToTheProbe(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		if _, err := io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`); err != nil {
			t.Errorf("write the refusal: %v", err)
		}
	}))
	defer server.Close()

	cfg := config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"}
	api := newS3API(cfg, staticCredentials{}, server.Client(), server.URL, time.Now)
	store := newStore(cfg, "deployment/site", newFakeBlocks(), api)

	err := store.CheckAccess(t.Context())
	if err == nil {
		t.Fatal("a refused read passed the access probe")
	}

	if got := awss3.StatusOf(err); got != http.StatusForbidden {
		t.Fatalf("the probe's error carries status %d, want 403 — `billet check` classifies "+
			"this branch as inconclusive and cannot see it", got)
	}
}

func TestNewRefusesATypedNilCredentialSource(t *testing.T) {
	t.Parallel()

	var credentials awscreds.SourceFunc
	_, err := New(config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "billet-cache-example",
	}, "deployment/site", credentials)
	if err == nil {
		t.Fatal("New accepted a typed-nil credential source")
	}
}

func TestEBSVolumesAndSnapshotsCarryOwnershipAndEncryption(t *testing.T) {
	t.Parallel()

	var actions []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		actions = append(actions, values)
		if !strings.Contains(r.Header.Get("Authorization"), "/ec2/aws4_request") {
			t.Errorf("request is not signed for EC2: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/xml")
		switch values.Get("Action") {
		case "CreateVolume":
			writeAWSResponse(t, w, `<CreateVolumeResponse><volumeId>vol-123</volumeId><status>creating</status></CreateVolumeResponse>`)
		case "DescribeVolumes":
			writeAWSResponse(t, w, `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-123</volumeId><status>available</status></item></volumeSet></DescribeVolumesResponse>`)
		case "CreateSnapshot":
			writeAWSResponse(t, w, `<CreateSnapshotResponse><snapshotId>snap-123</snapshotId><status>pending</status></CreateSnapshotResponse>`)
		case "DescribeSnapshots":
			if values.Get("Filter.1.Name") == "tag:"+snapshotTokenTag {
				// Nothing carries the token yet, so the lookup that precedes the
				// creation finds nothing.
				writeAWSResponse(t, w, `<DescribeSnapshotsResponse><snapshotSet/></DescribeSnapshotsResponse>`)

				return
			}
			writeAWSResponse(t, w, `<DescribeSnapshotsResponse><snapshotSet><item><snapshotId>snap-123</snapshotId><status>completed</status><startTime>2026-08-16T12:00:00.000Z</startTime></item></snapshotSet></DescribeSnapshotsResponse>`)
		default:
			t.Fatalf("unexpected action %q", values.Get("Action"))
		}
	}))
	defer server.Close()

	api := newEBSAPI(config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", KMSKeyID: "alias/billet",
	}, "deployment/site", staticCredentials{}, server.Client(), server.URL,
		func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) },
		func(context.Context, time.Duration) error { return nil })
	volume, err := api.CreateVolume(t.Context(), "", 10<<30, "volume-token")
	if err != nil || volume != "vol-123" {
		t.Fatalf("CreateVolume = %q, %v", volume, err)
	}
	snapshot, err := api.CreateSnapshot(t.Context(), volume,
		time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), "snapshot-token")
	if err != nil || snapshot != "snap-123" {
		t.Fatalf("CreateSnapshot = %q, %v", snapshot, err)
	}

	var createSnapshot url.Values
	for _, action := range actions {
		if action.Get("Action") == "CreateSnapshot" {
			if createSnapshot != nil {
				t.Fatal("CreateSnapshot was sent twice for one token")
			}
			createSnapshot = action
		}
	}
	if createSnapshot == nil {
		t.Fatal("CreateSnapshot was never sent")
	}
	// EC2's CreateSnapshot refuses ClientToken as UnknownParameter (measured), so
	// the token must ride as a tag and never as the parameter.
	if _, present := createSnapshot["ClientToken"]; present {
		t.Fatalf("CreateSnapshot carried ClientToken, which EC2 refuses: %v", createSnapshot)
	}
	if createSnapshot.Get("VolumeId") != volume ||
		createSnapshot.Get("TagSpecification.1.ResourceType") != "snapshot" ||
		createSnapshot.Get("TagSpecification.1.Tag.1.Value") != "deployment" ||
		createSnapshot.Get("TagSpecification.1.Tag.2.Key") != cacheOwnerTag ||
		createSnapshot.Get("TagSpecification.1.Tag.2.Value") != "deployment/site" ||
		createSnapshot.Get("TagSpecification.1.Tag.3.Key") != snapshotTokenTag ||
		createSnapshot.Get("TagSpecification.1.Tag.3.Value") != "snapshot-token" {
		t.Fatalf("CreateSnapshot params = %v", createSnapshot)
	}

	createVolume := actions[0]
	if createVolume.Get("AvailabilityZone") != "us-west-2a" ||
		createVolume.Get("Encrypted") != "true" || createVolume.Get("KmsKeyId") != "alias/billet" ||
		createVolume.Get("Size") != "10" || createVolume.Get("VolumeType") != "gp3" ||
		createVolume.Get("TagSpecification.1.Tag.1.Value") != "deployment" ||
		createVolume.Get("TagSpecification.1.Tag.2.Key") != cacheOwnerTag ||
		createVolume.Get("TagSpecification.1.Tag.2.Value") != "deployment/site" ||
		createVolume.Get("ClientToken") != "volume-token" {
		t.Fatalf("CreateVolume params = %v", createVolume)
	}
}

// tokenFake is an EC2 that models what CreateSnapshot cannot see: whether a
// failed create nevertheless acted, and how many lookups pass before a created
// snapshot becomes visible (DescribeSnapshots is eventually consistent). Every
// snapshot carrying the token is listed, in whatever state it was given, across
// as many pages as pageSize allows.
type tokenFake struct {
	t *testing.T
	// attempts describes each CreateSnapshot in order; a create past the end
	// succeeds normally.
	attempts []tokenAttempt
	// pre are snapshots carrying the token before the first call, by state.
	pre []string
	// lookupsUntilVisible is how many lookups a created snapshot stays hidden for.
	lookupsUntilVisible int
	pageSize            int
	// emptyFirstPage answers the first page of a lookup with no items and a
	// NextToken, which EC2 may do; cycle makes every page name the first one.
	emptyFirstPage, cycle bool

	creates, lookups, pages int
	snapshots               []tokenSnapshot
}

type tokenAttempt struct {
	status int    // HTTP status of the answer; 0 is a success
	code   string // an AWS error code in the body; empty means an unparseable body
	acts   bool   // the snapshot was created despite the failed answer
}

type tokenSnapshot struct {
	id, state string
	visibleAt int // the lookup count from which the snapshot is listed
}

func (f *tokenFake) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "text/xml")
	switch values.Get("Action") {
	case "CreateSnapshot":
		f.creates++
		if _, present := values["ClientToken"]; present {
			f.t.Errorf("CreateSnapshot carried ClientToken: %v", values)
		}
		if values.Get("TagSpecification.1.Tag.3.Key") != snapshotTokenTag ||
			values.Get("TagSpecification.1.Tag.3.Value") != "snapshot-token" {
			f.t.Errorf("CreateSnapshot did not tag the token: %v", values)
		}
		id := fmt.Sprintf("snap-c%d", f.creates)
		var attempt tokenAttempt
		if f.creates <= len(f.attempts) {
			attempt = f.attempts[f.creates-1]
		}
		if attempt.status == 0 {
			f.snapshots = append(f.snapshots, tokenSnapshot{id: id, state: "pending", visibleAt: f.lookups + f.lookupsUntilVisible})
			writeAWSResponse(f.t, w, `<CreateSnapshotResponse><snapshotId>`+id+`</snapshotId><status>pending</status></CreateSnapshotResponse>`)

			return
		}
		if attempt.acts {
			f.snapshots = append(f.snapshots, tokenSnapshot{id: id, state: "pending", visibleAt: f.lookups + f.lookupsUntilVisible})
		}
		w.WriteHeader(attempt.status)
		if attempt.code != "" {
			writeAWSResponse(f.t, w, `<Response><Errors><Error><Code>`+attempt.code+`</Code><Message>refused</Message></Error></Errors></Response>`)
		} else {
			writeAWSResponse(f.t, w, `lost`)
		}
	case "DescribeSnapshots":
		if values.Get("Filter.1.Name") != "tag:"+snapshotTokenTag {
			id := values.Get("SnapshotId.1")
			writeAWSResponse(f.t, w, `<DescribeSnapshotsResponse><snapshotSet><item><snapshotId>`+id+`</snapshotId><status>completed</status><startTime>2026-08-16T12:00:00.000Z</startTime></item></snapshotSet></DescribeSnapshotsResponse>`)

			return
		}
		if values.Get("NextToken") == "" {
			f.lookups++
		}
		if values.Get("Filter.1.Value.1") != "snapshot-token" || values.Get("Owner.1") != "self" ||
			values.Get("Filter.2.Name") != "tag:"+cacheOwnerTag ||
			values.Get("Filter.2.Value.1") != "deployment/site" {
			f.t.Errorf("token lookup filters = %v", values)
		}
		var visible []tokenSnapshot
		for i, state := range f.pre {
			visible = append(visible, tokenSnapshot{id: fmt.Sprintf("snap-p%d", i+1), state: state})
		}
		for _, s := range f.snapshots {
			if s.visibleAt <= f.lookups {
				visible = append(visible, s)
			}
		}
		f.pages++
		// A regression in either loop guard must fail here rather than hang the
		// suite: no lookup in this test legitimately needs this many pages.
		if f.pages > 200 {
			f.t.Errorf("the token lookup fetched %d pages without ending", f.pages)
			w.WriteHeader(http.StatusInternalServerError)

			return
		}
		if f.cycle {
			// A NON-ADJACENT cycle, page-0 → page-1 → page-0, so that only a guard
			// remembering every token seen catches it; comparing with the previous
			// token alone would loop forever.
			next := "page-1"
			if values.Get("NextToken") == "page-1" {
				next = "page-0"
			}
			writeAWSResponse(f.t, w, `<DescribeSnapshotsResponse><snapshotSet/><nextToken>`+next+`</nextToken></DescribeSnapshotsResponse>`)

			return
		}
		if f.emptyFirstPage && values.Get("NextToken") == "" {
			writeAWSResponse(f.t, w, `<DescribeSnapshotsResponse><snapshotSet/><nextToken>page-0</nextToken></DescribeSnapshotsResponse>`)

			return
		}
		start := 0
		if next := values.Get("NextToken"); next != "" {
			if _, err := fmt.Sscanf(next, "page-%d", &start); err != nil {
				f.t.Errorf("NextToken = %q", next)
			}
		}
		end := len(visible)
		nextToken := ""
		if f.pageSize > 0 && start+f.pageSize < len(visible) {
			end = start + f.pageSize
			nextToken = fmt.Sprintf("<nextToken>page-%d</nextToken>", end)
		}
		var items strings.Builder
		for _, s := range visible[start:end] {
			items.WriteString(`<item><snapshotId>` + s.id + `</snapshotId><status>` + s.state + `</status><startTime>2026-08-16T12:00:00.000Z</startTime></item>`)
		}
		writeAWSResponse(f.t, w, `<DescribeSnapshotsResponse><snapshotSet>`+items.String()+`</snapshotSet>`+nextToken+`</DescribeSnapshotsResponse>`)
	default:
		f.t.Fatalf("unexpected action %q", values.Get("Action"))
	}
}

// A snapshot token is spent at most once, and without a ClientToken that is
// billet's to keep: a refusal AWS parsed is never retried, an ambiguous answer
// is reconciled against an eventually-consistent listing before any second
// create, a token another attempt spent is found on any page and in any state,
// and two live snapshots under one token are refused.
func TestASnapshotTokenIsSpentAtMostOnce(t *testing.T) {
	t.Parallel()

	const hidden = 5
	ambiguous := tokenAttempt{status: http.StatusInternalServerError}
	lostButActed := tokenAttempt{status: http.StatusInternalServerError, acts: true}

	cases := []struct {
		name         string
		fake         tokenFake
		wantCreates  int
		wantSnapshot string
		wantErr      string
		minWaits     int
	}{
		{name: "the response to the first attempt is lost", fake: tokenFake{attempts: []tokenAttempt{lostButActed}},
			wantCreates: 1, wantSnapshot: "snap-c1", minWaits: hidden - 1},
		{name: "an unparseable 4xx is ambiguous, not a refusal",
			fake:        tokenFake{attempts: []tokenAttempt{{status: http.StatusBadRequest, acts: true}}},
			wantCreates: 1, wantSnapshot: "snap-c1", minWaits: hidden - 1},
		{name: "another attempt already spent the token", fake: tokenFake{pre: []string{"pending"}},
			wantCreates: 0, wantSnapshot: "snap-p1"},
		{name: "a refusal AWS parsed is not retried",
			fake:        tokenFake{attempts: []tokenAttempt{{status: http.StatusBadRequest, code: "InvalidParameterValue"}}},
			wantCreates: 1, wantErr: "InvalidParameterValue"},
		{name: "an ambiguous answer with nothing created earns one more create",
			fake:        tokenFake{attempts: []tokenAttempt{ambiguous}},
			wantCreates: 2, wantSnapshot: "snap-c2", minWaits: snapshotReconcileAttempts - 1},
		{name: "only the second ambiguous answer acts, and late",
			fake:        tokenFake{attempts: []tokenAttempt{ambiguous, lostButActed}},
			wantCreates: 2, wantSnapshot: "snap-c2", minWaits: snapshotReconcileAttempts - 1 + hidden - 1},
		{name: "two ambiguous answers with nothing created earn no third",
			fake:        tokenFake{attempts: []tokenAttempt{ambiguous, ambiguous}},
			wantCreates: 2, wantErr: "not creating a third", minWaits: 2 * (snapshotReconcileAttempts - 1)},
		{name: "a snapshot that entered error state spent the token", fake: tokenFake{pre: []string{"error"}},
			wantCreates: 0, wantErr: "entered error state"},
		{name: "two live snapshots carrying one token are refused", fake: tokenFake{pre: []string{"pending", "completed"}},
			wantCreates: 0, wantErr: "2 snapshots carry one request token"},
		{name: "a match behind an empty first page is found", fake: tokenFake{pre: []string{"pending"}, emptyFirstPage: true},
			wantCreates: 0, wantSnapshot: "snap-p1"},
		{name: "a duplicate on the second page is refused", fake: tokenFake{pre: []string{"pending", "pending"}, pageSize: 1},
			wantCreates: 0, wantErr: "2 snapshots carry one request token"},
		{name: "a listing that repeats its page token is refused", fake: tokenFake{cycle: true},
			wantCreates: 0, wantErr: "repeated its token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := tc.fake
			fake.t = t
			fake.lookupsUntilVisible = hidden
			server := httptest.NewServer(http.HandlerFunc(fake.handle))
			defer server.Close()

			var waits int
			api := newEBSAPI(config.EBSS3Config{
				Region: "us-west-2", AvailabilityZone: "us-west-2a",
			}, "deployment/site", staticCredentials{}, server.Client(), server.URL,
				func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) },
				func(context.Context, time.Duration) error { waits++; return nil })
			snapshot, err := api.CreateSnapshot(t.Context(), "vol-123",
				time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), "snapshot-token")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("CreateSnapshot = %q, %v; want an error containing %q", snapshot, err, tc.wantErr)
				}
			} else if err != nil || snapshot != tc.wantSnapshot {
				t.Fatalf("CreateSnapshot = %q, %v; want %q", snapshot, err, tc.wantSnapshot)
			}
			if fake.creates != tc.wantCreates {
				t.Fatalf("CreateSnapshot sent %d time(s), want %d", fake.creates, tc.wantCreates)
			}
			if fake.lookups == 0 {
				t.Fatal("the token was never looked up before creating")
			}
			// The waits are what prove reconciliation POLLED rather than looked once:
			// a snapshot hidden for `hidden` lookups can only be found after at least
			// hidden-1 waits, and an ambiguous answer that never acts must be given
			// the whole window before a second create is risked.
			if waits < tc.minWaits {
				t.Fatalf("reconciliation waited %d time(s), want at least %d", waits, tc.minWaits)
			}
		})
	}
}

// waitSnapshot has no deadline of its own, so which states it waits out is a
// CLOSED set: the ones AWS leaves by itself. An archived snapshot and a state
// this binary has never heard of are refused before a single wait.
func TestWaitSnapshotWaitsOnlyForStatesAWSLeavesOnItsOwn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		states    []string // answered in order; the last one repeats
		wantErr   string
		wantWaits int // exact: a transitional state must be waited out, a terminal one never
	}{
		{name: "completed", states: []string{"completed"}},
		{name: "error", states: []string{"error"}, wantErr: "entered error state"},
		{name: "pending then completed", states: []string{"pending", "pending", "completed"}, wantWaits: 2},
		{name: "recovering then completed", states: []string{"recovering", "completed"}, wantWaits: 1},
		{name: "recoverable is refused without waiting", states: []string{"recoverable"},
			wantErr: "does not wait out"},
		{name: "an unknown state is refused without waiting", states: []string{"frobnicating"},
			wantErr: "does not wait out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var asks int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				values, err := url.ParseQuery(string(body))
				if err != nil {
					t.Fatal(err)
				}
				if values.Get("Action") != "DescribeSnapshots" || values.Get("SnapshotId.1") != "snap-1" {
					t.Fatalf("unexpected request %v", values)
				}
				// A regression that loops around DescribeSnapshots without the
				// injected wait must fail here rather than hang the suite.
				if asks >= 10 {
					t.Errorf("waitSnapshot asked %d times without settling", asks)
					w.WriteHeader(http.StatusInternalServerError)

					return
				}
				state := tc.states[min(asks, len(tc.states)-1)]
				asks++
				w.Header().Set("Content-Type", "text/xml")
				writeAWSResponse(t, w, `<DescribeSnapshotsResponse><snapshotSet><item><snapshotId>snap-1</snapshotId><status>`+state+`</status><startTime>2026-08-16T12:00:00.000Z</startTime></item></snapshotSet></DescribeSnapshotsResponse>`)
			}))
			defer server.Close()

			var waits int
			api := newEBSAPI(config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a"},
				"deployment/site", staticCredentials{}, server.Client(), server.URL, time.Now,
				func(context.Context, time.Duration) error {
					waits++
					if waits > 10 {
						return errors.New("test: waited too many times")
					}

					return nil
				})
			err := api.waitSnapshot(t.Context(), "snap-1")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("waitSnapshot = %v; want an error containing %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("waitSnapshot = %v", err)
			}
			if waits != tc.wantWaits {
				t.Fatalf("waited %d time(s), want exactly %d", waits, tc.wantWaits)
			}
		})
	}
}

// An empty token would make every retry find every other attempt's snapshot,
// so it is refused before anything is asked of AWS.
func TestCreateSnapshotRefusesAnEmptyToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("AWS was asked something for an empty token")
	}))
	defer server.Close()

	api := newEBSAPI(config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a",
	}, "deployment/site", staticCredentials{}, server.Client(), server.URL,
		time.Now, func(context.Context, time.Duration) error { return nil })
	if _, err := api.CreateSnapshot(t.Context(), "vol-123", time.Now(), " "); err == nil {
		t.Fatal("CreateSnapshot accepted an empty token")
	}
}

func TestTargetedDeletesRefuseResourcesWithoutThisStoresOwnershipTags(t *testing.T) {
	t.Parallel()

	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		switch values.Get("Action") {
		case "DescribeVolumes":
			writeAWSResponse(t, w, `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-foreign</volumeId><tagSet><item><key>sh.billet.owner</key><value>other</value></item><item><key>sh.billet.cache-owner</key><value>other/site</value></item></tagSet></item></volumeSet></DescribeVolumesResponse>`)
		case "DescribeSnapshots":
			writeAWSResponse(t, w, `<DescribeSnapshotsResponse><snapshotSet><item><snapshotId>snap-foreign</snapshotId><status>completed</status><tagSet><item><key>sh.billet.owner</key><value>other</value></item><item><key>sh.billet.cache-owner</key><value>other/site</value></item></tagSet></item></snapshotSet></DescribeSnapshotsResponse>`)
		case "DeleteVolume", "DeleteSnapshot":
			deleted = true
		default:
			t.Fatalf("unexpected action %q", values.Get("Action"))
		}
	}))
	defer server.Close()

	api := newEBSAPI(config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a"},
		"deployment/site", staticCredentials{}, server.Client(), server.URL, time.Now,
		func(context.Context, time.Duration) error { return nil })
	// The refusal is the errNotOwned SENTINEL, not merely some error, because Evict
	// relies on errors.Is to tell a foreign resource (skip) from a real failure.
	if err := api.DeleteVolume(t.Context(), "vol-foreign"); !errors.Is(err, errNotOwned) {
		t.Fatalf("DeleteVolume of a foreign resource: got %v, want errNotOwned", err)
	}
	if err := api.DeleteSnapshot(t.Context(), "snap-foreign"); !errors.Is(err, errNotOwned) {
		t.Fatalf("DeleteSnapshot of a foreign resource: got %v, want errNotOwned", err)
	}
	if deleted {
		t.Fatal("a targeted delete reached EC2 for a foreign cache resource")
	}
}

func TestS3ListParsesEveryStateObject(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("prefix") != "state/" {
			t.Errorf("query = %v", r.URL.Query())
		}
		response := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Contents    []string `xml:"Contents>Key"`
			IsTruncated bool     `xml:"IsTruncated"`
		}{Contents: []string{"state/a.json", "state/b.json"}}
		if err := xml.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
		staticCredentials{}, server.Client(), server.URL, time.Now)
	keys, err := api.List(t.Context(), "state/")
	if err != nil || strings.Join(keys, ",") != "state/a.json,state/b.json" {
		t.Fatalf("List = %v, %v", keys, err)
	}
}

func TestEveryAWSPaginationCycleIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("S3", func(t *testing.T) {
		t.Parallel()

		var page int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			tokens := []string{"A", "B", "A"}
			writeAWSResponse(t, w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>`+
				tokens[page%len(tokens)]+`</NextContinuationToken></ListBucketResult>`)
			page++
		}))
		defer server.Close()

		api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
			staticCredentials{}, server.Client(), server.URL, time.Now)
		if _, err := api.List(t.Context(), "state/"); err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("S3 listing cycle error = %v", err)
		}
		if page != 3 {
			t.Fatalf("S3 listing made %d requests, want 3", page)
		}
	})

	for _, action := range []string{"DescribeSnapshots", "DescribeVolumes"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			var page int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				values, err := url.ParseQuery(string(body))
				if err != nil {
					t.Fatal(err)
				}
				if values.Get("Action") != action {
					t.Fatalf("action = %q, want %q", values.Get("Action"), action)
				}
				tokens := []string{"A", "B", "A"}
				writeAWSResponse(t, w, "<"+action+"Response><nextToken>"+tokens[page%len(tokens)]+
					"</nextToken></"+action+"Response>")
				page++
			}))
			defer server.Close()

			api := newEBSAPI(config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a"},
				"deployment/site", staticCredentials{}, server.Client(), server.URL, time.Now,
				func(context.Context, time.Duration) error { return nil })
			var err error
			if action == "DescribeSnapshots" {
				_, err = api.ListSnapshots(t.Context())
			} else {
				_, err = api.ListAvailableVolumes(t.Context())
			}
			if err == nil || !strings.Contains(err.Error(), "token") {
				t.Fatalf("EBS listing cycle error = %v", err)
			}
			if page != 3 {
				t.Fatalf("EBS listing made %d requests, want 3", page)
			}
		})
	}
}

type leakyCredentialSource struct {
	Secret string
}

func (l leakyCredentialSource) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{AccessKeyID: "AKID", SecretAccessKey: l.Secret}, nil
}

func TestCredentialHoldingAWSStoresRedactEveryRenderingPath(t *testing.T) {
	t.Parallel()

	const secret = "must-never-render"
	creds := leakyCredentialSource{Secret: secret}
	httpClient := &http.Client{}
	cfg := config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "billet-cache-example",
	}
	s3 := newS3API(cfg, creds, httpClient, "https://s3.example", time.Now)
	ebs := newEBSAPI(cfg, "deployment/site", creds, httpClient, "https://ec2.example",
		time.Now, func(context.Context, time.Duration) error { return nil })
	store := newStore(cfg, "deployment/site", ebs, s3)

	for name, value := range map[string]any{
		"s3 pointer": s3, "s3 value": *s3,
		"ebs pointer": ebs, "ebs value": *ebs,
		"store pointer": store, "store value": *store,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			jsonBody, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("json: %v", err)
			}
			var log bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&log, nil))
			logger.Info("value", "value", value)
			for path, rendered := range map[string]string{
				"fmt":  fmt.Sprintf("%+v %#v %q", value, value, value),
				"json": string(jsonBody),
				"slog": log.String(),
			} {
				if strings.Contains(rendered, secret) {
					t.Errorf("%s exposed the credential source: %s", path, rendered)
				}
			}
		})
	}
}

func writeAWSResponse(t *testing.T, w io.Writer, response string) {
	t.Helper()
	if _, err := io.WriteString(w, response); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// s3API.Delete is a signed, empty-body DELETE that treats S3's idempotent 204
// (whether or not the object existed) as success, and surfaces any other status.
func TestS3DeleteIsSignedIdempotentAndSurfacesErrors(t *testing.T) {
	t.Parallel()

	emptyBodyHash := awssig.SHA256Hex(nil)

	var status int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		if r.URL.Path != "/state/key.json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/s3/aws4_request") {
			t.Errorf("delete is not sigv4-signed: %v", r.Header)
		}
		// An empty-body DELETE: the content hash is the empty-body hash and there is
		// no body.
		if r.Header.Get("X-Amz-Content-Sha256") != emptyBodyHash || r.ContentLength != 0 {
			t.Errorf("delete carried a body: hash=%q len=%d", r.Header.Get("X-Amz-Content-Sha256"), r.ContentLength)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()

	api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
		staticCredentials{}, server.Client(), server.URL, func() time.Time {
			return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
		})

	// 204 (deleted, or already gone) is success.
	status = http.StatusNoContent
	if err := api.Delete(t.Context(), "state/key.json"); err != nil {
		t.Fatalf("Delete(204): %v", err)
	}
	// 200 is also success.
	status = http.StatusOK
	if err := api.Delete(t.Context(), "state/key.json"); err != nil {
		t.Fatalf("Delete(200): %v", err)
	}
	// Any other status is an error.
	status = http.StatusForbidden
	if err := api.Delete(t.Context(), "state/key.json"); err == nil {
		t.Fatal("Delete(403) did not surface an error")
	}
}

// THE STORE'S TWO ENDPOINTS FOLLOW THE PARTITION.
//
// Both were built by hand with the commercial suffix, which names hosts that do
// not exist in cn-north-1 — and config admits such a region deliberately, and
// the terraform module renders an aws-cn policy for it. So the grant applied,
// the listener came up, the bucket existed, and the store behind it could reach
// neither the EBS API nor its own objects: every job fetched cold with the
// whole cache correct on paper.
//
// READ BACK OUT OF THE STORE `New` BUILT, not recomputed from awsjson here: a
// test that asks awsjson the same question awsjson answered proves only that
// awsjson is consistent with itself, and would stay green with this file's two
// lines unchanged.
//
// Mutation: restoring ".amazonaws.com" to either line fails the China case.
func TestTheStoreEndpointsFollowThePartition(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		region, ebs, s3 string
	}{
		{
			"us-west-2",
			"https://ec2.us-west-2.amazonaws.com/",
			"https://billet-cache.s3.us-west-2.amazonaws.com/",
		},
		{
			"cn-north-1",
			"https://ec2.cn-north-1.amazonaws.com.cn/",
			"https://billet-cache.s3.cn-north-1.amazonaws.com.cn/",
		},
	} {
		store, err := New(config.EBSS3Config{
			Region:           c.region,
			AvailabilityZone: c.region + "a",
			Bucket:           "billet-cache",
			Prefix:           "billet-cache",
		}, "deployment/site", staticCredentials{})
		if err != nil {
			t.Fatalf("%s: %v", c.region, err)
		}

		blocks, ok := store.blocks.(*ebsAPI)
		if !ok {
			t.Fatalf("%s: the store's block API is %T", c.region, store.blocks)
		}

		if blocks.endpoint != c.ebs {
			t.Errorf("%s reaches EBS at %q, want %q", c.region, blocks.endpoint, c.ebs)
		}

		objects, ok := store.objects.(*s3API)
		if !ok {
			t.Fatalf("%s: the store's object API is %T", c.region, store.objects)
		}

		if objects.endpoint != c.s3 {
			t.Errorf("%s reaches its objects at %q, want %q", c.region, objects.endpoint, c.s3)
		}
	}
}
