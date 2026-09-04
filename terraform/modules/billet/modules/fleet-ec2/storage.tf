# THE CACHE STORE. S3 holds the small fenced pointer and lease state; the EBS
# volumes (created by billet at runtime, not here) hold the block generations.
# Cache bytes never leave the availability zone — the bucket is not a data path.

resource "aws_kms_key" "cache" {
  count = local.enable_cache_kms ? 1 : 0

  description             = "${var.name} cache EBS encryption"
  deletion_window_in_days = 7
  enable_key_rotation     = true
  tags                    = local.tags
}

resource "aws_kms_alias" "cache" {
  count = local.enable_cache_kms ? 1 : 0

  name          = "alias/${var.name}-cache"
  target_key_id = aws_kms_key.cache[0].key_id
}

resource "aws_s3_bucket" "cache" {
  count = var.enable_cache ? 1 : 0

  bucket = local.cache_bucket
  tags   = local.tags
}

resource "aws_s3_bucket_public_access_block" "cache" {
  count = var.enable_cache ? 1 : 0

  bucket                  = aws_s3_bucket.cache[0].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "cache" {
  count = var.enable_cache ? 1 : 0

  bucket = aws_s3_bucket.cache[0].id
  rule {
    apply_server_side_encryption_by_default {
      # The S3 bucket carries only the small fenced pointer and lease state, and
      # billet's node role's KMS grant is scoped to EC2 (the EBS volumes), not S3.
      # So the bucket stays on S3-managed AES256; the customer key encrypts the
      # cache's EBS volumes, which billet creates at runtime.
      sse_algorithm = "AES256"
    }
  }
}

# THE SPOT INTERRUPTION QUEUES. Created when spot is enabled; a node consumes its
# own to learn one of its instances is about to be reclaimed, and billet requires
# that queue's basename to equal the node's name, so there is exactly one per spot
# node. Delivering a warning to the RIGHT node's queue needs a tag-scoped router,
# because EventBridge cannot match on an instance tag; that router is in spot.tf,
# and its handler is told every name below so it can tell a foreign queue from its
# own grant failing.
locals {
  # The primary queue's name, declared once: the resource, spot_node_names'
  # validation and the router all read it from here.
  spot_queue_name = "${var.name}-spot-interruptions"
}

resource "aws_sqs_queue" "interruptions" {
  count = var.enable_spot ? 1 : 0

  name                       = local.spot_queue_name
  message_retention_seconds  = 300
  visibility_timeout_seconds = 120
  sqs_managed_sse_enabled    = true
  tags                       = local.tags
}

# One further queue per further spot node, named exactly the node. A SEPARATE
# resource rather than the primary widened to for_each: the primary's key would be
# an expression of var.name, which a moved block cannot name, so an existing
# deployment would have its queue replaced. spot_node_names' validation is what
# keeps this empty without enable_spot.
resource "aws_sqs_queue" "spot_nodes" {
  for_each = toset(var.spot_node_names)

  name = each.key
  # THE SAME ATTRIBUTES AS THE PRIMARY. The visibility timeout in particular is
  # the whole two-minute warning: a consumer that hides a message for longer
  # hides it past the reclaim.
  message_retention_seconds  = 300
  visibility_timeout_seconds = 120
  sqs_managed_sse_enabled    = true
  tags                       = local.tags
}

locals {
  # EVERY QUEUE THIS MODULE SERVES, the primary first and then the extras in the
  # order they were named, read from the resources rather than rebuilt. The node
  # grant, the router grant and the router's environment are all derived from this
  # one list, which is what makes them move together.
  spot_queues      = concat(aws_sqs_queue.interruptions, [for n in var.spot_node_names : aws_sqs_queue.spot_nodes[n]])
  spot_queue_arns  = [for q in local.spot_queues : q.arn]
  spot_queue_names = [for q in local.spot_queues : q.name]
}
