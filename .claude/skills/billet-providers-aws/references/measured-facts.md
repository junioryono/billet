# Measured facts

Part of the `billet-providers-aws` skill: what was measured, dated where it was recorded.

- SDK cost: 13.2MB and 15 modules for one `RunInstances`, versus 21.8MB for all of billet.
- `CreateSnapshot` takes no `ClientToken` (`UnknownParameter`); the token travels as the `sh.billet.snapshot-token` tag.
- SSM standard parameter: 4096 characters; JIT configs exceed it.
- `StartBuild` idempotency: five minutes; same token same id; changed parameter refused.
- Positional SSM cursor: page two after a delete skips a parameter.
- Default `aws/ssm` key: `WithDecryption=true` returns plaintext with no `kms:*` grant; a customer-managed key refuses `kms:Decrypt` and a mixed page fails whole.
- `DeleteProject` succeeds under a live build; the build runs on.
- EC2 cold start: 47.6, 52.2, 58.7 seconds to the first job step (2026-08-18).
- `RunInstances` evaluates a snapshot a block-device mapping NAMES, and names it account-less in the refusal; the launch was still allowed with the AMI's own untagged backing snapshot, for the one image and request measured (EC2 itself, `--dry-run`, 2026-09-04).
- `arn:<p>:ec2:*::snapshot/*` does not match an account-qualified snapshot ARN and `arn:<p>:ec2:*:*:snapshot/*` matches both spellings (`iam:SimulateCustomPolicy`, not an EC2 observation).
- SQS endpoint suffixes, resolved 2026-09-04: China is `amazonaws.com.cn` for all three forms (standard, legacy `<region>.queue.`, and `vpce`), the commercial names are NXDOMAIN there and vice versa, and **GovCloud takes the commercial suffix** — so the partition is derived from the region's `cn-` prefix (`config.AWSDNSSuffix`, which `awsjson.DNSSuffixFor` calls) rather than from the partition name.
- S3 GET, both HTTP 404 (2026-09-04, us-east-1, unauthenticated): a missing key answers `<Code>NoSuchKey</Code>` with the key echoed; a bucket that does not exist answers `<Code>NoSuchBucket</Code>` with the bucket name.
