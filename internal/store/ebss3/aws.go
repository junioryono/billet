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
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awss3"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
)

const (
	ebsAPIVersion = "2016-11-15"
	awsBodyLimit  = 8 << 20
	awsPollDelay  = 2 * time.Second
	ownerTag      = "sh.billet.owner"
	cacheOwnerTag = "sh.billet.cache-owner"
	// snapshotTokenTag carries the request token a snapshot was created under,
	// because EC2's CreateSnapshot takes no ClientToken — see CreateSnapshot.
	snapshotTokenTag = "sh.billet.snapshot-token"
)

// CredentialSource supplies the temporary or static credential used for both
// EBS and S3. Implementations must redact their own diagnostic rendering.
//
// AN ALIAS FOR awscreds.Source. It was a declaration, and so were the identically
// shaped ones in internal/archivestore and internal/provider/codebuild, because
// the chain lived inside internal/provider/ec2 and a store may not import a
// compute backend. Four names for one method, bridged by a conversion closure at
// every call site. The chain is a shared package now, and a store may import it.
type CredentialSource = awscreds.Source

// New constructs the AWS cache store for one deployment and site.
func New(cfg config.EBSS3Config, owner string, credentials CredentialSource) (*Store, error) {
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.AvailabilityZone = strings.TrimSpace(cfg.AvailabilityZone)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Prefix = strings.TrimSpace(cfg.Prefix)
	cfg.KMSKeyID = strings.TrimSpace(cfg.KMSKeyID)
	if cfg.Prefix == "" {
		cfg.Prefix = "billet-cache"
	}
	if err := errors.Join(config.CheckEBSS3(cfg)...); err != nil {
		return nil, fmt.Errorf("ebs-s3: %w", err)
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || strings.ContainsRune(owner, 0) {
		return nil, errors.New("ebs-s3: a store needs a deployment and site owner")
	}
	if credentials == nil || nilCredentialSource(credentials) {
		return nil, errors.New("ebs-s3: a store needs an AWS credential source")
	}

	httpClient := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ebsEndpoint := "https://ec2." + cfg.Region + ".amazonaws.com/"
	s3Endpoint := "https://" + cfg.Bucket + ".s3." + cfg.Region + ".amazonaws.com/"
	now := time.Now
	blocks := newEBSAPI(cfg, owner, credentials, httpClient, ebsEndpoint, now, waitContext)
	objects := newS3API(cfg, credentials, httpClient, s3Endpoint, now)

	return newStore(cfg, owner, blocks, objects), nil
}

func nilCredentialSource(credentials CredentialSource) bool {
	value := reflect.ValueOf(credentials)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type s3API struct {
	cfg      config.EBSS3Config
	creds    CredentialSource
	http     *http.Client
	endpoint string
	now      func() time.Time
}

func newS3API(
	cfg config.EBSS3Config,
	creds CredentialSource,
	httpClient *http.Client,
	endpoint string,
	now func() time.Time,
) *s3API {
	return &s3API{cfg: cfg, creds: creds, http: httpClient, endpoint: endpoint, now: now}
}

func (s s3API) String() string { return "ebss3.s3API{endpoint=" + s.endpoint + "}" }

// GoString covers %#v.
func (s s3API) GoString() string { return s.String() }

// Format keeps an unexported credential source out of every fmt verb.
func (s s3API) Format(f fmt.State, _ rune) {
	if _, err := io.WriteString(f, s.String()); err != nil {
		return
	}
}

// MarshalJSON keeps structural serializers from reaching the credential source.
func (s s3API) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// LogValue is the redaction boundary used by slog.
func (s s3API) LogValue() slog.Value { return slog.StringValue(s.String()) }

func (s s3API) request(
	ctx context.Context,
	method, key string,
	query url.Values,
	body []byte,
	headers http.Header,
) (*http.Response, error) {
	base, err := url.Parse(s.endpoint)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: invalid S3 endpoint: %w", err)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/" + key
	base.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, base.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: build S3 request: %w", err)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("X-Amz-Content-Sha256", awssig.SHA256Hex(body))
	credentials, err := s.creds.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: resolve AWS credentials for S3: %w", err)
	}
	if err := awssig.Sign(req, body, credentials, s.cfg.Region, "s3", s.now()); err != nil {
		return nil, err
	}
	response, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: call S3: %w", err)
	}

	return response, nil
}

