# Metrics

Each role can serve Prometheus metrics: the control plane from `server.metrics`, a node from `node.metrics`. Neither does unless its block is in `billet.yaml`. There is no default address, and a configuration without the block opens no port.

```yaml
server:
  # ...
  metrics:
    listen: 127.0.0.1:9180

node:
  # ...
  metrics:
    listen: 127.0.0.1:9181
```

The endpoint serves `/metrics` in the Prometheus text format. It binds when the process starts, before a standby waits for the controller claim, so a standby can be scraped too. A port that is already taken stops the process at startup with an error naming the address. An upgrade probe never serves metrics, because the service it runs beside holds the port.

## What is served

| Metric | What it is |
|---|---|
| `billet_build_info{version,revision,role}` | always 1; the running binary's version and revision, and whether this process is the `server` or the `node` |
| `go_*` | the Go runtime: goroutines, heap, garbage collection |
| `process_*` | the process: CPU seconds, resident memory, open file descriptors (Linux) |

## Reaching it from another host

The endpoint has no authentication, and what it reports is the deployment's own business. So `listen` must be a literal loopback address, `127.0.0.1` or `[::1]`, unless `allow_remote: true` is set beside it. A name such as `localhost` is not accepted on its own, because the bind would resolve it again and a resolver could map it to another interface:

```yaml
server:
  metrics:
    listen: 10.0.0.4:9180
    allow_remote: true
```

Set it only on a network where whoever can reach that port may read the fleet's state. A metrics address may not share a socket with any other listener in the file: `server.listen`, `server.bootstrap_listen`, `node.cache.listen` or the other role's metrics endpoint. A wildcard counts as every address on its port, and the comparison is of the socket rather than the spelling: `[::ffff:127.0.0.1]:7717` is `127.0.0.1:7717`, and port `07717` is `7717`.

## The profiler

`pprof: true` also serves Go's profiler under `/debug/pprof/` on the same endpoint:

```yaml
node:
  metrics:
    listen: 127.0.0.1:9181
    pprof: true
```

```bash
go tool pprof http://127.0.0.1:9181/debug/pprof/heap
curl -o trace.out 'http://127.0.0.1:9181/debug/pprof/trace?seconds=5'
```

A heap or goroutine profile carries whatever memory held when it was taken, a credential included. So `pprof` is refused on any `listen` that is not a literal loopback address, with or without `allow_remote`. Leave it off unless you are diagnosing something.

## A scrape configuration

```yaml
scrape_configs:
  - job_name: billet
    static_configs:
      - targets: ['127.0.0.1:9180']
        labels: {role: server}
      - targets: ['127.0.0.1:9181']
        labels: {role: node}
```
