# An untrusted fork pull request on Tart

A pull request from a fork ran on an untrusted Tart tier on 2026-10-25, twice, green both times, and every isolation rule billet states for that tier held inside the job and from outside it. This is #23's Tart half. On the way it found two defects that every stub had accepted, both fixed in the same change, without which no untrusted launch on that Mac could succeed.

## What ran

- **The host:** a development Mac (Apple M2 Max, 32 GiB, macOS 26.0), not the reference deployment's Mac. tart 2.36.0 and softnet 0.23.0, with softnet's setuid-root grant in place (`billet check`: "setuid-root grant configured").
- **The guest:** `ghcr.io/cirruslabs/ubuntu-runner-arm64@sha256:dbc204bb40c046150ca212e5c1a38419f0f59b19d2602467dc6c67caea81186b`, Linux 7.0.0-28-generic aarch64, 4 vCPU and 16 GiB. A Linux guest rather than a macOS one, because the Mac had 61 GiB free and a macOS image would not have fit beside it.
- **The deployment:** `billet acceptance up` on a base config written by `billet init --provider tart --guest-os linux`. It had one tier, `trust: untrusted`, `node.tart.untrusted_isolation: softnet`, and its own runner group. The derived deployment was `28fa3c875e333ed08ba593181a8dd821`, tier `b1t12t-billet-linux-arm64-4vcpu`. The control plane and the node ran as `billet acceptance run`'s children, built from this change.
- **GitHub:** billet's test organization, with a public repository whose only workflow is the probe below. A runner group admitted public repositories, restricted to that one repository. The repository required approval for every outside contributor's fork workflow, so nobody else's pull request could reach the runner while the repository was public. A fork in another account opened pull request #1. Its run `40208604132` ran twice: attempt 1 and a re-run, attempt 2.

## What the job saw

Every check prints one `PROBE <name> <value>` line. Both attempts printed the same values, apart from the run attempt and the runner's name.

