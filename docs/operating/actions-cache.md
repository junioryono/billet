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

`intercept` defaults to `false`. A trusted pool must use a non-default GitHub runner group restricted to exactly the listed `workflows`; Billet validates that policy at server startup and again before every registration is minted. `cache_scope` is static because GitHub chooses a job only after the JIT runner has joined the pool, so launch-time cache authority cannot safely come from the assignment that happened to cause scale-up. Keep interception absent on release, deployment, and secret-bearing tiers until the exact image and runner release have passed the consumer-owned workflow installed by `billet cache conformance install` in the deployment that will use them.

The guest image must speak the contract required by the running Billet binary. `billet images compatible` and the host-upgrade transaction enforce that before a tier can launch. The interception contract includes the guest-side DNS-remap passthrough, runner hook, CA propagation, container resolver, and the loopback cache adapter; an older image is refused rather than launched without one of those pieces.

## What is local

The node proxy accepts CONNECT only for `results-receiver.actions.githubusercontent.com`, terminates TLS for that origin, and handles exactly three JSON Twirp paths from the official toolkit client: `CreateCacheEntry`, `FinalizeCacheEntryUpload`, and `GetCacheEntryDownloadURL` in `github.actions.results.api.v1.CacheService`. The official client identifies itself with its `@actions/cache-` user-agent prefix; this is a compatibility selector, while the VM session credential, statically configured pool scope, trusted runner-group workflow boundary, and central policy are the security controls. A request that names itself as Billet's own loopback adapter, and carries a usable loopback origin for its signed blob URLs, is admitted the same way — that is the opt-in path described under [BuildKit `type=gha`](#buildkit-typegha-on-a-container-driver-builder) below, and it is what lets a client Billet cannot identify by user agent be served deliberately rather than by widening the selector. Every other client, path, and method on that origin, including a `type=gha` builder that has not opted in and all `ArtifactService` calls, is sent to GitHub with the opaque runtime authorization unchanged. Only the results origin is DNS-remapped to the guest passthrough; every other host, including signed artifact blob origins, resolves normally and connects directly, so the node listener is reachable for that one origin alone and cannot be used as a general proxy into the host's network.

The guest authenticates its CONNECT request with the unguessable cache-session capability Billet created for that VM. The node does not decode `ACTIONS_RUNTIME_TOKEN`. The tier's `cache_scope` supplies the owner, repository, and workflow ref before a pooled runner launches; a durable cache key is scoped by deployment and site, a digest of that identity, a digest of the cache version, and the workflow's cache key. Validation requires the scope's workflow ref to be one of the trusted runner group's declared workflows. Restore-prefix matching never crosses any of those boundaries.

Untrusted jobs receive no interception and no CA and stay entirely on GitHub's cache service. Billet does not reproduce GitHub's fork and default-branch cache policy from incomplete local evidence, and an untrusted job never publishes a local generation that trusted work could restore.

Archives are limited to 10 GiB. A thin-provisioned upload volume reserves 22 GiB so a boundary staged-block upload can hold the 10 GiB block set and its 10 GiB assembled archive simultaneously with ext4 metadata and journal headroom. Billet deletes the staged files and trims their freed extents before snapshotting, so the published generation retains only the assembled archive rather than both copies. One job may hold at most 32 pending uploads and active downloads before additional requests fall back to GitHub. Uploads accept the Azure Block Blob single-request and staged-block shapes used by the official toolkit, assemble declared block order on a fresh host-mounted ext4 volume, unmount and verify that filesystem, snapshot it, and publish through the site's fenced generation CAS. Downloads clone the exact immutable generation, mount it read-only with journal replay disabled, and support the byte ranges used by the official client. Active clones remain under the existing cache lease and eviction rules.

## Failure behavior

