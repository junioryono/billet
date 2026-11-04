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
		body, _ = io.ReadAll(r.Body)
	})

	if err := api.PutStream(t.Context(), "key", strings.NewReader("hello"), 5, true); err != nil {
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

func TestAnEmptyStreamedPutIsNotChunked(t *testing.T) {
	var seen *http.Request
	api := bulkServer(t, func(_ http.ResponseWriter, r *http.Request) { seen = r.Clone(r.Context()) })

	if err := api.PutStream(t.Context(), "marker", strings.NewReader(""), 0, false); err != nil {
		t.Fatalf("PutStream: %v", err)
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
		_, _ = io.WriteString(w, noSuchKey)
	})
	if _, err := api.GetAll(t.Context(), "key"); !errors.Is(err, errObjectAbsent) {
		t.Fatalf("NoSuchKey = %v, want errObjectAbsent", err)
	}

	api = bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchBucket</Code></Error>`)
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
		_, _ = io.WriteString(w, "abc")
	})
	body, err := api.GetRange(t.Context(), "key", 10, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer body.Close()
	if asked != "bytes=10-12" {
		t.Fatalf("Range = %q", asked)
	}
}

// S3 answers a whole body's range 200 when it ignores the header. Reading on from
// there would hand the client the wrong bytes.
func TestARangedReadRefusesAWholeObjectAnswer(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "whole") })
	if _, err := api.GetRange(t.Context(), "key", 10, 3); err == nil {
		t.Fatal("a 200 to a ranged read was accepted")
	}
}

func TestAPartCopyThatAnswers200AndFailedIsAFailure(t *testing.T) {
	var attempts atomic.Int32
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_, _ = io.WriteString(w, errorBody)
	})
	_, err := api.CopyPart(t.Context(), "key", "id", 1, "source")
	if err == nil {
		t.Fatal("a 200 whose body is an Error was taken for a copied part")
	}
	if attempts.Load() != bulkAttempts {
		t.Fatalf("tried %d times, want %d: S3 says this is worth retrying", attempts.Load(), bulkAttempts)
	}
}

func TestAPartCopyQuotesTheETagAndNamesItsSource(t *testing.T) {
	var source string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		source = r.Header.Get("X-Amz-Copy-Source")
		_, _ = io.WriteString(w, `<CopyPartResult><ETag>&quot;abc&quot;</ETag></CopyPartResult>`)
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
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, errorBody) })
	err := api.CompleteMultipart(t.Context(), "key", "id", []completedPart{{Number: 1, ETag: `"a"`}})
	if !errors.Is(err, errObjectAmbiguous) {
		t.Fatalf("CompleteMultipart = %v, want errObjectAmbiguous", err)
	}
}

func TestACompletionIsConditionalAndNamesItsParts(t *testing.T) {
	var condition, body string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		condition = r.Header.Get("If-None-Match")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"x"</ETag></CompleteMultipartUploadResult>`)
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
	if !strings.Contains(body, "<PartNumber>1</PartNumber>") || !strings.Contains(body, "<PartNumber>2</PartNumber>") {
		t.Errorf("body = %s", body)
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
		_, _ = io.WriteString(w, `<Error><Code>NoSuchUpload</Code></Error>`)
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
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated>`+
				`<NextContinuationToken>next</NextContinuationToken>`+
				`<Contents><Key>a</Key><Size>7</Size><LastModified>2026-11-04T01:02:03.000Z</LastModified></Contents>`+
				`</ListBucketResult>`)

			return
		}
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>b</Key><Size>9</Size><LastModified>2026-11-04T01:02:04.000Z</LastModified></Contents>`+
			`</ListBucketResult>`)
	})
	objects, err := api.ListObjects(t.Context(), "prefix/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if calls != 2 || len(objects) != 2 || objects[0].Size != 7 || objects[1].Key != "b" ||
		objects[1].Modified.IsZero() {
		t.Fatalf("calls %d, objects %+v", calls, objects)
	}
}

func TestATruncatedListingThatDoesNotMoveIsRefused(t *testing.T) {
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated>`+
			`<NextContinuationToken>same</NextContinuationToken></ListBucketResult>`)
	})
	if _, err := api.ListObjects(t.Context(), "prefix/"); err == nil {
		t.Fatal("a listing that repeats its token was followed")
	}
}

func TestSlowDownIsTriedAgainAndARefusalIsNot(t *testing.T) {
	var attempts atomic.Int32
	api := bulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)

			return
		}
		_, _ = io.WriteString(w, "body")
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
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
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
	var meta, marker string
	api := bulkServer(t, func(w http.ResponseWriter, r *http.Request) {
		meta = r.Header.Get(metaUpload)
		_, marker = "", r.URL.RawQuery
		_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>mpu</UploadId></InitiateMultipartUploadResult>`)
	})
	id, err := api.CreateMultipart(t.Context(), "key", "up1")
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if id != "mpu" || meta != "up1" || !strings.Contains(marker, "uploads") {
		t.Fatalf("id %q, meta %q, query %q", id, meta, marker)
	}
}
