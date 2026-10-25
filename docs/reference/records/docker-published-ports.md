# Docker's published ports, with and without the userland proxy

A job reaches a service container on `localhost:<port>`, the address GitHub's own documentation gives, and Docker carries that connection through its userland proxy. #163 measured that path at about half the speed of reaching the container by its own address, on the reference node's Firecracker guests (2026-09-21). This record is the trial #163 asked for before anyone changes it, run on 2026-10-25. Turning the proxy off made the published path nearly as fast as the direct one, and broke `localhost` for Node 18 and `[::1]` for every client. billet's guest images keep the proxy.

## How it was measured

- **The guest:** a billet EC2 guest on an untrusted tier, `c7i.xlarge` (4 vCPU), booting `ami-07ba26bf4a1783003`, which `billet ami build` made that day from the same pinned declaration as the Firecracker guest image. Linux 7.0.0-1014-aws, Docker 29.1.3, `daemon.json` `{"features":{"containerd-snapshotter":false},"storage-driver":"overlay2"}`.
- **The job:** one workflow job, run `40214476753` in the test organization's repository. It measured everything once with Docker's default (the proxy on), then set `"userland-proxy": false`, restarted Docker and measured everything again.
- **How it reached Docker's configuration:** this guest's runner account has no sudo, but it is in the `docker` group. So the job rewrote `daemon.json` from a privileged container in the host's namespaces, and handed the restart to systemd.
- **The services:** a `postgres:16` container on a user-defined network with `-p 5432:5432`, the way the runner publishes a service, and an `nginx:1.27-alpine` with `-p 8080:80`.
- **The clients:** run in containers on the host's network, so they reach the published ports exactly as a step on the runner does.

## Throughput

pgbench at scale 32, 16 clients, 4 threads, 30 seconds a run, two runs each. The table gives the mean in transactions per second.

| Path | tpcb, proxy on | tpcb, proxy off | select-only, proxy on | select-only, proxy off |
|---|---|---|---|---|
| unix socket, inside the container | 6,092 | 6,173 | 112,938 | 114,351 |
| the container's own address | 5,614 | 5,622 | 87,990 | 89,354 |
| the published port on `127.0.0.1` | 4,858 | 5,561 | 55,525 | 84,768 |

With the proxy, the published port ran at 87% of the container address on tpcb and 63% on select-only. Without it, it ran at 99% and 95%. The gain on select-only, where each transaction is one round trip, was 53%. This guest's ratio is milder than #163's Firecracker guest, where the published port ran at half the direct path's tpcb. The direction is the same.

## What stopped working

The same services, reached by every name a job uses, under each setting. "ok" is a connection or an HTTP 200.

| Client | `localhost`, proxy on | `127.0.0.1`, proxy on | `[::1]`, proxy on | `localhost`, proxy off | `127.0.0.1`, proxy off | `[::1]`, proxy off |
|---|---|---|---|---|---|---|
| curl | ok | ok | ok | ok | ok | failed |
| psql | ok | ok | ok | ok | ok | failed |
| Python `socket.create_connection`, `urllib` | ok (via `::1`) | ok | ok | ok (via `127.0.0.1`) | ok | refused (`ConnectionRefusedError`); `urllib` failed |
| Go `net.Dial` | ok (via `[::1]`) | ok | ok | ok (via `127.0.0.1`) | ok | failed |
| Node 18 `net.connect`, `fetch` | ok (via `::1`) | ok | ok | **refused** (`ECONNREFUSED`) | ok | refused (`ECONNREFUSED`) |
| Node 20 and 22 `net.connect`, `fetch` | ok (via `::1`) | ok | ok | ok (via `127.0.0.1`) | ok | refused (`ECONNREFUSED`) |

"Failed" is a client that reported only that it could not connect; "refused" is one that said the connection was refused.

Three separate facts explain the matrix:
- **Resolution:** `localhost` resolved to `::1` first for every client that reported its peer: with the proxy, Python, Go and Node all connected through `::1`.
- **Who answers on `::1`:** with the proxy, Docker also listens on the IPv6 loopback and forwards those connections to the container's IPv4 address. Without it, this guest's bridge is IPv4 only, and the port is published by an IPv4 rule alone, so nothing answers on `::1`. A bridge with IPv6 enabled would publish differently, and was not tested.
- **What a client does next:** when `::1` refused, every client tested tried `127.0.0.1` next, except Node 18 in its default configuration, which tried `::1` alone and failed. Node 18 can be told to try both families; a workflow that never asked for that cannot be assumed to have.

