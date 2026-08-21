# Transparent Actions cache

Billet can serve `actions/cache` from the same site's block store as a Firecracker runner without requiring workflow changes. The feature is deliberately opt-in per tier because GitHub carries cache and artifact metadata on the same results origin.

## Enable it

Interception currently requires one Linux Firecracker provider, a configured `node.cache` listener, and the site's Ceph store. Validation refuses Docker, EC2, macOS, mixed-provider fallback tiers, and a Firecracker node without its cache endpoint rather than accepting a tier that can route work somewhere the local archive does not exist.

```yaml
tiers:
  - label: billet-4vcpu-cache
    provider: firecracker
    trust: trusted
    runner_group: billet-cache-trusted
    workflows:
      - acme/api/.github/workflows/ci.yml@refs/heads/main
    guest_os: linux
    vcpu: 4
    memory: 16GiB
    disk: 80GiB
    image: ubuntu-2404-x64@verified
    intercept: true
    cache_scope:
      owner: acme
      repository: api
      workflow_ref: acme/api/.github/workflows/ci.yml@refs/heads/main
```

`intercept` defaults to `false`. A trusted pool must use a non-default GitHub runner group restricted to exactly the listed `workflows`; Billet validates that policy at server startup and again before every registration is minted. `cache_scope` is static because GitHub chooses a job only after the JIT runner has joined the pool, so launch-time cache authority cannot safely come from the assignment that happened to cause scale-up. Keep interception absent on release, deployment, and secret-bearing tiers until the exact image and runner release have passed `.github/workflows/cache-conformance.yml` in the deployment that will use them.

The guest image must speak the contract required by the running Billet binary. `billet images compatible` and the host-upgrade transaction enforce that before a tier can launch. The interception contract includes the guest-side DNS-remap passthrough, runner hook, CA propagation, and container resolver; an older image is refused rather than launched without one of those pieces.

## What is local

The node proxy accepts CONNECT only for `results-receiver.actions.githubusercontent.com`, terminates TLS for that origin, and handles exactly three JSON Twirp paths from the official toolkit client: `CreateCacheEntry`, `FinalizeCacheEntryUpload`, and `GetCacheEntryDownloadURL` in `github.actions.results.api.v1.CacheService`. The official client identifies itself with its `@actions/cache-` user-agent prefix; this is a compatibility selector, while the VM session credential, statically configured pool scope, trusted runner-group workflow boundary, and central policy are the security controls. Every other client, path, and method on that origin, including BuildKit `type=gha` and all `ArtifactService` calls, is sent to GitHub with the opaque runtime authorization unchanged. Only the results origin is DNS-remapped to the guest passthrough; every other host, including signed artifact blob origins, resolves normally and connects directly, so the node listener is reachable for that one origin alone and cannot be used as a general proxy into the host's network.

The guest authenticates its CONNECT request with the unguessable cache-session capability Billet created for that VM. The node does not decode `ACTIONS_RUNTIME_TOKEN`. The tier's `cache_scope` supplies the owner, repository, and workflow ref before a pooled runner launches; a durable cache key is scoped by deployment and site, a digest of that identity, a digest of the cache version, and the workflow's cache key. Validation requires the scope's workflow ref to be one of the trusted runner group's declared workflows. Restore-prefix matching never crosses any of those boundaries.

Untrusted jobs receive no interception and no CA and stay entirely on GitHub's cache service. Billet does not reproduce GitHub's fork and default-branch cache policy from incomplete local evidence, and an untrusted job never publishes a local generation that trusted work could restore.

Archives are limited to 10 GiB. A thin-provisioned upload volume reserves 22 GiB so a boundary staged-block upload can hold the 10 GiB block set and its 10 GiB assembled archive simultaneously with ext4 metadata and journal headroom. Billet deletes the staged files and trims their freed extents before snapshotting, so the published generation retains only the assembled archive rather than both copies. One job may hold at most 32 pending uploads and active downloads before additional requests fall back to GitHub. Uploads accept the Azure Block Blob single-request and staged-block shapes used by the official toolkit, assemble declared block order on a fresh host-mounted ext4 volume, unmount and verify that filesystem, snapshot it, and publish through the site's fenced generation CAS. Downloads clone the exact immutable generation, mount it read-only with journal replay disabled, and support the byte ranges used by the official client. Active clones remain under the existing cache lease and eviction rules.

