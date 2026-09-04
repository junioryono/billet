// Package archivestore puts a deployment archive somewhere other than the disk
// it protects, and fetches it back.
//
// A BACKUP ON THE DISK IT PROTECTS IS NOT ONE, and `billet local backup --out
// <dir>` alone leaves it there. The archive directory remains the contract —
// its manifest carries a digest and a size for every entry precisely so that
// somebody else's tooling can carry it, and an operator who already has restic
// or rclone needs nothing here.
//
// WHAT BILLET OWNS IS BOTH ENDS OF ONE NARROW HOP, because the fetch is the half
// that matters. On the day this is used the machine is new and holds the billet
// binary and nothing else; an upload-only answer would have somebody installing
// a second tool during an outage.
//
// WHAT IT IS NOT is a backup tool. No dedupe, no incremental, no catalogue and
// no retention: the bucket does retention through versioning and a lifecycle
// rule, the manifest does verification, and this package HAS NO DELETE. Its
// absence is the refusal — a credential on the one host that also holds the App
// key cannot destroy the history it wrote, whatever else goes wrong there.
//
// It signs its own requests through internal/awssig for the reason ADR-002
// records: one signer rather than a second reading of the specification, and no
// SDK on a control plane that is 21.8MB whole.
package archivestore

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
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsjson"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
)

// maxObject bounds what this package will read into memory.
//
// Every archive entry but the ledger is a few kilobytes; a ledger is a few
// megabytes on a busy deployment and has no useful bound of its own, so this is
// generous rather than tight. Without it, an object that is not what it claims
// to be is read whole before anything notices.
const maxObject = 512 << 20

// maxListing bounds one page of a bucket listing.
const maxListing = 8 << 20

// requestTimeout bounds one call. The whole upload is several of them, and a
// backup that hangs on a wedged endpoint is a scheduled unit that never exits.
const requestTimeout = 5 * time.Minute

// CredentialSource yields the credential each request is signed with.
//
// AN ALIAS FOR awscreds.Source. This was a declaration, and so were the identically
// shaped ones in internal/store/ebss3 and internal/provider/codebuild, because the
// chain lived inside internal/provider/ec2 and none of these packages could import
// a compute backend. Four names for one method, and a conversion closure at every
// call site to bridge them. The chain is a shared package now.
type CredentialSource = awscreds.Source

// S3 is an archive store backed by an S3-compatible bucket.
//
// IT REDACTS ITSELF ON EVERY RENDERING PATH, on a VALUE receiver, because it
// holds a credential source: fmt never consults String once Format exists, slog
// never consults fmt, and a pointer receiver is not consulted when a value is
// formatted. The rule and the four types it took to learn it are in the
// billet-security skill.
type S3 struct {
	cfg   config.BackupS3Config
	creds CredentialSource
	http  *http.Client
	// base is the bucket's address, with a trailing slash. Virtual-host style
	// against AWS; path style against a configured endpoint, because that is what
	// MinIO does not do by default.
	base string
	now  func() time.Time
}

// String renders the destination and never the credential.
func (s S3) String() string {
	return "archivestore.S3{bucket=" + s.cfg.Bucket + ", region=" + s.cfg.Region + "}"
}

// GoString covers %#v.
func (s S3) GoString() string { return s.String() }

// Format catches every fmt verb, including the ones that ignore String.
func (s S3) Format(f fmt.State, _ rune) {
	if _, err := io.WriteString(f, s.String()); err != nil {
		return
	}
}

// MarshalJSON keeps structural serializers away from the credential source.
func (s S3) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// LogValue is the redaction boundary slog uses; it consults nothing in fmt.
func (s S3) LogValue() slog.Value { return slog.StringValue(s.String()) }

// NewS3 builds a store for one deployment's archives.
//
// IT RE-VALIDATES ITS OWN CONFIGURATION, the rule alloc.New follows: this is
// exported, so a caller whose config did not come through config.Load must not
// be able to build a store pointed somewhere the file could never have named.
func NewS3(cfg config.BackupS3Config, creds CredentialSource) (*S3, error) {
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Prefix = strings.TrimSpace(cfg.Prefix)
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.KMSKeyID = strings.TrimSpace(cfg.KMSKeyID)

	if cfg.Prefix == "" {
		cfg.Prefix = "billet-backups"
	}

	if err := errors.Join(config.CheckBackupS3(cfg)...); err != nil {
		return nil, fmt.Errorf("archivestore: %w", err)
	}

	if creds == nil || nilCredentialSource(creds) {
		return nil, errors.New("archivestore: an archive store needs an AWS credential source")
	}

	// THE SUFFIX IS THE PARTITION'S. `backup.s3` is what `billet local backup`
	// writes to and what a restore reads back, and `billet init hybrid` now names
	// a bucket there for every generation — so a deployment in cn-north-1 would
	// have taken every backup against a host that does not exist and discovered
	// it at the restore.
	base := "https://" + cfg.Bucket + ".s3." + cfg.Region + "." +
		awsjson.DNSSuffixFor(cfg.Region) + "/"
	if cfg.Endpoint != "" {
		base = strings.TrimSuffix(cfg.Endpoint, "/") + "/" + cfg.Bucket + "/"
	}

	return &S3{
		cfg:   cfg,
		creds: creds,
		http: &http.Client{
			Timeout: requestTimeout,
			// A REDIRECT IS NOT FOLLOWED. The signature covers the host, so a
			// followed redirect either fails to authenticate or sends a credential
			// somewhere billet did not sign for; S3's own wrong-region answer is a
			// redirect, and it is reported rather than chased.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		base: base,
		now:  time.Now,
	}, nil
}

// nilCredentialSource catches a TYPED nil, which satisfies the interface, passes
// a plain == nil, and panics at the first signed request.
func nilCredentialSource(creds CredentialSource) bool {
	value := reflect.ValueOf(creds)

	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func,
		reflect.Chan, reflect.UnsafePointer:
		return value.IsNil()
	default:
		return false
	}
}

