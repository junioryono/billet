# An untrusted fork pull request on EC2

A pull request from a fork ran on an untrusted EC2 tier on 2026-10-25, green each time, and every isolation rule billet states for that tier held inside the job and from outside it. Default-branch jobs ran on the same pool: their Docker image store was published into the pool's own scope and read back by the fork, and nothing the fork wrote was ever published. This is #23's EC2 half. It also found that sticky disks cannot work on an EC2 guest: one defect is fixed in the same change, and one is a design question this record leaves open.

## What ran

- **The account:** the CI account (810711872940), us-west-2, the default VPC.
- **The orchestrator:** a `c7i.large` running `billet acceptance up --account 810711872940` and `billet acceptance run`, built from this change. Its instance role carried exactly the policy `billet init iam --account-wide --builder --payload-bucket` renders, and nothing else.
- **The guests:** `c7i.xlarge`, booting `ami-07ba26bf4a1783003`. `billet ami build` made it that day and booted it to stamp contract 3: Docker 29.1.3, runner 2.337.0, and the cache endpoint's issuer in the system trust store.
- **The tier:** one tier, `trust: untrusted`, with its own runner group and `cache_scope` set to the probe repository, so ADR-013's `default-branch` publication applies. `node.ec2.untrusted_security_group_ids` named a group with no ingress rule, and there was no `instance_profile`. The node served the cache over HTTPS on its private address, from an EBS and S3 store.
- **The deployment:** `fd0a67a938a132f8fabc26a0524a771a`, tier `b1t12-ec2-untrusted`.
- **GitHub:** billet's test organization and the same public probe repository as the [Tart record](untrusted-fork-tart.md), requiring approval for every outside contributor's fork workflow. A fork in another account opened pull request #2.

## The jobs

| Run | Event | What it is |
|---|---|---|
| `40212082871` | `push` to `main` | the first default-branch job: a cold image store, published afterwards |
| `40213645726` | `push` to `main` | a second default-branch job, the image store warm from the first |
| `40214025831` | `pull_request` from the fork, attempts 1 and 2 | the fork, the image store warm from the default branch |
| `40214476753` | `workflow_dispatch` on `main` | the published-port measurement for #163 ([Docker's published ports](docker-published-ports.md)) |
| `40217214502` | `push` to `main`, attempt 2 | a default-branch job that tags the image store (below) |
| `40219948619` | `pull_request` from the fork, attempts 1 and 2 | the fork again, on a new commit carrying the tagging step |
| `40220843182` | `push` to `main` | the last default-branch job, which reads what the fork left |

## What the job saw

Every check prints one `PROBE <name> <value>` line. The fork's two attempts printed the same values, apart from the attempt, the runner and the instance.

| Check | Value | What it shows |
|---|---|---|
| `event`, `head_repo`, `base_repo` | `pull_request`, the fork, the base repository | the job is a fork's pull request |
| `imds_security_groups` | `b1t12-untrusted` | the instance is on the untrusted group, and only that one |
| `imds_iam_info_status`, `imds_iam_credentials_status` | `404`, `404` | no instance profile: there is no credential to take |
| `imds_in_container` | unreachable | a container in the job cannot reach the metadata service at all (one hop) |
| `action_results_host`, `action_results_resolves` | `results-receiver.actions.githubusercontent.com`, a GitHub address | the runner talks to GitHub's own results origin |
| `hosts_remap`, `billet_ca_installed`, `action_ssl_cert_file`, `action_node_extra_ca_certs` | `0`, `no`, `unset`, `unset` | no interception: nothing remapped, no billet authority in the job |
| `results_tls_issuer` | a public CA's | the origin's certificate is GitHub's, not a node's |
| `cache_endpoint` | `present` | the pool has a cache session: an untrusted pool with a cache scope reads its own scope (ADR-013) |
| `gateway_22`, `gateway_5900`, `gateway_7717`, `gateway_7718` | `closed-or-timeout` | the VPC's router answers nothing |
| `internet_github` | `200` | the internet works |

## What was seen from outside

