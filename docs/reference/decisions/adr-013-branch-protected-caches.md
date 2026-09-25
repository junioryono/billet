# ADR-013: Branch-protected caches for pooled runners

Accepted, 2026-09-25. Issue #226.

## Context

A scale-set runner is a pool member: GitHub registers it, then gives it whichever job it chooses, after the runner's caches are already attached. Until this decision, trust was the only thing that gated a cache. A trusted pool published everything it wrote, and an untrusted pool discarded every write and read only the trusted baseline. The reference deployment's pools are all untrusted, because they run pull requests, so every job pulled its images again, cloned its repository again, and built from nothing. The hosted vendors billet competes with (Namespace, Blacksmith, BuildJet, WarpBuild, Depot) all cache for pull requests, and GitHub's own Actions cache does too, under one rule: **a run writes its own ref, reads its own ref and then the default branch, and only the default branch writes what everyone reads.**

## Decision

**Publication is decided by the ref GitHub proves, per tier.** `tiers[].cache.publish` is one of:
- `trusted-only`: every tier's rule before this decision, and a trusted tier's default.
- `default-branch`: an untrusted tier's default when it has a repository scope.
- `off`.

Under `default-branch`, a job's writes publish only when all of the following hold:
- **The binding agrees.** The job's JobStarted binding (owner, repository, event, run) is recorded on the pool runner, and its completion carries the same identity.
- **GitHub's record agrees.** GitHub's record of the run (`GET /repos/{o}/{r}/actions/runs/{id}`, read fresh at the completion) shows:
  - the same run;
  - a head repository equal to the job's repository, so never a fork;
  - a head branch equal to the repository's default branch, read uncached;
  - an event in GitHub's writing set: `push`, `schedule`, `workflow_dispatch`, `repository_dispatch`, `delete`, `registry_package`, `page_build` (GitHub changelog, 2026-06-26).
- **The job's workflow is the run's.** It is the run's top-level workflow, or one it called at the run's own commit.
- **The repository is the pool's scope.** It equals the tier's static repository.

Everything else, including a pull request, a tag, `pull_request_target`, a reusable workflow pinned elsewhere, and any failure to ask, publishes nothing. One function, `server.DecideCacheAuthority`, holds the rule, and the node receives its verdict with the completion's destroy command (wire 23). The App needs `actions: read` to read the run; `billet github-app create` requests it by default.

**Reads are the pool's, in a namespace of their own.** Default-branch keys live under `<namespace>.scoped/<trust>/<owner>/<repo>/<arch>/<kind>/…`. No trusted-only key and no guest-chosen key can reach that prefix, so an untrusted pool never reads a trusted pool's generations. The namespace is the tier's static repository because caches are attached before the job is chosen. Any job of that pool reads what it published, which is GitHub's model.

**Every cache is on by default where the tier can have it**: the Docker store, sticky disks, the Actions cache, a Git mirror, Bazel and Buck2, and Go. Go test results are the exception, off by default, because caching them changes what `go test` runs. A default a tier cannot have is off, never refused.

**Publication is off the command path and journaled.** A completion records an intent. The node's cache loop publishes after the compute is gone, within 30 minutes, asking the kill switch again before the pointer moves. A crash is reconciled from the journal and never snapshots twice. Content-addressed caches (Bazel, Go) merge into the newest generation, so concurrent jobs' writes both survive.

**An older node is refused only a tier it would exceed, reads included.** A node below wire 23 ignores the cache block and applies the old rule, with the pool's pre-#226 keys. That is more than a `default-branch` tier allows, because it would read keys other repositories' jobs wrote. It is also more for a trusted pool told to publish `off` or `default-branch`, and for a cache turned off or held smaller. Those tiers wait for an upgraded host of their own, and `billet status` names them.

## Measured

- An untrusted guest reaches the node cache listener and is refused without its session bearer (HTTP 401 in under a millisecond, reference deployment, 2026-09-25).
- `url.insteadOf` drops the header `actions/checkout` scopes to `https://github.com/`, so a Git proxy reached that way never sees the job's token. After a 401, git runs the credential helper in the repository, where that header is readable (git 2.51.0, 2026-09-25). The Git proxy's credential helper is built on that.
- `upload-pack` under protocol v2 serves an object no ref reaches even with `uploadpack.allowAnySHA1InWant=false`. Under v0 it refuses the commit, but still serves a tree or a blob, because its check walks commits. The proxy serves v0 from a mirror pruned to GitHub's refs, and sends any want that is not a commit or a current ref's object to GitHub (git 2.51.0, 2026-09-25).
- git keeps its old base URL when it follows a redirect on the retry after a 401, so the node follows a renamed repository's redirect itself.
- The rest, with their dates, are in [Cache parity](../records/cache-parity.md).

## Alternatives rejected

- **Trust the launch assignment's or the runtime token's claim of a branch.** The assignment that scaled a pool up is not the job the runner gets, and `ACTIONS_RUNTIME_TOKEN` is held by the guest.
- **Trust `jobWorkflowRef`.** A push to `feature` can call `owner/repo/.github/workflows/x.yml@main`, which reads as a default-branch job running feature code. The branch comes from GitHub's record of the run.
- **A Git proxy by remapping `github.com` in the guest.** It would terminate TLS for every github.com request, pushes and downloads included, and needs SSH forwarded besides. `insteadOf` rewrites HTTPS fetches alone.
- **Whole-volume last-write-wins for content-addressed caches.** It loses one of two concurrent jobs' writes; the merge keeps both.
- **Refuse every non-default cache block on an older node.** Once every cache is on by default, that pins every Firecracker tier to upgraded nodes through a whole rollout, for defaults an older node only does less with. Only `default-branch` tiers are pinned, because there the older node's reads are wider.