// Prefix is where this deployment's archives live in the bucket.
//
// THE DEPLOYMENT IDENTITY IS IN THE PATH, which is what lets two deployments
// share a bucket and what an IAM policy is scoped to. Without it one
// deployment's role could read another's App key.
func (s S3) Prefix(deployment string) string {
	return s.cfg.Prefix + "/" + deployment + "/"
}

// ArchiveKey is where one entry of one archive goes.
func (s S3) ArchiveKey(deployment, archive, entry string) string {
	return s.Prefix(deployment) + archive + "/" + entry
}

// Put writes one object, and REFUSES to replace one that is already there.
//
// If-None-Match: * IS THE NO-CLOBBER, and it is the same shape every credential
// publication in billet uses: a write that fails rather than replaces. An
// archive is named for the instant it was taken, so a key that already exists is
// either a retry of bytes that are already safe or a collision worth stopping
// for — and neither is a reason to overwrite a copy of a deployment's App key.
func (s S3) Put(ctx context.Context, key string, body []byte) error {
	headers := make(http.Header)
	headers.Set("If-None-Match", "*")

	if s.cfg.KMSKeyID != "" {
		headers.Set("X-Amz-Server-Side-Encryption", "aws:kms")
		headers.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", s.cfg.KMSKeyID)
	} else {
		headers.Set("X-Amz-Server-Side-Encryption", "AES256")
	}

	response, err := s.request(ctx, http.MethodPut, key, nil, body, headers)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusPreconditionFailed ||
		response.StatusCode == http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrObjectExists, key)
	case response.StatusCode != http.StatusOK:
		return s.statusError("PUT", key, response)
	}

	return nil
}

// ErrObjectExists is a destination that is already occupied. Named so a caller
// can tell "this archive is already uploaded" from a transport failure.
//
// IT WRAPS deployarchive's SENTINEL, so a caller holding either name recognises
// it. That is what makes an interrupted upload resumable: the retry has to tell
// "this entry is already published" from "the store said no", and only one of
// those is a reason to stop.
var ErrObjectExists = fmt.Errorf("archivestore: %w", deployarchive.ErrObjectExists)

// ErrNoSuchObject is an absent key, which is a fact rather than a failure — a
// caller listing archives and then fetching one races an operator's lifecycle
// rule.
var ErrNoSuchObject = errors.New("archivestore: no such object")

// Get fetches one object.
func (s S3) Get(ctx context.Context, key string) ([]byte, error) {
	response, err := s.request(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}

	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNoSuchObject, key)
	case response.StatusCode != http.StatusOK:
		return nil, s.statusError("GET", key, response)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxObject+1))
	if err != nil {
		return nil, fmt.Errorf("archivestore: read %s: %w", key, err)
	}

	if len(body) > maxObject {
		return nil, fmt.Errorf("archivestore: %s is larger than %d bytes, which no archive entry "+
			"is", key, maxObject)
	}

	return body, nil
}

// Archive is one uploaded archive as the bucket knows it.
type Archive struct {
	// Deployment is the identity it belongs to. Two deployments may share a
	// bucket, and on the day this matters the operator's machine is new and holds
	// no identity to filter by — so it is reported rather than assumed.
	Deployment string
	// Name is the archive's directory name, which is what `--from-backup` takes.
	Name string
	// Modified is when its manifest landed, which is when the upload FINISHED —
	// the manifest is written last for exactly that reason.
	Modified time.Time
}

// List reports the COMPLETE archives in the bucket.
//
// COMPLETE IS THE WHOLE POINT, and it is why the manifest is uploaded last. An
// interrupted upload leaves entries with no manifest, deployarchive.Open refuses
// such a directory as "not a billet backup", and an operator scanning this list
// on the worst day must not be offered one. So a prefix counts as an archive
// when — and only when — its manifest object exists.
//
// AN EMPTY DEPLOYMENT LISTS EVERY ONE IN THE BUCKET, which is not a convenience:
// the machine doing the restore is new and has no deployment identity of its
// own, so filtering by one it does not have would report nothing and read as an
// empty bucket.
func (s S3) List(ctx context.Context, deployment string) ([]Archive, error) {
	prefix := s.cfg.Prefix + "/"
	if deployment != "" {
		prefix = s.Prefix(deployment)
	}

	objects, err := s.list(ctx, prefix)
	if err != nil {
		return nil, err
	}

	var out []Archive

	for _, o := range objects {
		rest, ok := strings.CutPrefix(o.Key, s.cfg.Prefix+"/")
		if !ok {
			continue
		}

		parts := strings.Split(rest, "/")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != ManifestEntry {
			continue
		}

		out = append(out, Archive{Deployment: parts[0], Name: parts[1], Modified: o.Modified})
	}

	return out, nil
}

