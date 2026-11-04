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

**Beyond that number:** a standby in an active-passive pair is a process of its own and holds its own. An operator command run beside the server opens its own too, for as long as it runs, under the same limit; so does a backup, whose `pg_dump` is one more.

**A transaction-mode pooler cannot carry these.** The claim is a session advisory lock, and the writer's `lock_timeout` and the readers' read-only default are session settings, so a pooler handing a server connection to whoever comes next silently drops the exclusion. Point the DSN at the server, or at a session-mode pool.

## Memory on a packed host

MEMORY_PLACEHOLDER

## One App across deployments: the token broker

An operator running many deployments under one GitHub App would otherwise put its one key on every control plane, and a compromise of any of them could mint tokens for every installation of that App. A broker holds the key instead, and a deployment on one holds none. [ADR-023](../reference/decisions/adr-023-installation-tokens-from-a-broker.md) says why it is shaped this way.

**On the machine holding the keys,** write the broker's file ([Configuration](../reference/configuration.md#the-token-brokers-file)) and run it. It reads the keys under the rules a `private_key_path` always had, and needs nothing else of billet's:

```bash
billet broker serve --config /etc/billet-broker/broker.yaml
```

**On each control-plane host,** add the block, name no key on any target, and make the key this host proves its deployment with:

```yaml
server:
  token_broker:
    url: https://broker.internal:8443
    ca_file: /etc/billet/broker-ca.pem
    client_key_path: /etc/billet/broker-client.key
```

```bash
billet broker client-key --config /etc/billet/billet.yaml
```

It prints the deployment, the key's fingerprint and the stanza to add to the broker's file. Compare them on a channel you trust, as you would an enrollment: the registry is the trust decision. An active-passive pair runs this on each host and registers both, and a restored host makes and registers its own, because this key belongs to the host and travels in no archive.

**What a deployment may ask for is exactly what its stanza names**, and a token reaches what that deployment needs and nothing else: billet's permissions for the target, and for a repository target that repository alone. An installation registered as a whole organization belongs to one deployment, because its token reaches every repository the installation does.

**`billet check` says which it is using,** and what a check without the key cannot prove: whether the App is installed and not suspended is the broker's to check, which it does before it mints.

**A broker that is down costs little.** A deployment's long polls carry the Actions service's own token and do not ask, and a held token rides out silence until its last minute, so an outage shows only when a refresh falls due, and reads as could-not-tell rather than as the installation being gone.

