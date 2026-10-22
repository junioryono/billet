# ADR-020: The node serves the public npm registry to its guests

Accepted, 2026-10-22. Issue #631. Widens the rule that interception terminates TLS for one host (`billet-security`, "Interception terminates TLS for one host and serves three methods") to a second host, `registry.npmjs.org`, under the conditions below.

## Context

On 2026-10-22 a consumer's jobs on a Firecracker tier failed twice at the same step, `pnpm/setup@v3`, with `Could not reach https://registry.npmjs.org/@pnpm/exe.linux-x64/12.4.2: timed out after 30s`. In the second run three sibling jobs made the same fetch within the same minute and succeeded. Across that repository's last 400 runs (1,739 jobs) these were the only two network failures. They are the failure class `docs/reference/records/shared-uplink.md` records at the reference site: a guest's connection out stalls intermittently. An eleven-minute capture there counted 0.7% of outbound TCP handshakes needing a SYN retransmit, and 0.5% of the bridge resolver's upstream queries going unanswered. That record's own conclusion is that fewer connections per job out of the site is the durable answer, and that billet has no package cache of its own.

What reaches the step that failed:

- **`pnpm/setup` fetches only from its `registry` input.** In its own `action.yml` (read 2026-10-22), "registry environment variables such as `pnpm_config_registry` do not change it". It also verifies what it downloads against npm's signature, so a mirror "has to proxy the npm registry rather than re-publish pnpm". A cache offered only through the job's environment (`npm_config_registry`, an `.npmrc`) would serve `npm ci` and `pnpm install` but not this download. Reaching it would need a `registry:` input in every workflow, against billet's rule that a workflow moved onto billet changes `runs-on` and nothing else.
- **GitHub's hosted runners fetch from the registry directly,** and GitHub publishes no registry cache for them.
- **The hosted-runner services that cache npm ask the workflow to change.** Blacksmith's documented answer (read 2026-10-22) is a sticky disk mounted at `~/.npm` or `node_modules` through its own action. RunsOn's pull-through caches are an opt-in label extra, and they cover container registries. billet already ships a sticky disk (`actions/stickydisk`), and it has the same limit: it serves `npm ci` from a warm `~/.npm`, but not `pnpm/setup`'s download, and only in a workflow that adds the step.

So only interception reaches every npm fetch without a workflow change. billet already intercepts one origin, the Actions results host:

