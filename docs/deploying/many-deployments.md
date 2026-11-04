# Many deployments on one host

Running many separate billet deployments side by side, one control plane each, costs little and needs a few things said out loud. Each deployment keeps its own ledger, identity and fleet: nothing here shares scheduling state between them.

## What one idle control plane costs

Very little. Fifty idle control planes of six tiers each, on one 8 vCPU host, measured on 2026-11-04 ([the record](../reference/records/idle-control-plane.md)):

| | On SQLite | On PostgreSQL |
|---|---|---|
| Memory per plane (proportional set size) | 17 MiB | 15 MiB |
| All fifty | 854 MiB | 739 MiB, and the server's own 1.1 GiB |
| CPU per plane | 0.05% of a core | 0.08% of a core |
| All fifty | 0.03 cores | 0.04 cores |
| PostgreSQL connections per plane | none | 4 |

One target's long polls share one connection, however many tiers it has (against the fake; real GitHub's count was not sampled). Billet's own part of a restart is small, under 0.1 s a plane, but **a restart waits out GitHub's message session, about 68 s on one real deployment, clean stop or not**, because a clean stop hands over rather than closing it. Queued jobs wait; none is lost. A connected node's cost is not in these numbers.

## Connections to PostgreSQL

Many control planes on one PostgreSQL server share its `max_connections`, so add them up. One process of a deployment holds at most the writer, the controller claim's session and its readers, and `server.state.postgres.max_connections` is that total:

```yaml
server:
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
      max_connections: 3
```

The readers get what the writer and the claim leave, up to four, so unset is six. Three is the least a control plane can run on, and a value under it is refused with the arithmetic. `billet check` states what the deployment can hold. A settled control plane holds four, the writer, the claim and two readers, and fifty on one server held 200 at their peak, or 150 at a budget of three, for about 2.4 MiB of the server's memory per connection saved. A reader nothing has borrowed for a minute closes, so a deployment with no reads at all holds fewer than its limit.

**Beyond that number:** a standby in an active-passive pair is a process of its own and holds its own, one session while it waits for the claim. An operator command run beside the server opens its own too, for as long as it runs, under the same limit, except the tail of `billet server retire`, which finds the ledger from its journal with no config to read a limit from, and reads one row at a time. A backup's `pg_dump` is one more.

**A transaction-mode pooler cannot carry these.** The claim is a session advisory lock, and the writer's `lock_timeout` and the readers' read-only default are session settings, so a pooler handing a server connection to whoever comes next silently drops the exclusion. Point the DSN at the server. billet has not been measured behind a session-mode pooler either: these settings travel as startup parameters, which a pooler may refuse or drop, so measure one before relying on it.

## Memory on a packed host

**Do not set `GOMEMLIMIT` to save memory.** At 24 MiB it is never reached and changes nothing. At 16 MiB it saves 3 MiB a plane for fourteen times the CPU, the collector running constantly against a heap that cannot shrink further. An idle plane's memory is what it needs. If you want a limit as a guard against one plane growing, 32 MiB or more leaves an idle plane alone.
