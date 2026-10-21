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

The control plane also reads its ledger at every scrape, through the same reports `billet status` prints. No lease or job id is ever a label.

| Metric | What it is |
|---|---|
| `billet_tier_leases{tier,state}` | open leases by what they are doing: `discovery` and `pending` (capacity bought, no runner yet), `launching`, `idle` and `running` runners, `cleanup`, or `unknown` (a lease whose state could not be established: capacity the listener's report does not classify, or a runner whose record names another tier or a status billet does not know) |
| `billet_tier_floor{tier}` | the tier's configured floor |
| `billet_tier_headroom{tier}` | how many more of the tier's runners the allocator would grant right now |
| `billet_tier_advertised{tier}` | the capacity the tier's listener last told GitHub, from its last completed exchange; absent until it has completed one |
| `billet_tier_waiting{tier}` | how much of GitHub's assigned work the tier could not buy capacity for, as its listener last reported; absent until the listener has published a report |
| `billet_tier_waiting_seconds{tier}` | how long the tier has been waiting, while it is |
| `billet_tier_report_age_seconds{tier}` | how old the listener's last published report is; a listener that has stopped leaves a report that only ages. A report from a clock ahead of this one is age zero |
| `billet_nodes{state}` | registered hosts: `live`, `offline` or `decommissioned` |
| `billet_scrape_up{source}` | 1 when the source was read for this scrape: `ledger` for the gauges above, `github` for the REST budgets below, which are held in memory and always read; 0 when it could not be, in which case the gauges above are left out rather than reported as zero. A read is bounded at five seconds, and while one that overran is still running no other starts; a listener report that cannot be decoded fails the read too |
| `billet_ledger_write_wait_seconds` | how long each write waited for the ledger's single writer slot (a histogram); a rising tail is scheduling queueing behind other writers |
| `billet_ledger_write_held_seconds{outcome}` | how long each write held the slot, `committed` or `rolled_back` (a histogram) |
| `billet_ledger_write_busy_retries_total` | each time a write found the slot taken and tried again |
| `billet_job_queue_wait_seconds{tier}` | how long each job waited, from GitHub's queue time to billet recording its start (a histogram, buckets from 5 seconds to 6 hours), observed once a job when the ledger first holds both. The queue time is GitHub's clock and the start billet's, so a wait is only as exact as the two agree. A job with no GitHub queue time is not observed. The histogram counts what this process saw since it started; `billet jobs wait` reads the same waits from the ledger and splits them into their parts |
| `billet_job_queue_wait_negative_total{tier}` | jobs whose start on billet's clock came before their queue time on GitHub's: the clocks disagreed by more than the wait. Counted here and kept out of the histogram, where it would read as no wait |
| `billet_github_rest_limit{installation}` | an App installation's hourly REST budget, as GitHub stated it in its last answer to a request billet made with the installation's token; `installation` is the configuration's `installation_id`. A 401 states nothing about the installation (on 2026-10-20 GitHub's 401 carried no budget at all). Nor does an answer whose reset lies more than an hour and five minutes after billet read it, longer than GitHub's window. Absent until such a request has been answered, so a deployment whose control plane has not yet checked a runner group, read a run or read a queue time has none. Targets that share an installation report it once, and answers that arrive out of order are put back in order: the latest hour's, and within it the lowest remaining |
| `billet_github_rest_remaining{installation}` | what was left of that budget at that answer. Every client of the installation draws on it, not only the control plane: among billet's, the runner-group checks, the offline-runner inspection, the cache authority's run and branch reads, the scale-set client's registration-token requests, commands such as `billet jobs show` and `billet check`, and the queue-time sweep, which starts no run's read through an installation with less than a fifth of its limit left until the hour resets (a read already started finishes: at most eight pages, each with any redirect GitHub answers it with) |
| `billet_github_rest_used{installation}` | how much of it had been used at that answer |
| `billet_github_rest_reset_timestamp_seconds{installation}` | when GitHub starts the installation's next hour, in Unix seconds |
| `billet_github_rest_stated_timestamp_seconds{installation}` | when billet read the answer the four above come from, in Unix seconds: they are as old as this, and an installation billet has not called since its hour reset still shows the old hour's numbers |
| `billet_github_rest_requests_total{installation,operation,answer}` | each request billet made with the installation's token, by what it was for (the client's fixed operation names, such as `list the run's jobs` or `get runner-group policy`) and GitHub's answer: `2xx`, `3xx`, `4xx`, `5xx`, or `none` when no answer came (an answer whose body broke off or exceeded billet's bound is counted by its status, and each redirect a request follows is an answer of its own). The token exchange is answered from the App's own budget and is not counted. Nor is anything the scale-set client makes, its sessions, long polls and runner calls among them; what of that reaches GitHub's REST API still draws on the budget `billet_github_rest_remaining` reports |

