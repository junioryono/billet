# ADR-020: The node serves the public npm registry to its guests

Accepted, 2026-10-22. Issue #631. Widens the rule that interception terminates TLS for one host (`billet-security`, "Interception terminates TLS for one host and serves three methods") to a second host, `registry.npmjs.org`, under the conditions below.

## Context

On 2026-10-22 a consumer's jobs on a Firecracker tier failed twice at the same step, `pnpm/setup@v3`, with `Could not reach https://registry.npmjs.org/@pnpm/exe.linux-x64/12.4.2: timed out after 30s`. In the second run three sibling jobs made the same fetch within the same minute and succeeded. Across that repository's last 400 runs (1,739 jobs) these were the only two network failures. They are the failure class `docs/reference/records/shared-uplink.md` records at the reference site: a guest's connection out stalls intermittently. An eleven-minute capture there counted 0.7% of outbound TCP handshakes needing a SYN retransmit, and 0.5% of the bridge resolver's upstream queries going unanswered. That record's own conclusion is that fewer connections per job out of the site is the durable answer, and that billet has no package cache of its own.

What reaches the step that failed:

- **`pnpm/setup` fetches only from its `registry` input.** In its own `action.yml` (read 2026-10-22), "registry environment variables such as `pnpm_config_registry` do not change it". It also verifies what it downloads against npm's signature, so a mirror "has to proxy the npm registry rather than re-publish pnpm". A cache offered only through the job's environment (`npm_config_registry`, an `.npmrc`) would serve `npm ci` and `pnpm install` but not this download. Reaching it would need a `registry:` input in every workflow, against billet's rule that a workflow moved onto billet changes `runs-on` and nothing else.
- **GitHub's hosted runners fetch from the registry directly,** and GitHub publishes no registry cache for them.
- **The hosted-runner services that cache npm ask the workflow to change.** Blacksmith's documented answer (read 2026-10-22) is a sticky disk mounted at `~/.npm` or `node_modules` through its own action. RunsOn's pull-through caches are an opt-in label extra, and they cover container registries. billet already ships a sticky disk (`actions/stickydisk`), and it has the same limit: it serves `npm ci` from a warm `~/.npm`, but not `pnpm/setup`'s download, and only in a workflow that adds the step.

So only interception reaches every npm fetch without a workflow change. billet already intercepts one origin, the Actions results host. It does so with a DNS remap inside the guest, TLS terminated on the node by a CA the guest trusts (`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`), and a choice made on each connection's first request to answer it or splice the connection to the real origin.

## What was measured

`registry.npmjs.org`, 2026-10-22 21:33 UTC, with curl from the development Mac:

- **Package documents are fresh for five minutes, at npm's own edge too.**
  - The full document (`/@pnpm/exe.linux-x64`) and the abbreviated one (`Accept: application/vnd.npm.install-v1+json`) both answer `cache-control: public, max-age=300`, with an `ETag`, a `Last-Modified` and `vary: accept-encoding, accept`.
  - Cloudflare served both from its cache (`cf-cache-status: HIT`, `age` 63 and 72).
  - A request carrying the abbreviated document's `ETag` in `If-None-Match` is answered `304`.
- **A tarball is a year-long object.** `/@pnpm/exe.linux-x64/-/exe.linux-x64-12.4.2.tgz` answers `cache-control: public, must-revalidate, max-age=31557600`, 21,062,701 bytes, an `age` of 1,588,966 seconds (18 days at Cloudflare).
- **A version's tarball URL names the registry itself.** The version document's `dist.tarball` is `https://registry.npmjs.org/@pnpm/exe.linux-x64/-/exe.linux-x64-12.4.2.tgz`, and its `dist` carries `integrity`, `shasum`, `signatures` and `attestations`. A remap of the one host therefore also catches every tarball a document names, with nothing rewritten.
- **An answer does not say whether it was private.** A public package fetched with a bogus `Authorization: Bearer` header answers `200`, the same as without one. A missing package answers `404`.

## Decision

**A tier may have the node serve `registry.npmjs.org`** (`cache.npm`), through the interception the Actions cache uses:

- the guest's DNS remap, of one more host;
- TLS terminated by the node's interception authority, with the host added to the node's leaf;
- the per-connection choice to answer or to splice.

The Actions cache and the npm cache are separate switches. A tier may have either without the other, and the guest installs the interception authority when either is on.

**Only an anonymous read is ever answered from or written to the store.** A connection is answered by the node only when its first request is a `GET` or `HEAD` with no `Authorization`, `Cookie` or `npm-otp` header, for one of three shapes:

- a package document, `/<name>` or `/@<scope>%2f<name>` or `/@<scope>/<name>`;
- a version document, `/<name>/<version>`;
- a tarball, `/<name>/-/<file>.tgz`.

Each name must be a valid npm package name and each version a valid semantic version. Anything else is spliced to the registry unchanged, for the whole connection: a publish, a login, an audit, a search, a dist-tag change, and any request that carries a credential. A token is sent to the registry on every request once a job configures one, and an answer cannot say whether the package behind it was private, so an authenticated request is never cached. The node sees such a request in transit, as it sees the runner's own Actions token on the results host. It never stores it, never logs it, and forwards it unchanged.

