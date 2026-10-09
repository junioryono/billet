package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// BackupConfig is where this deployment's archives go when they leave the disk
// they protect.
//
// OPTIONAL, AND THE DIRECTORY REMAINS THE CONTRACT. `billet local backup --out
// <dir>` writes a manifest of digests and sizes precisely so that somebody
// else's tooling can carry it; an operator who already has restic, rclone or a
// NAS needs nothing here. What this adds is the half that matters on the day the
// machine is new: `billet local restore --from s3://…` fetches, verifies and
// restores in one command, where an upload-only answer would have them
// installing another tool mid-outage.
//
// WHAT BILLET DELIBERATELY DOES NOT BECOME is a backup tool. There is no dedupe,
// no incremental, no catalogue and no retention: the bucket does retention
// (versioning and a lifecycle rule), the manifest does verification, and billet
// never issues a delete — so the credential sitting on the one host that holds
// the App key cannot destroy the history it just wrote.
type BackupConfig struct {
	// S3 is an S3-compatible bucket. Nil means billet uploads nothing and the
	// archive directory is the seam.
	S3 *BackupS3Config `yaml:"s3,omitempty"`
}

// BackupS3Config names the bucket and how to reach it.
type BackupS3Config struct {
	// Bucket receives one object per archive entry.
	Bucket string `yaml:"bucket"`
	// Region is the SIGNING region. With no Endpoint it also selects the AWS
	// endpoint, so a typo there is a request signed for somewhere else.
	Region string `yaml:"region"`
	// Prefix isolates one deployment inside a shared bucket. Archives land under
	// <prefix>/<deployment-id>/<created-at>/, so IAM can be scoped by prefix and
	// two deployments cannot read each other's credentials.
	Prefix string `yaml:"prefix,omitempty"`
	// Endpoint overrides the AWS endpoint billet derives from Region, for an
	// S3-compatible store: Ceph RGW — which billet's own reference hardware
	// already runs — MinIO, or R2. Addressing is PATH style against it
	// (<endpoint>/<bucket>/<key>), because virtual-host style is what MinIO does
	// not do by default.
	Endpoint string `yaml:"endpoint,omitempty"`
	// KMSKeyID selects a customer-managed key for server-side encryption. Empty
	// uses SSE-S3 (AES256), which every S3-compatible store supports.
	//
	// AN ARCHIVE IS TWO PRIVATE KEYS AND A LEDGER, so what encrypts it at rest is
	// a real decision rather than a detail — and the one thing billet can enforce
	// from here is that it asks for encryption at all.
	KMSKeyID string `yaml:"kms_key_id,omitempty"`
}

func (b *BackupS3Config) normalize() {
	if b == nil {
		return
	}

	b.Bucket = strings.TrimSpace(b.Bucket)
	b.Region = strings.TrimSpace(b.Region)
	b.Prefix = strings.TrimSpace(b.Prefix)
	b.Endpoint = strings.TrimSpace(b.Endpoint)
	b.KMSKeyID = strings.TrimSpace(b.KMSKeyID)

	if b.Prefix == "" {
		b.Prefix = "billet-backups"
	}
}

// validateBackup checks the archive store, and refuses a block that names none.
//
// AN EMPTY `backup: {}` IS A MISTAKE, NOT A DEFAULT. It reads as "billet is
// looking after this" and does nothing at all, which is the one failure mode a
// backup must not have — the operator finds out on the day they need the archive
// that was never uploaded. Leaving the section out entirely is the supported way
// to say the directory is the seam.
//
// A BARE `backup:` WITH NOTHING UNDER IT CANNOT BE CAUGHT HERE, and that is
// MEASURED: yaml.v3 does not call UnmarshalYAML for a null value on a pointer
// field or a value one, so the field is left untouched and is indistinguishable
// from the key being absent. Catching it would mean decoding the document into
// a yaml.Node and walking it. What covers it instead is `billet local backup`
// itself, which says on EVERY run when an archive is still on the disk it
// protects.
func (c *Config) validateBackup() []error {
	if c.Backup == nil {
		return nil
	}

	if c.Backup.S3 == nil {
		return []error{errors.New("backup: is set but names no destination. Remove the section " +
			"if `billet local backup --out <dir>` and your own tooling are the plan — an empty " +
			"one reads as a backup billet is looking after and uploads nothing")}
	}

	// A BACKUP IS OF A CONTROL PLANE, so a node-only host declaring one has
	// written down somewhere nothing will ever put anything.
	if c.Server == nil {
		return []error{errors.New("backup: is set but this config declares no server, and a " +
			"backup is of a control plane: the ledger, the deployment identity and the " +
			"node-wire authority all live in server.state_dir")}
	}

	return CheckBackupS3(*c.Backup.S3)
}

