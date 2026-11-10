package ebss3

// IAM action names for the cache storage operations this package performs, kept
// beside the code that performs them. internal/awspolicy imports these and
// assembles the node's IAM policy; a drift test pins the result.
//
// The delete actions are separated from the rest because they are the ones a
// bundled policy scopes with a tag condition (below): a node may create and read
// volumes freely, but may delete only what billet owns.
const (
	IAMCreateVolume      = "ec2:CreateVolume"
	IAMDeleteVolume      = "ec2:DeleteVolume"
	IAMDescribeVolumes   = "ec2:DescribeVolumes"
	IAMCreateSnapshot    = "ec2:CreateSnapshot"
	IAMDeleteSnapshot    = "ec2:DeleteSnapshot"
	IAMDescribeSnapshots = "ec2:DescribeSnapshots"

	IAMS3GetObject  = "s3:GetObject"
	IAMS3PutObject  = "s3:PutObject"
	IAMS3ListBucket = "s3:ListBucket"
	// The archives and entries of a cache whose unit is a file (ADR-024) are evicted
	// by the node itself, by how recently a job used them, which no lifecycle rule
	// can see, and a commit that dies leaves a multipart upload only these end.
	IAMS3DeleteObject     = "s3:DeleteObject"
	IAMS3AbortMultipart   = "s3:AbortMultipartUpload"
	IAMS3ListMultipart    = "s3:ListBucketMultipartUploads"
	IAMS3ListMultipartOne = "s3:ListMultipartUploadParts"

	// KMS actions an EBS volume encrypted with a customer-managed key requires.
	// EBS uses grants: CreateGrant lets the EC2 service decrypt the volume for the
	// instance, and the data-key actions cover attach/detach of an encrypted disk.
	IAMKMSCreateGrant     = "kms:CreateGrant"
	IAMKMSDescribeKey     = "kms:DescribeKey"
	IAMKMSGenerateDataKey = "kms:GenerateDataKeyWithoutPlaintext"
	IAMKMSReEncrypt       = "kms:ReEncrypt*"
	IAMKMSDecrypt         = "kms:Decrypt"
)

// OwnerTagKey and CacheOwnerTagKey are the resource tags billet stamps on the EBS
// volumes and snapshots it owns. A bundled IAM policy conditions the delete
// actions on these so the node's role can remove only billet's own cache
// resources — billet rechecks the same tags in code, and IAM is the account-level
// backstop if state is corrupted or a future caller supplies the wrong id.
const (
	OwnerTagKey      = ownerTag
	CacheOwnerTagKey = cacheOwnerTag
)

// VolumeCreateIAMActions is CreateVolume, which a bundled policy grants TWICE:
// once on "*" conditioned on the owner tag being PRESENT IN THE REQUEST
// (aws:RequestTag, the new volume), and once on the account-less snapshot ARN
// conditioned on the owner tag ON THE RESOURCE, because a clone authorizes its
// source snapshot as well as the volume it creates — measured, a policy with
// only the first statement refuses every clone. Two statements rather than one,
// since a request-tag condition cannot be satisfied in the snapshot's
// authorization and a resource-tag condition would deny a fresh volume.
func VolumeCreateIAMActions() []string {
	return []string{IAMCreateVolume}
}

// SnapshotCreateIAMActions is CreateSnapshot, which a bundled policy conditions on
// the owner tag present in BOTH the request (the new snapshot) and on the resource
// (the source volume): billet always snapshots one of its own tagged volumes, so
// the source condition does not deny it and it stops the role from copying a
// foreign volume into a billet-tagged snapshot.
func SnapshotCreateIAMActions() []string {
	return []string{IAMCreateSnapshot}
}

// StorageDescribeIAMActions are the read-only EBS describes, which act on "*".
func StorageDescribeIAMActions() []string {
	return []string{IAMDescribeVolumes, IAMDescribeSnapshots}
}

// IAMAttachVolume and IAMDetachVolume are what a self-attaching store does with its
// volumes on its own instance (attach.go).
const (
	IAMAttachVolume = "ec2:AttachVolume"
	IAMDetachVolume = "ec2:DetachVolume"
)

// SelfAttachIAMActions are the attach and detach a node on AWS metal makes of its own
// cache volumes to its own instance. Each authorizes the volume and the instance, and a
// bundled policy scopes the volume by billet's owner tag and the instance by the pool's.
func SelfAttachIAMActions() []string {
	return []string{IAMAttachVolume, IAMDetachVolume}
}

// StorageDeleteIAMActions are the destructive EBS permissions a bundled policy
// scopes with the owner-tag condition on the RESOURCE.
func StorageDeleteIAMActions() []string {
	return []string{IAMDeleteVolume, IAMDeleteSnapshot}
}

// S3ObjectIAMActions are the per-object permissions the cache needs on its prefix.
func S3ObjectIAMActions() []string {
	return []string{IAMS3GetObject, IAMS3PutObject}
}

// S3ListIAMActions are the bucket-level permissions, scoped by prefix in the
// assembled statement's condition.
func S3ListIAMActions() []string {
	return []string{IAMS3ListBucket}
}

// S3ObjectDeleteIAMActions end what eviction and an abandoned upload leave. A bundled
// policy grants them on the object half's names only and never on the state objects,
// so the credential that can evict an archive cannot unpublish a sticky disk.
func S3ObjectDeleteIAMActions() []string {
	return []string{IAMS3DeleteObject, IAMS3AbortMultipart, IAMS3ListMultipartOne}
}

// S3ObjectDeleteSubtrees are the names under an owner those actions reach, as
// archive.go lays them out.
func S3ObjectDeleteSubtrees() []string {
	return []string{"archives", "archive-used", "stage", "blobs", "blob-uses"}
}

// S3ListMultipartIAMActions is the listing of unfinished uploads. Bucket-level, with
// no prefix condition: S3 evaluates s3:prefix for an object listing only.
func S3ListMultipartIAMActions() []string {
	return []string{IAMS3ListMultipart}
}

// KMSCryptoIAMActions are the direct-use permissions on a customer-managed EBS
// key. A bundled policy scopes them to the one key ARN and conditions them on
// kms:ViaService for EC2, so the key can be used only through EBS and not called
// directly by a compromised node role.
func KMSCryptoIAMActions() []string {
	return []string{IAMKMSDescribeKey, IAMKMSGenerateDataKey, IAMKMSReEncrypt, IAMKMSDecrypt}
}

// KMSGrantIAMActions is CreateGrant, which EBS uses to let the EC2 service decrypt
// the volume for the instance. A bundled policy conditions it on
// kms:GrantIsForAWSResource so the role can create grants only for AWS services,
// not delegate the key to an arbitrary principal.
func KMSGrantIAMActions() []string {
	return []string{IAMKMSCreateGrant}
}