func (s s3API) Get(ctx context.Context, key string) ([]byte, string, bool, error) {
	response, err := s.request(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// ONLY NoSuchKey IS ABSENCE. S3 answers 404 for an object that is not
		// there AND for a BUCKET that is not there, and reading the second as the
		// first made a misaddressed bucket indistinguishable from a cold cache:
		// every job fetched cold, the settlement path fails open, and nothing
		// anywhere reported a fault. An unrecognised 404 stays an error too —
		// could-not-tell never collapses into no.
		refusal := awss3.ReadRefusal(response)
		if refusal.Absent() {
			return nil, "", false, nil
		}

		return nil, "", false, s.refusalError("GET", refusal, response)
	}
	if response.Header.Get("ETag") == "" {
		return nil, "", false, errors.New("ebs-s3: S3 returned a state object with no ETag")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, awsBodyLimit+1))
	if err != nil {
		return nil, "", false, fmt.Errorf("ebs-s3: read S3 state: %w", err)
	}
	if len(body) > awsBodyLimit {
		return nil, "", false, errors.New("ebs-s3: S3 state object exceeds 8 MiB")
	}

	return body, response.Header.Get("ETag"), true, nil
}

// refusalError renders one S3 refusal, naming the cause when the answer carries
// one.
//
// A MISADDRESSED BUCKET IS THE CASE IT EXISTS FOR. node.ebs_s3.bucket is written
// by hand, or generated for an operator who has never written one, and a first
// run is exactly where a name that is not a bucket shows up — as a 404 that used
// to read as an empty cache on every path that read one.
//
// THE REFUSAL IS WRAPPED WITH %w, so `billet check` can ask what S3 answered
// rather than looking for a status in the words of a message.
func (s s3API) refusalError(op string, refusal *awss3.Refusal, response *http.Response) error {
	// THE REGION HINT FIRST: a hint at all means the bucket exists somewhere, so
	// the region is the thing to change and the code below would be the wrong
	// advice. Reported only when it DIFFERS — the header comes back on ordinary
	// answers too, and "the bucket's region is us-west-2" beside a config that
	// says us-west-2 is a diagnostic that sends an operator hunting.
	if hint := awss3.RegionHint(response); hint != "" && hint != s.cfg.Region {
		return fmt.Errorf("ebs-s3: S3 %s returned %w; the bucket's region is %s and "+
			"node.ebs_s3.region says %s", op, refusal, hint, s.cfg.Region)
	}

	switch {
	case refusal.Code == awss3.CodeNoSuchBucket:
		return fmt.Errorf("ebs-s3: S3 %s returned %w: node.ebs_s3.bucket names %q, which does "+
			"not exist in %s — create it or correct the name; until billet read this code, "+
			"every read of that bucket answered like an empty cache", op, refusal,
			s.cfg.Bucket, s.cfg.Region)
	case refusal.Status == http.StatusNotFound && refusal.Code == "":
		return fmt.Errorf("ebs-s3: S3 %s returned %w and named no error code billet "+
			"recognises; a 404 is an absent object only when S3 says %s, so this is reported "+
			"rather than read as an empty cache", op, refusal, awss3.CodeNoSuchKey)
	default:
		return fmt.Errorf("ebs-s3: S3 %s returned %w", op, refusal)
	}
}

