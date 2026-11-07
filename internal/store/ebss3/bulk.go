package ebss3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awss3"
	"github.com/junioryono/billet/internal/awssig"
)

const (
	// metaUpload names the upload that committed an archive, as object metadata,
	// which is how a retried commit tells its own lost answer from another writer.
	metaUpload = "X-Amz-Meta-Billet-Upload"
	// bulkHeaderLimit bounds waiting for S3's answer once a request is written. A
	// body has no bound but its context: a guest's block takes what its link takes.
	bulkHeaderLimit = 2 * time.Minute
	// bulkConnections is how many connections to S3 stay open for reuse, which is
	// what a node serving several restores at once holds in flight.
	bulkConnections = 64
	// bulkAttempts is how many times an idempotent request is tried. A body read
	// from a guest cannot be sent twice, so an upload is tried once and the client's
	// own retry is what covers it.
	bulkAttempts = 3
	bulkBackoff  = 500 * time.Millisecond
)

// objectInfo is one object of a listing.
type objectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
	ETag     string
}

// multipartInfo is one multipart upload S3 has not finished.
type multipartInfo struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// completedPart is one part of a multipart upload, as S3 named it.
type completedPart struct {
	Number int
	ETag   string
}

// errObjectAbsent is S3 saying NoSuchKey, and nothing else: a 404 for a bucket is
// a fault (see s3API.Get).
var errObjectAbsent = errors.New("ebs-s3: no such object")

// bulkAPI is the part of S3 whole objects need and the small state objects do not:
// bodies too large to hold, ranges, and listings that say more than a name.
type bulkAPI interface {
	PutStream(ctx context.Context, key string, body io.Reader, size int64, ifAbsent bool) error
	GetRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, error)
	GetAll(ctx context.Context, key string) (io.ReadCloser, error)
	Head(ctx context.Context, key string) (objectInfo, string, error)
	ListObjects(ctx context.Context, prefix string) ([]objectInfo, error)
	CreateMultipart(ctx context.Context, key, upload string) (string, error)
	CopyPart(ctx context.Context, key, uploadID string, number int, source string) (completedPart, error)
	UploadPart(ctx context.Context, key, uploadID string, number int, body []byte) (completedPart, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []completedPart) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
	ListMultipart(ctx context.Context, prefix string) ([]multipartInfo, error)
}

// bulkClient is the transport of whole objects. Not the state client's, whose
// thirty-second bound on a whole exchange would cut every large transfer.
func bulkClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	transport = transport.Clone()
	transport.ResponseHeaderTimeout = bulkHeaderLimit
	transport.MaxIdleConnsPerHost = bulkConnections
	transport.MaxConnsPerHost = 0

	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// send signs and sends one request. A nil body is signed as empty; a stream is
// signed as an unsigned payload, over the TLS every request here uses.
func (s s3API) send(
	ctx context.Context,
	method, key string,
	query url.Values,
	body io.Reader,
	size int64,
	headers http.Header,
) (*http.Response, error) {
	base, err := url.Parse(s.endpoint)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: invalid S3 endpoint: %w", err)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/" + key
	base.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, base.String(), body)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: build S3 request: %w", err)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	credentials, err := s.creds.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: resolve AWS credentials for S3: %w", err)
	}
	req.ContentLength = size
	if body == nil {
		req.Header.Set("X-Amz-Content-Sha256", awssig.SHA256Hex(nil))
		err = awssig.Sign(req, nil, credentials, s.cfg.Region, "s3", s.now())
	} else {
		err = awssig.SignStream(req, credentials, s.cfg.Region, "s3", s.now())
	}
	if err != nil {
		return nil, err
	}
	response, err := s.bulk.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: call S3: %w", err)
	}

	return response, nil
}

// idempotent tries an idempotent call again when S3 was unreachable or asked it
// to slow down, and returns the last failure otherwise.
func idempotent[T any](ctx context.Context, call func() (T, error)) (T, error) {
	var (
		value T
		err   error
	)
	for attempt := range bulkAttempts {
		value, err = call()
		if err == nil || !retryable(err) || attempt == bulkAttempts-1 {
			return value, err
		}
		if err := waitContext(ctx, bulkBackoff<<attempt); err != nil {
			return value, err
		}
	}

	return value, err
}

// retryable is a failure a second attempt can change: not a refusal, not absence.
func retryable(err error) bool {
	if errors.Is(err, errObjectAbsent) || errors.Is(err, errObjectConflict) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if status := awss3.StatusOf(err); status != 0 {
		return status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
	}

	return true
}

