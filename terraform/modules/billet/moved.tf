# THE RENAME LEDGER, permanent by design. Every resource the module split
# relocated is mapped here so an existing deployment plans ZERO changes — for
# the ledger volume this is not a nicety but the difference between a rename
# and a plan that prevent_destroy correctly refuses. These blocks never expire:
# a deployment created before the split can arrive at any future version and
# still land on the new addresses.

# --- control-plane-ec2-sqlite ---

moved {
  from = aws_instance.control_plane
  to   = module.control_plane.aws_instance.control_plane
}

moved {
  from = aws_cloudwatch_metric_alarm.control_plane_recover
  to   = module.control_plane.aws_cloudwatch_metric_alarm.control_plane_recover
}

moved {
  from = aws_ebs_volume.ledger
  to   = module.control_plane.aws_ebs_volume.ledger
}

moved {
  from = aws_volume_attachment.ledger
  to   = module.control_plane.aws_volume_attachment.ledger
}

moved {
  from = aws_security_group.control_plane
  to   = module.control_plane.aws_security_group.control_plane
}

moved {
  from = aws_vpc_security_group_ingress_rule.node_wire
  to   = module.control_plane.aws_vpc_security_group_ingress_rule.node_wire
}

moved {
  from = aws_vpc_security_group_ingress_rule.ssh
  to   = module.control_plane.aws_vpc_security_group_ingress_rule.ssh
}

moved {
  from = aws_vpc_security_group_egress_rule.control_plane_all
  to   = module.control_plane.aws_vpc_security_group_egress_rule.control_plane_all
}

# --- fleet-ec2 ---

moved {
  from = aws_iam_role.node
  to   = module.fleet.aws_iam_role.node
}

moved {
  from = aws_iam_role_policy.node
  to   = module.fleet.aws_iam_role_policy.node
}

moved {
  from = aws_iam_role_policy.pass_role
  to   = module.fleet.aws_iam_role_policy.pass_role
}

moved {
  from = aws_iam_instance_profile.node
  to   = module.fleet.aws_iam_instance_profile.node
}

moved {
  from = aws_iam_role_policy.spot
  to   = module.fleet.aws_iam_role_policy.spot
}

moved {
  from = aws_security_group.runner
  to   = module.fleet.aws_security_group.runner
}

moved {
  from = aws_vpc_security_group_egress_rule.runner_all
  to   = module.fleet.aws_vpc_security_group_egress_rule.runner_all
}

moved {
  from = aws_kms_key.cache
  to   = module.fleet.aws_kms_key.cache
}

moved {
  from = aws_kms_alias.cache
  to   = module.fleet.aws_kms_alias.cache
}

moved {
  from = aws_s3_bucket.cache
  to   = module.fleet.aws_s3_bucket.cache
}

moved {
  from = aws_s3_bucket_public_access_block.cache
  to   = module.fleet.aws_s3_bucket_public_access_block.cache
}

moved {
  from = aws_s3_bucket_server_side_encryption_configuration.cache
  to   = module.fleet.aws_s3_bucket_server_side_encryption_configuration.cache
}

moved {
  from = aws_sqs_queue.interruptions
  to   = module.fleet.aws_sqs_queue.interruptions
}

moved {
  from = aws_iam_role.spot_router
  to   = module.fleet.aws_iam_role.spot_router
}

moved {
  from = aws_iam_role_policy.spot_router
  to   = module.fleet.aws_iam_role_policy.spot_router
}

moved {
  from = aws_lambda_function.spot_router
  to   = module.fleet.aws_lambda_function.spot_router
}

moved {
  from = aws_cloudwatch_log_group.spot_router
  to   = module.fleet.aws_cloudwatch_log_group.spot_router
}

moved {
  from = aws_cloudwatch_event_rule.spot_interruption
  to   = module.fleet.aws_cloudwatch_event_rule.spot_interruption
}

moved {
  from = aws_cloudwatch_event_target.spot_router
  to   = module.fleet.aws_cloudwatch_event_target.spot_router
}

moved {
  from = aws_lambda_permission.spot_router
  to   = module.fleet.aws_lambda_permission.spot_router
}