func (s s3API) Put(
	ctx context.Context,
	key string,
	body []byte,
	expectedETag string,
) (string, error) {
	headers := make(http.Header)
	if expectedETag == "" {
		headers.Set("If-None-Match", "*")
	} else {
		headers.Set("If-Match", expectedETag)
	}
	headers.Set("X-Amz-Server-Side-Encryption", "AES256")
	response, err := s.request(ctx, http.MethodPut, key, nil, body, headers)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errObjectAmbiguous, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusPreconditionFailed {
		return "", errObjectConflict
	}
	if response.StatusCode >= http.StatusInternalServerError {
		return "", fmt.Errorf("%w: S3 conditional PUT returned %w",
			errObjectAmbiguous, awss3.ReadRefusal(response))
	}
	if response.StatusCode != http.StatusOK {
		return "", s.refusalError("conditional PUT", awss3.ReadRefusal(response), response)
	}

	etag := response.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("%w: S3 accepted a conditional PUT but returned no ETag",
			errObjectAmbiguous)
	}

	return etag, nil
}

func (s s3API) Delete(ctx context.Context, key string) error {
	response, err := s.request(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	// S3 DELETE is idempotent: it answers 204 whether or not the object existed, so
	// a missing object is a success, not an error — decommission must be re-runnable.
	// A 404 is therefore NOT that: it is the bucket, and a purge that reported
	// success against a bucket S3 has never heard of would say it removed a
	// deployment's cache without having looked at one.
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return s.refusalError("DELETE", awss3.ReadRefusal(response), response)
	}

	return nil
}