Interception reaches the runner and its containers by a DNS remap of the single Actions results origin, not a proxy variable. Only that origin resolves to a supervised guest-local transparent passthrough — the runner through `/etc/hosts`, containers through a guest resolver the Docker daemon is pointed at before it starts — while every other host resolves normally and connects directly. This is deliberate: a blanket proxy funnels all of the runner's traffic through one guest relay, and bulk transfers such as action tarballs, toolchains, and artifact blobs stall through it while small cache calls survive. The passthrough binds the VM's private Docker bridge address, so host steps and that job's containers reach it without exposing the cache-session capability on the microVM workload interface, and it tunnels the raw TLS stream to the node's authenticated proxy, which terminates TLS and decides per request whether to serve the cache locally or splice the rest to GitHub. A node listener outage removes local acceleration for the results origin without failing the traffic that shares it. When the node cannot take a tunnel, the passthrough relays the client's TLS straight to the real results origin — resolved before the guest remapped that name — so the cache call degrades to an ordinary miss while artifact, log-archive and step traffic on the same origin keeps working; the client trusts GitHub's real certificate through the same distribution roots it already carries. systemd owns the passthrough's listening socket through a transient socket unit, which is what keeps the results origin reachable across a passthrough crash: PID 1 bound `:443` (so the runner-account process holds no privileged-bind capability), and a crash of the process leaves that socket bound, so new connections queue in its backlog and the restarted process accepts them rather than being refused during the restart — though a stream the crashed process had already accepted is lost to the client's retry, which transparent process replacement cannot preserve. systemd restarts the passthrough and the guest resolver if they exit, and the daemon's DNS list keeps the real upstream servers after the guest resolver so a container still resolves names — it merely loses the cache remap — if the resolver is briefly down. The guest resolver's list is built only when a real upstream is present, so it is never a resolver-of-one that could take container DNS down with it.

Before Billet reserves an archive, an unavailable central policy, blocked scope, malformed metadata request, unsupported method, or local storage error is retried through GitHub. After the client has received a Billet-signed upload URL, its blob operations and matching finalization remain bound to that local reservation: a policy change or storage failure returns a local error because GitHub has no corresponding reservation to accept a replay. Billet persists a finalization receipt before advancing the generation pointer and marks it complete before returning success, so a lost response, transient publication failure, or node restart can retry locally without changing that boundary. The official cache action reports its normal non-fatal cache warning. Artifact routing is independent and remains passthrough.

The node-generated CA is combined with the distribution trust bundle. The runner process receives `NODE_EXTRA_CA_CERTS` and `SSL_CERT_FILE`; a supported job-started hook copies the bundle into `RUNNER_TEMP` and writes those two CA variables through `GITHUB_ENV`. The official runner translates that mounted path for both job containers and Docker actions, and the container resolver remaps the results origin for them so the trusted node certificate is what they meet there. Service containers do not execute Actions cache clients and are unaffected.

Because the DNS remap captures every process in the guest, not only the ones a proxy variable would have reached, the node CA is also installed into the guest's own system trust store before Docker starts, so daemon-side clients that resolve the results origin through the guest validate the node's leaf against the system store rather than failing the handshake. `type=gha` cache export on a `docker-container` builder is the one path this cannot cover, because that BuildKit runs in its own image with its own trust store that Billet does not populate; measured live, its calls to the remapped origin fail with `x509: certificate signed by unknown authority` and the export is fatal to the build. That case is served instead by an explicit opt-in, described next; the conformance suite keeps measuring the un-opted-in builder so the limitation an unchanged workflow meets stays a fact rather than a memory.

## BuildKit `type=gha` on a container-driver builder

A builder created with buildx's `docker-container` driver resolves the results origin through the guest like everything else and is presented a node leaf it cannot verify, so its `type=gha` export would fail rather than pass through. Billet cannot edit an arbitrary `moby/buildkit` image, and BuildKit has no per-endpoint CA setting for the cache backend — its certificate configuration is registry-scoped.

