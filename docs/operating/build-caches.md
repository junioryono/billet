# Build caches

A Firecracker node serves seven caches from the site's store, beside the compute that uses them: the Docker image store, sticky disks, the Actions cache, a Git mirror, a Bazel and Buck2 remote cache, and the Go build and test cache. Every one is on by default where the tier can have it, every one can be turned off per tier or by the central kill switch, and every one reports what it did for each job. An eighth, the npm registry cache, is served from the node's own disk and, since 2026-10-25, is on by default too; a tier whose jobs run a client in the runner's own process that ignores `NODE_EXTRA_CA_CERTS` and the system store fails TLS through it, and turns it off with `cache.npm.enabled: false`.

This page is the operator's view: what each cache is, what a tier gets without asking, how publication is decided, and how to see and stop a cache. [Trust and isolation](../concepts/trust-and-isolation.md) says what a cache can leak and to whom, and [ADR-013](../reference/decisions/adr-013-branch-protected-caches.md) records why publication is decided the way it is.

## What a tier gets without asking

| Cache | Default | Needs |
|---|---|---|
| Docker image store | on | any backend with `node.cache` |
| Sticky disks | on | any backend with `node.cache` |
| Actions cache | on where its scope already holds (below) | a Linux Firecracker tier and a repository scope, which an organization target's pool gets only by naming one ([An organization target](actions-cache.md#an-organization-target)) |
| Git mirror | on | a Linux Firecracker tier |
| Bazel and Buck2 | on where the tier can publish it (below) | a Linux Firecracker tier |
| Go build cache | on where the tier can publish it (below) | a Linux Firecracker tier |
| Go test results | **off** | the Go build cache |
| npm registry | on | a Linux Firecracker tier, and a guest image that knows the `npm-proxy` key |

A cache that is on by default but cannot run on a tier is simply off there: a Docker tier gets the image store and sticky disks, and a Firecracker node without `node.cache` runs its jobs cold. Only a cache a tier turns on explicitly is refused where it cannot run, because then the operator asked for something the node cannot give.

The Bazel and Go caches are on by default only where the tier can publish what they hold: a trusted tier under `trusted-only`, or any tier under `default-branch`. Their keys are scoped by trust, so an untrusted tier that publishes nothing would read a key nobody writes and every build would start cold, and that is worse than no billet cache at all: `GOCACHEPROG` takes precedence over `GOCACHE`, so the build cache a workflow restores itself (`setup-go`, `golangci-lint`) would be ignored. Turn either on explicitly to have it anyway.

Go test results are off because caching them lets `go test` skip a test whose inputs did not change, which changes what a job runs, not only how fast. The guest sets `GOFLAGS=-count=1` unless the tier opts in.

The Actions cache is on by default only where its scope rules already hold: an untrusted tier publishing from its default branch with a repository scope, or a trusted tier whose `cache_scope.workflow_ref` is one of its `workflows`. Anywhere else it stays off rather than refusing the tier.

## Publication

A cache's reads come from the newest published generation of its key; its writes go to the job's own clone, which is published or discarded when the job ends. `tiers[].cache.publish` decides which:

- **`trusted-only`**: a trusted pool publishes what it writes, and an untrusted pool publishes nothing. This is what every tier did before this setting existed, and what a trusted tier gets by default.
- **`default-branch`**: a write is published only for a job GitHub proves ran on the repository's default branch under an event GitHub itself lets write the default branch's cache (`push`, `schedule`, `workflow_dispatch`, `repository_dispatch`, `delete`, `registry_package`, `page_build`; GitHub's changelog, 2026-06-26). A pull request, a fork, a tag, a `pull_request_target`, any reusable workflow (even one called at the run's own commit, since nothing GitHub records tells a branch from a tag of the same name at that commit), and anything billet cannot prove publish nothing, and read what the default branch published. An untrusted tier with a repository gets this by default.
- **`off`**: nothing is published.

