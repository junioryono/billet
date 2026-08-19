# Transparent Actions cache

Billet can serve `actions/cache` from the same site's block store as a Firecracker runner without requiring workflow changes. The feature is deliberately opt-in per tier because GitHub carries cache and artifact metadata on the same results origin.

## Enable it

Interception currently requires one Linux Firecracker provider, a configured `node.cache` listener, and the site's Ceph store. Validation refuses Docker, EC2, macOS, mixed-provider fallback tiers, and a Firecracker node without its cache endpoint rather than accepting a tier that can route work somewhere the local archive does not exist.

```yaml
tiers:
  - label: billet-4vcpu-cache
    provider: firecracker
    guest_os: linux
    vcpu: 4
    memory: 16GiB
    disk: 80GiB
    image: ubuntu-2404-x64@verified
    intercept: true
```

`intercept` defaults to `false`. Keep it absent on release, deployment, and secret-bearing tiers until the exact image and runner release have passed `.github/workflows/cache-conformance.yml` in the deployment that will use them.

The guest image must speak the contract required by the running Billet binary. `billet images compatible` and the host-upgrade transaction enforce that before a tier can launch. The interception contract includes the guest-side fail-open forwarder, runner hook, CA propagation, and container-visible proxy address; an older image is refused rather than launched without one of those pieces.

## What is local

The node proxy accepts CONNECT only for `results-receiver.actions.githubusercontent.com`, terminates TLS for that origin, and handles exactly three JSON Twirp paths from the official toolkit client: `CreateCacheEntry`, `FinalizeCacheEntryUpload`, and `GetCacheEntryDownloadURL` in `github.actions.results.api.v1.CacheService`. The official client identifies itself with its `@actions/cache-` user-agent prefix; this is a compatibility selector, while the VM session credential, server-supplied job identity, trust classification, and central policy are the security controls. Every other client, path, and method on that origin, including BuildKit `type=gha` and all `ArtifactService` calls, is sent to GitHub with the opaque runtime authorization unchanged. The guest-local forwarder connects directly to every other host, including signed artifact blob origins, so the node listener cannot be used as a general proxy into the host's network.

The guest authenticates its CONNECT request with the unguessable cache-session capability Billet created for that VM. The node does not decode `ACTIONS_RUNTIME_TOKEN`. The assignment identity delivered by GitHub supplies the owner, repository, and workflow ref; a durable cache key is scoped by deployment and site, a digest of that identity, a digest of the cache version, and the workflow's cache key. Restore-prefix matching never crosses any of those boundaries.

Untrusted jobs receive no interception proxy or CA and stay entirely on GitHub's cache service. Billet does not reproduce GitHub's fork and default-branch cache policy from incomplete local evidence, and an untrusted job never publishes a local generation that trusted work could restore.

Archives are limited to 10 GiB. A thin-provisioned upload volume reserves 22 GiB so a boundary staged-block upload can hold the 10 GiB block set and its 10 GiB assembled archive simultaneously with ext4 metadata and journal headroom. Billet deletes the staged files and trims their freed extents before snapshotting, so the published generation retains only the assembled archive rather than both copies. One job may hold at most 32 pending uploads and active downloads before additional requests fall back to GitHub. Uploads accept the Azure Block Blob single-request and staged-block shapes used by the official toolkit, assemble declared block order on a fresh host-mounted ext4 volume, unmount and verify that filesystem, snapshot it, and publish through the site's fenced generation CAS. Downloads clone the exact immutable generation, mount it read-only with journal replay disabled, and support the byte ranges used by the official client. Active clones remain under the existing cache lease and eviction rules.

## Failure behavior

The guest sets `HTTPS_PROXY` to a supervised guest-local forwarder, not to the node. The forwarder binds only the VM's private Docker bridge address so host steps and that job's containers can reach it without exposing the cache-session capability on the microVM workload interface. It connects directly for every ordinary host. For the Actions results host it first tries the authenticated node proxy and connects directly to GitHub if that CONNECT cannot be established. A node listener outage therefore removes local acceleration without removing general HTTPS, and systemd restarts the guest forwarder if it exits.

Before Billet reserves an archive, an unavailable central policy, blocked scope, malformed metadata request, unsupported method, or local storage error is retried through GitHub. After the client has received a Billet-signed upload URL, its blob operations and matching finalization remain bound to that local reservation: a policy change or storage failure returns a local error because GitHub has no corresponding reservation to accept a replay. Billet persists a finalization receipt before advancing the generation pointer and marks it complete before returning success, so a lost response, transient publication failure, or node restart can retry locally without changing that boundary. The official cache action reports its normal non-fatal cache warning. Artifact routing is independent and remains passthrough.

The node-generated CA is combined with the distribution trust bundle. The runner process receives `NODE_EXTRA_CA_CERTS` and `SSL_CERT_FILE`; a supported job-started hook copies the bundle into `RUNNER_TEMP` and writes the proxy and CA variables through `GITHUB_ENV`. The official runner translates that mounted path for both job containers and Docker actions. Service containers do not execute Actions cache clients and receive only the runner's ordinary proxy environment.

## Kill switch

The control plane owns a deny list in its state database, and the node asks it before every locally handled cache operation. Before any local reservation exists, policy lookup failure or denial is passthrough, never local permission. A blob upload or finalization already bound to a Billet reservation fails locally instead of being misrouted to GitHub, which never received that reservation. Organisation and repository names are case-insensitive.

```bash
billet cache disable --org acme
billet cache disable --repository acme/payments
billet cache enable --repository acme/payments
billet cache enable --org acme
```

An organisation block covers every repository below it. Enabling one repository removes only that repository's explicit block; it does not override a remaining organisation block. Enabling an organisation removes only the organisation block and deliberately leaves narrower repository blocks intact. These commands use `state.OpenAdmin`, so they are safe to run while the server owns the control-plane process lock.

## Conformance gate

Run the reusable or manually dispatched `Cache and artifact conformance` workflow with the cache-enabled label. Supply the exact Billet tag or commit as `billet_ref`; cross-repository callers may also override `billet_repository`. The workflow checks out only the fixture action from that explicit revision rather than reading scripts or a Go version from the caller repository. Supply `expected_runner_version` and `expected_guest_contract` when qualifying a candidate image so a passing run cannot accidentally describe another generation.

The moving-major lane exercises `actions/cache@v5`, `upload-artifact@v7`, and `download-artifact@v8` on the host and in a job container. A second lane pins the reviewed action commits exactly. Separate moving and pinned lanes exercise the embedded cache clients in `actions/setup-node`, `actions/setup-java`, `actions/setup-python`, `actions/setup-go`, and `actions/setup-dotnet`, then prove their real package-cache directories survive a second VM. Poisoning probes isolate `ACTIONS_*`, Node, CA/TLS, proxy, loader, tar, and path variables to cache steps and then require a clean artifact upload/download to remain byte-identical.

A guest image or runner release is not promoted onto an interception-enabled tier until this workflow passes against that candidate. The workflow is intentionally reusable because the runner label and candidate-promotion mechanism belong to the deployment; Billet supplies the protocol gate without embedding any consuming repository or infrastructure name.
