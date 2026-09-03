# Reaching Billet hosts from CI through AWS Systems Manager.
#
# OPTIONAL, AND NOT PART OF THE billet ROOT MODULE. Billet needs no inbound
# connectivity to run jobs: nodes always dial outbound. This exists only so a
# deployment already administered from AWS can converge its hosts from CI without
# opening a port or holding an SSH key.
#
# WHY A MODULE RATHER THAN A CHECKLIST. The Ansible SSM connection plugin requires
# an S3 bucket, and its own documentation states that files transit that bucket
# EVEN FOR MODULES THAT SEND NO FILES, because Ansible ships the module's own .py
# through it -- and that secrets in a task's arguments are written into those
# objects in plaintext. The Billet host role installs a GitHub App private key. So
# the difference between a safe bucket and a permanent copy of that key is a
# setting nobody would think to check, and encoding it is the point of this module.

locals {
  tags = merge(var.tags, { "sh.billet.component" = "converge-aws-ssm" })

  oidc_provider_arn = var.github_oidc_provider_arn != "" ? var.github_oidc_provider_arn : aws_iam_openid_connect_provider.github[0].arn

  # AN EXACT SUBJECT, NEVER A WILDCARD. See the trust policy below.
  github_subject = var.github_subject != "" ? var.github_subject : "repo:${var.github_repository}:ref:refs/heads/${var.github_branch}"

  # Tagged so the CI role's StartSession can be scoped to THESE nodes. An
  # activation propagates its tags to every instance registered through it.
  node_tag = "sh.billet.converge"
}

data "aws_caller_identity" "current" {}

# --- the transfer bucket -----------------------------------------------------

# A CUSTOMER-MANAGED KEY, because of what transits this bucket.
#
# SSE-S3 encrypts at rest and gives the operator no key-level control: no policy
# of their own on who may decrypt, no separate audit trail, and no revocation
# short of deleting objects. For a bucket that carries a GitHub App private key in
# plaintext objects, that control is worth a key. Rotation is on because the key
# outlives any single converge.
resource "aws_kms_key" "transfer" {
  description             = "${var.name} Ansible SSM transfer bucket"
  enable_key_rotation     = true
  deletion_window_in_days = 7

  tags = local.tags
}

resource "aws_kms_alias" "transfer" {
  name          = "alias/${var.name}-transfer"
  target_key_id = aws_kms_key.transfer.key_id
}

resource "aws_s3_bucket" "transfer" {
  bucket        = "${var.name}-${data.aws_caller_identity.current.account_id}"
  force_destroy = true

  tags = local.tags
}

# SUSPENDED, DELIBERATELY, AND THIS IS THE LOAD-BEARING SETTING IN THE MODULE.
#
# Ansible deletes its transfer objects at the end of a run. With versioning
# ENABLED, the delete leaves a non-current version behind, so a GitHub App private
# key that passed through a task's arguments persists in version history -- exactly
# what the plugin's documentation warns about, and unreadable from the object
# listing that looks empty. Suspended keeps the delete final.
resource "aws_s3_bucket_versioning" "transfer" {
  bucket = aws_s3_bucket.transfer.id

  versioning_configuration {
    status = "Suspended"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "transfer" {
  bucket = aws_s3_bucket.transfer.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.transfer.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "transfer" {
  bucket = aws_s3_bucket.transfer.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# A BACKSTOP, NOT THE MECHANISM. Ansible cleans up after itself; this bounds what
# an interrupted run leaves, because those objects can carry a private key.
resource "aws_s3_bucket_lifecycle_configuration" "transfer" {
  bucket = aws_s3_bucket.transfer.id

  rule {
    id     = "expire-transfer-objects"
    status = "Enabled"

    filter {}

    expiration {
      days = var.transfer_object_expiry_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

resource "aws_s3_bucket_policy" "transfer" {
  bucket = aws_s3_bucket.transfer.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyUnencryptedTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource = [
        aws_s3_bucket.transfer.arn,
        "${aws_s3_bucket.transfer.arn}/*",
      ]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
}

# --- the hybrid activation ---------------------------------------------------

# HYBRID, BECAUSE A BILLET HOST IS NOT AN EC2 INSTANCE. Most SSM documentation
# assumes it is; a bare-metal machine in an office or a rack registers through an
# activation instead, which is why this module exists rather than a note saying
# "turn on SSM".
resource "aws_iam_role" "managed_instance" {
  name = "${var.name}-managed-instance"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ssm.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })

  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "managed_instance" {
  role       = aws_iam_role.managed_instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# THE ACTIVATION CODE IS A REGISTRATION SECRET and lands in Terraform state. It is
# single-purpose and expiring -- it enrolls a machine as a managed instance and
# grants nothing else -- but treat the state file accordingly.
resource "aws_ssm_activation" "hosts" {
  name               = var.name
  iam_role           = aws_iam_role.managed_instance.name
  registration_limit = 20

  # PROPAGATED TO EVERY NODE REGISTERED THROUGH THIS ACTIVATION, which is what
  # makes the session policy below scopeable to this deployment's hosts rather
  # than to every managed node in the account.
  tags = merge(local.tags, { (local.node_tag) = var.name })

  depends_on = [aws_iam_role_policy_attachment.managed_instance]
}

# --- the CI role -------------------------------------------------------------

resource "aws_iam_openid_connect_provider" "github" {
  count = var.github_oidc_provider_arn == "" ? 1 : 0

  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]

  tags = local.tags
}

resource "aws_iam_role" "ci" {
  name = "${var.name}-ci"

  # AN EXACT SUBJECT, AND ONE REPOSITORY IS NOT ENOUGH ON ITS OWN.
  #
  # The first version matched `repo:owner/repo:*`, reasoning that naming one
  # repository was the control. It is not: GitHub gives a pull-request job the
  # subject `repo:owner/repo:pull_request`, which that wildcard admits. So any
  # runnable PR workflow with id-token: write could assume a role that reads the
  # bucket a GitHub App private key transits, and open SSM sessions.
  #
  # A REF SUBJECT, AND AN ENVIRONMENT SUBJECT WOULD NOT HAVE FIXED IT.
  #
  # The second version of this trusted repo:<repo>:environment:converge, on the
  # reasoning that an environment carries required reviewers. It does -- and it
  # does NOT exclude pull requests: GitHub emits the environment subject whenever
  # a job REFERENCES an environment, whatever triggered it, so a PR job declaring
  # `environment: converge` matched. Referencing an environment that does not
  # exist also creates it, unprotected, which this module neither provisions nor
  # verifies.
  #
  # A ref subject is the shape a pull-request job cannot produce: its subject is
  # either `pull_request` or the environment form above. An operator who wants
  # reviewers as well should set github_subject to the environment form AND
  # configure that environment's protection rules -- knowing it admits PR jobs
  # that name it.
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = local.oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "token.actions.githubusercontent.com:aud" = "sts.amazonaws.com"
          "token.actions.githubusercontent.com:sub" = local.github_subject
        }
      }
    }]
  })

  tags = local.tags
}