The way out is not a certificate. Billet runs a second guest listener that speaks plaintext HTTP and does the TLS itself: it rewrites the request head, opens the same authenticated tunnel to the node, and copies bytes. It is bound on the guest's docker gateway address, the one address both the guest and every container on its bridge can dial, so a container-driver builder reaches it with no `network=host`. Nothing outside the microVM routes to that address, the node mints blob URLs naming it only for the adapter, and the job's containers are the job's own.

**No workflow change is needed.** The buildx client points BuildKit's cache at whatever `ACTIONS_RESULTS_URL` says in its own environment, and the runner sets that variable for every step from the job message, where nothing billet controls can change it. So the guest image installs a `docker` shim at `/usr/local/bin/docker`, ahead of the real client on the job's PATH: for a build invocation (`docker build`, `docker buildx build`, `docker buildx bake`, `docker compose build`, `docker compose up --build`) it points that one variable at the adapter and selects the v2 service, then execs the real client. Everything that is not a build is exec'd untouched, because the results origin also carries artifacts and live logs, which the adapter deliberately does not serve, and because `docker run -e ACTIONS_RESULTS_URL` would forward the rewritten value into a container. A workflow that changed only `runs-on` therefore gets billet's cache from a plain `docker/setup-buildx-action` plus `docker/build-push-action` with `cache-to: type=gha`.

A workflow may still name the adapter itself, which is what earlier images required and remains supported:

```yaml
- uses: docker/setup-buildx-action@v3
  with:
    driver: docker-container
- uses: docker/build-push-action@v6
  with:
    context: .
    cache-to: type=gha,mode=max,version=2,url_v2=${{ env.BILLET_ACTIONS_CACHE_URL }},scope=app
    cache-from: type=gha,version=2,url_v2=${{ env.BILLET_ACTIONS_CACHE_URL }},scope=app
```

`url` is ignored by the v2 cache backend; `url_v2` is the parameter that is read, and supplying it is itself what selects v2. `BILLET_ACTIONS_CACHE_URL` is published into `GITHUB_ENV` by the job-started hook, and only while the adapter is actually serving — a job that finds it empty is running on a tier or an image without interception, and the shim then does nothing, so an ordinary `type=gha` line goes to GitHub exactly as it would anywhere else. `network=host` is no longer required, and a workflow that set it for the loopback listener keeps working.

The adapter fails open, by two different routes. If the node cannot take the tunnel at all — it is restarting, unreachable, or refuses the session — the adapter dials the real results origin itself, over TLS it verifies, against the addresses resolved before the guest remapped that name. If the node takes the tunnel and then declines to serve locally, because the kill switch blocks the scope or storage is unavailable, its ordinary splice carries the request upstream and GitHub answers with a signed URL on its own storage, which the builder reaches directly. Either way the build degrades to GitHub's cache instead of failing. The listener itself is started only when an interception session was issued for that VM, never on an untrusted tier, and never on an address outside the guest: the node refuses to mint a signed blob URL naming anything but a loopback or private address, and the adapter refuses to serve anywhere else.

Two limits are worth knowing. A build launched from inside a `container:` job uses that container's own `docker` client, which the shim does not front, so it needs the explicit `url_v2` form; host steps need nothing. And BuildKit writes one cache entry per layer blob while a job may hold at most 32 local archives and clones at once, so `mode=max` on a large image can reach that ceiling; entries past it are answered by GitHub individually, which leaves the build correct and partly accelerated rather than failed.

## Kill switch

The control plane owns a deny list in its state database, and the node asks it before every locally handled cache operation. Before any local reservation exists, policy lookup failure or denial is passthrough, never local permission. A blob upload or finalization already bound to a Billet reservation fails locally instead of being misrouted to GitHub, which never received that reservation. Organisation and repository names are case-insensitive.

```bash
billet cache disable --org acme
billet cache disable --repository acme/payments
billet cache enable --repository acme/payments
billet cache enable --org acme
```