// ManifestEntry is the archive entry whose presence makes a prefix an archive.
//
// DECLARED HERE RATHER THAN IMPORTED so this package stays a transport that
// knows nothing about what it carries; the command layer asserts the two agree.
const ManifestEntry = "manifest.json"

type listedObject struct {
	Key      string
	Modified time.Time
}

func (s S3) list(ctx context.Context, prefix string) ([]listedObject, error) {
	var (
		out          []listedObject
		continuation string
		seen         = map[string]bool{}
	)

	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if continuation != "" {
			query.Set("continuation-token", continuation)
		}

		response, err := s.request(ctx, http.MethodGet, "", query, nil, nil)
		if err != nil {
			return nil, err
		}

		payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxListing+1))
		closeErr := response.Body.Close()

		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("archivestore: read the bucket listing: %w",
				errors.Join(readErr, closeErr))
		}

		if response.StatusCode != http.StatusOK {
			return nil, s.statusError("LIST", prefix, response)
		}

		if len(payload) > maxListing {
			return nil, errors.New("archivestore: a bucket listing page exceeds 8 MiB")
		}

		var result struct {
			Contents []struct {
				Key      string    `xml:"Key"`
				Modified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			Truncated    bool   `xml:"IsTruncated"`
			Continuation string `xml:"NextContinuationToken"`
		}

		if err := xml.Unmarshal(payload, &result); err != nil {
			return nil, fmt.Errorf("archivestore: parse the bucket listing: %w", err)
		}

		for _, c := range result.Contents {
			out = append(out, listedObject{Key: c.Key, Modified: c.Modified})
		}

		if !result.Truncated {
			return out, nil
		}

		// A TRUNCATED PAGE WITH NO NEW TOKEN IS A LOOP, and a listing that loops
		// forever on a scheduled backup is a unit that never exits.
		if result.Continuation == "" || seen[result.Continuation] {
			return nil, errors.New(
				"archivestore: a truncated bucket listing has no new continuation token")
		}

		seen[result.Continuation] = true
		continuation = result.Continuation
	}
}

// statusError reports an unexpected status, naming the bucket's real region when
// the store tells us — an operator staring at a bare 301 has no other way to see
// that their region is wrong.
func (s S3) statusError(op, key string, response *http.Response) error {
	if hint := response.Header.Get("X-Amz-Bucket-Region"); hint != "" && hint != s.cfg.Region {
		return fmt.Errorf("archivestore: %s %s returned HTTP %d; the bucket's region is %s and "+
			"backup.s3.region says %s", op, key, response.StatusCode, hint, s.cfg.Region)
	}

	return fmt.Errorf("archivestore: %s %s returned HTTP %d", op, key, response.StatusCode)
}

// request signs and sends one call.
//
// NOTHING RENDERS THE ENDPOINT OR THE CREDENTIAL. url.Parse's error embeds the
// whole URL, which is why the parse failure below says only that it could not be
// built — the same rule config.CheckEC2Endpoint follows and for the same reason.
func (s S3) request(
	ctx context.Context, method, key string, query url.Values, body []byte, headers http.Header,
) (*http.Response, error) {
	base, err := url.Parse(s.base)
	if err != nil {
		return nil, errors.New("archivestore: backup.s3 does not resolve to a url billet can dial")
	}

	base.Path = strings.TrimSuffix(base.Path, "/") + "/" + key
	base.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, method, base.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("archivestore: build the %s request: %w", method, err)
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
		return nil, fmt.Errorf("archivestore: resolve AWS credentials: %w", err)
	}

	if err := awssig.Sign(req, body, credentials, s.cfg.Region, "s3", s.now()); err != nil {
		return nil, err
	}

	// THE TRANSPORT'S ERROR CARRIES THE URL, and that is allowed HERE and is not
	// allowed for node.ec2.endpoint, so the difference is worth stating rather
	// than leaving for the next reader to re-litigate. *url.Error embeds the whole
	// request URL; what makes rendering one safe is that CheckBackupEndpoint has
	// already refused a userinfo section, a query string and a fragment, so the
	// URL provably carries no credential. What remains is the host, the bucket,
	// the deployment identity and the entry — none of which is a secret, all of
	// which the command prints on the line above, and every one of which an
	// operator needs in order to tell a wrong bucket from an unreachable host from
	// a single failed entry. A fixed diagnostic would take that away to protect
	// nothing.
	response, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("archivestore: call the archive store: %w", err)
	}

	return response, nil
}
