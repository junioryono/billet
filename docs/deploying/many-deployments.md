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

## Many deployments behind one address

A node dials its control plane by name, and that name is in its TLS ClientHello, in the clear. So a router that only reads the name and copies bytes can send each connection to its own deployment, with TLS still end to end: **the router never terminates TLS and holds no key.** The certificate a node verifies and the certificate it presents are its deployment's alone.

Each deployment listens on loopback, where only the router reaches it, and says what nodes dial:

```yaml
server:
  listen: 127.0.0.1:17001
  bootstrap_listen: 127.0.0.1:17002
  tls_router:
    node_addr: a.nodes.example.com:443
    enroll_addr: a.enroll.example.com:443
```

With `tls_router` a loopback wire serves mutual TLS, which a loopback wire otherwise does not, and the certificate it presents names those two addresses. They are DNS names because a client sends no SNI for an IP address, so the router could not route it; each deployment's two listeners have a name each, because the node wire demands a client certificate in the handshake and enrollment cannot. `billet ca token` prints these addresses for a joining machine. A node on the same host dials the router like any other, with a certificate: a `node` block in this file takes `node_addr` as its `server_addr`, and any other address is refused.

The router's rule is that **a connection with no SNI, or naming no deployment, is refused at the router, never sent to a default deployment.** A node of one deployment pointed at another's name reaches that deployment's handshake and is refused there, because the other's authority did not sign its certificate.

**The reference configuration is HAProxy,** at `deploy/router/haproxy.cfg` in the repository; billet's test suite runs that file through the whole scenario. Add a `use_backend` and a backend per listener per deployment:

```
# The reference router for many billet deployments behind one address: TLS
# passthrough by SNI, so a node's connection reaches its own deployment and TLS stays
# end to end. docs/deploying/many-deployments.md says how to use it.
#
# internal/e2e/router_haproxy_test.go runs THIS FILE, with the two deployments below
# at the ports its test gives them, so an edit here is an edit to what was tested.
#
# THIS ROUTER HOLDS NO KEY AND TERMINATES NOTHING. There is no `ssl` on any bind or
# server line and no certificate anywhere in this file. HAProxy reads the name from
# the ClientHello, in the clear, and copies bytes.
#
# ONE ROUTE PER LISTENER PER DEPLOYMENT, each its own name: the node wire demands a
# client certificate in the handshake and enrollment cannot, so a deployment's two
# listeners are two backends. Add both for each deployment you serve.
#
# METRICS HAVE NO ROUTE. A deployment's metrics endpoint stays on loopback, and is
# scraped there.

global
    log stderr format short daemon notice
    # What one host of nodes and enrolling machines may hold open at once. A node
    # holds a connection for as long as it runs.
    maxconn 20000

defaults
    mode tcp
    log global
    option tcplog
    timeout connect 5s
    # LONGER THAN THE NODE WIRE'S 120 SECONDS OF IDLE, which is what should end an
    # idle connection, so that a command poll is never cut by this hop.
    timeout client 5m
    timeout server 5m

frontend billet
    # The port your nodes dial. `server.tls_router` in each deployment's config states
    # the address nodes reach it by, port included.
    bind :443

    # WAIT FOR THE CLIENTHELLO AND REFUSE ANYTHING THAT IS NOT ONE. Without the wait
    # the name is not read yet when the routes below are judged, and every connection
    # would match none of them.
    tcp-request inspect-delay 5s
    tcp-request content accept if { req.ssl_hello_type 1 }
    tcp-request content reject

    # NO default_backend, AND THAT IS THE RULE: a connection with no SNI, or naming no
    # deployment, is refused here, never sent to somebody's control plane. A node that
    # dials this address by IP sends no SNI at all, so it lands there too.
    use_backend deployment-a-nodes if { req.ssl_sni -i a.nodes.billet.example }
    use_backend deployment-a-enroll if { req.ssl_sni -i a.enroll.billet.example }
    use_backend deployment-b-nodes if { req.ssl_sni -i b.nodes.billet.example }
    use_backend deployment-b-enroll if { req.ssl_sni -i b.enroll.billet.example }

# Each deployment's `server.listen` and `server.bootstrap_listen`, on loopback.
backend deployment-a-nodes
    server plane 127.0.0.1:17001

backend deployment-a-enroll
    server plane 127.0.0.1:17002

backend deployment-b-nodes
    server plane 127.0.0.1:17003

backend deployment-b-enroll
    server plane 127.0.0.1:17004
```

**The metrics endpoint stays on loopback,** with no route through the router: scrape it from the host.

**What a deployment sees of a routed connection is the router's address, and nothing reads it.** The wire's two connection budgets count connections rather than addresses, so nothing here needs the PROXY protocol, and no deployment speaks it. They are per deployment, so one deployment's traffic cannot use up another's. What they do not do is limit one source address; a limiter in front belongs on the router.
