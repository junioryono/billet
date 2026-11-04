# Many deployments on one host

Running many separate billet deployments side by side, one control plane each, costs little and needs a few things said out loud. Each deployment keeps its own ledger, identity and fleet: nothing here shares scheduling state between them.

## What one idle control plane costs

MEASUREMENT_PLACEHOLDER

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

The readers get what the writer and the claim leave, up to four, so unset is six. Three is the least a control plane can run on, and a value under it is refused with the arithmetic. `billet check` states what the deployment can hold. A reader nothing has borrowed for a minute closes, so a quiet deployment holds fewer than its limit.

**Beyond that number:** a standby in an active-passive pair is a process of its own and holds its own, one session while it waits for the claim. An operator command run beside the server opens its own too, for as long as it runs, under the same limit, except the tail of `billet server retire`, which finds the ledger from its journal with no config to read a limit from, and reads one row at a time. A backup's `pg_dump` is one more.

**A transaction-mode pooler cannot carry these.** The claim is a session advisory lock, and the writer's `lock_timeout` and the readers' read-only default are session settings, so a pooler handing a server connection to whoever comes next silently drops the exclusion. Point the DSN at the server. billet has not been measured behind a session-mode pooler either: these settings travel as startup parameters, which a pooler may refuse or drop, so measure one before relying on it.

## Memory on a packed host

MEMORY_PLACEHOLDER
