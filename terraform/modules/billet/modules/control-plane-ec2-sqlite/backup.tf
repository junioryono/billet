# The off-site copy of everything that makes this deployment itself.
#
# A BACKUP ON THE DISK IT PROTECTS IS NOT ONE. `billet local backup --out <dir>`
# writes the ledger, the deployment identity, the GitHub App private key and the
# node-wire authority as one verified unit — and leaves it on the same volume as
# the deployment. ADR-001 has always said the copy belongs in S3 with a REHEARSED
# restore; this is the bucket, and docs/reference/records/restore-rehearsal.md is the rehearsal.
#
# CREATED ONLY WHEN ASKED, and adoptable: an operator who already has a backup
# bucket sets create_backup_bucket = false and passes backup_bucket, and this
# child grants the controller access to it without owning it.

locals {
  backup_enabled = var.create_backup_bucket || var.backup_bucket != ""

  # THE NAME billet WILL WRITE TO, composed rather than read back off the
  # resource: a created bucket's `id` is unknown until apply, and taking it from
  # there makes the IAM document below unknown at plan too — so nothing could
  # assert what the controller is granted, which is the thing most worth
  # asserting. The name is ours to choose, so it is known.
  backup_bucket = var.create_backup_bucket ? "${var.name}-backups" : var.backup_bucket
  backup_prefix = trimsuffix(var.backup_prefix, "/")
}

resource "aws_s3_bucket" "backups" {
  count = var.create_backup_bucket ? 1 : 0

  bucket = local.backup_bucket

  # THE ARCHIVES OUTLIVE THE STACK, exactly as the ledger volume does. A
  # `terraform destroy` that took the backups with it would remove the one thing
  # that can rebuild what it is destroying — and force_destroy stays false so a
  # bucket holding objects refuses rather than emptying itself.
  tags = merge(local.tags, { "Name" = "${var.name}-backups" })

  lifecycle {
    prevent_destroy = true
  }
}

# VERSIONING IS WHAT MAKES THE NO-DELETE GRANT MEAN SOMETHING. billet's own
# credential carries s3:GetObject and s3:PutObject and no delete at all, and its
# writes are conditional (If-None-Match: *) so they refuse an occupied key — but
# a credential is only ever one mistake from being wider than intended, and a
# versioned bucket keeps the previous object whatever happens to the current one.
resource "aws_s3_bucket_versioning" "backups" {
  count = var.create_backup_bucket ? 1 : 0

  bucket = aws_s3_bucket.backups[0].id

  versioning_configuration {
    status = "Enabled"
  }
}

# AN ARCHIVE IS TWO PRIVATE KEYS AND A LEDGER. Encryption at rest is not a
# detail here, and bucket_key_enabled keeps a KMS key from being called per
# object.
resource "aws_s3_bucket_server_side_encryption_configuration" "backups" {
  count = var.create_backup_bucket ? 1 : 0

  bucket = aws_s3_bucket.backups[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = var.backup_kms_key_arn == "" ? "AES256" : "aws:kms"
      kms_master_key_id = var.backup_kms_key_arn == "" ? null : var.backup_kms_key_arn
    }

    bucket_key_enabled = var.backup_kms_key_arn != ""
  }
}

resource "aws_s3_bucket_public_access_block" "backups" {
  count = var.create_backup_bucket ? 1 : 0

  bucket = aws_s3_bucket.backups[0].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# RETENTION IS THE BUCKET'S JOB, because billet has no delete. Old NONCURRENT
# versions expire; the current object of every archive stays until an operator
# removes it deliberately. A rule that expired current objects would delete
# backups on a timer, which is not something to configure quietly — set
# backup_retention_days to 0 and no rule is created at all.
resource "aws_s3_bucket_lifecycle_configuration" "backups" {
  count = var.create_backup_bucket && var.backup_retention_days > 0 ? 1 : 0

  bucket = aws_s3_bucket.backups[0].id

  rule {
    id     = "expire-noncurrent-archives"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.backup_retention_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.backups]
}

# THE CONTROLLER'S GRANT: read, write and list its own prefix, and NEVER delete.
#
# BILLET'S OWN GENERATOR'S OUTPUT, exactly as fleet-ec2's node policy is: the
# module commits a rendering of internal/awspolicy (kept equal by
# internal/tfpolicy's drift test) and substitutes this deployment's bucket,
# prefix, partition and region for the sentinels in it. Restating the statements
# in HCL — which the first version of this file did — is a second source of truth
# for what billet may do, and the whole point of the generator is that the
# actions the code performs and the permissions the policy grants are decided in
# one place.
#
# There are no compute permissions in it at all: a control plane launches
# nothing, and NoCompute in the generator is what says so.
locals {
  _backup_policy_file = (
    var.backup_kms_key_arn == ""
    ? "${path.module}/policy/backup-policy.json"
    : "${path.module}/policy/backup-policy-kms.json"
  )

  # The KMS-ARN replace is INNERMOST so it matches the whole sentinel ARN (which
  # itself contains TFPARTITION and TFREGION) before those are substituted
  # individually — the same ordering fleet-ec2's rendering needs, and for the
  # same reason.
  backup_policy = replace(replace(replace(replace(replace(replace(
    file(local._backup_policy_file),
    "arn:TFPARTITION:kms:TFREGION:000000000000:key/TFKMSKEY",
    var.backup_kms_key_arn),
    "TFPARTITION", data.aws_partition.this.partition),
    "TFDNSSUFFIX", data.aws_partition.this.dns_suffix),
    "TFREGION", local.region),
    "TFBUCKET", local.backup_bucket),
  "TFPREFIX", local.backup_prefix)
}

# AN INLINE ROLE POLICY rather than a managed one, deliberately: a managed
# policy's document is normalised by the provider and therefore unknown at plan,
# so nothing could assert what it grants under the mocked test provider — and
# what this grants is exactly the thing worth asserting.
resource "aws_iam_role_policy" "backups" {
  count = local.backup_enabled && var.create_instance_profile ? 1 : 0

  name   = "${var.name}-control-plane-backups"
  role   = aws_iam_role.this[0].id
  policy = local.backup_policy
}
