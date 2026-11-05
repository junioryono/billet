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

The caches [ADR-023](../decisions/adr-023-caches-off-firecracker.md) gives an EC2 guest, measured by jobs in a private repository `billet-testbed/b1t22-cache`, on `c7i.2xlarge` instances in `us-west-2a` launched from an image of `AMIContract` 5, by one `ec2` node at `10.77.0.10` on the same subnet, whose store is `ebs-s3` through an S3 gateway endpoint. Both tiers were untrusted with that repository as their `cache_scope`, so their caches published under `default-branch`, and every job was a `workflow_dispatch` on `main`. The times are what the job printed, so they include the step's own start, and come from one run each: they show which side of GitHub billet is on, not a distribution.

**The Actions cache against GitHub's own.** `actions/cache/save@v5` and `restore@v5` of 1 GiB from `/dev/urandom`, which no compression helps, saved in one job and restored twice in another. Run `41967274694` was on a tier with `cache.actions` off, so GitHub served it; run `41967278340`, on the tier with it on, was served by the node, whose answers the toolkit reported as `Cache Size: ~1024 MB` rather than GitHub's progress lines.

| 1 GiB | GitHub's cache | the node's |
|---|---|---|
| save | 47.51 s | 17.91 s |
| first restore | 31.87 s | 22.06 s |
| second restore | 20.60 s | 4.79 s |

The first restore in each job also paid for that step's own start; inside the node's, the download took under a second (`Cache Size` at 04:27:59.19, `Cache restored successfully` at 04:27:59.85).

**Git.** `actions/checkout@v5` and a `git fetch --depth=50` through the rewrite to the relay (`url.http://172.17.0.1:41322/v1/git/github.com/.insteadof=https://github.com/`): 0.62 s cold and 0.59 s warm, which billet recorded as `git warm`, the node answering `git-upload-pack` from its mirror in 4 ms. The repository is a few kilobytes, so this proves the path and not a speed.

**Go.** `go build std` through `GOCACHEPROG=/opt/billet/bin/billet cache gocacheprog`: 44.67 s cold, and then 114.00 s warm, which billet recorded as `go warm`. A warm build slower than a cold one is no cache at all. The helper answered the go command's requests one at a time, so the build waited for the sum of every request, and on this node an entry it did not hold locally is a request to S3. The helper now answers eight at once, and the node keeps what a published job wrote in its read cache. Those changes are in the image built after this run, and this record will be updated with what that image measures.

**Bazel.** The first Bazel job on EC2 failed: `Unexpected error reading config file '/etc/bazel.bazelrc': (error: 13): Permission denied`, exit 37. The boot script's `umask 077`, which keeps the runner's registration to root, had reached the step that writes every tool's configuration, so the bazelrc was `0600`. A cache must never fail a build, and the step now runs under `022`.

**What this does not prove.** One run of each, on one node, with no other load. The cache conformance workflow had not run on EC2: its candidate-identity check reads `/etc/billet-image`, which the EC2 image did not then write. Trusted-tier interception is unproved here, because the test organization has no runner group a trusted tier can be restricted to. Buck2, npm and the BuildKit cache were not measured.

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