An organisation block covers every repository below it. Enabling one repository removes only that repository's explicit block; it does not override a remaining organisation block. Enabling an organisation removes only the organisation block and deliberately leaves narrower repository blocks intact. These commands use `state.OpenAdmin`, so they are safe to run while the server owns the control-plane process lock.

The installed conformance workflow defaults to `mode: intercept`, which requires the Billet local-cache response header before any other job runs. To accept a kill-switch change, keep the repository or organisation block active for a fresh manual dispatch with `mode: passthrough`. That mode requires the same Billet runner and candidate image, requires an HTTP response from GitHub while accepting that GitHub may reject the deliberately synthetic lookup, proves the local response header is absent, and then runs the unchanged cache, artifact, embedded-client, container, BuildKit, fault, and recovery lanes against GitHub. Re-enable the scope after the run; a passing interception run does not substitute for the disabled-mode acceptance.

## Conformance gate

GitHub applies a restricted organization runner group only to jobs directly defined in a selected workflow. A caller whose only job invokes `junioryono/billet/.github/workflows/cache-conformance.yml` does not qualify: those jobs are defined in Billet's repository, and an organization runner group cannot authorize a workflow owned by another organization. Do not work around that boundary by disabling workflow restrictions.

Run the released Billet binary from the root of the private repository that will own the gate:

```bash
billet cache conformance install \
  --repository acme/api \
  --runner-label billet-4vcpu-cache
```

The command resolves the binary's release tag through GitHub's Git object API, peels annotated tags to a full commit SHA, downloads the canonical workflow from that SHA, and bakes only the SHA, the pinned Actions runner version, the guest contract, and the one runner label into a consumer-owned `.github/workflows/billet-cache-conformance.yml`. It then verifies that every job still defines its own steps. A source build has no release identity, so pass `--billet-ref vMAJOR.MINOR.PATCH` or a full 40-character commit SHA explicitly. A version-shaped tag is only a locator and is never left in an executable fixture checkout. Forks may set `--billet-repository`; candidate qualification may override `--expected-runner-version` and `--expected-guest-contract` explicitly.

Commit the generated workflow on the selected branch before changing the runner group. The command prints the exact `owner/repository/.github/workflows/billet-cache-conformance.yml@refs/heads/main` identity to use for the runner group's selected workflow, the tier's `workflows` entry, and `cache_scope.workflow_ref`. Use `--workflow-ref` when the selected branch or tag is not `refs/heads/main`. Re-run the newer released binary with `--force` to update the checked-in file; review and commit that deterministic diff before admitting the new Billet release.

The cross-organization reusable workflow remains available for runner groups that are not workflow-restricted and for direct manual runs in Billet itself. It is not a valid acceptance gate for a trusted restricted pool.

The moving-major lane exercises `actions/cache@v5`, `upload-artifact@v7`, and `download-artifact@v8` on the host and in a job container. A second lane pins the reviewed action commits exactly. Separate moving and pinned lanes exercise the embedded cache clients in `actions/setup-node`, `actions/setup-java`, `actions/setup-python`, `actions/setup-go`, and `actions/setup-dotnet`, then prove their real package-cache directories survive a second VM. Poisoning probes isolate `ACTIONS_*`, Node, CA/TLS, proxy, loader, tar, and path variables to cache steps and then require a clean artifact upload/download to remain byte-identical. CA/TLS and proxy failures are interceptor-owned and must remain cache misses. Actions-service and process variables are runner-owned lifecycle probes: the gate records whether GitHub's runner restores or normalises them for the post action, but does not claim a DNS/TLS/Twirp interceptor can control a subprocess before its request reaches Billet. Clean cache recovery after transport faults and byte-checked artifact passthrough remain mandatory regardless of those observations.

A guest image or runner release is not promoted onto an interception-enabled tier until the consumer-owned workflow passes against that candidate. Billet supplies and validates the full protocol gate without embedding any consuming repository or infrastructure name in Billet itself.