`default-branch` needs a **static repository**: a repository target, or `cache_scope.owner` and `cache_scope.repository`. A pooled runner's caches are attached before GitHub chooses its job, so the namespace cannot come from the job. The proof comes from GitHub's own record of the run (`GET /repos/{owner}/{repo}/actions/runs/{run_id}` and the repository's default branch, read fresh at every completion), which needs the App's **`actions: read`** permission. `billet github-app create` requests it by default (`--actions-read=false` leaves it out), and `billet check` fails a target whose tiers publish from a default branch while its App lacks it. Without it, every such completion is could-not-tell and nothing publishes.

Publication happens off the node's command path, after the compute is gone, journaled so a crash never snapshots twice and never publishes a stale clone, within 30 minutes of the completion or not at all. The kill switch is asked again before the pointer moves.

## The caches

**Docker image store.** Each job's `/var/lib/docker` is a clone of the newest generation for its deployment, site and architecture, attached before the runner starts so service containers are pulled from it. `cache.docker.max_size` (100 GiB) bounds the clone; a baseline larger than that is not cloned, and the job starts cold at its own size.

**Sticky disks.** `actions/stickydisk` attaches a volume by key; it is committed when the job ends and published under the tier's rule. Under `default-branch` the commit is deferred to the completion, which decides it. `cache.sticky_disks.max_size` clamps what a job may ask for.

**Actions cache.** `actions/cache` and every toolkit cache client are served from the site's store with no workflow change; see [Transparent Actions cache](actions-cache.md). Under `default-branch` a job restores from its own ref, then its pull request's base branch, then the default branch, which is GitHub's order, and saves only under its own ref, only where GitHub would let it.

**Git mirror.** The guest rewrites `https://github.com/` fetches to the node, which keeps a bare mirror per trust class and repository. Every fetch is authorised by GitHub with the job's own credentials before anything is served, and a job GitHub authorised is served whatever its repository's mirror holds, as GitHub would serve it ([ADR-018](../reference/decisions/adr-018-git-proxy-answers-as-github.md)). Fetches under `sudo` go through the node too, and a job that clones another private repository with its own token (a credential store, `gh auth setup-git`) has that token handed to the node, which asks GitHub with it, when checkout set no header of its own. Pushes and SSH go to GitHub directly, and a fetch the mirror cannot answer (an object it lacks, a mirror still being refreshed or whose refresh failed, a repository larger than `cache.git.max_size`, a refusal from the mirror's own `upload-pack`) goes to GitHub. A mirror belongs to the repository GitHub's API names, by its id: the node asks `GET /repos/{owner}/{repo}` with the job's own credentials, and a mirror of a name that has since passed to another repository is removed before anything of it is served. A job waits at most ten seconds for its mirror to be refreshed; the refresh goes on for the jobs after it. A git request through the node ends when no byte has moved for three minutes, and each one leaves a line in the node's log, `a git request`, saying where it was answered and why. A mirror is repacked after a refresh, off the request path: once a day into one pack with a bitmap, and between those whenever it holds eight packs; about once a week, when a refresh runs, objects found unreachable at least fourteen days before are pruned, during which the mirror's jobs fetch from GitHub. `billet cache git forget --repository <owner/repo>`, run on a node, stops that repository's mirrors being served at once, for an object GitHub has removed that must not be served again, and the node removes them at the next fetch or a cleanup, which it tries every six hours. Mirrors unused for seven days are reaped. The node needs `git`; `billet check` says when it has none.

**Bazel and Buck2.** The node speaks Bazel's HTTP cache at `/v1/cas/bazel` and the cache half of the Remote Execution API over gRPC on the same listener. The guest writes `/etc/bazel.bazelrc` with the remote cache and a credential helper (`billet cache credential-helper`) that answers the bearer from the environment and only for the node's host, so the bearer is never written to a file. Buck2 has no HTTP cache; point its `[buck2_re_client]` at the node's endpoint over `grpc://` with `http_headers = Authorization: Bearer $BILLET_CACHE_TOKEN`. A blob is stored only when its bytes hash to its digest, and an action result is served only while every blob it names is present.

**Go build cache.** The guest names `billet cache gocacheprog` as `GOCACHEPROG`. Its local cache is the go command's own `GOCACHE` (`~/.cache/go-build` unless the job sets `GOCACHE`), in the go command's own layout, and it shares objects with the node's content-addressed volume; a refused or unreachable node leaves it a plain local cache, never a failed build. Because the local cache is `GOCACHE` itself, a build cache the workflow restores there (with `actions/cache`, say) still serves through the helper, and what the helper builds is there for the workflow to save: the helper is never slower than no helper. When another job's publication of the same cache holds its writer, a job's writes wait their turn, within the 30-minute publication window, instead of being dropped. A workflow opts out with `GOCACHEPROG=` (empty) in `env`, because the go command prefers `GOCACHEPROG` to `GOCACHE`. Concurrent jobs' writes are merged into the newest generation, so both survive.

**npm registry.** On every firecracker Linux tier unless it writes `cache.npm.enabled: false`, the node serves `registry.npmjs.org` to the tier's guests ([ADR-020](../reference/decisions/adr-020-npm-registry-interception.md)), with no workflow change and no registry setting, so `pnpm/setup`'s own download is served too.
- **What it answers.** It answers from a store on its own disk only anonymous reads: a package document, a version document or a tarball, asked for with no credential and no query. Everything else (a publish, a login, an audit, a request carrying an `.npmrc` token) goes to the registry unchanged.
- **One store per node.** The store holds only what the registry gives anyone without a credential, fetched by the node itself, so it serves every tier and trust class on the node. It keeps 10 GiB, least recently used first.
- **How long an answer lasts.** A document is fresh for as long as the registry says (five minutes, counted down from its `Age`). A tarball is asked for again after a day.
- **A registry that stalls.** A fetch that gets no headers within 8 seconds is tried once more on a new connection. If both attempts get no answer, an entry that expired within the last hour may be served instead, and the node logs a warning naming it. That happens only while the registry's own lifetime for it still holds, or when the registry's answer allowed reuse after it (it named none of `must-revalidate`, `proxy-revalidate` or `s-maxage`). So a tarball past billet's one-day ceiling but within the registry's year qualifies, and an entry the registry said to revalidate does not.
- **Only the guest's own processes.** The registry is remapped in the guest's `/etc/hosts` only. Containers (`container:` jobs, service containers, `docker run`, Dockerfile `RUN` steps) resolve it through the guest's container resolver, which never reads that file, and reach the registry directly, because they do not trust the node's interception authority. So on an npm tier, as on a tier with the Actions cache, a container on a user-defined network no longer resolves an entry a job adds to `/etc/hosts`.
- **The guest's other clients.** The guest's relay serves only processes of the runner's own user in the guest's own mount namespace, and passes every other connection straight to the registry, as a miss, logging each one with the reason in the guest's `billet-npm-proxy` unit. So `sudo npm` reaches the registry directly, uncached: root keeps the bundle that names the node's authority (which the Actions cache's remap needs), but its npm requests are not served from the node. A `--network host` container shares the guest's `/etc/hosts` and so meets the remap, and is passed straight to the registry the same way.
- **An image that knows the key.** It needs a guest image that knows the `npm-proxy` metadata key; an older image leaves npm going direct.

**Container jobs.** A job in `container:` gets the Go helper mounted read-only and its environment copied in, where the job's own `env` wins. The Bazel rc and the Git rewrite live in the guest and do not reach a job container.

## Configuration

```yaml
tiers:
  - label: linux-8
    provider: firecracker
    trust: untrusted
    cache_scope: {owner: acme, repository: api}
    cache:
      publish: default-branch          # the default here; trusted-only | default-branch | off
      docker:       {enabled: true,  max_size: 100GiB}
      sticky_disks: {enabled: true,  max_size: 100GiB}
      actions:      {enabled: true,  max_archive: 10GiB}
      git:          {enabled: true,  max_size: 50GiB}
      bazel:        {enabled: true,  max_size: 50GiB}
      go:           {enabled: true,  max_size: 20GiB, test_results: false}
      npm:          {enabled: true}        # on, as every cache is; no max_size, the store is the node's
```

Everything above is what that tier gets with no `cache` block at all. [Configuration](../reference/configuration.md) has every field.

## Registry mirrors

Without a mirror, every guest pulls from the registry over the site's public address. Docker Hub limits an unauthenticated address, per IPv4 address or IPv6 /64. Its documentation said 100 pulls in 6 hours when read on 2026-11-04, and a multi-architecture image counts one pull for each architecture ([Docker's documentation](https://docs.docker.com/docker-hub/usage/pulls/)). Its own answer to an EC2 address on 2026-11-05 was `ratelimit-limit: 100;w=3600`, which is 100 an hour, and reading one image's index and one platform's manifest used two of them. A layer is not counted. GitHub-hosted runners are exempt, and self-hosted runners are not, so a busy site behind one NAT address starts failing pulls. The Docker image store absorbs repeat pulls of images already in its generation, but not the first pull of a new tag, or a pull from a service container built at run time.

**What runs.** The host role runs three [Distribution](https://github.com/distribution/distribution) pull-through mirrors on every Firecracker host, one per upstream because a proxy accepts one upstream, and writes `node.registry_mirrors` into that node's configuration:

| Upstream | Unit | Port on the first guest bridge |
|---|---|---|
| Docker Hub | `billet-registry@docker-io` | 7740 |
| GHCR | `billet-registry@ghcr-io` | 7741 |
| Quay | `billet-registry@quay-io` | 7742 |

They serve public content with no upstream credential, to every guest on the host, trusted or not: a credential there would hand what it can read to every job. A proxy refuses a push, so one job cannot put anything in front of another. Each host runs its own, because a guest may not reach a private address across the site, so a site of several hosts pulls a new image once per host.

**What reaches them.** BuildKit under `setup-docker-builder` uses all three. Docker Engine has a mirror setting for Docker Hub alone, so `docker pull`, a service container and a plain `docker build` use that one, and their GHCR and Quay pulls go to the registry. A `docker-container` builder a workflow creates itself runs on its own configuration and uses none. A private image is not served: the mirror holds no credential, and the client goes on to the registry with its own.

**How a guest trusts them.** The role makes an authority on each host, under `/etc/billet-registry`, and a certificate naming every bridge's address. The authority's key stays root's on that host. The node reads the authority's certificate at each launch and hands it to the guest, which trusts it for the mirrors alone (Docker's `certs.d`, BuildKit's `ca`), never in its system store. **This takes a guest image that knows the `registry-mirrors-ca-pem` metadata key.** An older image cannot verify the mirror and pulls from the registry, as if there were none: measured on 2026-11-04, dockerd under the guest's own configuration and BuildKit each moved on to the registry in about a second.

**Their disk.** Each mirror keeps what it pulled under `/var/lib/billet-registry` for `billet_registry_mirror_ttl` after pulling it, 168 hours unless changed. Distribution has no size limit, and any job on the host can ask for any public image, so `billet-registry-guard.timer` asks every ten seconds how full that filesystem is, and at `billet_registry_mirror_disk_limit_percent` (85) it stops the mirrors, discards everything they pulled and starts them again, which it says in its journal. It leaves them alone when they hold less than `billet_registry_mirror_discard_floor_mb` (1024), because then something else is filling the disk and a discard would free nothing. A controller retirement renders each node's file as an ordinary converge would, mirrors included, on that node's own evidence that the role installed them, which is the authority at `/etc/billet-registry/ca.pem`. A node whose file names the role's mirrors with that file gone is refused, and an ordinary converge of it settles it. The next pull comes from the registry and fills them again. This is a watch and not a quota: what a host can pull in ten seconds has to fit in what is left past the limit. A host whose disk is small beside its line, or that hits the guard often, shortens the time, lowers the limit or gives that directory a disk of its own.

**How to tell the limit was hit.** A pull fails with `toomanyrequests` and `You have reached your pull rate limit`, in the job's log and in the Docker Engine's, and a mirror logs the same `429` in `journalctl -u billet-registry@docker-io` when it is the one that met it. What an address has left is in the `ratelimit-remaining` header of a `HEAD` of a manifest, which does not count as a pull; Docker's page above has the two commands. A mirror's own pulls count too, once per image rather than once per job. `billet check` says whether each mirror answers, or `NO DOCKER HUB MIRROR` when the site has none.

**How to turn it off.** Set `billet_registry_mirrors: false` and converge. The role stops and removes the mirrors, their authority and the firewall ports, and takes `node.registry_mirrors` out of the node's configuration, which drains the node once, as adding it did. What they pulled stays, unless `billet_registry_mirrors_purge` is true. To use your own mirrors, name them in `node.registry_mirrors` in the inventory, with `node.registry_mirrors_ca` if no public authority signs them, and the role runs none.

## Seeing and stopping a cache

```bash
billet cache status                               # every tier's caches, the kill switch, what each cache did (last 24h)
billet cache disable --repository acme/api --kind git
billet cache disable --org acme                   # every cache (--kind all is the default)
billet cache enable  --repository acme/api --kind git
```

A block covers one cache or all of them, for an organisation or a repository, and so reaches the tiers whose caches belong to a repository: a repository target's, or one with `cache_scope`. The node asks it before attaching a cache, again every 30 seconds while a content-addressed cache is in use, and again before a deferred publication moves the pointer. A tier with no repository scope is nothing a block can name, so turn its caches off in its own `cache` block. A block an older binary wrote covers the Actions cache, which is all it ever meant.

Each job's history records what every cache did: `warm` (it already held something the job used), `cold` (the job used it and it held nothing the job used), `disabled`, `unavailable` (the store failed and the job went on without it) or `unused`. A job whose outcome was not observed is counted as such, never as a miss.

## Older nodes

A node too old to read a tier's cache block (wire below 23) applies the rule every tier had before: a trusted pool publishes, an untrusted one discards, only the image store and sticky disks run, and both use the pool's pre-#226 keys. Where that is the same or less than the tier allows, such a node still runs the tier: an unscoped tier's new caches simply build cold there. Where it would be more, the tier is placed only on a node at 23 or later:

- **Every `default-branch` tier**, which is an untrusted tier with a repository by default. Its namespace is its repository's, and the older node would read the pool's wider keys instead.
- **A trusted pool** publishing `off` or `default-branch`.
- **A Docker store or sticky disk** turned off or held smaller.
- **An Actions archive** held below 10 GiB.

During a rollout such a tier waits for the first of its own hosts to upgrade, and `billet status` names it as waiting.

The npm cache needs no newer node: one below wire 31 ignores `cache.npm`, which leaves the tier's npm traffic going to the registry, as it always did.
