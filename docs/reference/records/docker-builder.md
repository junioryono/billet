# What a Docker build's cache saves

`setup-docker-builder` keeps a whole BuildKit state on a sticky disk, so a job's next build finds what the last one left, cache mounts included (#542). This record is what that saved against the other ways a job can cache a build, for four kinds of project and four kinds of change. It also covers what the default `cache-key` does when two Dockerfiles share it, how attaching the state grows with its size, and what the Docker image store saves a job that uses a large image. Measured on 2026-11-05.

## Where

- **The node.** An EC2 m8i.4xlarge (16 vCPU, 64 GiB) in a test account, Ubuntu 24.04, under nested virtualization, with single-host Ceph on four 150 GB gp3 OSDs. It is the host of [A store that stops answering](cache-breaker.md).
- **The guests.** 4 vCPU and 10 GiB per job on an untrusted tier publishing under `default-branch`, at most three at once because the control plane's ceiling was 14 vCPU.
- **Pulls.** The node ran its Docker Hub mirror (#543), but neither the guests' Docker nor the builders trusted it (the image predates that), so every pull went to Docker Hub.
- **The project.** A private repository of four apps, each with its own Dockerfile:
  - **go:** a service importing gRPC, client-go, Prometheus and x/text, built with cache mounts for the module and build caches.
  - **rust:** axum, tokio, serde and reqwest, with cache mounts for the registry and `target`.
  - **node:** `npm ci` of Next.js, React and Express, then `tsc`.
  - **python:** lxml, psycopg2 and cffi built from source, with a pip cache mount.

## The matrix

**Each series is four jobs in order, each its own VM.**
- **cold:** a key, scope or tag nothing has used.
- **warm:** the same build again.
- **source:** an edit to one source file.
- **lockfile:** one dependency bumped (golang.org/x/text, serde_json, lodash and psycopg2).

**The backends:**
- **none:** a fresh `docker-container` builder.
- **gha:** `type=gha`, `mode=max`, through billet's own Actions cache.
- **registry:** `type=registry` to GHCR, `mode=max`.
- **persistent:** `setup-docker-builder` with a key of its own for each series.

Every backend ran three series (48 jobs each). The table gives the median seconds of the build step over the three, with the number of build steps BuildKit reported `CACHED` in brackets.

| Workload | Backend | cold | warm | source | lockfile |
|---|---|---|---|---|---|
| go | none | 50 (0) | 47 (0) | 49 (0) | 49 (0) |
| go | gha | 111 (0) | 63 (5) | 135 (3) | 134 (1) |
| go | registry | 57 (0) | 5 (5) | 56 (2) | 61 (0) |
| go | persistent | 54 (0) | 2 (6) | 15 (4) | 28 (2) |
| rust | none | 45 (0) | 47 (0) | 44 (0) | 43 (0) |
| rust | gha | 107 (0) | 57 (4) | 106 (2) | 128 (1) |
| rust | registry | 51 (0) | 5 (4) | 51 (1) | 50 (0) |
| rust | persistent | 46 (0) | 1 (5) | 11 (3) | 15 (2) |
| node | none | 28 (0) | 27 (0) | 26 (0) | 26 (0) |
| node | gha | 149 (0) | 82 (5) | 127 (3) | 143 (1) |
| node | registry | 42 (0) | 5 (5) | 24 (2) | 42 (0) |
| node | persistent | 25 (0) | 1 (6) | 5 (4) | 14 (2) |
| python | none | 146 (0) | 142 (0) | 143 (0) | 143 (0) |
| python | gha | 223 (0) | 57 (6) | 96 (4) | 231 (2) |
| python | registry | 149 (0) | 5 (6) | 19 (3) | 150 (1) |
| python | persistent | 141 (0) | 2 (7) | 2 (5) | 12 (3) |

**What it shows:**
- **The persistent builder is the only one that saves a change.** After a source edit or a lockfile bump it built in 2 to 28 seconds where the others took most of a cold build. The registry and gha caches carry layers and not cache mounts, so once a change invalidates the step that compiles, its module, crate and pip caches are gone with it, and the step compiles from nothing. The persistent builder's whole state includes the cache mounts.
- **A registry cache is as good on an unchanged build** (5 s, against 1 or 2 for the persistent builder). Python's source edit (19 s) is the one change it absorbed, because there the compile is in a layer before the source is copied.
- **`type=gha` through billet's Actions cache cost more than it saved.** Its cold builds took 1.5 to 5 times as long as no cache, and its changed builds were slower than no cache except python's source edit. Exporting the cache was most of it: one go cold build spent 144 s in `sending cache export`. Billet's Actions cache serves `actions/cache` saves of one archive well ([Transparent Actions cache](../../operating/actions-cache.md)), and BuildKit's exporter writes many blobs, one reservation each. Why that is slow here was not looked into.
- **One of the three warm persistent builds missed** for go (48 s), node (26 s) and python (141 s): the warm job did not find its cold job's state, so the median is the honest figure and the worst case is a cold build. Whether the warm job started before the cold job's deferred publication had landed, or that publication failed (#743), was not established.
- **One gha series failed** (go, series 1): its export's finalize was refused by an unmap #743 describes, and the build failed. Its other two series are the medians above.

**Plain `docker build` in the Docker image store** was measured too, and is left out of the table. The image store is one generation per repository and architecture, and every job of the repository publishes it when it ends. During these hours ten of its publications succeeded and most failed on #743, so what each job found depended on which publication had last succeeded rather than on the build.

## Two Dockerfiles on one key

`setup-docker-builder`'s `cache-key` defaults to the constant `Dockerfile`, so every Dockerfile a repository builds with it shares one state. Two of the apps above built through it on the default key:

| Order | Builds | Seconds | Cached steps |
|---|---|---|---|
| one at a time | go, node, go, node | 57, 22, 1.6, 1.4 | 0, 1, 6, 6 |
| both at once, unchanged, three times | go and node | 1.3 to 1.4 | 6 each |
| both at once, each with a source edit, three times | go and node | 13 to 14 (go), 4.5 to 4.6 (node) | 4 each |

- **One at a time, sharing the key costs nothing.** Each build's state held the other's, so the second go and the second node were warm.
- **At once, each with something new, neither kept the other's new layers:** the edit was the same each round, and every round rebuilt it. Last write wins would have kept one of the two. Most of this key's publications in those minutes failed on #743, so this does not show what a shared key does to concurrent builds on a healthy node. The default stays as it is until that is measured, and the guidance below gives each Dockerfile a key of its own when they build at once.

## The state's size

A cache mount grown to the size shown, under a key of its own, then attached by two more jobs. The times are the step's own, from the job.

| State | Attach (`setup-docker-builder`) | The step that grew it | Its post step |
|---|---|---|---|
| 1 GiB | 3 to 4 s | 9 s, then 1 s | 1 to 3 s |
| 5 GiB | 3 s | 32 s, then 2 s | 0 to 9 s |

Attaching is a clone of the published generation, which is copy-on-write, so it did not grow with the state. A 20 GiB series was started and not finished: by then the rehearsal's cache pool, on two of the four OSDs, was 96% full, every write to it waited, and one of its OSDs stopped. So the record has nothing past 5 GiB. Committing it happens after the job, under `default-branch`, and is not in the job's time.

## The Docker image store and a large image

`pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime` (6.1 GB unpacked), pulled and then run (`import torch`) by two waves of three concurrent jobs, the second after the first's stores had published.

| Wave | Pull | Run | Read from the store |
|---|---|---|---|
| first, the image in no generation | 82 to 88 s, from Docker Hub | 3 to 4 s | 989 to 994 MiB |
| second, the image in the generation | 0.9 s | 6.5 to 7 s | 297 MiB |

The second wave read a twentieth of the image: a clone is read lazily, and running `import torch` touches only what it opens. Its run took twice as long as the first wave's, probably because the first wave had just written those files and read them back from the guest's page cache.

## What this does not establish

- **A bare host, or a store under other load.** Nested virtualization, one host, at most three builds at once.
- **The persistent builder against a busy site.** Its attach and publication shared this node with these jobs and nothing else.
- **Concurrent builds on one key on a healthy node** (#743).
- **Why `type=gha` exports slowly through billet's Actions cache.**
- **A builder state larger than 5 GiB.**
- **Plain `docker build` in the image store** (#743).
