package archivestore

// IAM action names for the operations this package performs, kept beside the
// code that performs them. internal/awspolicy imports these and assembles the
// policy; a drift test pins the result, and the Terraform module consumes that
// same committed rendering.
//
// THERE IS NO DELETE HERE AND THERE MUST NOT BE ONE. This store never issues a
// delete, so granting one would be a permission nothing uses on a host that also
// holds the GitHub App private key and the node-wire CA — which is exactly the
// host whose compromise the off-site copy exists to survive. Retention is the
// bucket's job, through versioning and a lifecycle rule; a role that can PUT and
// GET but not DELETE cannot destroy the archives it wrote.
const (
	IAMS3GetObject  = "s3:GetObject"
	IAMS3PutObject  = "s3:PutObject"
	IAMS3ListBucket = "s3:ListBucket"

	// KMS actions a bucket encrypted with a customer-managed key requires. S3
	// server-side encryption needs a data key to write and a decrypt to read;
	// there is no grant, because nothing hands this key to another service.
	IAMKMSDescribeKey     = "kms:DescribeKey"
	IAMKMSGenerateDataKey = "kms:GenerateDataKey"
	IAMKMSDecrypt         = "kms:Decrypt"
)

// ObjectIAMActions are the per-object permissions an archive store needs, scoped
// by a bundled policy to this deployment's prefix.
func ObjectIAMActions() []string {
	return []string{IAMS3GetObject, IAMS3PutObject}
}

// ListIAMActions are the bucket-level permissions, scoped by prefix in the
// assembled statement's condition.
func ListIAMActions() []string {
	return []string{IAMS3ListBucket}
}

// KMSCryptoIAMActions are the permissions a customer-managed bucket key needs.
// A bundled policy scopes them to the one key ARN and conditions them on
// kms:ViaService for S3, so the key can be used through the bucket and not
// called directly by a compromised control-plane role.
func KMSCryptoIAMActions() []string {
	return []string{IAMKMSDescribeKey, IAMKMSGenerateDataKey, IAMKMSDecrypt}
}
