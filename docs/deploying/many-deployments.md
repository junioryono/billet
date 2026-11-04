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
billet broker client-key --config /etc/billet/billet.yaml --new-deployment
```

The key's directory has to exist already, where the service account can enter it. The command prints the deployment, the key's fingerprint and the stanza to add to the broker's file. Compare them on a channel you trust, as you would an enrollment: the registry is the trust decision. `--new-deployment` says this host is starting one, and mints its identity: without it the command refuses a host that holds none, so a replacement host cannot mint an identity that stops the restore it is about to run. An active-passive pair runs this on each host and registers both, and a restored host runs it after `billet local restore`, with no flag, because this key belongs to the host and travels in no archive. The Ansible collection neither installs nor asks for this key: run the command once on each host and register what it prints.

**What a deployment may ask for is exactly what its stanza names**, and a token reaches what that deployment needs and nothing else: billet's permissions for the target, and for a repository target that repository alone. An installation registered as a whole organization belongs to one deployment, because its token reaches every repository the installation does.

**`billet check` says which it is using,** and what a check without the key cannot prove: whether the App is installed and not suspended is the broker's to check, which it does before it mints.

**A broker that is down costs little, while the control plane stays up.** A deployment's long polls carry the Actions service's own token and do not ask, and a held token rides out silence until its last minute, so an outage shows only when a refresh falls due, and reads as could-not-tell rather than as the installation being gone. Held tokens live in memory, though: **a control plane that restarts during an outage cannot provision its scale sets, and does not start until the broker answers.** Do not upgrade the two together.

**The broker has one budget for everyone it serves.** A deployment holds only what it is registered for, and a failure is answered from memory for 30 s rather than spending the App's budget at GitHub again, but connections are not limited per deployment: a compromised control plane can use up the broker's 4096. Limit the port in front if your deployments do not trust one another's availability.

## Many deployments behind one address

A node dials its control plane by name, and that name is in its TLS ClientHello, in the clear. So a router that only reads the name and copies bytes can send each connection to its own deployment, with TLS still end to end: **the router never terminates TLS and holds no key.** The certificate a node verifies and the certificate it presents are its deployment's alone.

Each deployment listens on loopback, where only the router reaches it, and says what nodes dial:

```yaml
server:
  listen: 127.0.0.1:17001
  bootstrap_listen: 127.0.0.1:17002
  tls_router:
    node_addr: a.nodes.billet.example:443
    enroll_addr: a.enroll.billet.example:443
```

With `tls_router` a loopback wire serves mutual TLS, which a loopback wire otherwise does not, and the certificate it presents names those two addresses. They are DNS names because a client sends no SNI for an IP address, so the router could not route it; each deployment's two listeners have a name each, because the node wire demands a client certificate in the handshake and enrollment cannot. `billet ca token` prints these addresses for a joining machine. A node on the same host dials the router like any other, with a certificate: a `node` block in this file takes `node_addr` as its `server_addr`, and any other address is refused.

The router's rule is that **a connection with no SNI, or naming no deployment, is refused at the router, never sent to a default deployment.** A node of one deployment pointed at another's name reaches that deployment's handshake and is refused there, because the other's authority did not sign its certificate.

**The reference configuration is HAProxy,** at `deploy/router/haproxy.cfg` in the repository; billet's test suite runs that file through the whole scenario. Add a `use_backend` and a backend per listener per deployment:

```{literalinclude} ../../deploy/router/haproxy.cfg
:language: text
```

**The metrics endpoint stays on loopback,** with no route through the router: scrape it from the host.

**What a deployment sees of a routed connection is the router's address, and nothing reads it.** The wire's two connection budgets count connections rather than addresses, so nothing here needs the PROXY protocol, and no deployment speaks it. They are per deployment, so one deployment's traffic cannot use up another's on its own listeners. **The router is shared, though:** every deployment behind it has the one `maxconn`, and the reference file limits one source address to 2000 connections so that a stranger's silent connections cannot hold them all. Raise that past the most nodes you have behind one NAT.

**A node dialling the router by IP address gets no explanation.** It sends no SNI, so the router refuses it, and on the node that reads as a connection failing and retrying without end. Set `server_addr` to the name in `node_addr`.

**An active-passive pair needs a router on each controller,** or a router that can reach both, because the listeners stay on loopback. `billet server retire` asks for `--endpoint-failover-verified` here, because a routed node endpoint is always a DNS name.
