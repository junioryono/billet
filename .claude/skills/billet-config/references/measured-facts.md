# Measured facts

Part of the `billet-config` skill: what was measured, dated where it was recorded.

- Virtualization.framework refuses a third concurrent macOS guest ("The number of VMs exceeds the system limit") and a guest under 4GiB (`LessThanMinimalResourcesError`).
- Runner-group round trip: `&#;%+` are damaged; `github.org`: `#%/?`; each segment of `github.repository` the same (`checkOwnerSegment`, swept over ASCII against the vendored URL parse and the REST path, 2026-09-04).
- `billet init --repository` refuses `--runner-group`/`--workflow` (a repository has no pool) and refuses the docker backend (it admits only trusted work), each by name, before the generic docker-needs-a-policy answer can send an operator to create a runner group GitHub has nowhere to put.
- `image_pool` padding reached `rbd` unchanged before `normalize` existed; site-name padding was found three times in `internal/config`.
- A codebuild-only sited tier advertised 0 with `billet check` healthy (first live acceptance re-run).
- SQS hosts, resolved 2026-09-04: `sqs.cn-north-1.amazonaws.com` and `cn-north-1.queue.amazonaws.com` are NXDOMAIN, `sqs.cn-north-1.amazonaws.com.cn` answers and `cn-north-1.queue.amazonaws.com.cn` is a CNAME onto it; `sqs.us-west-2.amazonaws.com.cn` is NXDOMAIN. **GovCloud is a separate partition on the COMMERCIAL suffix** (`sqs.us-gov-west-1.amazonaws.com` answers, the `.cn` form does not), so the rule is keyed on the region's `cn-` prefix and not on "is this the commercial partition". The `vpce` zone is delegated per partition too (`vpce.amazonaws.com` → `awsdns-22.co.uk`, `vpce.amazonaws.com.cn` → `awsdns-cn-60.com`).
- The legacy `<region>.queue.<suffix>` form does not exist for `us-east-1` (whose legacy name is the bare `queue.amazonaws.com`) or `us-gov-east-1`. Both stay accepted: the validator is a shape, not a list, for the same reason `awsRegionRe` is.
- Go's own behaviour behind that validator, measured 2026-09-04: `strings.ToLower` folds U+0130 to an ASCII `i` and U+212A to a `k`, so 63 of either are a label only legal AFTER folding; `url.Parse` refuses a port that is not decimal digits (`+443`, `443 `, `0x1bb`) but keeps `0443`, which `net.LookupPort` resolves to 443. Hence: check the host before folding, and compare the port as a number.