**One store per node, shared by every tier on it, whatever its trust.**

- Every answer in the store is what the public registry gives anyone who asks without a credential. The node fetches it itself over a verified TLS connection, with none of the job's headers but `Accept` and `Accept-Encoding`. So nothing a job sends, only the name it asks for, decides what another job reads.
- An untrusted pool may have it for the same reason: the cache serves nothing scoped to a pool. The Actions cache is different, because it serves the pool's own entries, and it keeps its trust rules.
- A job can fill the store with real packages and evict others. That costs warm hits, not correctness, and the store's size bound limits it.
- The store lives on the node's own disk, not in a site volume: the uplink cost is then one fetch per node rather than one per job. Sharing one store across a site's nodes is not decided here.

**Package documents follow npm's own freshness; tarballs are kept.**

- A package or version document is stored per path and per `Accept` variant (full or abbreviated, which `vary: accept` names), with its `ETag`.
- It is served from the store for the `max-age` the registry gave it (five minutes, as measured), and then revalidated with `If-None-Match`.
- A tarball is stored once, by path, and served unchanged, so the client's own check of the document's `integrity` and npm's signature still holds. npm does not let a published version's tarball change: a version that has been unpublished cannot be published again.
- A stored entry is evicted when the store passes its bound, least recently served first.

**A stalled upstream fetch is retried inside the client's patience, and an answer that went stale is better than none.**

- The node gives one attempt at the registry 10 seconds to produce response headers. It then makes one more attempt on a new connection, so both fit inside the 30 seconds `pnpm/setup` waits.
- When both fail and the store holds the document, the stale document is served, for up to 24 hours past its freshness. Every such answer is recorded.
- A stale document can lack versions published in the last day. A job asking for one of those then fails as it would have failed against the stalled registry. An install from a lockfile, which names each version and its integrity, mostly fetches tarballs, not documents.
- A missing tarball has no stale form: two failed attempts fail the request, as the registry would have.

**It is off by default for its first release.** ADR-013's rule is that every cache is on by default where a tier can have it. This one is the exception until it has served a real fleet, because it adds a second intercepted host to every guest of every tier that turns it on. A mistake in it breaks every npm install on that tier, where a mistake in the other caches costs a cold start. Turning it on by default is a later decision with its own evidence: a real fleet's jobs served through it, and the guest passthrough measured under a large `npm ci` (below).

**A guest remaps a host only when the node names it.** The guest image is published and outlives node releases. A guest that remapped `registry.npmjs.org` on its own, under a node too old to serve it, would send every npm fetch to a proxy that refuses it. So the guest remaps the host only when its launch metadata names it, as the Actions remap already depends on the node naming its proxy. A node that does not name it leaves npm going direct. An older node reads the tier's `cache.npm` as absent, which is safe: doing less leaves the job on the registry.

**What each job's npm traffic did is an observation.** The node records it for the lease, in the closed vocabulary the other caches use, so `billet jobs show` names it for a job:

- how many requests it served warm, fetched or revalidated, and how many it spliced;
- the stale answers it gave;
- each upstream attempt that stalled.

That is the first half of #631's evidence for npm. The resolver and the guest's other connections are #631's second item and are not decided here.

## What is not decided here

- **The guest passthrough under a large install.** The Actions interception's guest-side relay was chosen over an `HTTPS_PROXY` because bulk transfers through a catch-all relay stalled (2026-10-04). An `npm ci` of a large lockfile is thousands of requests and hundreds of megabytes through the same relay. The implementation measures one on a real guest before the cache is offered, and if the relay is the limit, the relay changes, not the rule.
- **Other registries.** PyPI, the Go module proxy and Ubuntu's archive fit the same shape, a public anonymous read with immutable artifacts, and each would be its own host under this rule.
- **A store shared by a site's nodes,** in Ceph as the other caches are.

## Rejected

- **The job's environment only** (`npm_config_registry`, `pnpm_config_registry`, a written `.npmrc`). It serves `npm ci` and `pnpm install` without interception, but not `pnpm/setup`, the download that failed, and not a workflow that writes its own registry setting.
- **A sticky disk at `~/.npm`,** the hosted services' answer. It needs a workflow change and does not reach `pnpm/setup`.
- **A separate registry proxy at the site,** such as a Verdaccio instance the Ansible role deploys, as #543 plans for container registries. Reaching `pnpm/setup` would still need the interception. It would add a service to deploy, upgrade and secure. And its usual configuration serves an authenticated request from a cache shared by every job, which this record refuses.
- **An `HTTPS_PROXY` for every guest.** It routes all of a job's traffic through one relay, which stalled bulk transfers on 2026-10-04 (`scripts/build-guest-image.sh`).
- **Caching an authenticated request by its path.** The registry's answer does not say whether the package was private, so a private package's document or tarball would be served to another job.