// CheckEBSS3 applies the safety rules needed by both config loading and the
// exported cloud-store constructor.
// CheckBackupS3 reports everything wrong with the archive store.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckEBSS3 and CheckCeph: the store
// constructor is exported too, so a caller whose configuration did not come
// through config.Load must not be able to build one that points somewhere else.
//
// THE REGION IS TWO DIFFERENT FACTS depending on the endpoint, and conflating
// them refuses correct deployments. With no endpoint it selects the AWS host
// billet dials, so it has to look like an AWS region or every request goes
// somewhere that does not exist. With one, it is only the SIGNING region — the
// server on the far side decides what it accepts, and Ceph RGW and MinIO
// deployments legitimately use names AWS never issued.
func CheckBackupS3(b BackupS3Config) []error {
	var errs []error

	switch {
	case b.Endpoint == "":
		if err := CheckEC2Region(b.Region); err != nil {
			errs = append(errs, errors.New("backup.s3.region is required and must look like an "+
				"aws region (something like us-west-2): with no backup.s3.endpoint it selects "+
				"the host billet dials, and it is signed into every request"))
		}
	case b.Region == "" || strings.ContainsAny(b.Region, " \t\r\n\x00"):
		errs = append(errs, errors.New("backup.s3.region is required even with an endpoint: "+
			"SigV4 signs a region into every request, and the store on the far side compares "+
			"it. Use the one that store expects, conventionally us-east-1"))
	}

	if err := CheckBackupEndpoint(b.Endpoint); err != nil {
		errs = append(errs, err)
	}

	// THE SAME BUCKET RULE THE CACHE STORE USES. A dot in the name breaks TLS
	// against the virtual-host address, which is the one billet builds when no
	// endpoint overrides it.
	if !s3BucketName.MatchString(b.Bucket) || strings.Contains(b.Bucket, ".") ||
		net.ParseIP(b.Bucket) != nil || strings.Contains(b.Bucket, "..") ||
		strings.Contains(b.Bucket, ".-") || strings.Contains(b.Bucket, "-.") {
		errs = append(errs, fmt.Errorf(
			"backup.s3.bucket %q is not a TLS-compatible S3 bucket name without dots", b.Bucket))
	}

	// A LITERAL PREFIX, because it is what an IAM policy is scoped to. A wildcard
	// there widens the grant to every sibling prefix in the bucket — which is
	// every other deployment's archives, each of which is two private keys.
	if strings.HasPrefix(b.Prefix, "/") || strings.HasSuffix(b.Prefix, "/") ||
		strings.ContainsRune(b.Prefix, 0) || strings.ContainsAny(b.Prefix, "*?") {
		errs = append(errs, errors.New("backup.s3.prefix must be a relative object prefix with "+
			"no leading or trailing slash, no NUL and no wildcard"))
	}

	for segment := range strings.SplitSeq(b.Prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			errs = append(errs,
				errors.New("backup.s3.prefix contains an empty, dot, or dot-dot segment"))

			break
		}
	}

	return errs
}

// CheckBackupEndpoint applies the same rule to the archive store's endpoint.
//
// THE SAME RULE, NOT A SECOND READING OF IT. Both endpoints receive requests
// billet signs and sends a session token with, so both refuse plaintext outside
// loopback for one reason — and a second implementation is a second security
// boundary, which is the argument internal/awssig already makes about having one
// signer rather than two.
func CheckBackupEndpoint(endpoint string) error {
	return checkSignedEndpoint("backup.s3.endpoint", endpoint)
}
