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
- `billet ami build` from GitHub's template (ADR-022), us-west-2, 2026-10-25, through billet's own code path: x64 on c7i.4xlarge from Canonical's noble AMI, template and layer 40 min, snapshot to available 59 min 30 s, the verifier's boot 4 min 38 s, 1 h 44 min in all, the Android SDK and Firefox accepted, an image of 51 GiB used and 64 free on the 120 GiB default; arm64 on c7g.4xlarge, 38 min, 45 min 30 s and about 4 min 30 s, 1 h 29 min in all, an image of 33 GiB used. A prototype of the same template on a 200 GiB root peaked at 42 GiB on x64 and 35 on arm64 (the same day). GitHub's whole template, run without the guest-only differences, passed its own tests on EC2 on both architectures. Again from the reviewed build path, 2026-10-26: x64 accepting all five components, 42 min 43 s, 59 min 59 s and 6 min 26 s, 1 h 49 min, an image of 54 GiB used and 61 free; arm64, 39 min 16 s, 48 min 11 s and 4 min 53 s, 1 h 32 min, 33 GiB used, Firefox alone owed and recorded.
- `GetConsoleOutput` on an instance that powered itself off (`Client.InstanceInitiatedShutdown`) answered status 0 with empty output, with and without `Latest`, at once and for a minute after the stop (t4g.nano, Canonical's noble arm64 AMI, us-west-2, 2026-10-26). So a build's verdict is read while the builder runs, and nothing it said before stopping can be read after; the AMI bootstrap therefore refuses to build twice on one machine rather than billet looking for a later failure.
- needrestart on Canonical's noble AMI (3.6-7ubuntu4.5, arm64, 2026-10-26) evaluates every `/etc/needrestart/conf.d/*.conf` after its own defaults (`needrestart.conf` lines 229-230), so the AMI build's `$nrconf{override_rc}{qr(^billet-ami-build)} = 0;` adds the key `(?^:^billet-ami-build) => 0` beside GitHub's own and `billet-ami-build.service` matches it as exempt from restarts.