| Check | Value | What it shows |
|---|---|---|
| `event`, `head_repo`, `base_repo` | `pull_request`, the fork, the base repository | the job is a fork's pull request |
| `kernel` | `Linux 7.0.0-28-generic aarch64` | a guest kernel of its own, not the Mac's |
| `gateway` | `192.168.2.1` | softnet's network, not tart's shared NAT (`192.168.64.0/24`) |
| `gateway_22`, `gateway_5900`, `gateway_7717`, `gateway_7718` | every one `closed-or-timeout` | the gateway, which is the Mac, answers nothing |
| `lan_192.168.1.254:80` (the LAN's router), `lan_192.168.1.126:22` (another host on the LAN) | `closed-or-timeout` | the private network is out of reach; both answer the Mac itself (below) |
| `lan_192.168.1.103:22`, `lan_192.168.1.103:5900` (the Mac's own LAN address) | `closed-or-timeout` | see the control below: nothing listens there, so this alone shows nothing |
| `internet_github`, `dns_github` | `200`, an address | the internet and name resolution still work |
| `cache_endpoint` | `absent` | no cache session: a Mac serves no cache (`node.cache` is refused there) |
| `billet_ca_installed`, `ssl_cert_file`, `node_extra_ca_certs`, `hosts_remap` | `no`, `unset`, `unset`, `0` | no interception: no billet authority, nothing remapped |
| `imds` | `none` | there is no metadata service to read |
| `main_disk_cache_hit`, `own_disk_cache_hit`, `own_disk_mounted` | `false`, `false`, `no`, after the action's "billet sticky disk is unavailable; this job will continue cold" | no sticky disk is attached, so nothing a fork writes can persist |

**Was the Mac itself unreachable, or just not listening?** A separate control answered that, on the same Mac the same day. A listener on `0.0.0.0:18722` answered the Mac itself with 200. Two VMs were booted by hand from the same image:
- on tart's default NAT, the guest reached the listener at its gateway (`192.168.64.1`) and at the Mac's LAN address, 200 both times;
- with exactly the flags billet passes (`--net-softnet --net-softnet-block=@host`), the guest reached neither, and got no answer within 6 seconds at either.

The router's port 80 and the other host's SSH both answered from the Mac, so their timeouts inside the job are softnet's block, not absent services.

## What was seen from outside

- **The VM's command line,** while attempt 2 ran: `tart run billet-abe96fc8f47b85181dd8d398b846a9ed --no-graphics --net-softnet --net-softnet-block=@host`.
- **Its directory** carried the ownership marker `billet-owner`, containing `28fa3c875e333ed08ba593181a8dd821`, the run's deployment.
- **The node** logged `launched VM … isolation="--net-softnet --net-softnet-block=@host"` and `started a runner tier=b1t12t-billet-linux-arm64-4vcpu … trust=untrusted`, then `destroyed VM` once each job ended.
- **`billet acceptance evidence`** lists the two jobs with `trust: untrusted`, `event: pull_request`, the base repository, `provider: tart`, GitHub's `succeeded` and billet's `done`. It shows no cache observation for either, because nothing was offered. The compute barrier read `proved clear`, with the node empty from 16:33:41Z to 16:38:41Z.
- **The evidence also lists two failed leases.** One is the first launch, which the first defect below failed. The other was bought while the run was stopped to replace its binary; it never launched and was released.

## The two defects the run found

**An isolated launch did not wait for the guest agent.**
- **The failure:** the first launch failed with `configure the isolated guest's resolver: tart exec: … Failed to connect to the VM using its control socket … is the Tart Guest Agent running?`.
- **The cause:** billet configures an isolated guest's resolver before it delivers the registration, and it asked once. tart lists a VM as running before the guest has started its agent, and a `tart exec` that finds no agent blocks for 30 seconds and then fails.
- **Measured on this Mac:** three boots of this image, the agent answered 30.7, 40.5 and 30.8 seconds after the VM was listed, and each failed exec had blocked 30.0 to 30.1 seconds. So every untrusted launch here failed.
- **Why it had not shown before:** the registration's delivery already retried this refusal. A faster Mac's guest wins the race, which may be why the reference Mac never showed it.
- **The fix:** the resolver now retries that refusal, and only that one, until the caller's deadline. The first green launch reported `took=1m19.68s`.

**A failed launch's cleanup killed `tart delete` part-way.**
- **The failure:** the stray cleanup after the failed launch has a 30-second bound. It ran out during the delete (`tart delete: signal: killed`).
- **What it left:** a lease-named directory holding `config.json`, `nvram.bin`, the control socket and the ownership marker, but no disk.
- **Why that stops the node:** tart cannot read the directory (`VM is missing files for a supported layout`), and billet's `List` rightly refuses to guess what it is. So every sweep and custody pass on the node failed until the directory was removed by hand. Before removing it, `lsof +D` and the process table showed no opener and no VM process.
- **The fix:** a delete that has begun now finishes on its own two-minute bound, the store lock's, whatever its caller does.

**And one operator trap the run walked into, not fixed here.** On macOS a unix socket path is at most 103 bytes. A workspace under a long temporary directory made the node's upgrade socket 167 bytes, and the node refused to start ("use a shorter node.state_dir path"). `billet acceptance up` does not check this; a short `--workspace` avoids it.

## The probe, exactly as it ran

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
    runs-on: b1t12t-billet-linux-arm64-4vcpu
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

      - name: The default branch's sticky disk
        id: main_disk
        uses: junioryono/billet/actions/stickydisk@main
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
        uses: junioryono/billet/actions/stickydisk@main
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

- **A macOS guest.** The isolation is the same softnet path billet passes for either guest OS, and an earlier measurement on the reference Mac covered macOS guests with and without `@host` ([Run jobs on a Mac](../../deploying/mac-tart.md#trusted-or-untrusted)). A macOS guest running a fork's job end to end is still not proved.
- **The reference Mac.** This was a development Mac, on an older tart (2.36.0 against the reference Mac's 2.37.0) and a slower guest boot.
- **A cache on a Mac.** There is none to test: `node.cache` is refused there, so "is served no cache" is true by construction.
