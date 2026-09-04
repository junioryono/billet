# Configuration

`billet.yaml` has nine top-level blocks. `billet server` reads `server`, `github`, `tiers`, `nodes`, `sites`, `backup`, `images` and `release`; `billet node` reads `node` and `tiers` (and `server`, on a loopback node, to learn which deployment it joins). One machine running both roles reads one file. The annotated [`billet.example.yaml`](https://github.com/junioryono/billet/blob/main/billet.example.yaml) describes the measured Firecracker deployment; `billet init` writes a runnable file for any backend, and both are parsed by the test suite so they cannot drift from the schema.

Byte sizes are written as `32GiB`, `512MiB` and parsed exactly; durations as Go strings (`30m`, `6h`).

## `server`

| Key | Required | Meaning |
|---|---|---|
| `listen` | yes | the address nodes dial. A loopback address serves plain HTTP with no certificates; anything else requires mTLS |
| `bootstrap_listen` | no | a second address serving only `/v1/ca` and `/v1/enroll`, for a machine that has no certificate yet. Absent means no network enrollment (`billet ca issue` is the way in). Refused against a loopback `listen` |
| `node_tls_hosts` | when `listen` is a wildcard | every name and address a node will dial, including the one it dials `bootstrap_listen` by; both listeners present one certificate |
| `state_dir` | one of `state_dir` or `identity_dir`+`state` | shorthand for `identity_dir: <dir>` plus `state: {backend: sqlite}`; must be local storage |
| `identity_dir` | with `state` | the deployment identity, the node-wire CA and its rotation state, the process lock and the maintenance fence |
| `state.backend` | with `identity_dir` | `sqlite` or `postgres` |
| `state.postgres.dsn_env` | for `postgres` | the name of the environment variable holding the DSN, never the DSN |
| `controllers` | no | `single` (default) or `active-passive`; refused on SQLite |
| `identity.backend` | no | `file` (default) or `aws-ssm` |
| `identity.aws_ssm` | for `aws-ssm` | `region`, `prefix` (isolates one deployment in an account), `kms_key_id` |
| `max_vcpu`, `max_memory` | yes, positive | the deployment ceiling across every tier |
| `placement` | no | `pack` (default) or `spread` |
| `drain_timeout` | no | when billet starts reporting a drain as long; never a deadline |

## `github`

| Key | Required | Meaning |
|---|---|---|
| `org` | yes | the organization; matched exactly, so padding is refused |
| `app_id` | yes | written by `billet github-app create` |
| `client_id` | no | when set, the scale-set client signs with it; `billet check` reports which issuer it tested |
| `installation_id` | yes | creating an App does not install it |
| `private_key_path` | yes | the key GitHub issued once |

## `node`

| Key | Required | Meaning |
|---|---|---|
| `name` | on loopback | otherwise the certificate's name; defaults to the hostname |
| `server_addr` | yes | the control plane's node wire |
| `bootstrap_addr` | no | used once at `--enroll`; falls back to `server_addr` |
| `provider` | yes | `docker`, `firecracker`, `tart`, `ec2`, `codebuild` |
| `site` | no | one of the control plane's `sites`; required with `ebs_s3`; refused on `codebuild` |
| `max_vcpu`, `max_memory` | for `ec2` and `codebuild` | what this host contributes; detected on a host backend when unset |
| `tls.cert`, `tls.key`, `tls.ca` | for a non-loopback `server_addr` | from `billet ca issue` or enrollment; refused against a loopback server |
| `lock_dir`, `allow_unlocked_deployment` | no | the host-wide deployment lock; a shared directory must be setgid; failing to place the lock is an error unless opted out |
| `state_dir` | yes | generation pointers, image cache, the node's identity |
| `max_custody` | no | a bound on compute billet is holding without a completion; empty means none, and a job killed by it is archived failed |
| `drain_timeout` | no | when the node starts reporting a drain as long |
| `cache.listen` | for interception and EC2 caches | one literal non-loopback address; `tls_cert`/`tls_key` required for EC2 and refused on the Firecracker bridge |
| `registry_mirrors` | no | `docker.io`, `ghcr.io`, `quay.io` origins |

### `node.firecracker` (required for firecracker, refused otherwise)

`binary_path`, `jailer_path`, `kernel_image` (required; the fallback for a generation that recorded no kernel), `kernel_dir`, `chroot_base`, `jail_uid_min`, `jail_uid_count`, `bridge` (required), `untrusted_bridge` (absent means untrusted work is refused), `image_verify_port`.

### `node.ceph` (required for firecracker, refused otherwise)

`conf_path`, `user` (defaults to `billet`; `admin` is refused), `keyring_path`, `image_pool` (required), `cache_pool` (required).

### `node.tart` (optional)

`untrusted_isolation` (`softnet`; absent means untrusted work is refused, and naming it makes `billet check` fatal until softnet is setuid root), `untrusted_dns`.

### `node.ec2` (required for ec2)

`region` (required), `endpoint` (https, loopback excepted), `subnet_id` (required), `security_group_ids`, `untrusted_security_group_ids` (absent means untrusted work is refused), `assign_public_ip`, `instance_profile` (optional, and empty is the right answer unless a job needs one), `instance_types` (ordered; each `type`, `vcpu`, `memory`, `price_usd_per_hour`), `spot`, `interruption_queue_url` (required with `spot`; its basename must equal the node's name).

### `node.codebuild` (required for codebuild, refused otherwise)

`region` (required), `endpoint`, `project` (required; billet's alone), `fleet_arn` (reserved capacity; the only route to macOS; untrusted work is refused with it set), `environment_type` (`LINUX_CONTAINER`, `ARM_CONTAINER`, `LINUX_GPU_CONTAINER`, `LINUX_EC2`, `ARM_EC2`, `MAC_ARM`), `compute_types` (ordered; each declares what it holds and costs), `accept_external_build_ceiling` (no default; absent is a refusal, because the 36-hour and 8-hour ceilings are CodeBuild's), `build_timeout_minutes`, `queued_timeout_minutes`, `jit_parameter_path` (required; an IAM boundary), `jit_kms_key_id`, `log_group`, `privileged_mode`, `untrusted_vpc_id`, `untrusted_subnets`, `untrusted_security_group_ids` (all three or fork work is refused; refused beside `fleet_arn`).

### `node.ebs_s3` (for an EC2 site's cache)

`region`, `availability_zone` (must match the subnet's), `bucket`, `prefix`, `kms_key_id`. It exists only to back `node.cache`: without that listener nothing reads it, so `billet check` refuses the pairing rather than letting every job run cold on the instance's root volume.

## `tiers`

| Key | Meaning |
|---|---|
| `label` | the `runs-on` value and the scale set's name |
| `trust` | `untrusted` (default) or `trusted`; trusted requires `runner_group` and `workflows` |
| `runner_group` | a non-default group GitHub restricts; empty means GitHub's default group; `&`, `#`, `;`, `%` and `+` are refused because the client does not escape them |
| `workflows` | the exact allowlist the group must carry, with refs |
| `provider` or `providers` | one backend, or an ordered preference list; never both |
| `launch.<provider>` | per-backend `image` and `command` for a multi-provider tier |
| `guest_os` | `linux` (default), `macos`, `windows` |
| `node`, `site` | pins; a single-backend macOS tier pins a node |
| `vcpu`, `memory` | the request; or `sizes` with `memory_per_vcpu` to expand a template into several tiers |
| `disk` | usable root capacity on Firecracker and EC2; ignored by Docker |
| `shm`, `buildkit_cache_mount_limit` | the shared-memory size and the per-mount BuildKit ceiling |
| `image` | a Firecracker image `name@generation` or `name@verified` (a bare name is refused), an AMI id, a container image, or a tart OCI reference |
| `command` | the guest command; `command-missing` is a conclusive launch failure |
| `intercept`, `cache_scope` | transparent Actions caching, Linux Firecracker only, with a static scope |
| `max_concurrent`, `reserved` | a ceiling and a floor; a macOS tier's ceiling defaults to what its hosts permit between them |
| `warm_pool` | refused when non-zero; no backend implements it |

## `nodes`

Per-host policy, not a roster: `name`, `provider` (decides only whether an unpinned tier could land here), `guest_os` allowlist, `macos_vm_limit` (defaults to Apple's two on a host that runs work; a cloud fleet gets no default cap). `macos_vm_limit > 0` beside an allowlist excluding macOS is refused.

## `sites`

`name` (matched exactly) and `store` (`ceph` or `ebs-s3`). A node's provider must be one the site's store serves; a tier that names a site may not list `codebuild`.

## `backup`

`s3.bucket`, `s3.region` (required even with an endpoint), `s3.prefix` (default `billet-backups`), `s3.endpoint` (path-style, for RGW, MinIO or R2), `s3.kms_key_id` (empty means SSE-S3). billet never deletes from the bucket.

## `images`

`source` (empty means billet's own published images), `signing_identity` and `signing_issuer` (both required for a non-default source).

## `release`

`channel` (`stable` or `candidate`) or `version` (an exact tag or a 40-hex commit; `latest` and `main` are refused; setting both is an error), `automatic` (on unless written `false`; the one switch for the control plane starting rollouts, the hosts' scheduled updaters acting on them, and the daily guest-image refresh), `maintenance_window.start`/`end` (HH:MM UTC; bounds when an automatic rollout may begin and never stops one), `signing_identity` and `signing_issuer` (both or neither; honoured by `rollout start`, `host-upgrade` and the automatic starter alike).

## Rules that are not visible in the schema

- Tiers and `nodes` are read at startup and snapshotted; changing them restarts the control plane. Nodes register dynamically.
- A path, pool, region or endpoint is trimmed; an identity (`sites[].name`, `tiers[].site`, `node.site`, `github.org`, `tiers[].runner_group`) is refused with padding, because a trimmed identity names a different deployment on another machine.
- `node.ceph` is refused on every backend but firecracker; `node.cache` is refused on docker; `node.ceph` and `node.ebs_s3` are refused on codebuild.
- The mirror of those — a store with no `node.cache` to serve it — is a `billet check` verdict rather than a load refusal, because `billet decommission` and `billet init iam` both read a store block on a config that has lost its listener. `node.ebs_s3` without a listener FAILS the check; `node.ceph` without one is reported, since `image_pool` still boots every guest and `cache_pool` is a required field.
- `billet init` writes a whole file and refuses to replace one it cannot prove is its own output (it writes `<path>.new` instead; `--force` overrides); `billet github-app create --config` edits five scalars under `github:` in place and preserves everything else.
- `--config` is never defaulted to the working directory; `billet check -h` prints the per-user default, and the packaged services use `/etc/billet/billet.yaml` (Linux) or `/usr/local/etc/billet/billet.yaml` (macOS).
