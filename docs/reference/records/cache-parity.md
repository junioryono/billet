# Cache parity

What [ADR-013](../decisions/adr-013-branch-protected-caches.md) and the [build caches](../../operating/build-caches.md) rest on: what a job costs without them on the reference deployment, and what git and GitHub measurably do. Every number names its date and where it was taken. The after half is to be taken once the caches are deployed, and says so rather than being estimated.

## The listener is closed to a guest without its session

2026-09-25, reference deployment, v0.12.10, guest image `g20260925025322`: untrusted guests on `billet1` reached the node cache listener at `172.31.0.1:7718` and were answered HTTP 401 in 0.26 to 0.39 ms. TLS to the same port fails with `wrong version number`: the listener is plain HTTP on the guest bridge, and the session bearer is the boundary.

## Before: what an untrusted job paid with no cache

Reference deployment, 2026-09-25, from two instrumented workflow runs on untrusted tiers (2 vCPU and 8 vCPU), step timestamps to 0.1 s:

| Step | 2 vCPU | 8 vCPU |
|---|---|---|
| `docker pull postgres:17.6-bookworm`, cold (437,486,087 bytes) | 7.8 s, 8.1 s | 6.3 s, 7.3 s |
| full-history clone of the reference deployment's largest repository (`.git` 349 MB) | 22.7 s, 28.1 s | 34.3 s; a second attempt stalled 11 min 14 s and was reset by the shared uplink |
| the same, `--depth 1` (`.git` 162 MB) | 13.2 s, 15.0 s | 12.2 s |
| `actions/checkout` with `fetch-depth: 1` | 17 s | |
| `go build ./...` of billet's own tree, cold | 83.6 s | |
| `go test ./... -run XXX` (compile only, after the build) | 70.3 s | |

The guest image put no `go` on `PATH`; the build used `actions/setup-go` (Go 1.26.8). The 8 vCPU stall is the shared uplink [measured before](shared-uplink.md), and is what a node-local Git mirror removes from the checkout path.

## What git does, which the Git proxy is designed around

git 2.51.0, 2026-09-25, on a development Mac, with probes kept beside this work:

- **The rewrite drops checkout's credentials.** With `url.<proxy>.insteadOf https://github.com/`, the header `actions/checkout` writes (`http.https://github.com/.extraheader`) was not sent to the rewritten origin; a header scoped to the rewritten origin was.
- **The credential helper runs in the repository.** After the rewritten origin answered 401 with `WWW-Authenticate: Basic`, git ran the origin's credential helper with the repository's worktree as its directory and `GIT_DIR=.git`, where `git config --get http.https://github.com/.extraheader` returned checkout's header. The first request carried `Git-Protocol: version=2`.
- **Pushes can stay direct.** With `url.<upstream>.pushInsteadOf` set to the same prefix, a fetch reached the proxy and a push went to the upstream.
- **Protocol v2 does not guard wants.** An object reachable only from a deleted branch was served under protocol v2 with `uploadpack.allowAnySHA1InWant=false`, and refused under v0 ("Server does not allow request for unadvertised object").
- **Protocol v0 guards only commits.** It still served that branch's tree and file blob when asked for them by object id, because its reachability check walks commits.
- **Redirects keep the old base.** git followed a 301 on the retry after a 401 but kept its old base URL, so its fetch went to the old path.

## Binary size

2026-09-25, this development Mac, Go 1.26.6, `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 -trimpath -ldflags='-s -w'`: 37,843,106 bytes before the Remote Execution API and 38,305,954 after, +462,848 bytes (1.2%). gRPC was already linked through an existing dependency.

## On per-job EC2, 2026-11-05

The caches [ADR-024](../decisions/adr-024-caches-off-firecracker.md) gives an EC2 guest, measured by jobs in a private repository of the test organization, on `c7i.2xlarge` instances in `us-west-2a` launched from an image of `AMIContract` 5, by one `ec2` node at `10.77.0.10` on the same subnet, whose store is `ebs-s3` through an S3 gateway endpoint. Both tiers were untrusted with that repository as their `cache_scope`, so their caches published under `default-branch`, and every job was a `workflow_dispatch` on `main`. The times are what the job printed, so they include the step's own start, and come from one run each: they show which side of GitHub billet is on, not a distribution.

**The Actions cache against GitHub's own.** `actions/cache/save@v5` and `restore@v5` of 1 GiB from `/dev/urandom`, which no compression helps, saved in one job and restored twice in another. Run `41967274694` was on a tier with `cache.actions` off, so GitHub served it; run `41967278340`, on the tier with it on, was served by the node, whose answers the toolkit reported as `Cache Size: ~1024 MB` rather than GitHub's progress lines.

| 1 GiB | GitHub's cache | the node's |
|---|---|---|
| save | 47.51 s | 17.91 s |
| first restore | 31.87 s | 22.06 s |
| second restore | 20.60 s | 4.79 s |

The first restore in each job also paid for that step's own start; inside the node's, the download took under a second (`Cache Size` at 04:27:59.19, `Cache restored successfully` at 04:27:59.85).

