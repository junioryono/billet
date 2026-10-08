# ADR-016: Prometheus metrics, off by default, behind one package

Accepted, 2026-10-08. Issue #356 (Phase 6), partly addressing issue #28.

## Context

billet had no observability surface: no metrics endpoint, no profiler, no expvar. `monitoring` in `billet.yaml` means something else, the per-job usage a node writes to the ledger. What an operator could learn about a running control plane came from `billet status`, which reads the ledger once, and from logs. A stall, a slow ledger or a heartbeat pass that overran left nothing a scraper could have recorded on the way.

## Decision

**Prometheus's Go client, `github.com/prometheus/client_golang`,** because the exposition format is what every scraper and dashboard reads, and the client also gives the Go runtime and process collectors, which are most of what a first look at a stalled process needs.

**Confined to one package.** `internal/metrics` is the only package that imports the client, and the only one that imports `net/http/pprof`, whose import registers handlers on `http.DefaultServeMux`; the depguard rule `metrics` holds both. Every other package reports through what `internal/metrics` declares, so the ledger writers (`state`, `alloc`, `rollout`) never import a metrics library, and a gauge read at scrape time goes through the reader pool, never the writer slot.

**Off by default, per role.** `server.metrics` and `node.metrics` each name a `listen` address, and a configuration without the block opens no port. The endpoint is unauthenticated, so `listen` must be loopback unless `allow_remote: true`. The profiler is a separate switch, `pprof`, refused beside `allow_remote`, because a heap or goroutine profile carries whatever memory held when it was taken, a credential included. A metrics address may share a socket with no other listener in the file, and it is never one of the node wire's listeners. The endpoint binds at process start, before a standby waits for the claim, and is not started by an upgrade probe, which runs beside the service holding the port.

## Measured

The stripped binary (`-trimpath -ldflags "-s -w"`, `CGO_ENABLED=0`) at `f802fd57`, before and after the client, the package, the configuration and the wiring:

| Platform | Before | After | Cost |
|---|---|---|---|
| darwin/arm64 | 37,593,698 B | 38,647,490 B | +1,053,792 B (+2.8%) |
| linux/amd64 | 39,288,994 B | 40,435,874 B | +1,146,880 B (+2.9%) |

Smaller than its module list suggests: `google.golang.org/protobuf` and `github.com/cespare/xxhash/v2`, two of the client's heavier dependencies, were already in the binary (protobuf for the Remote Execution API cache the node serves). The modules the binary links that it did not before, read from `go version -m`: `prometheus/client_golang`, `client_model`, `common` and `procfs`, `beorn7/perks` and `munnerz/goautoneg`.

## Rejected

**Writing the exposition format by hand.** The text format is simple to emit, and a hand-written emitter would cost almost nothing. But it would also re-implement the runtime and process collectors, histograms and label escaping, all of which are where the client's value is, and a second implementation of a format other tools parse is one that drifts.

**Serving metrics on the node wire's listener.** It would need no new port, but the wire's listener demands a client certificate in the handshake, and the bootstrap listener is the anonymous one whose connection budget is kept away from everything else. A scraper is neither a node nor an enrolling machine.

**On by default.** An endpoint nobody asked for is a port nobody audited.
