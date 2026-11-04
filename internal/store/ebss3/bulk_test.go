package ebss3

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awss3"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
)

func bulkServer(t *testing.T, handler http.HandlerFunc) *s3API {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "bucket"}, staticCredentials{},
		server.Client(), server.URL+"/", time.Now)
	api.bulk = server.Client()

	return api
}

// answer writes a response body.
func answer(t *testing.T, w io.Writer, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write the answer: %v", err)
	}
}

const (
	errorBody = `<?xml version="1.0"?><Error><Code>InternalError</Code><Message>nope</Message></Error>`
	noSuchKey = `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`
)

// A body too large to hold has no digest to sign, so it goes as an unsigned payload,
// and S3 checks what it can only over TLS.
func TestAStreamedPutIsUnsignedAndCarriesItsLength(t *testing.T) {
	var seen *http.Request
	var body []byte
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			t.Errorf("read the body: %v", err)
		}
	})

	// A READER net/http CANNOT SIZE, as a guest's block is: its length is the store's to say.
	if err := api.PutStream(t.Context(), "key", io.MultiReader(strings.NewReader("hello")), 5, true); err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if got := seen.Header.Get("X-Amz-Content-Sha256"); got != awssig.UnsignedPayload {
		t.Errorf("x-amz-content-sha256 = %q", got)
	}
	if seen.ContentLength != 5 || len(seen.TransferEncoding) != 0 {
		t.Errorf("length %d, encoding %v; S3 refuses chunked on a PUT", seen.ContentLength, seen.TransferEncoding)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q", body)
	}
	if got := seen.Header.Get("If-None-Match"); got != "*" {
		t.Errorf("If-None-Match = %q", got)
	}
	if !strings.Contains(seen.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization = %q", seen.Header.Get("Authorization"))
	}
	if got := seen.Header.Get("X-Amz-Server-Side-Encryption"); got != "AES256" {
		t.Errorf("encryption = %q", got)
	}
}

// unreadReader says whether anything read it.
type unreadReader struct{ read atomic.Bool }

func (r *unreadReader) Read([]byte) (int, error) {
	r.read.Store(true)

	return 0, io.EOF
}

// net/http READS A BODY OF UNKNOWN LENGTH TO LEARN WHETHER IT HAS ONE, and goes chunked
// if that takes too long, so an empty body is not given the chance.
func TestAnEmptyStreamedPutIsNotChunked(t *testing.T) {
	var seen *http.Request
	api := bulkServer(t, func(_ http.ResponseWriter, r *http.Request) { seen = r.Clone(r.Context()) })

	empty := &unreadReader{}
	if err := api.PutStream(t.Context(), "marker", empty, 0, false); err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if empty.read.Load() {
		t.Fatal("the transport was left to find out an empty body is empty")
	}
	if len(seen.TransferEncoding) != 0 {
		t.Fatalf("an empty marker went chunked: %v", seen.TransferEncoding)
	}
}

func TestAConditionalPutThatLostIsAConflict(t *testing.T) {
	for _, status := range []int{http.StatusPreconditionFailed, http.StatusConflict} {
		api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		err := api.PutStream(t.Context(), "key", strings.NewReader("x"), 1, true)
		if !errors.Is(err, errObjectConflict) {
			t.Errorf("status %d = %v, want errObjectConflict", status, err)
		}
	}
}

func TestA404IsAbsenceOnlyWhenS3SaysNoSuchKey(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		answer(t, w, noSuchKey)
	})
	if _, err := api.GetAll(t.Context(), "key"); !errors.Is(err, errObjectAbsent) {
		t.Fatalf("NoSuchKey = %v, want errObjectAbsent", err)
	}

	api = bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		answer(t, w, `<Error><Code>NoSuchBucket</Code></Error>`)
	})
	_, err := api.GetAll(t.Context(), "key")
	if err == nil || errors.Is(err, errObjectAbsent) {
		t.Fatalf("NoSuchBucket = %v; a missing bucket must not read as an empty cache", err)
	}
}

func TestARangedReadAsksForExactlyItsRange(t *testing.T) {
	var asked string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		answer(t, w, "abc")
	})
	body, err := api.GetRange(t.Context(), "key", "", 10, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer body.Close()
	if asked != "bytes=10-12" {
		t.Fatalf("Range = %q", asked)
	}
}

func TestARangedReadOfANamedObjectIsConditionalOnIt(t *testing.T) {
	var condition string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		condition = r.Header.Get("If-Match")
		w.WriteHeader(http.StatusPartialContent)
		answer(t, w, "abc")
	})
	body, err := api.GetRange(t.Context(), "key", `"e1"`, 0, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer body.Close()
	if condition != `"e1"` {
		t.Fatalf("If-Match = %q", condition)
	}
}