**Git.** `actions/checkout@v5` and a `git fetch --depth=50` through the rewrite to the relay (`url.http://172.17.0.1:41322/v1/git/github.com/.insteadof=https://github.com/`): 0.62 s cold and 0.59 s warm, which billet recorded as `git warm`, the node answering `git-upload-pack` from its mirror in 4 ms. The repository is a few kilobytes, so this proves the path and not a speed.

**Go.** `go build std` through `GOCACHEPROG=/opt/billet/bin/billet cache gocacheprog`: 44.67 s cold, and then 114.00 s warm, which billet recorded as `go warm`. A warm build slower than a cold one is no cache at all. The helper answered the go command's requests one at a time, so the build waited for the sum of every request, and on this node an entry it did not hold locally is a request to S3. The helper now answers eight at once, and the node keeps what a published job wrote in its read cache. On the image built after those changes (`billet-b1t22-x64-6`), run `42005240181` built `go build std` warm from entries the node had to fetch from S3 in 44.59 s, and run `42006416034`, with the entries already in the node's read cache, in 52.90 s, both recorded `go warm`. The regression is gone and the cache still does not beat compiling here, and the faster path, entries already on the node, was the slower run, so what costs the time is per request rather than the store. Each entry is a round trip from the helper through the guest's relay, which on EC2 also does the TLS to the node; that relay is the suspect and is not yet measured (#748).

**Bazel.** The first Bazel job on EC2 failed: `Unexpected error reading config file '/etc/bazel.bazelrc': (error: 13): Permission denied`, exit 37. The boot script's `umask 077`, which keeps the runner's registration to root, had reached the step that writes every tool's configuration, so the bazelrc was `0600`. A cache must never fail a build, and the step now runs under `022`. On the image built after that, the same job built cold in run `42005240181` (`Elapsed time: 14.523s, Critical Path: 0.76s`, `1 processwrapper-sandbox`) and warm in run `42006416034` (`Elapsed time: 15.362s, Critical Path: 0.24s`, `1 remote cache hit`): the hit is the point, and the target, one genrule, is too small for its time to say anything but what Bazel's own start costs.

**The conformance workflow.** `billet cache conformance install --expected-guest-contract ec2:5`, pinned to the branch's commit, run `42020149606` on the cache tier: 17 jobs passed, among them every host save and restore, the pinned and embedded clients, the poisoned clients, `buildkit-gha`, the BuildKit loopback export and import, the v2 loopback save and restore, and `compose-database`; five failed, and none of them on anything this change added:

- `client-trust` asks `HEAD /` of the results origin and looks for billet's header. Every client's TLS to the origin succeeded through the node's CA, and the answer was GitHub's own `404` with `Server: github.com`: the node writes `X-Billet-Actions-Cache` only on what it answers itself, and splices a request it does not admit to GitHub as it came. That splice is the node's code on every backend; this batch did not run the job on Firecracker to say whether it fails there too.
- `save-container` and `container-docker-buildkit` fail in `@actions/core` with `Input does not meet YAML 1.2 "Core Schema" specification` for `include-hidden-files` and `keep-state`. The container hook runs `docker exec -e NAME` through billet's shim, a `#!/bin/sh` script, and on this image `/bin/sh` is dash, which drops every environment variable whose name is not a shell identifier: `env 'INPUT_A-B=x' /bin/sh -c env` shows none and bash shows it. A hyphenated action input therefore reaches the action empty.
- `services-host` prints the right row and fails its own comparison, because `psql -tA` with three `-c` prints `CREATE TABLE` and `INSERT 0 1` before it, into the file the step compares.
- `services-container` cannot resolve its service, `postgres`, from the job container. Not yet explained.

A first run, `42005379156`, also lost both loopback restores, to this harness: the tunnel that carried the control plane to the node failed, and the node, unable to ask what a job may do, sent it to GitHub, as it is meant to.

**What this does not prove.** One run of each, on one node, with no other load. Trusted-tier interception is unproved here, because the test organization has no runner group a trusted tier can be restricted to. Buck2, npm and the BuildKit cache were not measured.

## Still to measure, once deployed

Each of these is taken on the reference deployment after the release carrying the caches, and recorded here with its date:

- **M1, the binding against GitHub's record.** For each shape of job, the JobStarted identity against GitHub's record of the run:
  - a push to the default branch;
  - a same-repository pull request;
  - a dispatch on the default branch and on another branch;
  - a schedule;
  - a local reusable workflow, called from the default branch and from a pull request;
  - a same-repository reusable workflow pinned to the default branch and to a commit, called from a feature branch.

  Fork pull requests cannot be measured on this fleet, whose repositories are private.
- **M2, after.** The table above repeated, cold and warm, with a BuildKit sticky-disk build and a Bazel and a Buck2 build beside it.
- **M4.** The bytes each cache holds after a day.
- **M6.** Buck2's global configuration for a remote cache over `grpc://` with a bearer header.
- **M7.** Watchers on a clone (`rbd status`) at commit and at teardown.