A standby serves the ledger's gauges too: it reads the same ledger, and they show what the active controller has recorded. It makes no GitHub request, so it has none of the `billet_github_rest_*` series, and an alert on one that is absent fires on every standby.

A node with `node.monitoring` also counts how its job sampler keeps up, as the sampler does it. Without `node.monitoring` they stay at zero. No lease or job id is a label here either; a job's own gaps are in its series, which `billet jobs show` lists.

| Metric | What it is |
|---|---|
| `billet_node_usage_tick_seconds` | how long each tick of the sampler took to read every job it measures (a histogram); a tick near the sampling interval is one about to miss the next |
| `billet_node_usage_gaps_total{cause}` | intervals of a job's series the sampler did not see, by cause: `missed_tick`, `slow_read`, `failed_read`, `late_first_read`, `counter_fell`, `handover` and the rest the [configuration reference](../reference/configuration.md) lists, each reported from the start at zero. Counted as the sampler keeps the sample that ends one (a counter that fell and the catch-up after it among them) or marks earlier ones; a job's last interval, which its final read decides, is not counted |
| `billet_node_usage_io_bounded_total` | reads of a job's disk written that writeback kept moving on every pass, recorded as a lower bound that the next read corrects rather than as a gap |

A node with `node.ceph` also reads its kernel's RBD mappings at every scrape, with the same could-not-tell rule (`billet_scrape_up{source="rbd"}`). A node without `node.ceph` reports none.

| Metric | What it is |
|---|---|
| `billet_node_rbd_mappings{state}` | this host's kernel mappings of images in the site's pools, by what accounts for each: `root_disk` (a guest's root disk, which its microVM holds), `generation` (a read-only check of a cache generation), `session` (a cache volume a cache session on this node names), `recent` (a cache volume named less than six hours ago, whose custody record may not be written yet), `unaccounted` (a cache volume named longer ago that no cache session on this node names, held by no job of this node's; on a host that runs a second node identity, that one's volumes count here too), and `other` (anything else in those pools, such as a map an interrupted `billet images pull` left). The node's eviction pass, at start and every six hours, unmaps without force each `unaccounted` mapping that an earlier pass in the same process also found unaccounted, at least an hour before, and whose volume records this node as the one that mapped it, unless the kernel holds it open; a count that stays above zero is a mapping something on the host still has open, named in the node's `kept kernel mappings` warning |

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

## The flight recorder

A stall is usually over by the time anyone looks, and what was holding it is gone with it. With `flight_recorder: true` the controller keeps the last two minutes of its Go execution trace in memory and writes them to a file when one of two things has already gone wrong:

```yaml
server:
  # ...
  flight_recorder: true
```

| Reason | When |
|---|---|
| `heartbeat-overrun` | a tier's heartbeat pass, which renews every lease the tier holds, is still running when the next pass is due. The file is written while the pass is stuck, so it shows what the pass is waiting for |
| `leadership-lost` | the ledger refused a write because another controller took the deployment; written before the process stops |

A file lands in `flight-recorder/` under the identity directory, named for the time and the reason (`flight-20261008T150405.123Z-heartbeat-overrun.trace`), readable by the service account alone. Each reason is written at most once every ten minutes, so a stall that repeats every pass leaves one file rather than one a pass, and the four newest files are kept. The window aims at 32 MiB, which the runtime treats as a target rather than a limit and may meet by keeping less than two minutes, so allow for four files of about that size, and one more being written. Only the controller records; a standby starts recording once it takes the claim.

```bash
go tool trace /var/lib/billet/flight-recorder/flight-20261008T150405.123Z-heartbeat-overrun.trace
```

A trace holds what each goroutine did and where, with its stacks, and nothing of the process's memory. The runtime's tracer costs CPU for as long as it records, and the window holds about that much memory, which is why it is off unless asked for. A snapshot still being written when the controller stops is waited for ten seconds and then left unfinished, so a slow disk never holds the claim from a successor.

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