// S3 answers a whole body's range 200 when it ignores the header. Reading on from
// there would hand the client the wrong bytes.
func TestARangedReadRefusesAWholeObjectAnswer(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) { answer(t, w, "whole") })
	if _, err := api.GetRange(t.Context(), "key", "", 10, 3); err == nil ||
		!strings.Contains(err.Error(), "S3 GET returned") {
		t.Fatalf("a 200 to a ranged read = %v", err)
	}
}

func TestAPartCopyThatAnswers200AndFailedIsAFailure(t *testing.T) {
	var attempts atomic.Int32
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		answer(t, w, errorBody)
	})
	_, err := api.CopyPart(t.Context(), "key", "id", 1, "source")
	if err == nil {
		t.Fatal("a 200 whose body is an Error was taken for a copied part")
	}
	// READ AS THE REFUSAL IT IS, not merely as a body with no ETag in it.
	if status := awss3.StatusOf(err); status != http.StatusInternalServerError {
		t.Fatalf("the failure reads as status %d: %v", status, err)
	}
	if attempts.Load() != bulkAttempts {
		t.Fatalf("tried %d times, want %d: S3 says this is worth retrying", attempts.Load(), bulkAttempts)
	}
}

func TestAPartCopyQuotesTheETagAndNamesItsSource(t *testing.T) {
	var source string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		source = r.Header.Get("X-Amz-Copy-Source")
		answer(t, w, `<CopyPartResult><ETag>&quot;abc&quot;</ETag></CopyPartResult>`)
	})
	part, err := api.CopyPart(t.Context(), "key", "id", 3, "stage/up/block")
	if err != nil {
		t.Fatalf("CopyPart: %v", err)
	}
	if part.Number != 3 || part.ETag != `"abc"` {
		t.Fatalf("part = %+v", part)
	}
	if source != "/bucket/stage/up/block" {
		t.Fatalf("copy source = %q", source)
	}
}

func TestACompletionThatAnswers200AndFailedIsAmbiguousNotSuccess(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) { answer(t, w, errorBody) })
	err := api.CompleteMultipart(t.Context(), "key", "id", []completedPart{{Number: 1, ETag: `"a"`}})
	if !errors.Is(err, errObjectAmbiguous) {
		t.Fatalf("CompleteMultipart = %v, want errObjectAmbiguous", err)
	}
}

func TestACompletionIsConditionalAndNamesItsParts(t *testing.T) {
	var condition, body string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		condition = r.Header.Get("If-None-Match")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the body: %v", err)
		}
		body = string(raw)
		answer(t, w, `<CompleteMultipartUploadResult><ETag>"x"</ETag></CompleteMultipartUploadResult>`)
	})
	err := api.CompleteMultipart(t.Context(), "key", "id", []completedPart{
		{Number: 1, ETag: `"a"`}, {Number: 2, ETag: `"b"`},
	})
	if err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	if condition != "*" {
		t.Errorf("If-None-Match = %q; the first writer is what this condition decides", condition)
	}
	for _, part := range []string{
		"<Part><PartNumber>1</PartNumber><ETag>&#34;a&#34;</ETag></Part>",
		"<Part><PartNumber>2</PartNumber><ETag>&#34;b&#34;</ETag></Part>",
	} {
		if !strings.Contains(body, part) {
			t.Errorf("body lacks %s: %s", part, body)
		}
	}
}

func TestACompletionThatLostTheKeyIsAConflict(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
	})
	err := api.CompleteMultipart(t.Context(), "key", "id", []completedPart{{Number: 1, ETag: `"a"`}})
	if !errors.Is(err, errObjectConflict) {
		t.Fatalf("CompleteMultipart = %v, want errObjectConflict", err)
	}
}

func TestAnAbortOfAnUploadAlreadyGoneIsNotAFailure(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		answer(t, w, `<Error><Code>NoSuchUpload</Code></Error>`)
	})
	if err := api.AbortMultipart(t.Context(), "key", "id"); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
}