# EXACTLY THE OPERATIONS THE PLUGIN PERFORMS. Its documentation names them:
# GetObject, PutObject, ListBucket, DeleteObject and GetBucketLocation. A broader
# grant on a bucket that carries a private key buys nothing.
resource "aws_iam_role_policy" "ci" {
  name = "${var.name}-ci"
  role = aws_iam_role.ci.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
        Resource = "${aws_s3_bucket.transfer.arn}/*"
      },
      {
        Effect   = "Allow"
        Action   = ["s3:ListBucket", "s3:GetBucketLocation"]
        Resource = aws_s3_bucket.transfer.arn
      },
      {
        # WITHOUT THIS EVERY TRANSFER FAILS. A CMK-encrypted bucket needs the
        # caller to be able to use the key, not merely reach the object.
        Effect   = "Allow"
        Action   = ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey"]
        Resource = aws_kms_key.transfer.arn
      },
      # SESSIONS ONLY ON THIS DEPLOYMENT'S NODES.
      #
      # The first version put StartSession on Resource "*", which made possession
      # of the CI role equivalent to shell access on every SSM-managed node in the
      # account -- unrelated infrastructure included. Naming one repository in the
      # trust policy bounds WHO can assume the role; it says nothing about what
      # the role reaches once assumed.
      #
      # The activation tags every node it registers, so the tag is the scope.
      {
        Effect = "Allow"
        Action = ["ssm:StartSession"]
        Resource = [
          "arn:aws:ec2:*:*:instance/*",
          "arn:aws:ssm:*:*:managed-instance/*",
        ]
        Condition = {
          StringEquals = { "ssm:resourceTag/${local.node_tag}" = var.name }
        }
      },
      {
        # THE DOCUMENT ANSIBLE ACTUALLY USES, which is not the SSH one.
        #
        # The first version granted AWS-StartSSHSession, reasoning from "this is
        # SSH over SSM". The amazon.aws.aws_ssm plugin does not take the SSH proxy
        # path -- it opens a standard shell session, whose document is
        # SSM-SessionManagerRunShell, and AWS requires the document in the policy.
        # So that grant would have denied every legitimate converge while looking
        # deliberate.
        Effect   = "Allow"
        Action   = ["ssm:StartSession"]
        Resource = "arn:aws:ssm:*:*:document/SSM-SessionManagerRunShell"
      },
      {
        # ONLY THIS CALLER'S OWN SESSIONS. Terminate and resume on "*" is
        # authority over other people's sessions, which a converge never needs.
        Effect   = "Allow"
        Action   = ["ssm:TerminateSession", "ssm:ResumeSession"]
        Resource = "arn:aws:ssm:*:*:session/$${aws:userid}-*"
      },
      {
        # WITHOUT THIS THE SESSION OPENS AND CARRIES NOTHING. The control channel
        # is ssm:*, the DATA channel is ssmmessages:*, and a policy with only the
        # former produces a session that connects and then stalls.
        Effect   = "Allow"
        Action   = ["ssmmessages:OpenDataChannel"]
        Resource = "arn:aws:ssm:*:*:session/$${aws:userid}-*"
      },
      {
        # SCOPEABLE, and it was grouped with the fleet describes by assumption.
        # GetConnectionStatus names an instance, so it takes the same tag
        # condition as StartSession.
        Effect = "Allow"
        Action = ["ssm:GetConnectionStatus"]
        Resource = [
          "arn:aws:ec2:*:*:instance/*",
          "arn:aws:ssm:*:*:managed-instance/*",
        ]
        Condition = {
          StringEquals = { "ssm:resourceTag/${local.node_tag}" = var.name }
        }
      },
      {
        # Genuinely not resource-scopeable: these describe the fleet rather than
        # act on a member of it.
        Effect   = "Allow"
        Action   = ["ssm:DescribeInstanceInformation", "ssm:DescribeSessions"]
        Resource = "*"
      },
    ]
  })
}
