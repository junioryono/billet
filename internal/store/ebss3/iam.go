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