func (s s3API) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	continuation := ""
	seen := map[string]bool{}
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if continuation != "" {
			query.Set("continuation-token", continuation)
		}
		response, err := s.request(ctx, http.MethodGet, "", query, nil, nil)
		if err != nil {
			return nil, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, awsBodyLimit+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("ebs-s3: read S3 listing: %w", errors.Join(readErr, closeErr))
		}
		if response.StatusCode != http.StatusOK {
			// The payload is already in hand here, so the refusal is parsed out of
			// it rather than read from a body this loop has closed.
			return nil, s.refusalError("LIST",
				awss3.ParseRefusal(response.StatusCode, payload), response)
		}
		if len(payload) > awsBodyLimit {
			return nil, errors.New("ebs-s3: S3 listing exceeds 8 MiB")
		}
		var result struct {
			Keys         []string `xml:"Contents>Key"`
			Truncated    bool     `xml:"IsTruncated"`
			Continuation string   `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(payload, &result); err != nil {
			return nil, fmt.Errorf("ebs-s3: parse S3 listing: %w", err)
		}
		keys = append(keys, result.Keys...)
		if !result.Truncated {
			return keys, nil
		}
		if result.Continuation == "" || seen[result.Continuation] {
			return nil, errors.New("ebs-s3: a truncated S3 listing has no new continuation token")
		}
		seen[result.Continuation] = true
		continuation = result.Continuation
	}
}

type ebsAPI struct {
	cfg             config.EBSS3Config
	owner           string
	deploymentOwner string
	creds           CredentialSource
	http            *http.Client
	endpoint        string
	now             func() time.Time
	wait            func(context.Context, time.Duration) error
}

func (e ebsAPI) String() string { return "ebss3.ebsAPI{endpoint=" + e.endpoint + "}" }

// GoString covers %#v.
func (e ebsAPI) GoString() string { return e.String() }

// Format keeps an unexported credential source out of every fmt verb.
func (e ebsAPI) Format(f fmt.State, _ rune) {
	if _, err := io.WriteString(f, e.String()); err != nil {
		return
	}
}

// MarshalJSON keeps structural serializers from reaching the credential source.
func (e ebsAPI) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }

// LogValue is the redaction boundary used by slog.
func (e ebsAPI) LogValue() slog.Value { return slog.StringValue(e.String()) }

type awsAPIError struct {
	Code   string
	Status int
}

func (e *awsAPIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("ebs-s3: AWS API returned HTTP %d", e.Status)
	}

	return fmt.Sprintf("ebs-s3: AWS API returned %s (HTTP %d)", e.Code, e.Status)
}

func awsCode(err error) string {
	if apiErr, ok := errors.AsType[*awsAPIError](err); ok {
		return apiErr.Code
	}

	return ""
}

func newEBSAPI(
	cfg config.EBSS3Config,
	owner string,
	creds CredentialSource,
	httpClient *http.Client,
	endpoint string,
	now func() time.Time,
	wait func(context.Context, time.Duration) error,
) *ebsAPI {
	deploymentOwner, _, _ := strings.Cut(owner, "/")

	return &ebsAPI{
		cfg: cfg, owner: owner, deploymentOwner: deploymentOwner, creds: creds,
		http: httpClient, endpoint: endpoint, now: now, wait: wait,
	}
}

func (e ebsAPI) call(ctx context.Context, values url.Values, output any) error {
	values.Set("Version", ebsAPIVersion)
	body := []byte(values.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ebs-s3: build EC2 request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.ContentLength = int64(len(body))
	credentials, err := e.creds.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("ebs-s3: resolve AWS credentials for EBS: %w", err)
	}
	if err := awssig.Sign(req, body, credentials, e.cfg.Region, "ec2", e.now()); err != nil {
		return err
	}
	response, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("ebs-s3: call EC2: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, awsBodyLimit+1))
	if err != nil {
		return fmt.Errorf("ebs-s3: read EC2 response: %w", err)
	}
	if len(payload) > awsBodyLimit {
		return fmt.Errorf("ebs-s3: EC2 %s response exceeds 8 MiB", values.Get("Action"))
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Codes []string `xml:"Errors>Error>Code"`
		}
		if err := xml.Unmarshal(payload, &refusal); err != nil {
			return &awsAPIError{Status: response.StatusCode}
		}
		code := ""
		if len(refusal.Codes) > 0 {
			code = refusal.Codes[0]
		}

		return &awsAPIError{Code: code, Status: response.StatusCode}
	}
	if output != nil {
		if err := xml.Unmarshal(payload, output); err != nil {
			return fmt.Errorf("ebs-s3: parse EC2 %s response: %w", values.Get("Action"), err)
		}
	}

	return nil
}

func (e ebsAPI) ownership(values url.Values, resource string) {
	values.Set("TagSpecification.1.ResourceType", resource)
	values.Set("TagSpecification.1.Tag.1.Key", ownerTag)
	values.Set("TagSpecification.1.Tag.1.Value", e.deploymentOwner)
	values.Set("TagSpecification.1.Tag.2.Key", cacheOwnerTag)
	values.Set("TagSpecification.1.Tag.2.Value", e.owner)
}

func (e ebsAPI) callIdempotent(ctx context.Context, values url.Values, output any) error {
	err := e.call(ctx, values, output)
	if err == nil || ctx.Err() != nil {
		return err
	}

	return e.call(ctx, values, output)
}

func (e ebsAPI) CreateVolume(
	ctx context.Context,
	snapshot string,
	sizeBytes int64,
	token string,
) (string, error) {
	values := url.Values{
		"Action":           {"CreateVolume"},
		"AvailabilityZone": {e.cfg.AvailabilityZone},
		"Encrypted":        {"true"},
		"VolumeType":       {"gp3"},
		"ClientToken":      {token},
	}
	if snapshot == "" {
		const gib = int64(1 << 30)
		values.Set("Size", strconv.FormatInt((sizeBytes+gib-1)/gib, 10))
	} else {
		values.Set("SnapshotId", snapshot)
	}
	if e.cfg.KMSKeyID != "" {
		values.Set("KmsKeyId", e.cfg.KMSKeyID)
	}
	e.ownership(values, "volume")
	var result struct {
		ID string `xml:"volumeId"`
	}
	if err := e.callIdempotent(ctx, values, &result); err != nil {
		return "", err
	}
	if !strings.HasPrefix(result.ID, "vol-") {
		return "", errors.New("ebs-s3: CreateVolume returned no volume id")
	}
	if err := e.waitVolume(ctx, result.ID, "available"); err != nil {
		return "", err
	}

	return result.ID, nil
}

func (e ebsAPI) waitVolume(ctx context.Context, id, wanted string) error {
	for {
		var result struct {
			Volumes []struct {
				ID    string `xml:"volumeId"`
				State string `xml:"status"`
			} `xml:"volumeSet>item"`
		}
		if err := e.call(ctx, url.Values{
			"Action": {"DescribeVolumes"}, "VolumeId.1": {id},
		}, &result); err != nil {
			return err
		}
		if len(result.Volumes) != 1 || result.Volumes[0].ID != id {
			return fmt.Errorf("ebs-s3: volume %s disappeared while waiting for %s", id, wanted)
		}
		if result.Volumes[0].State == wanted {
			return nil
		}
		if result.Volumes[0].State == "error" || result.Volumes[0].State == "deleted" {
			return fmt.Errorf("ebs-s3: volume %s entered %s while waiting for %s", id, result.Volumes[0].State, wanted)
		}
		if err := e.wait(ctx, awsPollDelay); err != nil {
			return err
		}
	}
}

type resourceTag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

// errNotOwned is the refusal to delete a block resource that does not carry BOTH
// this store's ownership tags. It is a sentinel rather than a plain message so
// Evict can tell "this one is not mine, skip it" (errors.Is) apart from a real
// API failure: the snapshot listing filters on the cache-owner tag alone, but the
// delete also checks the deployment-owner tag, so a resource from another
// deployment sharing this cache owner is LISTED and then REFUSED — and a plain
// error there aborts the whole sweep, stranding all the genuine garbage behind one
// foreign resource ordinary traffic can create.
var errNotOwned = errors.New("resource lacks this store's ownership tags")

func (e ebsAPI) owned(tags []resourceTag) bool {
	deployment, cache := false, false
	for _, tag := range tags {
		switch tag.Key {
		case ownerTag:
			deployment = tag.Value == e.deploymentOwner
		case cacheOwnerTag:
			cache = tag.Value == e.owner
		}
	}

	return deployment && cache
}

func (e ebsAPI) volumeOwned(ctx context.Context, id string) (bool, bool, error) {
	var result struct {
		Volumes []struct {
			ID   string        `xml:"volumeId"`
			Tags []resourceTag `xml:"tagSet>item"`
		} `xml:"volumeSet>item"`
	}
	err := e.call(ctx, url.Values{
		"Action": {"DescribeVolumes"}, "VolumeId.1": {id},
	}, &result)
	if awsCode(err) == "InvalidVolume.NotFound" {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if len(result.Volumes) != 1 || result.Volumes[0].ID != id {
		return false, false, fmt.Errorf("ebs-s3: volume %s was not uniquely described", id)
	}

	return e.owned(result.Volumes[0].Tags), true, nil
}

func (e ebsAPI) DeleteVolume(ctx context.Context, id string) error {
	owned, found, err := e.volumeOwned(ctx, id)
	if err != nil || !found {
		return err
	}
	if !owned {
		return fmt.Errorf("ebs-s3: refusing to delete volume %s: %w", id, errNotOwned)
	}

	err = e.call(ctx, url.Values{"Action": {"DeleteVolume"}, "VolumeId": {id}}, nil)
	if awsCode(err) == "InvalidVolume.NotFound" {
		return nil
	}

	return err
}

func (e ebsAPI) CreateSnapshot(
	ctx context.Context,
	volume string,
	now time.Time,
	token string,
) (string, error) {
	// EC2's CreateSnapshot HAS NO ClientToken. CreateVolume and RunInstances take
	// one; this call answers `UnknownParameter` (HTTP 400) to it — measured on
	// 2026-09-03, on the first real publication the ebs-s3 store ever attempted,
	// after every fake had pinned the parameter as present. So the token travels
	// as a TAG, and at-most-once is billet's to keep, with these rules:
	//
	//   - the token names the OPERATION (the store derives it from the key and the
	//     volume, never from the clock), so an attempt that starts after another
	//     died — in this process or a restarted one — finds that attempt's
	//     snapshot rather than making a second;
	//   - a lookup runs before the first create, and a REFUSAL (a 4xx AWS parsed
	//     and answered) means AWS did not act, so it is returned and never retried;
	//   - an AMBIGUOUS answer (a transport failure, a 5xx, an unparseable body) is
	//     reconciled by polling the lookup for about a minute, because
	//     DescribeSnapshots is eventually consistent and one negative lookup right
	//     after a lost response proves nothing; only a window with nothing in it
	//     earns ONE more create, and after a second ambiguous answer nothing is
	//     created again;
	//   - a snapshot that entered error state spent the token, and the answer is
	//     that failure; two snapshots carrying one token is refused rather than
	//     chosen between.
	//
	// What this cannot close is two PROCESSES running the same operation at once
	// with neither able to see the other's create yet; the writer lease the
	// store takes before it snapshots is what keeps that to one process.
	if strings.TrimSpace(token) == "" {
		return "", errors.New("ebs-s3: CreateSnapshot needs a request token")
	}
	if id, err := e.snapshotByToken(ctx, token); err != nil {
		return "", err
	} else if id != "" {
		return id, e.waitSnapshot(ctx, id)
	}
	values := url.Values{
		"Action":      {"CreateSnapshot"},
		"VolumeId":    {volume},
		"Description": {"billet cache generation " + now.UTC().Format(time.RFC3339)},
	}
	e.ownership(values, "snapshot")
	values.Set("TagSpecification.1.Tag.3.Key", snapshotTokenTag)
	values.Set("TagSpecification.1.Tag.3.Value", token)
	var result struct {
		ID string `xml:"snapshotId"`
	}
	for attempt := 1; ; attempt++ {
		err := e.call(ctx, values, &result)
		if err == nil {
			break
		}
		if ctx.Err() != nil || awsRefused(err) {
			return "", err
		}
		id, lookupErr := e.reconcileSnapshot(ctx, token)
		switch {
		case lookupErr != nil:
			return "", errors.Join(err, lookupErr)
		case id != "":
			return id, e.waitSnapshot(ctx, id)
		case attempt == 2:
			return "", fmt.Errorf("ebs-s3: CreateSnapshot answered ambiguously twice and no "+
				"snapshot carries its token; not creating a third: %w", err)
		}
	}
	if !strings.HasPrefix(result.ID, "snap-") {
		return "", errors.New("ebs-s3: CreateSnapshot returned no snapshot id")
	}
	if err := e.waitSnapshot(ctx, result.ID); err != nil {
		return "", err
	}

	return result.ID, nil
}

// snapshotReconcileAttempts bounds how long an ambiguous CreateSnapshot is given
// to become visible before billet concludes it did not act: this many lookups,
// awsPollDelay apart, is about a minute (the first lookup waits for nothing).
const snapshotReconcileAttempts = 30

// awsRefused reports an answer AWS parsed and declined, which is proof it did
// not act. A throttle is a refusal too, and it is deliberately not retried here:
// the settlement above this fails and the image store is discarded, which costs
// one cold job, where a create billet cannot account for costs a snapshot.
func awsRefused(err error) bool {
	apiErr, ok := errors.AsType[*awsAPIError](err)

	return ok && apiErr.Code != "" && apiErr.Status >= 400 && apiErr.Status < 500
}

// reconcileSnapshot polls for a snapshot carrying the token until one appears
// or the window ends. An empty id with a nil error means the window ended with
// nothing carrying it.
func (e ebsAPI) reconcileSnapshot(ctx context.Context, token string) (string, error) {
	for attempt := range snapshotReconcileAttempts {
		if attempt > 0 {
			if err := e.wait(ctx, awsPollDelay); err != nil {
				return "", err
			}
		}
		id, err := e.snapshotByToken(ctx, token)
		if err != nil || id != "" {
			return id, err
		}
	}

	return "", nil
}

// snapshotByToken answers the snapshot a request token was spent on, walking
// every page. A pending one is still the one to wait for; one in error state is
// the operation having failed; and two of them is refused, since choosing
// between them would leave the other unreferenced and say nothing about which
// one the caller's data is on.
func (e ebsAPI) snapshotByToken(ctx context.Context, token string) (string, error) {
	var found, failed []string
	next := ""
	seen := map[string]bool{}
	for {
		values := url.Values{
			"Owner.1":          {"self"},
			"Filter.1.Name":    {"tag:" + snapshotTokenTag},
			"Filter.1.Value.1": {token},
			"Filter.2.Name":    {"tag:" + cacheOwnerTag},
			"Filter.2.Value.1": {e.owner},
		}
		if next != "" {
			values.Set("NextToken", next)
		}
		page, err := e.describeSnapshots(ctx, values)
		if err != nil {
			return "", fmt.Errorf("ebs-s3: look for a snapshot by its token: %w", err)
		}
		for _, snapshot := range page.Snapshots {
			if !strings.HasPrefix(snapshot.ID, "snap-") {
				continue
			}
			if snapshot.State == "error" {
				failed = append(failed, snapshot.ID)
			} else {
				found = append(found, snapshot.ID)
			}
		}
		if page.NextToken == "" {
			break
		}
		if seen[page.NextToken] {
			return "", errors.New("ebs-s3: a paginated EBS snapshot listing repeated its token")
		}
		seen[page.NextToken] = true
		next = page.NextToken
	}
	// A snapshot that ENTERED ERROR still spent the token: the operation ran and
	// failed, and the answer is that failure, never a second try under the same
	// name that would leave two snapshots claiming one publication.
	if len(failed) > 0 {
		return "", fmt.Errorf("ebs-s3: the snapshot for this request token entered error "+
			"state (%s); the operation failed", strings.Join(failed, ", "))
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("ebs-s3: %d snapshots carry one request token (%s); refusing to "+
			"choose between them", len(found), strings.Join(found, ", "))
	}
}

type snapshotItem struct {
	ID      string        `xml:"snapshotId"`
	State   string        `xml:"status"`
	Created time.Time     `xml:"startTime"`
	Tags    []resourceTag `xml:"tagSet>item"`
}

type snapshotPage struct {
	Snapshots []snapshotItem `xml:"snapshotSet>item"`
	NextToken string         `xml:"nextToken"`
}

func (e ebsAPI) describeSnapshots(ctx context.Context, values url.Values) (snapshotPage, error) {
	values.Set("Action", "DescribeSnapshots")
	var result snapshotPage
	if err := e.call(ctx, values, &result); err != nil {
		return snapshotPage{}, err
	}

	return result, nil
}

func (e ebsAPI) waitSnapshot(ctx context.Context, id string) error {
	for {
		page, err := e.describeSnapshots(ctx, url.Values{"SnapshotId.1": {id}})
		if err != nil {
			return err
		}
		if len(page.Snapshots) != 1 || page.Snapshots[0].ID != id {
			return fmt.Errorf("ebs-s3: snapshot %s disappeared before completion", id)
		}
		// A CLOSED set of states, because this loop has no deadline of its own:
		// only a state AWS will leave on its own is waited for. `recoverable` is an
		// archived snapshot somebody would have to restore, and a state this binary
		// has never heard of is not one it should sit in.
		switch state := page.Snapshots[0].State; state {
		case "completed":
			return nil
		case "error":
			return fmt.Errorf("ebs-s3: snapshot %s entered error state", id)
		case "pending", "recovering":
		default:
			return fmt.Errorf("ebs-s3: snapshot %s is %q, which billet does not wait out", id, state)
		}
		if err := e.wait(ctx, awsPollDelay); err != nil {
			return err
		}
	}
}

func (e ebsAPI) SnapshotExists(ctx context.Context, id string) (bool, error) {
	page, err := e.describeSnapshots(ctx, url.Values{"SnapshotId.1": {id}})
	if awsCode(err) == "InvalidSnapshot.NotFound" {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return len(page.Snapshots) == 1 && page.Snapshots[0].ID == id &&
		page.Snapshots[0].State == "completed", nil
}

func (e ebsAPI) ListSnapshots(ctx context.Context) ([]snapshotInfo, error) {
	var out []snapshotInfo
	next := ""
	seen := map[string]bool{}
	for {
		values := url.Values{
			"Owner.1":          {"self"},
			"Filter.1.Name":    {"tag:" + cacheOwnerTag},
			"Filter.1.Value.1": {e.owner},
		}
		if next != "" {
			values.Set("NextToken", next)
		}
		page, err := e.describeSnapshots(ctx, values)
		if err != nil {
			return nil, err
		}
		for _, snapshot := range page.Snapshots {
			if snapshot.State == "completed" {
				out = append(out, snapshotInfo{ID: snapshot.ID, Created: snapshot.Created})
			}
		}
		if page.NextToken == "" {
			return out, nil
		}
		if seen[page.NextToken] {
			return nil, errors.New("ebs-s3: a paginated EBS snapshot listing repeated its token")
		}
		seen[page.NextToken] = true
		next = page.NextToken
	}
}

func (e ebsAPI) DeleteSnapshot(ctx context.Context, id string) error {
	page, err := e.describeSnapshots(ctx, url.Values{"SnapshotId.1": {id}})
	if awsCode(err) == "InvalidSnapshot.NotFound" {
		return nil
	}
	if err != nil {
		return err
	}
	if len(page.Snapshots) != 1 || page.Snapshots[0].ID != id {
		return fmt.Errorf("ebs-s3: snapshot %s was not uniquely described", id)
	}
	if !e.owned(page.Snapshots[0].Tags) {
		return fmt.Errorf("ebs-s3: refusing to delete snapshot %s: %w", id, errNotOwned)
	}

	err = e.call(ctx, url.Values{"Action": {"DeleteSnapshot"}, "SnapshotId": {id}}, nil)
	if awsCode(err) == "InvalidSnapshot.NotFound" {
		return nil
	}

	return err
}

// ListAvailableVolumes finds only this store's unattached volumes. Attached
// volumes are live guest custody and are never candidates for this orphan sweep.
func (e ebsAPI) ListAvailableVolumes(ctx context.Context) ([]volumeInfo, error) {
	var out []volumeInfo
	next := ""
	seen := map[string]bool{}
	for {
		values := url.Values{
			"Action":           {"DescribeVolumes"},
			"Filter.1.Name":    {"tag:" + cacheOwnerTag},
			"Filter.1.Value.1": {e.owner},
			"Filter.2.Name":    {"status"},
			"Filter.2.Value.1": {"available"},
		}
		if next != "" {
			values.Set("NextToken", next)
		}
		var result struct {
			Volumes []struct {
				ID      string    `xml:"volumeId"`
				Created time.Time `xml:"createTime"`
			} `xml:"volumeSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := e.call(ctx, values, &result); err != nil {
			return nil, err
		}
		for _, volume := range result.Volumes {
			if strings.HasPrefix(volume.ID, "vol-") && !volume.Created.IsZero() {
				out = append(out, volumeInfo{ID: volume.ID, Created: volume.Created})
			}
		}
		if result.NextToken == "" {
			return out, nil
		}
		if seen[result.NextToken] {
			return nil, errors.New("ebs-s3: a paginated EBS volume listing repeated its token")
		}
		seen[result.NextToken] = true
		next = result.NextToken
	}
}