## Failure behavior

Interception reaches the runner and its containers by a DNS remap of the single Actions results origin, not a proxy variable. Only that origin resolves to a supervised guest-local transparent passthrough — the runner through `/etc/hosts`, containers through a guest resolver the Docker daemon is pointed at before it starts — while every other host resolves normally and connects directly. This is deliberate: a blanket proxy funnels all of the runner's traffic through one guest relay, and bulk transfers such as action tarballs, toolchains, and artifact blobs stall through it while small cache calls survive. The passthrough binds the VM's private Docker bridge address, so host steps and that job's containers reach it without exposing the cache-session capability on the microVM workload interface, and it tunnels the raw TLS stream to the node's authenticated proxy, which terminates TLS and decides per request whether to serve the cache locally or splice the rest to GitHub. A node listener outage removes local acceleration for the results origin without failing the traffic that shares it. When the node cannot take a tunnel, the passthrough relays the client's TLS straight to the real results origin — resolved before the guest remapped that name — so the cache call degrades to an ordinary miss while artifact, log-archive and step traffic on the same origin keeps working; the client trusts GitHub's real certificate through the same distribution roots it already carries. systemd owns the passthrough's listening socket through a transient socket unit, which is what keeps the results origin reachable across a passthrough crash: PID 1 bound `:443` (so the runner-account process holds no privileged-bind capability), and a crash of the process leaves that socket bound, so new connections queue in its backlog and the restarted process accepts them rather than being refused during the restart — though a stream the crashed process had already accepted is lost to the client's retry, which transparent process replacement cannot preserve. systemd restarts the passthrough and the guest resolver if they exit, and the daemon's DNS list keeps the real upstream servers after the guest resolver so a container still resolves names — it merely loses the cache remap — if the resolver is briefly down. The guest resolver's list is built only when a real upstream is present, so it is never a resolver-of-one that could take container DNS down with it.

Before Billet reserves an archive, an unavailable central policy, blocked scope, malformed metadata request, unsupported method, or local storage error is retried through GitHub. After the client has received a Billet-signed upload URL, its blob operations and matching finalization remain bound to that local reservation: a policy change or storage failure returns a local error because GitHub has no corresponding reservation to accept a replay. Billet persists a finalization receipt before advancing the generation pointer and marks it complete before returning success, so a lost response, transient publication failure, or node restart can retry locally without changing that boundary. The official cache action reports its normal non-fatal cache warning. Artifact routing is independent and remains passthrough.

The node-generated CA is combined with the distribution trust bundle. The runner process receives `NODE_EXTRA_CA_CERTS` and `SSL_CERT_FILE`; a supported job-started hook copies the bundle into `RUNNER_TEMP` and writes those two CA variables through `GITHUB_ENV`. The official runner translates that mounted path for both job containers and Docker actions, and the container resolver remaps the results origin for them so the trusted node certificate is what they meet there. Service containers do not execute Actions cache clients and are unaffected.

Because the DNS remap captures every process in the guest, not only the ones a proxy variable would have reached, the node CA is also installed into the guest's own system trust store before Docker starts, so daemon-side clients that resolve the results origin through the guest validate the node's leaf against the system store rather than failing the handshake. `type=gha` cache export is the one path this cannot fully cover: it runs on a BuildKit builder created with the `docker-container` driver, which runs in its own image with its own trust store that billet does not populate (the embedded `docker` driver cannot export `type=gha` at all), so its calls to the remapped origin cannot validate the node leaf and the export fails unless the node CA is added to the builder image. The conformance suite exercises this exact path so the behavior — fatal, or a non-fatal warning that leaves the build succeeding — is measured rather than assumed.

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