- **The fork job's instance, described while it ran** (`aws ec2 describe-instances`):
  - `SecurityGroups` was `b1t12-untrusted` alone, and `IamInstanceProfile` was `null`;
  - `MetadataOptions` had `HttpTokens: required` and `HttpPutResponseHopLimit: 1`;
  - its tags were `sh.billet.owner=fd0a67a938a132f8fabc26a0524a771a`, `sh.billet.node=b1t12-ec2` and a `billet-<lease>` name.
- **The node** logged `started a runner tier=b1t12-ec2-untrusted … trust=untrusted` for every job, and `attached a Docker image store … took=` with the outcome.
- **`billet acceptance evidence`** lists every job with `trust: untrusted`, its event (`push`, `pull_request`, `workflow_dispatch`), the base repository, `provider: ec2`, `instance_type: c7i.xlarge`, and its image-store observation. The compute barrier read `proved clear`.

## Whose writes the pool kept

**The image store, by the node's log:**
- The first default-branch job got a cold store and wrote to it. It was published as `snap-00adf0207995e4eb6` under `…scoped/untrusted/billet-testbed/b1t12-fork-target/amd64/docker`: an untrusted pool's own namespace, which no trusted key reaches.
- Every later job, the fork's included, attached that generation warm.
- The `workflow_dispatch` job's writes were published as `snap-0db26ddb05277b740`. A dispatch on the default branch is a writing event under ADR-013.
- No job's writes from the fork were ever published.

**And by a tag each job wrote.** From run `40217214502` on, the probe tags an image `b1t12-written-by:<event>-<run>-<attempt>` into the store, after listing every such tag it finds there:

| Run | Event | Tags it found | Tag it wrote | Its writes published |
|---|---|---|---|---|
| `40217214502`, attempt 2 | `push` | none | `push-40217214502-2` | yes, as `snap-00039f729a12da102` |
| `40219948619`, attempt 1 | `pull_request` from the fork | `push-40217214502-2` | `pull_request-40219948619-1` | no |
| `40219948619`, attempt 2 | `pull_request` from the fork | `push-40217214502-2` | `pull_request-40219948619-2` | no |
| `40220843182` | `push` | `push-40217214502-2` | `push-40220843182-1` | yes, at 18:26:26Z |

- **The pool's reads:** the fork read the default branch's tag, because every job of the pool attaches the pool's newest generation.
- **The fork's writes:** the fork's re-run did not find what its first attempt wrote, and the last default-branch job did not find what either attempt wrote. Each fork job's store was settled and then discarded.
- **Every attach after `40217214502`** used its generation, `snap-00039f729a12da102`: the fork's writes never became one.
- **The rule:** a fork's pull request is not a writing event, and its head repository is not the pool's, so `server.DecideCacheAuthority` refuses it publication (ADR-013).

## What the run found about sticky disks

**Node did not trust the cache endpoint's issuer. Fixed here.**
- **The failure:** the first runs' `stickydisk` steps failed with "unable to verify the first certificate" and ran cold.
- **The cause:** the AMI installs the cache endpoint's issuer in the distribution's trust store, and the image-store helper's curl reads that store. The sticky-disk action is Node, whose `https` trusts only the roots it was built with.
- **The fix:** the action now adds `/etc/ssl/certs/ca-certificates.crt` to Node's roots for an HTTPS request. From then on its cache call succeeded and the node attached the disk; the evidence records the sticky disk `cold`, not `unused`.

**The runner cannot mount it. Not fixed here.**
- **The failure:** the action formats and mounts the disk with `sudo`. On this guest that failed with "sudo: a password is required".
- **The cause:** the image gives the `runner` account no sudo, on purpose (`internal/provider/ec2/build.go`: running untrusted jobs as root "would give away the only isolation this backend has above the instance boundary").
- **The consequence:** sticky disks cannot work on an EC2 guest as built. `actions/stickydisk`, and `setup-docker-builder` built on it, run cold there. A fork's sticky-disk write is discarded because it never happens, which proves nothing about the publication rule. The image store above is what proves it on this backend.

**One more thing the image's comment does not account for.** The `runner` account is in the `docker` group, and membership of that group is root on the host: the #163 job rewrote `/etc/docker/daemon.json` and restarted Docker from a privileged container. On an EC2 guest the instance is the boundary, there is no instance profile, and the registration is consumed, so the root a job can reach is its own instance's. But the image's stated reason for withholding sudo does not hold, and that question belongs beside the sticky-disk one.

