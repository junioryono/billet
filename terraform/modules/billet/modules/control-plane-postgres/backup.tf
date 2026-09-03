# The off-site copy of the half of this deployment that is still on this machine.
#
# WHAT IT HOLDS IS AN IDENTITY-ONLY ARCHIVE, and the omission is deliberate
# rather than a gap. `billet local backup` does not copy a PostgreSQL ledger — a
# consistent copy of one is pg_dump or your provider's snapshot, and a
# half-measure copying rows through billet's own connection would produce an
# archive that looks like a backup and is not — so what it writes here is the
# deployment identity, the node-wire CA and its rotation state, and the GitHub
# App private key, with the manifest recording the ledger as external and naming
# the engine and the dsn_env variable.
#
# The restore side pairs the halves rather than trusting them to line up: it
# refuses such an archive onto a host whose config names a local ledger, and
# refuses it at all without --external-ledger-attached, which is the operator
# answering the one question billet cannot — whether the database on the other
# end of that DSN is back.
#
# THAT IS MORE LOAD-BEARING HERE, NOT LESS, WHICH IS WHY THE GAP IS STATED RATHER
# THAN GLOSSED. There is no ledger volume on this profile — the instance is
# disposable by design, and its root volume is deleted on termination — so
# nothing but an off-site copy stands between a failed controller and a fleet
# whose certificates no replacement can honour. A ledger without its identity is
# a fresh authority that cannot see the compute the old one launched, and the
# GitHub App private key is issued exactly once.
#
# CREATED ONLY WHEN ASKED, and adoptable, exactly as on the SQLite profile.

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

  # THE ARCHIVES OUTLIVE THE STACK. A `terraform destroy` that took the backups
  # with it would remove the one thing that can rebuild what it is destroying —
  # and on this profile that is the ONLY copy of the deployment identity, since
  # there is no retained volume beside it. force_destroy stays false so a bucket
  # holding objects refuses rather than emptying itself.
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

# AN ARCHIVE IS TWO PRIVATE KEYS. Encryption at rest is not a detail here, and
# bucket_key_enabled keeps a KMS key from being called per object.
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
# BILLET'S OWN GENERATOR'S OUTPUT, byte-for-byte the same renderings the SQLite
# profile commits and pinned by the same drift test in internal/tfpolicy — the
# principal is identical, so a second answer to what it may do would be a second
# source of truth. Restating the statements in HCL is exactly what that test
# exists to prevent.
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
  # individually.
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
