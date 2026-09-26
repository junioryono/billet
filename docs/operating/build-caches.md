# Build caches

A Firecracker node serves seven caches from the site's store, beside the compute that uses them: the Docker image store, sticky disks, the Actions cache, a Git mirror, a Bazel and Buck2 remote cache, and the Go build and test cache. Every one is on by default where the tier can have it, every one can be turned off per tier or by the central kill switch, and every one reports what it did for each job.

This page is the operator's view: what each cache is, what a tier gets without asking, how publication is decided, and how to see and stop a cache. [Trust and isolation](../concepts/trust-and-isolation.md) says what a cache can leak and to whom, and [ADR-013](../reference/decisions/adr-013-branch-protected-caches.md) records why publication is decided the way it is.

## What a tier gets without asking

| Cache | Default | Needs |
|---|---|---|
| Docker image store | on | any backend with `node.cache` |
| Sticky disks | on | any backend with `node.cache` |
| Actions cache | on where its scope already holds (below) | a Linux Firecracker tier and a repository scope |
| Git mirror | on | a Linux Firecracker tier |
| Bazel and Buck2 | on | a Linux Firecracker tier |
| Go build cache | on | a Linux Firecracker tier |
| Go test results | **off** | the Go build cache |

A cache that is on by default but cannot run on a tier is simply off there: a Docker tier gets the image store and sticky disks, and a Firecracker node without `node.cache` runs its jobs cold. Only a cache a tier turns on explicitly is refused where it cannot run, because then the operator asked for something the node cannot give.

Go test results are off because caching them lets `go test` skip a test whose inputs did not change, which changes what a job runs, not only how fast. The guest sets `GOFLAGS=-count=1` unless the tier opts in.

The Actions cache is on by default only where its scope rules already hold: an untrusted tier publishing from its default branch with a repository scope, or a trusted tier whose `cache_scope.workflow_ref` is one of its `workflows`. Anywhere else it stays off rather than refusing the tier.

## Publication

A cache's reads come from the newest published generation of its key; its writes go to the job's own clone, which is published or discarded when the job ends. `tiers[].cache.publish` decides which:

- **`trusted-only`**: a trusted pool publishes what it writes, and an untrusted pool publishes nothing. This is what every tier did before this setting existed, and what a trusted tier gets by default.
- **`default-branch`**: a write is published only for a job GitHub proves ran on the repository's default branch under an event GitHub itself lets write the default branch's cache (`push`, `schedule`, `workflow_dispatch`, `repository_dispatch`, `delete`, `registry_package`, `page_build`; GitHub's changelog, 2026-06-26). A pull request, a fork, a tag, a `pull_request_target`, a reusable workflow pinned somewhere else, and anything billet cannot prove publish nothing, and read what the default branch published. An untrusted tier with a repository gets this by default.
- **`off`**: nothing is published.

`default-branch` needs a **static repository**: a repository target, or `cache_scope.owner` and `cache_scope.repository`. A pooled runner's caches are attached before GitHub chooses its job, so the namespace cannot come from the job. The proof comes from GitHub's own record of the run (`GET /repos/{owner}/{repo}/actions/runs/{run_id}` and the repository's default branch, read fresh at every completion), which needs the App's **`actions: read`** permission. `billet github-app create` requests it by default (`--actions-read=false` leaves it out), and `billet check` fails a target whose tiers publish from a default branch while its App lacks it. Without it, every such completion is could-not-tell and nothing publishes.

Publication happens off the node's command path, after the compute is gone, journaled so a crash never snapshots twice and never publishes a stale clone, within 30 minutes of the completion or not at all. The kill switch is asked again before the pointer moves.

## The caches

**Docker image store.** Each job's `/var/lib/docker` is a clone of the newest generation for its deployment, site and architecture, attached before the runner starts so service containers are pulled from it. `cache.docker.max_size` (100 GiB) bounds the clone; a baseline larger than that is not cloned, and the job starts cold at its own size.

**Sticky disks.** `actions/stickydisk` attaches a volume by key; it is committed when the job ends and published under the tier's rule. Under `default-branch` the commit is deferred to the completion, which decides it. `cache.sticky_disks.max_size` clamps what a job may ask for.

**Actions cache.** `actions/cache` and every toolkit cache client are served from the site's store with no workflow change; see [Transparent Actions cache](actions-cache.md). Under `default-branch` a job restores from its own ref, then its pull request's base branch, then the default branch, which is GitHub's order, and saves only under its own ref, only where GitHub would let it.

**Git mirror.** The guest rewrites `https://github.com/` fetches to the node, which keeps a bare mirror per trust class and repository. Every fetch is authorised by GitHub with the job's own credentials before anything is served, pushes and SSH go to GitHub directly, and a fetch the mirror may not answer (a want no ref reaches, a refresh that failed, a repository larger than `cache.git.max_size`) goes to GitHub. Mirrors unused for seven days are reaped. The node needs `git`; `billet check` says when it has none.

**Bazel and Buck2.** The node speaks Bazel's HTTP cache at `/v1/cas/bazel` and the cache half of the Remote Execution API over gRPC on the same listener. The guest writes `/etc/bazel.bazelrc` with the remote cache and a credential helper (`billet cache credential-helper`) that answers the bearer from the environment and only for the node's host, so the bearer is never written to a file. Buck2 has no HTTP cache; point its `[buck2_re_client]` at the node's endpoint over `grpc://` with `http_headers = Authorization: Bearer $BILLET_CACHE_TOKEN`. A blob is stored only when its bytes hash to its digest, and an action result is served only while every blob it names is present.

**Go build cache.** The guest names `billet cache gocacheprog` as `GOCACHEPROG`. It keeps a local directory for the go command and shares objects with the node's content-addressed volume; a refused or unreachable node leaves it a plain local cache, never a failed build. A workflow opts out with `GOCACHEPROG=` (empty) in `env`, because the go command prefers `GOCACHEPROG` to `GOCACHE`. Concurrent jobs' writes are merged into the newest generation, so both survive.

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
```

Everything above is what that tier gets with no `cache` block at all. [Configuration](../reference/configuration.md) has every field.

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