## Teardown

- `billet acceptance down` deleted the scale set and found no instance left, and `acceptance sweep` found nothing carrying the deployment.
- The cache purge then stopped at S3: `DELETE returned AccessDenied`. The generated node policy grants no delete beside the store, and the acceptance role this record's earlier runs used carried one extra delete for exactly this (see [AWS acceptance](aws-acceptance.md)). The purge ran once more under the operator's own credentials, passed on stdin, and removed the run's seven S3 state objects. Afterwards no snapshot, volume or instance carried the deployment's owner tag.
- **A second `acceptance run` on the same workspace inherits the first one's seal.** Its listener says the deployment is "no longer taking new work" and assigns nothing until `billet resume`. One push job queued while the deployment was sealed was never assigned after the resume, and ran once it was cancelled and run again.

## The probe, exactly as the fork ran it

```yaml
name: probe
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  probe:
    runs-on: b1t12-ec2-untrusted
    timeout-minutes: 30
    env:
      # Addresses an untrusted guest must not reach, besides its own gateway:
      # this LAN's router, another host on the LAN, and the Mac that runs the
      # Tart node. Unused on EC2, where they are not routable at all.
      LAN_TARGETS: "192.168.1.254:80 192.168.1.126:22 192.168.1.103:22 192.168.1.103:5900"
    steps:
      - name: Who and where
        run: |
          echo "PROBE event ${{ github.event_name }}"
          echo "PROBE head_repo ${{ github.event.pull_request.head.repo.full_name || github.repository }}"
          echo "PROBE base_repo ${{ github.repository }}"
          echo "PROBE run ${{ github.run_id }} attempt ${{ github.run_attempt }}"
          echo "PROBE runner $RUNNER_NAME"
          echo "PROBE kernel $(uname -srm)"

      - name: Interception and the cache session
        run: |
          host=$(printf '%s' "${ACTIONS_RESULTS_URL:-}" | sed -E 's#^https?://([^/:]+).*#\1#')
          echo "PROBE results_host ${host:-unset}"
          echo "PROBE results_resolves $(getent ahostsv4 "${host:-results-receiver.actions.githubusercontent.com}" | awk 'NR==1{print $1}')"
          echo "PROBE hosts_remap $(grep -c 'results-receiver.actions.githubusercontent.com' /etc/hosts || true)"
          echo "PROBE billet_ca_installed $(test -e /usr/local/share/ca-certificates/billet-actions-cache.crt && echo yes || echo no)"
          echo "PROBE ssl_cert_file $([ -n "${SSL_CERT_FILE:-}" ] && echo set || echo unset)"
          echo "PROBE node_extra_ca_certs $([ -n "${NODE_EXTRA_CA_CERTS:-}" ] && echo set || echo unset)"
          echo "PROBE cache_endpoint $(test -n "${BILLET_CACHE_ENDPOINT:-}" && echo present || echo absent)"
          echo "PROBE results_tls_issuer $(echo | timeout 10 openssl s_client -connect "${host:-results-receiver.actions.githubusercontent.com}:443" -servername "${host:-results-receiver.actions.githubusercontent.com}" 2>/dev/null | openssl x509 -noout -issuer 2>/dev/null | sed 's/^issuer=//')"

      - name: The results origin as an action sees it
        uses: actions/github-script@v7
        with:
          script: |
            const raw = process.env.ACTIONS_RESULTS_URL || "";
            const host = raw ? new URL(raw).host : "unset";
            console.log(`PROBE action_results_host ${host}`);
            try {
              const { address } = await require("dns").promises.lookup(
                host === "unset" ? "results-receiver.actions.githubusercontent.com" : host);
              console.log(`PROBE action_results_resolves ${address}`);
            } catch (e) {
              console.log(`PROBE action_results_resolves failed ${e.code}`);
            }
            console.log(`PROBE action_ssl_cert_file ${process.env.SSL_CERT_FILE ? "set" : "unset"}`);
            console.log(`PROBE action_node_extra_ca_certs ${process.env.NODE_EXTRA_CA_CERTS ? "set" : "unset"}`);

      - name: Instance metadata (EC2)
        run: |
          if ! token=$(curl -sf --max-time 3 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60'); then
            echo "PROBE imds none"
            exit 0
          fi
          get() { curl -s --max-time 3 -o /dev/null -w '%{http_code}' -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/meta-data/$1"; }
          echo "PROBE imds_instance $(curl -s --max-time 3 -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/instance-id)"
          echo "PROBE imds_security_groups $(curl -s --max-time 3 -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/security-groups | tr '\n' ' ')"
          echo "PROBE imds_iam_info_status $(get iam/info)"
          echo "PROBE imds_iam_credentials_status $(get iam/security-credentials/)"
          echo "PROBE imds_in_container $(docker run --rm curlimages/curl:8.11.1 -s -o /dev/null -w '%{http_code}' --max-time 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null || echo unreachable)"

      - name: What the network reaches
        run: |
          probe() { if timeout 6 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; then echo open; else echo closed-or-timeout; fi; }
          gw=$(ip -4 route show default | awk 'NR==1{print $3}')
          echo "PROBE gateway $gw"
          for port in 22 5900 7717 7718; do echo "PROBE gateway_${port} $(probe "$gw" "$port")"; done
          for t in $LAN_TARGETS; do echo "PROBE lan_${t} $(probe "${t%:*}" "${t#*:}")"; done
          echo "PROBE internet_github $(curl -s -o /dev/null -w '%{http_code}' --max-time 10 https://github.com)"
          echo "PROBE dns_github $(getent ahostsv4 github.com | awk 'NR==1{print $1}')"

      - name: The Docker image store, read and written by this run
        run: |
          # A tag only this run writes, so a later run can tell whose writes the
          # pool's store kept: a default branch's are published, a fork's never.
          tag="b1t12-written-by:${{ github.event_name }}-${{ github.run_id }}-${{ github.run_attempt }}"
          echo "PROBE docker_store_mount $(findmnt -n -o SOURCE /var/lib/docker || echo root-disk)"
          echo "PROBE docker_tags_found $(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep '^b1t12-written-by:' | sort | tr '\n' ' ')"
          docker pull -q busybox:1.36 >/dev/null
          docker tag busybox:1.36 "$tag"
          echo "PROBE docker_tag_written $tag"

      - name: The default branch's sticky disk
        id: main_disk
        uses: junioryono/billet/actions/stickydisk@junior-oct24-untrusted-fork-proof
        with:
          key: probe-main
          path: probe-main
          size-gb: 1

      - name: Read or write the default branch's disk
        run: |
          echo "PROBE main_disk_cache_hit ${{ steps.main_disk.outputs.cache-hit }}"
          echo "PROBE main_disk_marker $(cat probe-main/marker 2>/dev/null || echo none)"
          if [ "${{ github.event_name }}" = push ] && mountpoint -q probe-main; then
            echo "written by run ${{ github.run_id }} on ${{ github.ref_name }}" > probe-main/marker
          fi

      - name: This run's own sticky disk
        id: own_disk
        uses: junioryono/billet/actions/stickydisk@junior-oct24-untrusted-fork-proof
        with:
          key: probe-own-${{ github.event_name }}-${{ github.run_id }}
          path: probe-own
          size-gb: 1

      - name: Write this run's disk
        run: |
          echo "PROBE own_disk_cache_hit ${{ steps.own_disk.outputs.cache-hit }}"
          echo "PROBE own_disk_marker $(cat probe-own/marker 2>/dev/null || echo none)"
          echo "PROBE own_disk_mounted $(mountpoint -q probe-own && echo yes || echo no)"
          if mountpoint -q probe-own; then
            echo "written by run ${{ github.run_id }} attempt ${{ github.run_attempt }}" > probe-own/marker
          fi
```

## What this does not establish

- **Sticky disks on EC2,** as above.
- **A trusted EC2 tier's instance profile,** which this tier never had, by design.
- **The node's cache listener refusing other guests.** The untrusted group let these guests reach the node's cache port, which is the path their cache uses. A guest is refused without its session bearer: ADR-013 measured that on the reference deployment, not here.