// PutStream stores one object from a reader of known length.
func (s s3API) PutStream(
	ctx context.Context, key string, body io.Reader, size int64, ifAbsent bool,
) error {
	headers := make(http.Header)
	headers.Set("X-Amz-Server-Side-Encryption", "AES256")
	if ifAbsent {
		headers.Set("If-None-Match", "*")
	}
	if size == 0 {
		// A zero-length body is what net/http sends for http.NoBody. For any other
		// reader it first reads to see whether there is a body, and a reader slower
		// than its short wait goes chunked, which S3 refuses on a PUT.
		body = http.NoBody
	}
	response, err := s.send(ctx, http.MethodPut, key, nil, body, size, headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusConflict, http.StatusPreconditionFailed:
		return errObjectConflict
	default:
		return s.refusalError("PUT", awss3.ReadRefusal(response), response)
	}
}

// GetRange reads length bytes of an object from start, if it is still the object
// etag names, when one is named.
func (s s3API) GetRange(
	ctx context.Context, key, etag string, start, length int64,
) (io.ReadCloser, error) {
	headers := make(http.Header)
	headers.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10))
	if etag != "" {
		headers.Set("If-Match", etag)
	}

	return idempotent(ctx, func() (io.ReadCloser, error) {
		return s.get(ctx, key, headers, http.StatusPartialContent)
	})
}

// GetAll reads a whole object.
func (s s3API) GetAll(ctx context.Context, key string) (io.ReadCloser, error) {
	return idempotent(ctx, func() (io.ReadCloser, error) {
		return s.get(ctx, key, nil, http.StatusOK)
	})
}

func (s s3API) get(
	ctx context.Context, key string, headers http.Header, want int,
) (io.ReadCloser, error) {
	response, err := s.send(ctx, http.MethodGet, key, nil, nil, 0, headers)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != want {
		defer response.Body.Close()
		refusal := awss3.ReadRefusal(response)
		if refusal.Absent() {
			return nil, errObjectAbsent
		}

		return nil, s.refusalError("GET", refusal, response)
	}

	return response.Body, nil
}

// Head says what S3 holds at a key, and which upload committed it.
//
// A HEAD HAS NO BODY, SO IT CARRIES NO ERROR CODE, and a 404 here cannot tell an
// absent object from an absent bucket by the rule every GET holds to. The caller
// that needs the difference asks a GET; this one's 404 reads as absent only because
// the bucket answered a state read before any archive was asked about.
func (s s3API) Head(ctx context.Context, key string) (objectInfo, string, error) {
	type answer struct {
		info   objectInfo
		upload string
	}
	got, err := idempotent(ctx, func() (answer, error) {
		response, err := s.send(ctx, http.MethodHead, key, nil, nil, 0, nil)
		if err != nil {
			return answer{}, err
		}
		defer response.Body.Close()
		switch response.StatusCode {
		case http.StatusOK:
		case http.StatusNotFound:
			return answer{}, errObjectAbsent
		default:
			return answer{}, s.refusalError("HEAD", awss3.ReadRefusal(response), response)
		}
		size, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
		if err != nil || size < 0 {
			return answer{}, errors.New("ebs-s3: S3 answered a HEAD with no usable length")
		}
		modified, err := http.ParseTime(response.Header.Get("Last-Modified"))
		if err != nil {
			return answer{}, errors.New("ebs-s3: S3 answered a HEAD with no usable date")
		}

		return answer{objectInfo{Key: key, Size: size, Modified: modified}, response.Header.Get(metaUpload)}, nil
	})

	return got.info, got.upload, err
}

// ListObjects lists every object under a prefix with its size and date.
func (s s3API) ListObjects(ctx context.Context, prefix string) ([]objectInfo, error) {
	var objects []objectInfo
	continuation := ""
	seen := map[string]bool{}
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if continuation != "" {
			query.Set("continuation-token", continuation)
		}
		var page struct {
			Contents []struct {
				Key      string    `xml:"Key"`
				Size     int64     `xml:"Size"`
				Modified time.Time `xml:"LastModified"`
				ETag     string    `xml:"ETag"`
			} `xml:"Contents"`
			Truncated    bool   `xml:"IsTruncated"`
			Continuation string `xml:"NextContinuationToken"`
		}
		if err := s.readXML(ctx, "LIST", http.MethodGet, "", query, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Contents {
			objects = append(objects, objectInfo{
				Key: item.Key, Size: item.Size, Modified: item.Modified, ETag: item.ETag,
			})
		}
		if !page.Truncated {
			return objects, nil
		}
		if page.Continuation == "" || seen[page.Continuation] {
			return nil, errors.New("ebs-s3: a truncated S3 listing has no new continuation token")
		}
		seen[page.Continuation] = true
		continuation = page.Continuation
	}
}

