# THE SPOT INTERRUPTION ROUTER. EventBridge delivers an EC2 Spot Instance
# Interruption Warning carrying only the instance id, and cannot match on the
# sh.billet.node tag that says which node owns it — so a rule alone would fan every
# spot warning in the account at one queue, and a node re-queues (poison-acks) a
# warning tagged for a different node. This Lambda looks up the tag and forwards the
# warning to exactly that node's queue; an untagged (non-billet) instance is
# dropped. Created only when spot is enabled.

data "archive_file" "spot_router" {
  count = var.enable_spot ? 1 : 0

  type        = "zip"
  source_file = "${path.module}/lambda/spot_router.py"
  # Write the zip under the ROOT's .terraform (present after `init`, and writable
  # even when a git-sourced module's cache is read-only), keyed by account, region
  # and name so two module instances never share one path. The zip is deterministic
  # from the committed source, so even a concurrent write of the same key is
  # byte-identical. Operators who relocate .terraform with TF_DATA_DIR should point
  # it at a writable dir as usual.
  output_path = "${path.root}/.terraform/billet-spot-router-${local.account_id}-${local.region}-${var.name}.zip"
}

# jsonencode rather than aws_iam_policy_document: plan-known, so it stays
# assertable under the mocked test provider, and the Lambda service principal
# must follow the partition suffix like the node trust policy does.
resource "aws_iam_role" "spot_router" {
  count = var.enable_spot ? 1 : 0

  name = "${var.name}-spot-router"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "lambda.${data.aws_partition.this.dns_suffix}" }
    }]
  })
  tags = local.tags
}

# LEAST PRIVILEGE: DescribeInstances cannot be resource-scoped (it takes no
# resource ARN), but the queue actions are scoped to exactly the interruption
# queue this module created, and the logs to this function's own log group.
# jsonencode rather than the provider's policy-document data source so the
# whole document is assertable in the mocked plan tests with the queue and
# log-group ARNs overridden to known values.
resource "aws_iam_role_policy" "spot_router" {
  count = var.enable_spot ? 1 : 0

  name = "${var.name}-spot-router"
  role = aws_iam_role.spot_router[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ReadInstanceNodeTag"
        Effect   = "Allow"
        Action   = "ec2:DescribeInstances"
        Resource = "*"
      },
      {
        Sid      = "ForwardToNodeQueue"
        Effect   = "Allow"
        Action   = ["sqs:GetQueueUrl", "sqs:SendMessage"]
        Resource = aws_sqs_queue.interruptions[0].arn
      },
      {
        Sid      = "Logs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.spot_router[0].arn}:*"
      },
    ]
  })
}

# The log group is declared (not left to Lambda's implicit creation) so its
# retention is bounded and the router's policy can scope logs to exactly it.
resource "aws_cloudwatch_log_group" "spot_router" {
  count = var.enable_spot ? 1 : 0

  name              = "/aws/lambda/${var.name}-spot-router"
  retention_in_days = 14
  tags              = local.tags
}

resource "aws_lambda_function" "spot_router" {
  count = var.enable_spot ? 1 : 0

  function_name    = "${var.name}-spot-router"
  role             = aws_iam_role.spot_router[0].arn
  runtime          = "python3.13"
  handler          = "spot_router.handler"
  filename         = data.archive_file.spot_router[0].output_path
  source_code_hash = data.archive_file.spot_router[0].output_base64sha256
  timeout          = 15
  depends_on       = [aws_cloudwatch_log_group.spot_router]
  tags             = local.tags

  environment {
    variables = {
      # THE QUEUE THIS ROUTER SERVES, read from the resource rather than rebuilt
      # from var.name. Without it the handler cannot tell a tag naming another
      # deployment's queue from its own grant being absent, stale or not yet
      # propagated — AccessDenied is what AWS answers for both — and reading the
      # second as the first drops a real two-minute warning silently.
      BILLET_INTERRUPTION_QUEUE_NAME = aws_sqs_queue.interruptions[0].name
    }
  }
}

# THE ROUTER'S OWN FAILURES ARE VISIBLE OR THEY ARE NOTHING. The handler drops a
# warning only on proof it is not its to place; every other outcome it cannot
# complete is re-raised, which reaches an operator as this metric and nowhere else.
# treat_missing_data is notBreaching because a healthy router emits no Errors
# datapoints at all, and an alarm parked in INSUFFICIENT_DATA is one an operator
# learns to ignore. The period is a minute rather than five: nobody acts inside a
# two-minute warning, but every reclaim during the window is another warning lost,
# so the useful measure is how fast the operator learns the router is broken.
resource "aws_cloudwatch_metric_alarm" "spot_router_errors" {
  count = var.enable_spot ? 1 : 0

  alarm_name        = "${var.name}-spot-router-errors"
  alarm_description = "The spot interruption router could not place a two-minute reclaim warning and re-raised it for Lambda to retry. Its own SQS grant being absent, stale or not yet propagated is the usual cause; the function's log group says which call failed and with what. A warning the router can PROVE belongs to another deployment is dropped and never appears here."

  namespace   = "AWS/Lambda"
  metric_name = "Errors"
  # Read from the function rather than rebuilt, so a renamed function cannot leave
  # the alarm watching a dimension nothing publishes — which reads as healthy.
  dimensions = { FunctionName = aws_lambda_function.spot_router[0].function_name }

  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  datapoints_to_alarm = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  alarm_actions = var.spot_router_alarm_actions
  # The OK edge says the errors STOPPED, which is not the same as a warning having
  # landed: the Errors metric carries no delivery, and with missing data not
  # breaching a router nothing invoked clears the alarm too. It is worth sending
  # because whoever was paged needs to know the alarm cleared; it is not evidence.
  ok_actions = var.spot_router_alarm_actions

  tags = local.tags
}

resource "aws_cloudwatch_event_rule" "spot_interruption" {
  count = var.enable_spot ? 1 : 0

  name        = "${var.name}-spot-interruption"
  description = "EC2 Spot interruption warnings, routed to the owning node's queue."
  event_pattern = jsonencode({
    source        = ["aws.ec2"]
    "detail-type" = ["EC2 Spot Instance Interruption Warning"]
  })
  tags = local.tags
}

resource "aws_cloudwatch_event_target" "spot_router" {
  count = var.enable_spot ? 1 : 0

  rule = aws_cloudwatch_event_rule.spot_interruption[0].name
  arn  = aws_lambda_function.spot_router[0].arn
}

resource "aws_lambda_permission" "spot_router" {
  count = var.enable_spot ? 1 : 0

  statement_id  = "AllowEventBridgeInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.spot_router[0].function_name
  principal     = "events.${data.aws_partition.this.dns_suffix}"
  source_arn    = aws_cloudwatch_event_rule.spot_interruption[0].arn
}
