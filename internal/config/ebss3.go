package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// EBSS3Config points a cloud node at its site's EBS and S3 cache storage.
//
// IT EXISTS ONLY TO BACK node.cache. Nothing on a running node reads it by any
// other route, so without that listener every field here is inert and every job
// runs on the instance's root volume — which `billet check` refuses rather than
// config load, because `billet decommission` purges what the cache left behind
// and `billet init iam` renders its grants, and both read this block on a config
// whose listener is already gone.
type EBSS3Config struct {
	// Region is the signing region for both services and must match node.ec2.
	Region string `yaml:"region"`
	// AvailabilityZone is where cache volumes are created. EBS volumes and the
	// instances consuming them must be in the same zone.
	AvailabilityZone string `yaml:"availability_zone"`
	// Bucket holds the atomic pointer, lease and fencing state objects.
	Bucket string `yaml:"bucket"`
	// Prefix isolates one deployment and site inside a bucket.
	Prefix string `yaml:"prefix,omitempty"`
	// KMSKeyID optionally selects one customer-managed key for EBS volumes and
	// snapshots. Empty uses the account's EBS encryption default key.
	//
	// A PER-DEPLOYMENT key is what closes the cross-deployment READ boundary:
	// the IAM tag conditions cannot stop another deployment cloning a snapshot
	// and reading the cache (ec2:CreateVolume does not authorize the parent
	// snapshot; the value-scoped conditions give destructive integrity only).
	// With each deployment's snapshots encrypted under its own key — whose key
	// policy delegates to IAM and admits no foreign role — and its role's KMS
	// grants scoped to exactly that key, as `billet init iam` and the terraform
	// module both do, the foreign clone fails at the KMS grant instead
	// (measured with iam:SimulateCustomPolicy: every KMS action on another
	// deployment's key is implicitly denied). Sharing one key between
	// deployments silently reopens that boundary — as does a key whose own
	// policy or grants admit other roles, which no identity policy can see.
	// Leaving this EMPTY encrypts under the ACCOUNT's default EBS key — the
	// AWS-managed aws/ebs unless the account configured another — and aws/ebs
	// authorizes any principal in the account through EC2, so an opted-out
	// deployment's snapshots stay readable no matter what keys its neighbours
	// set. A key protects only snapshots created after it was set; evict or
	// re-snapshot older generations to bring them under it.
	KMSKeyID string `yaml:"kms_key_id,omitempty"`
}

func (e *EBSS3Config) normalize() {
	if e == nil {
		return
	}

	e.Region = strings.TrimSpace(e.Region)
	e.AvailabilityZone = strings.TrimSpace(e.AvailabilityZone)
	e.Bucket = strings.TrimSpace(e.Bucket)
	e.Prefix = strings.TrimSpace(e.Prefix)
	e.KMSKeyID = strings.TrimSpace(e.KMSKeyID)
	if e.Prefix == "" {
		e.Prefix = "billet-cache"
	}
}

var (
	s3BucketName       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{1,61}[a-z0-9])$`)
	availabilityZoneID = regexp.MustCompile(`^[a-z0-9-]+$`)
)

func (c *Config) validateEBSS3Node() []error {
	if c.Node.EBSS3 == nil {
		return nil
	}
	if c.Node.Provider != ProviderEC2 {
		return []error{fmt.Errorf("node.ebs_s3 is set but this node's provider is %s; EBS volumes can attach only to EC2 instances in their availability zone", c.Node.Provider)}
	}

	e := c.Node.EBSS3
	var errs []error
	if c.Node.Site == "" {
		errs = append(errs, errors.New("node.site is required with node.ebs_s3 because cache keys are scoped by site"))
	}
	errs = append(errs, CheckEBSS3(*e)...)
	if c.Node.EC2 != nil && e.Region != c.Node.EC2.Region {
		errs = append(errs, fmt.Errorf("node.ebs_s3.region %q differs from node.ec2.region %q; a cache volume and the instance using it must be in one region", e.Region, c.Node.EC2.Region))
	}
	if c.Node.Site != "" {
		for _, site := range c.Sites {
			if site.Name == c.Node.Site && site.Store != SiteStoreEBSS3 {
				errs = append(errs, fmt.Errorf("node.site %q selects %s storage but node.ebs_s3 is configured", site.Name, site.Store))
			}
		}
	}

	return errs
}

func CheckEBSS3(e EBSS3Config) []error {
	var errs []error
	if err := CheckEC2Region(e.Region); err != nil {
		errs = append(errs, fmt.Errorf("node.ebs_s3.region: %w", err))
	}
	if e.AvailabilityZone == e.Region || !strings.HasPrefix(e.AvailabilityZone, e.Region) ||
		!availabilityZoneID.MatchString(e.AvailabilityZone) {
		errs = append(errs, fmt.Errorf("node.ebs_s3.availability_zone %q is not a zone in region %q", e.AvailabilityZone, e.Region))
	}
	if !s3BucketName.MatchString(e.Bucket) || strings.Contains(e.Bucket, ".") || net.ParseIP(e.Bucket) != nil ||
		strings.Contains(e.Bucket, "..") || strings.Contains(e.Bucket, ".-") ||
		strings.Contains(e.Bucket, "-.") {
		errs = append(errs, fmt.Errorf("node.ebs_s3.bucket %q is not a TLS-compatible S3 bucket name without dots", e.Bucket))
	}
	if strings.HasPrefix(e.Prefix, "/") || strings.HasSuffix(e.Prefix, "/") ||
		strings.ContainsRune(e.Prefix, 0) {
		errs = append(errs, errors.New("node.ebs_s3.prefix must be a relative object prefix without a trailing slash or NUL"))
	}
	for _, segment := range strings.Split(e.Prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			errs = append(errs, errors.New("node.ebs_s3.prefix contains an empty, dot, or dot-dot segment"))

			break
		}
	}

	return errs
}