// readXML makes one idempotent request and decodes its answer.
func (s s3API) readXML(
	ctx context.Context, op, method, key string, query url.Values, into any,
) error {
	_, err := idempotent(ctx, func() (struct{}, error) {
		response, err := s.send(ctx, method, key, query, nil, 0, nil)
		if err != nil {
			return struct{}{}, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, awsBodyLimit+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return struct{}{}, fmt.Errorf("ebs-s3: read S3 %s: %w", op, errors.Join(readErr, closeErr))
		}
		if response.StatusCode != http.StatusOK {
			return struct{}{}, s.refusalError(op, awss3.ParseRefusal(response.StatusCode, payload), response)
		}
		if len(payload) > awsBodyLimit {
			return struct{}{}, fmt.Errorf("ebs-s3: S3 %s exceeds 8 MiB", op)
		}
		if err := xml.Unmarshal(payload, into); err != nil {
			return struct{}{}, fmt.Errorf("ebs-s3: parse S3 %s: %w", op, err)
		}

		return struct{}{}, nil
	})

	return err
}

// CreateMultipart starts the upload an archive is assembled by. upload is recorded
// on the object as it will be published.
func (s s3API) CreateMultipart(ctx context.Context, key, upload string) (string, error) {
	return idempotent(ctx, func() (string, error) {
		headers := make(http.Header)
		headers.Set("X-Amz-Server-Side-Encryption", "AES256")
		headers.Set(metaUpload, upload)
		response, err := s.send(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, 0, headers)
		if err != nil {
			return "", err
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, awsBodyLimit+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return "", fmt.Errorf("ebs-s3: read S3 multipart start: %w", errors.Join(readErr, closeErr))
		}
		if response.StatusCode != http.StatusOK {
			return "", s.refusalError("multipart start", awss3.ParseRefusal(response.StatusCode, payload), response)
		}
		var result struct {
			UploadID string `xml:"UploadId"`
		}
		if err := xml.Unmarshal(payload, &result); err != nil || result.UploadID == "" {
			return "", errors.New("ebs-s3: S3 started a multipart upload and named none")
		}

		return result.UploadID, nil
	})
}

func partQuery(uploadID string, number int) url.Values {
	return url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {uploadID}}
}

// CopyPart makes one object a part, inside S3, so assembling an archive moves no
// bytes through the node.
//
// S3 ANSWERS A COPY 200 AND CAN STILL HAVE FAILED: the body is what says so, and an
// <Error> there is the failure. Its ETag is what the commit has to quote.
func (s s3API) CopyPart(
	ctx context.Context, key, uploadID string, number int, source string,
) (completedPart, error) {
	return idempotent(ctx, func() (completedPart, error) {
		headers := make(http.Header)
		headers.Set("X-Amz-Copy-Source", "/"+s.cfg.Bucket+"/"+source)
		response, err := s.send(ctx, http.MethodPut, key, partQuery(uploadID, number), nil, 0, headers)
		if err != nil {
			return completedPart{}, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, awsBodyLimit+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return completedPart{}, fmt.Errorf("ebs-s3: read S3 part copy: %w", errors.Join(readErr, closeErr))
		}
		if response.StatusCode != http.StatusOK {
			return completedPart{}, s.refusalError("part copy",
				awss3.ParseRefusal(response.StatusCode, payload), response)
		}
		var result struct {
			XMLName xml.Name
			ETag    string `xml:"ETag"`
		}
		if err := xml.Unmarshal(payload, &result); err != nil {
			return completedPart{}, fmt.Errorf("ebs-s3: parse S3 part copy: %w", err)
		}
		if result.XMLName.Local == "Error" {
			return completedPart{}, s.refusalError("part copy",
				awss3.ParseRefusal(http.StatusInternalServerError, payload), response)
		}
		if result.ETag == "" {
			return completedPart{}, errors.New("ebs-s3: S3 copied a part and returned no ETag")
		}

		return completedPart{Number: number, ETag: result.ETag}, nil
	})
}

// UploadPart sends one part the node holds.
func (s s3API) UploadPart(
	ctx context.Context, key, uploadID string, number int, body []byte,
) (completedPart, error) {
	return idempotent(ctx, func() (completedPart, error) {
		response, err := s.send(ctx, http.MethodPut, key, partQuery(uploadID, number),
			bytes.NewReader(body), int64(len(body)), nil)
		if err != nil {
			return completedPart{}, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return completedPart{}, s.refusalError("part upload", awss3.ReadRefusal(response), response)
		}
		etag := response.Header.Get("ETag")
		if etag == "" {
			return completedPart{}, errors.New("ebs-s3: S3 took a part and returned no ETag")
		}

		return completedPart{Number: number, ETag: etag}, nil
	})
}