- the guest remaps it in `/etc/hosts` for its own processes, and in a resolver on the Docker gateway for containers;
- a relay in the guest tunnels the connection to the node;
- the node terminates TLS with a CA the guest trusts (`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, the guest's system store);
- the node chooses, on the connection's first request, to answer it or to splice the connection to the real origin, and closes a connection it answered, so every request a client sends after an answer arrives on a new connection and is classified afresh.

## What was measured

`registry.npmjs.org`, 2026-10-22 21:33 UTC, with curl from the development Mac:

- **Package documents are fresh for five minutes, and npm's edge counts that time down.**
  - The full document (`/@pnpm/exe.linux-x64`) and the abbreviated one (`Accept: application/vnd.npm.install-v1+json`) both answer `cache-control: public, max-age=300`, with an `ETag`, a `Last-Modified` and `vary: accept-encoding, accept`.
  - Cloudflare served both from its cache (`cf-cache-status: HIT`), with `age` 63 and 72, so those answers had about 237 and 228 seconds of freshness left, not 300.
  - A request carrying the abbreviated document's `ETag` in `If-None-Match` is answered `304`.
- **A tarball is a year-long object.** `/@pnpm/exe.linux-x64/-/exe.linux-x64-12.4.2.tgz` answers `cache-control: public, must-revalidate, max-age=31557600`, 21,062,701 bytes, `age` 1,588,966 seconds (18 days at Cloudflare).
- **The measured version's tarball URL names the registry itself.** That version document's `dist.tarball` is `https://registry.npmjs.org/@pnpm/exe.linux-x64/-/exe.linux-x64-12.4.2.tgz`, and its `dist` carries `integrity`, `shasum`, `signatures` and `attestations`. A tarball a document names on another host is simply not intercepted.
- **A bogus credential changed nothing billet can see.** A public package fetched with a bogus `Authorization: Bearer` header answers `200`, the same as without one. A missing package answers `404`. A private package's answer was not measured.
- **npm's own client sends one of two `Accept` headers for a package document** (`npm/pacote`, `lib/registry.js`, read 2026-10-22): `application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*` for the abbreviated document, `application/json` for the full one.

## Decision

**A tier may have the node serve `registry.npmjs.org`** (`cache.npm`), through the interception the Actions cache uses:

- the guest's DNS remap of one more host, in `/etc/hosts` only (below);
- TLS terminated by the node's interception authority, with the host added to the node's leaf;
- the per-connection choice to answer or to splice, and the close after every answer.

The Actions cache and the npm cache are separate switches. A tier may have either without the other, and the guest installs the interception authority when either is on.

**What may be answered from the store is a closed set, decided from the first request alone.** A connection is answered by the node only when its first request meets all of these:

- **The method is `GET`, or `HEAD` of an entry the store already holds.** A `HEAD` of an entry the store lacks is spliced, so a `HEAD` never creates an entry.
- **No `Authorization`, `Cookie` or `npm-otp` header.** A job that configures a token for the registry sends it with its requests to it, and billet cannot tell from an answer whether the package behind it was private, so a request with a credential is never answered from the store and never fills it.
- **No query string.** npm asks for `?write=true` when it needs a document fresh enough to update, and nothing with a query is ever answered from the store.
- **The path is exactly one of three shapes, parsed once by one rule** that classification, the upstream fetch and the store's key all share:
  - a package document, `/<name>` or `/@<scope>%2f<name>` (either case of the `2f`);
  - a version document, `/<name>/<version>` or `/@<scope>/<name>/<version>`;
  - a tarball, `/<name>/-/<file>.tgz` or `/@<scope>/<name>/-/<file>.tgz`.

  Each name must be a valid npm package name and each version a valid semantic version. Any other percent-encoding, a dot segment, a repeated or trailing slash, or a double encoding is spliced, never normalised into something the store would answer.
- **For a document, the `Accept` header is one of a closed set**, and its value chooses one of two variants:
  - the abbreviated variant, for npm's exact abbreviated header or `application/vnd.npm.install-v1+json` alone;
  - the full variant, for `application/json`, `*/*` or no `Accept` at all.

  Any other `Accept` is spliced. A tarball ignores `Accept`.
- **The client accepts an unencoded answer.** The node serves every answer without an HTTP content coding (below), so a request whose `Accept-Encoding` refuses `identity` (`identity;q=0`, or `*;q=0` without `identity` named) is spliced.
- **No `Range` header.** A partial answer is never served from the store.

Everything else is spliced to the registry unchanged, for the whole connection: a publish, a login, an audit, a search, a dist-tag change, and every request the rules above do not admit. The node sees a spliced request in transit, as it sees the runner's own Actions token on the results host. It never stores it, never logs it, and forwards it unchanged.

**What the store may hold is decided from the registry's answer alone.**

- The node fills and revalidates an entry with its own `GET`, carrying none of the job's headers, whatever method the client used. A document is asked for with the variant's one canonical `Accept` (`application/vnd.npm.install-v1+json`, or `application/json`), so the variant is the only thing a job's request chooses.
- No answer the node serves carries an HTTP content coding. A document is asked for with `Accept-Encoding: gzip` and stored and served decoded. A tarball is asked for with `Accept-Encoding: identity` and stored as the registry's bytes, so its own gzip, which `integrity` covers, is what the client receives.
- An answer is stored only when all of these hold:
  - its status is `200`;
  - it is complete: its declared length, or the end of its chunked body, was reached;
  - it sets no cookie;
  - its `Cache-Control` names none of `private`, `no-store` or `no-cache`.
- A `304` refreshes only an entry the store already holds with the `ETag` that was sent. Its headers are merged into the entry's as RFC 9111 merges them, and the entry is held to the same conditions again: a `304` that sets a cookie or names `private`, `no-store` or `no-cache` removes the entry. The validator was the node's, not the client's, so that `304` is never relayed: the node asks again without a validator, under the disposition below, and relays that answer to this one client without storing it. No cookie is ever stored or replayed.
- A redirect is relayed to the client and never followed, so nothing in the store came from another origin.
- Any other status is relayed and not stored.

**One store per node, shared by every tier on it, whatever its trust.**

- Every entry in the store is what the public registry gives a request without a credential, fetched by the node itself over a verified TLS connection. Nothing a job sends decides what another job reads, beyond the name and the variant it asks for.
- An untrusted pool may have it for the same reason: the cache serves nothing scoped to a pool. The Actions cache is different, because it serves the pool's own entries, and it keeps its trust rules.
- A job can fill the store with real packages and evict others. That costs warm hits, not correctness, and the store's size bound limits it.
- The store lives on the node's own disk, as the Git mirrors do, not in a site volume: the uplink cost is then one fetch per node rather than one per job. Sharing one store across a site's nodes is not decided here.

**An entry is served only while it is fresh, and freshness is counted from the registry's own clock.**

- An entry's freshness lifetime is the answer's `s-maxage` if it has one (the store is a shared cache), else its `max-age`. Its current age is computed as RFC 9111 computes it, from the answer's `Age` and `Date` and the time since the node received it. The node sends the client an `Age` that includes the time the entry spent in the store.
- A tarball's freshness lifetime is also capped at 24 hours from when it was last fetched or revalidated: billet's own ceiling, far shorter than the registry's year.
- After that, an entry is revalidated with `If-None-Match`. A `304` makes it fresh again, under the merge above. A `200` replaces it. A `401`, `403`, `404` or `410` removes it and is relayed. A definitive answer is never replaced by a stored one.
- What the ceiling bounds is how long billet goes on serving a tarball after npm's edge stops serving it, not after npm removes it. A revalidation reaches the registry's edge, which may answer from its own cache. So a version npm removes (a takedown, an unpublish within npm's policy window) stops being served at most a day after the edge answers it gone, or a day and an hour when the registry gives no answer during that hour (below).
- An entry is evicted when the store passes its bound, least recently served first.

**Every fetch from the registry, a fill or a revalidation, has one disposition, in order.**

1. **An attempt.** One attempt has 8 seconds from the start of its connection to its response headers, counting DNS, the TCP connect and the TLS handshake. Once headers arrive, its body must keep moving: 10 seconds with no byte ends it. A body that keeps moving is bounded only by the node's existing per-request limit for its caches, 12 minutes 45 seconds.
2. **One retry, while nothing has reached the client.** An attempt that gave no answer is made once more, on a new connection. No answer means no connection, no headers in time, a `502`, `503` or `504`, or a `200` document's body that stalled before any of it was sent: a document is read whole before the client gets a byte, so its stall is still the node's to retry. Every other status is decided at its headers: a definitive one (a `401`, `403`, `404` or `410`) removes the entry and is relayed as it is, whatever its body then does, and no stale entry answers that request. An answer is held, not relayed, until the attempts are decided, so a retryable status is never sent to the client before the retry, and a partial fill is discarded.
3. **A stale entry, when both attempts gave no answer.** An entry that expired less than one hour ago is served, unless the registry's own lifetime for it has ended and its answer carried `must-revalidate`, `proxy-revalidate`, `no-cache` or `s-maxage` (which, in a shared cache, forbids the same reuse). billet's own ceiling on a tarball is not the registry's expiry, so a tarball still fresh by the registry's own lifetime may be served this way whatever its directives. Every stale answer is recorded and logged. It is a policy choice, not a property of the registry: it can carry a version that was removed, or lack a deprecation that was added, within that hour. It is bounded to the hour because the failure it answers is a stall of seconds.
4. **Otherwise the failure.** The registry's last `502`, `503` or `504` is relayed, or, when it gave no answer at all, the node answers `504`. The connection is then closed.
5. **A body that stalls after bytes reached the client** fails that one request: the node closes the connection mid-answer and discards the partial fill. A tarball's `200` begins as soon as its headers arrive (a `502`, `503` or `504` is still held for the retry), so a tarball's body is never retried. It cannot be, without sending the client the same bytes twice.

That budget is the node's alone. The guest's relay and the client's own connection to the node add to it, and whether a whole fetch fits inside the 30 seconds `pnpm/setup` waits is measured on a real guest before the cache is offered (below).

**The remap reaches only clients the CA reaches.** A client that does not trust the node's authority fails its TLS handshake before the node sees a request, so it cannot be spliced: that is an error, not a miss. So `registry.npmjs.org` is remapped in the guest's `/etc/hosts` only, never by the resolver containers use. That resolver is the guest's dnsmasq, which reads `/etc/hosts` unless told not to, so it runs with `--no-hosts` and keeps the Actions remap as its one explicit `--address`. Then:

- the runner and its steps on the guest reach the node, and trust it through `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE` and the guest's system store, as they do for the results host;
- a job container, a service container, a `docker run` container and a Dockerfile `RUN` stage resolve through the container resolver, are not remapped, and reach the registry directly, as they did before this cache. Job and action containers do receive the CA through the job-started hook, but a `docker run` container or a build stage does not, and one resolver serves them all.

A step that runs `sudo` loses the runner's environment unless the guest's `sudo` keeps it. A container on the host's network sees the guest's `/etc/hosts` without the guest's trust. Both are measured on a real guest before the cache is offered, and the image keeps what they need or the ADR is revisited.

**It is off by default for its first release.** ADR-013's rule is that every cache is on by default where a tier can have it. This one is the exception until it has served a real fleet, because a client the CA does not reach fails rather than missing, and a mistake in it breaks every npm install on the tier, where a mistake in the other caches costs a cold start. Turning it on by default is a later decision with its own evidence: a real fleet's jobs served through it, and the measurements this record defers.

**A guest remaps the host only when its node names it.** The guest image is published and outlives node releases. A guest that remapped `registry.npmjs.org` on its own, under a node too old to serve it, would send every npm fetch to a proxy that refuses it. So the guest remaps the host only when its launch metadata names it, as the Actions remap depends on the node naming its proxy. A node that does not name it leaves npm going direct. An older node reads the tier's `cache.npm` as absent, which is safe: doing less leaves the job on the registry.

**What the cache did is observed like the other caches.** For each job, the node records one outcome from the closed vocabulary the build caches share: warm, cold, disabled, unavailable or unused. `billet jobs show` prints it. The node also logs one line for every request: where it was answered, why it was not answered from the store, its status, its bytes and its time. Every stale answer and every stalled attempt is counted. Which counts travel with a job's report belongs to #631's evidence item, beside the resolver and the guest's other connections, and is not decided here.

## What is not decided here

- **The guest under a real install.** The Actions interception's guest relay replaced an `HTTPS_PROXY` because bulk transfers through a catch-all relay stalled (2026-10-04). An `npm ci` of a large lockfile is thousands of requests and hundreds of megabytes through the same relay. The implementation measures one on a real guest, together with `pnpm/setup` against the 30-second budget, `sudo`, a host-network container, and a container resolving the guest's own host name once the resolver stops reading `/etc/hosts`, before the cache is offered. If the relay is the limit, the relay changes, not the rule.
- **Other registries.** PyPI, the Go module proxy and Ubuntu's archive fit the same shape, a public anonymous read with long-lived artifacts, and each would be its own host under this rule.
- **A store shared by a site's nodes,** in Ceph as the other caches are.

## Rejected

- **The job's environment only** (`npm_config_registry`, `pnpm_config_registry`, a written `.npmrc`). It serves `npm ci` and `pnpm install` without interception, but not `pnpm/setup`, the download that failed, and not a workflow that writes its own registry setting.
- **A sticky disk at `~/.npm`,** the hosted services' answer. It needs a workflow change and does not reach `pnpm/setup`.
- **A separate registry proxy at the site,** such as a Verdaccio instance the Ansible role deploys, as #543 plans for container registries. Reaching `pnpm/setup` would still need the interception. It would add a service to deploy, upgrade and secure. And its usual configuration serves an authenticated request from a cache shared by every job, which this record refuses.
- **An `HTTPS_PROXY` for every guest.** It routes all of a job's traffic through one relay, which stalled bulk transfers on 2026-10-04 (`scripts/build-guest-image.sh`).
- **Remapping the host for containers too.** Neither a Dockerfile `RUN` stage nor an arbitrary container trusts the node's authority, so each would fail its TLS handshake instead of reaching the registry.
- **Caching an authenticated request by its path.** billet cannot tell from an answer whether a package was private, so a private package's document or tarball could be served to another job.
- **Keeping a tarball for the registry's year.** A version npm removes would go on being served from the store long after the registry stopped serving it.
- **Relaying a `503` at once.** A retryable answer sent to the client could no longer be replaced by the retry or a stale entry, which is what the retry is for.
