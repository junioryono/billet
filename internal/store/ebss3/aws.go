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
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
)

const (
	ebsAPIVersion = "2016-11-15"
	awsBodyLimit  = 8 << 20
	awsPollDelay  = 2 * time.Second
	ownerTag      = "sh.billet.owner"
	cacheOwnerTag = "sh.billet.cache-owner"
)

// CredentialSource supplies the temporary or static credential used for both
// EBS and S3. Implementations must redact their own diagnostic rendering.
type CredentialSource interface {
	Credentials(context.Context) (awssig.Credentials, error)
}

// CredentialSourceFunc adapts a function into a CredentialSource.
type CredentialSourceFunc func(context.Context) (awssig.Credentials, error)

// Credentials calls f.
func (f CredentialSourceFunc) Credentials(ctx context.Context) (awssig.Credentials, error) {
	return f(ctx)
}

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

func (s *s3API) request(
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

func (s *s3API) Get(ctx context.Context, key string) ([]byte, string, bool, error) {
	response, err := s.request(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("ebs-s3: S3 GET returned HTTP %d", response.StatusCode)
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

func (s *s3API) Put(
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
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ebs-s3: S3 conditional PUT returned HTTP %d", response.StatusCode)
	}

	etag := response.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("ebs-s3: S3 conditional PUT returned no ETag")
	}

	return etag, nil
}

func (s *s3API) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	continuation := ""
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
			return nil, fmt.Errorf("ebs-s3: S3 LIST returned HTTP %d", response.StatusCode)
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
		if result.Continuation == "" || result.Continuation == continuation {
			return nil, errors.New("ebs-s3: a truncated S3 listing has no new continuation token")
		}
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
	var apiErr *awsAPIError
	if errors.As(err, &apiErr) {
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

func (e *ebsAPI) call(ctx context.Context, values url.Values, output any) error {
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

func (e *ebsAPI) ownership(values url.Values, resource string) {
	values.Set("TagSpecification.1.ResourceType", resource)
	values.Set("TagSpecification.1.Tag.1.Key", ownerTag)
	values.Set("TagSpecification.1.Tag.1.Value", e.deploymentOwner)
	values.Set("TagSpecification.1.Tag.2.Key", cacheOwnerTag)
	values.Set("TagSpecification.1.Tag.2.Value", e.owner)
}

func (e *ebsAPI) callIdempotent(ctx context.Context, values url.Values, output any) error {
	err := e.call(ctx, values, output)
	if err == nil || ctx.Err() != nil {
		return err
	}

	return e.call(ctx, values, output)
}

func (e *ebsAPI) CreateVolume(
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

func (e *ebsAPI) waitVolume(ctx context.Context, id, wanted string) error {
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

func (e *ebsAPI) owned(tags []resourceTag) bool {
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

func (e *ebsAPI) volumeOwned(ctx context.Context, id string) (bool, bool, error) {
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

func (e *ebsAPI) DeleteVolume(ctx context.Context, id string) error {
	owned, found, err := e.volumeOwned(ctx, id)
	if err != nil || !found {
		return err
	}
	if !owned {
		return fmt.Errorf("ebs-s3: refusing to delete volume %s without this store's ownership tags", id)
	}

	err = e.call(ctx, url.Values{"Action": {"DeleteVolume"}, "VolumeId": {id}}, nil)
	if awsCode(err) == "InvalidVolume.NotFound" {
		return nil
	}

	return err
}

func (e *ebsAPI) CreateSnapshot(
	ctx context.Context,
	volume string,
	now time.Time,
	token string,
) (string, error) {
	values := url.Values{
		"Action":      {"CreateSnapshot"},
		"VolumeId":    {volume},
		"Description": {"billet cache generation " + now.UTC().Format(time.RFC3339)},
		"ClientToken": {token},
	}
	e.ownership(values, "snapshot")
	var result struct {
		ID string `xml:"snapshotId"`
	}
	if err := e.callIdempotent(ctx, values, &result); err != nil {
		return "", err
	}
	if !strings.HasPrefix(result.ID, "snap-") {
		return "", errors.New("ebs-s3: CreateSnapshot returned no snapshot id")
	}
	if err := e.waitSnapshot(ctx, result.ID); err != nil {
		return "", err
	}

	return result.ID, nil
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

func (e *ebsAPI) describeSnapshots(ctx context.Context, values url.Values) (snapshotPage, error) {
	values.Set("Action", "DescribeSnapshots")
	var result snapshotPage
	if err := e.call(ctx, values, &result); err != nil {
		return snapshotPage{}, err
	}

	return result, nil
}

func (e *ebsAPI) waitSnapshot(ctx context.Context, id string) error {
	for {
		page, err := e.describeSnapshots(ctx, url.Values{"SnapshotId.1": {id}})
		if err != nil {
			return err
		}
		if len(page.Snapshots) != 1 || page.Snapshots[0].ID != id {
			return fmt.Errorf("ebs-s3: snapshot %s disappeared before completion", id)
		}
		if page.Snapshots[0].State == "completed" {
			return nil
		}
		if page.Snapshots[0].State == "error" {
			return fmt.Errorf("ebs-s3: snapshot %s entered error state", id)
		}
		if err := e.wait(ctx, awsPollDelay); err != nil {
			return err
		}
	}
}

func (e *ebsAPI) SnapshotExists(ctx context.Context, id string) (bool, error) {
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

func (e *ebsAPI) ListSnapshots(ctx context.Context) ([]snapshotInfo, error) {
	var out []snapshotInfo
	next := ""
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
		if page.NextToken == next {
			return nil, errors.New("ebs-s3: a paginated EBS snapshot listing repeated its token")
		}
		next = page.NextToken
	}
}

func (e *ebsAPI) DeleteSnapshot(ctx context.Context, id string) error {
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
		return fmt.Errorf("ebs-s3: refusing to delete snapshot %s without this store's ownership tags", id)
	}

	err = e.call(ctx, url.Values{"Action": {"DeleteSnapshot"}, "SnapshotId": {id}}, nil)
	if awsCode(err) == "InvalidSnapshot.NotFound" {
		return nil
	}

	return err
}

// ListAvailableVolumes finds only this store's unattached volumes. Attached
// volumes are live guest custody and are never candidates for this orphan sweep.
func (e *ebsAPI) ListAvailableVolumes(ctx context.Context) ([]volumeInfo, error) {
	var out []volumeInfo
	next := ""
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
		if result.NextToken == next {
			return nil, errors.New("ebs-s3: a paginated EBS volume listing repeated its token")
		}
		next = result.NextToken
	}
}