func TestAListingFollowsItsContinuationAndKeepsSizesAndDates(t *testing.T) {
	calls := 0
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("continuation-token") == "" {
			answer(t, w, `<ListBucketResult><IsTruncated>true</IsTruncated>`+
				`<NextContinuationToken>next</NextContinuationToken>`+
				`<Contents><Key>a</Key><Size>7</Size><LastModified>2026-11-04T01:02:03.000Z</LastModified>`+
				`<ETag>&quot;e1&quot;</ETag></Contents>`+
				`</ListBucketResult>`)

			return
		}
		answer(t, w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>b</Key><Size>9</Size><LastModified>2026-11-04T01:02:04.000Z</LastModified></Contents>`+
			`</ListBucketResult>`)
	})
	objects, err := api.ListObjects(t.Context(), "prefix/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if calls != 2 || len(objects) != 2 || objects[0].Size != 7 || objects[1].Key != "b" ||
		objects[1].Modified.IsZero() || objects[0].ETag != `"e1"` {
		t.Fatalf("calls %d, objects %+v", calls, objects)
	}
}

func TestATruncatedListingThatDoesNotMoveIsRefused(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(t, w, `<ListBucketResult><IsTruncated>true</IsTruncated>`+
			`<NextContinuationToken>same</NextContinuationToken></ListBucketResult>`)
	})
	if _, err := api.ListObjects(t.Context(), "prefix/"); err == nil ||
		!strings.Contains(err.Error(), "no new continuation token") {
		t.Fatalf("a listing that repeats its token = %v", err)
	}
}

func TestAMultipartListingFollowsItsMarkers(t *testing.T) {
	var asked []string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		asked = append(asked, query.Get("key-marker")+"|"+query.Get("upload-id-marker"))
		if _, ok := query["uploads"]; !ok || query.Get("prefix") != "prefix/" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if query.Get("key-marker") == "" {
			answer(t, w, `<ListMultipartUploadsResult><IsTruncated>true</IsTruncated>`+
				`<NextKeyMarker>k1</NextKeyMarker><NextUploadIdMarker>u1</NextUploadIdMarker>`+
				`<Upload><Key>k1</Key><UploadId>u1</UploadId><Initiated>2026-11-04T01:02:03.000Z</Initiated></Upload>`+
				`</ListMultipartUploadsResult>`)

			return
		}
		answer(t, w, `<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>`+
			`<Upload><Key>k2</Key><UploadId>u2</UploadId><Initiated>2026-11-04T01:02:04.000Z</Initiated></Upload>`+
			`</ListMultipartUploadsResult>`)
	})
	uploads, err := api.ListMultipart(t.Context(), "prefix/")
	if err != nil {
		t.Fatalf("ListMultipart: %v", err)
	}
	if len(uploads) != 2 || uploads[0].Key != "k1" || uploads[0].UploadID != "u1" ||
		uploads[1].UploadID != "u2" || uploads[1].Initiated.IsZero() {
		t.Fatalf("uploads = %+v", uploads)
	}
	if len(asked) != 2 || asked[1] != "k1|u1" {
		t.Fatalf("asked with markers %q", asked)
	}
}

func TestATruncatedMultipartListingThatDoesNotMoveIsRefused(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(t, w, `<ListMultipartUploadsResult><IsTruncated>true</IsTruncated>`+
			`<NextKeyMarker>k</NextKeyMarker><NextUploadIdMarker>u</NextUploadIdMarker></ListMultipartUploadsResult>`)
	})
	if _, err := api.ListMultipart(t.Context(), "prefix/"); err == nil ||
		!strings.Contains(err.Error(), "no new marker") {
		t.Fatalf("a listing that repeats its marker = %v", err)
	}
}

func TestSlowDownIsTriedAgainAndARefusalIsNot(t *testing.T) {
	var attempts atomic.Int32
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			answer(t, w, `<Error><Code>SlowDown</Code></Error>`)

			return
		}
		answer(t, w, "body")
	})
	body, err := api.GetAll(t.Context(), "key")
	if err != nil {
		t.Fatalf("GetAll after a SlowDown: %v", err)
	}
	_ = body.Close()
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}

	attempts.Store(0)
	api = bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
		answer(t, w, `<Error><Code>AccessDenied</Code></Error>`)
	})
	if _, err := api.GetAll(t.Context(), "key"); err == nil {
		t.Fatal("AccessDenied was accepted")
	}
	if attempts.Load() != 1 {
		t.Fatalf("a refusal was tried %d times", attempts.Load())
	}
}

func TestHeadReadsWhichUploadCommittedAnObject(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		w.Header().Set("Last-Modified", "Wed, 04 Nov 2026 01:02:03 GMT")
		w.Header().Set(metaUpload, "up1")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			t.Errorf("method = %s", r.Method)
		}
	})
	info, upload, err := api.Head(t.Context(), "key")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if info.Size != 12 || upload != "up1" || info.Modified.IsZero() {
		t.Fatalf("info %+v, upload %q", info, upload)
	}
}

func TestAMultipartStartRecordsTheUploadOnTheObject(t *testing.T) {
	var meta, marker, encryption string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		meta = r.Header.Get(metaUpload)
		encryption = r.Header.Get("X-Amz-Server-Side-Encryption")
		marker = r.URL.RawQuery
		answer(t, w, `<InitiateMultipartUploadResult><UploadId>mpu</UploadId></InitiateMultipartUploadResult>`)
	})
	id, err := api.CreateMultipart(t.Context(), "key", "up1")
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if id != "mpu" || meta != "up1" || !strings.Contains(marker, "uploads") {
		t.Fatalf("id %q, meta %q, query %q", id, meta, marker)
	}
	if encryption != "AES256" {
		t.Fatalf("encryption = %q; what the upload assembles is an object like any other", encryption)
	}
}