// CompleteMultipart publishes an assembled archive unless the key is already
// taken, in which case it answers errObjectConflict and the upload is left to abort.
//
// LIKE A COPY, A COMPLETION CAN ANSWER 200 AND HAVE FAILED, in its body, after S3
// spent minutes on it. A request that lost its answer may have landed, so this is
// not tried again here: the caller asks whose archive the key now holds.
func (s s3API) CompleteMultipart(
	ctx context.Context, key, uploadID string, parts []completedPart,
) error {
	var body bytes.Buffer
	body.WriteString("<CompleteMultipartUpload>")
	for _, part := range parts {
		body.WriteString("<Part><PartNumber>" + strconv.Itoa(part.Number) + "</PartNumber><ETag>")
		if err := xml.EscapeText(&body, []byte(part.ETag)); err != nil {
			return fmt.Errorf("ebs-s3: encode a part's ETag: %w", err)
		}
		body.WriteString("</ETag></Part>")
	}
	body.WriteString("</CompleteMultipartUpload>")

	headers := make(http.Header)
	headers.Set("If-None-Match", "*")
	response, err := s.send(ctx, http.MethodPost, key, url.Values{"uploadId": {uploadID}},
		bytes.NewReader(body.Bytes()), int64(body.Len()), headers)
	if err != nil {
		return fmt.Errorf("%w: %w", errObjectAmbiguous, err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, awsBodyLimit+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("%w: read S3 completion: %w", errObjectAmbiguous, errors.Join(readErr, closeErr))
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusConflict, http.StatusPreconditionFailed:
		return errObjectConflict
	default:
		if response.StatusCode >= http.StatusInternalServerError {
			return fmt.Errorf("%w: S3 completion returned %w", errObjectAmbiguous,
				awss3.ParseRefusal(response.StatusCode, payload))
		}

		return s.refusalError("completion", awss3.ParseRefusal(response.StatusCode, payload), response)
	}
	var result struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(payload, &result); err != nil || result.XMLName.Local != "CompleteMultipartUploadResult" {
		return fmt.Errorf("%w: S3 answered a completion 200 without completing it", errObjectAmbiguous)
	}

	return nil
}

// AbortMultipart releases an upload's parts. An upload already gone is not an
// error: a retried cleanup must be able to finish.
func (s s3API) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := idempotent(ctx, func() (struct{}, error) {
		response, err := s.send(ctx, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil, 0, nil)
		if err != nil {
			return struct{}{}, err
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusOK {
			return struct{}{}, nil
		}
		refusal := awss3.ReadRefusal(response)
		if refusal.Code == "NoSuchUpload" {
			return struct{}{}, nil
		}

		return struct{}{}, s.refusalError("multipart abort", refusal, response)
	})

	return err
}

// ListMultipart lists the uploads under a prefix that S3 has not finished, which
// no object listing shows and which S3 bills for all the same.
func (s s3API) ListMultipart(ctx context.Context, prefix string) ([]multipartInfo, error) {
	var uploads []multipartInfo
	keyMarker, uploadMarker := "", ""
	for {
		query := url.Values{"uploads": {""}, "prefix": {prefix}}
		if keyMarker != "" {
			query.Set("key-marker", keyMarker)
			query.Set("upload-id-marker", uploadMarker)
		}
		var page struct {
			Uploads []struct {
				Key       string    `xml:"Key"`
				UploadID  string    `xml:"UploadId"`
				Initiated time.Time `xml:"Initiated"`
			} `xml:"Upload"`
			Truncated    bool   `xml:"IsTruncated"`
			NextKey      string `xml:"NextKeyMarker"`
			NextUploadID string `xml:"NextUploadIdMarker"`
		}
		if err := s.readXML(ctx, "multipart listing", http.MethodGet, "", query, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Uploads {
			uploads = append(uploads, multipartInfo{Key: item.Key, UploadID: item.UploadID, Initiated: item.Initiated})
		}
		if !page.Truncated {
			return uploads, nil
		}
		if page.NextKey == "" || page.NextKey == keyMarker && page.NextUploadID == uploadMarker {
			return nil, errors.New("ebs-s3: a truncated multipart listing has no new marker")
		}
		keyMarker, uploadMarker = page.NextKey, page.NextUploadID
	}
}