## The decision

**Not adopted.** On this guest, turning the proxy off broke a workflow that reaches its service container on `localhost` from Node 18, or on `[::1]` from anything, and that is a regression measured on billet. It is also very likely a difference from GitHub's hosted runners. The runner-images template billet vendors writes no `daemon.json` (`images/ubuntu/scripts/build/install-docker.sh` at `e7563390`, the commit in `scripts/runner-images/COMMIT`), so its Docker runs with the default, the proxy on. That was read from the template, not measured on a hosted runner. The published path's speed is a real cost: up to a third of a round-trip-bound workload on this guest. But a broken service connection is not a slower one. A workflow that wants the fast path can reach its service by the container's address, or run its client in the service's network, without billet changing what `localhost` means for everyone else.

## The workflow, exactly as it ran

```yaml
name: ports
on:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  ports:
    runs-on: b1t12-ec2-untrusted
    timeout-minutes: 60
    steps:
      - name: The guest and its Docker
        run: |
          echo "PORTS kernel $(uname -srm)"
          echo "PORTS cpus $(nproc)"
          echo "PORTS docker $(docker version --format '{{.Server.Version}}')"
          echo "PORTS daemon_json $(cat /etc/docker/daemon.json | tr -d '\n ')"
          echo "PORTS hosts_localhost $(grep -E '^(127\.0\.0\.1|::1)[[:space:]]' /etc/hosts | tr '\n' ';')"
          for image in postgres:16 nginx:1.27-alpine node:18-alpine node:20-alpine node:22-alpine python:3.12-alpine golang:1.23-alpine; do
            docker pull -q "$image" >/dev/null
          done

      - name: Measure under both settings
        run: |
          set -euo pipefail
          cat >/tmp/client.go <<'EOF'
          package main

          import (
          	"fmt"
          	"net"
          	"os"
          	"time"
          )

          func main() {
          	for _, addr := range os.Args[1:] {
          		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
          		if err != nil {
          			fmt.Printf("%s refused-or-failed\n", addr)
          			continue
          		}
          		fmt.Printf("%s ok via %s\n", addr, c.RemoteAddr())
          		c.Close()
          	}
          }
          EOF
          cat >/tmp/client.js <<'EOF'
          const net = require("net");
          const targets = [["localhost", 8080], ["127.0.0.1", 8080], ["::1", 8080]];
          (async () => {
            for (const [host, port] of targets) {
              const r = await new Promise((resolve) => {
                const s = net.connect({ host, port, timeout: 5000 });
                s.on("connect", () => { resolve(`ok via ${s.remoteAddress}`); s.destroy(); });
                s.on("error", (e) => resolve(`failed ${e.code}`));
                s.on("timeout", () => { resolve("timeout"); s.destroy(); });
              });
              console.log(`net.connect ${host} ${r}`);
            }
            for (const url of ["http://localhost:8080/", "http://127.0.0.1:8080/", "http://[::1]:8080/"]) {
              try { const res = await fetch(url); console.log(`fetch ${url} ${res.status}`); }
              catch (e) { console.log(`fetch ${url} failed ${e.cause ? e.cause.code : e.message}`); }
            }
          })();
          EOF
          cat >/tmp/client.py <<'EOF'
          import socket, urllib.request
          for host in ("localhost", "127.0.0.1", "::1"):
              try:
                  s = socket.create_connection((host, 8080), timeout=5)
                  print(f"create_connection {host} ok via {s.getpeername()[0]}")
                  s.close()
              except OSError as e:
                  print(f"create_connection {host} failed {e.__class__.__name__}")
          for url in ("http://localhost:8080/", "http://127.0.0.1:8080/", "http://[::1]:8080/"):
              try:
                  print(f"urllib {url} {urllib.request.urlopen(url, timeout=5).status}")
              except Exception as e:
                  print(f"urllib {url} failed {e.__class__.__name__}")
          EOF

          measure() {
            setting=$1
            echo "PORTS $setting docker_proxy_processes $(pgrep -c docker-proxy || true)"
            docker network create bench >/dev/null
            docker run -d --name pg --network bench -p 5432:5432 -e POSTGRES_HOST_AUTH_METHOD=trust \
              postgres:16 -c shared_buffers=512MB -c max_connections=200 >/dev/null
            docker run -d --name web --network bench -p 8080:80 nginx:1.27-alpine >/dev/null
            until docker exec pg pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1; do sleep 1; done
            sleep 3
            echo "PORTS $setting docker_proxy_processes_with_ports $(pgrep -c docker-proxy || true)"

            # Reachability, by every name a job uses for a service.
            for host in localhost 127.0.0.1 '[::1]'; do
              echo "PORTS $setting curl $host $(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$host:8080/" || echo failed)"
            done
            for host in localhost 127.0.0.1 ::1; do
              if docker run --rm --network host postgres:16 psql -h "$host" -U postgres -tAc 'select 1' >/dev/null 2>&1; then r=ok; else r=failed; fi
              echo "PORTS $setting psql $host $r"
            done
            docker run --rm --network host -v /tmp/client.py:/c.py:ro python:3.12-alpine python /c.py | sed "s/^/PORTS $setting python /"
            docker run --rm --network host -v /tmp/client.go:/c.go:ro golang:1.23-alpine go run /c.go localhost:8080 127.0.0.1:8080 '[::1]:8080' | sed "s/^/PORTS $setting go /"
            for v in 18 20 22; do
              docker run --rm --network host -v /tmp/client.js:/c.js:ro "node:$v-alpine" node /c.js | sed "s/^/PORTS $setting node$v /"
            done

            # Throughput, the three paths #163 measured.
            docker exec pg pgbench -U postgres -i -s 32 -q postgres >/dev/null 2>&1
            ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' pg)
            for mode in tpcb select; do
              flag=""
              [ "$mode" = select ] && flag="-S"
              for path in socket container published; do
                for rep in 1 2; do
                  case $path in
                    socket) out=$(docker exec pg pgbench -U postgres $flag -c 16 -j 4 -T 30 postgres 2>&1) ;;
                    container) out=$(docker run --rm --network host postgres:16 pgbench -h "$ip" -U postgres $flag -c 16 -j 4 -T 30 postgres 2>&1) ;;
                    published) out=$(docker run --rm --network host postgres:16 pgbench -h 127.0.0.1 -p 5432 -U postgres $flag -c 16 -j 4 -T 30 postgres 2>&1) ;;
                  esac
                  echo "PORTS $setting pgbench $mode $path rep$rep $(printf '%s\n' "$out" | awk '/^tps/ {print $3; exit}')"
                done
              done
            done
            docker rm -f pg web >/dev/null
            docker network rm bench >/dev/null
          }

          measure proxy-on
          # NO SUDO ON THIS GUEST, AND NONE NEEDED: the runner account is in the
          # docker group, which is root on the host. A privileged container enters
          # the host's namespaces, rewrites daemon.json and hands the restart to
          # systemd, because the restart ends the container that asked for it.
          jq -c '. + {"userland-proxy": false}' /etc/docker/daemon.json >/tmp/daemon.json
          docker run --rm --privileged --pid=host --net=host -v /tmp/daemon.json:/new.json:ro ubuntu:24.04 \
            sh -c 'cp /new.json /proc/1/root/etc/docker/daemon.json && chmod 0644 /proc/1/root/etc/docker/daemon.json'
          docker run --rm --privileged --pid=host --net=host ubuntu:24.04 \
            nsenter -t 1 -m -u -i -n -p -- systemd-run --no-block --collect systemctl restart docker
          sleep 5
          until docker info >/dev/null 2>&1; do sleep 1; done
          echo "PORTS daemon_json_after $(cat /etc/docker/daemon.json | tr -d '\n ')"
          measure proxy-off
          echo "PORTS route_localnet $(sysctl -n net.ipv4.conf.all.route_localnet) $(sysctl -n net.ipv4.conf.docker0.route_localnet 2>/dev/null || echo -)"
```

## What this does not establish

- **The Firecracker guest itself.** This ran on an EC2 guest built from the same declaration, with the same Docker. #163's Firecracker numbers are the reason to expect the same direction, not a measurement of the new setting there.
- **A per-tier switch.** billet has none, and none was built: the trial was to decide a default.
