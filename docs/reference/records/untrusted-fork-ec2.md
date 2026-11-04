# An untrusted fork pull request on EC2

A pull request from a fork ran on an untrusted EC2 tier on 2026-10-25, green each time, and every isolation check it made held, inside the job and from outside it. Reachability between two guests, and the node's cache listener refusing another guest, were not probed here. Default-branch jobs ran on the same pool: their Docker image store was published into the pool's own scope and read back by the fork, and nothing the fork wrote was ever published. This is #23's EC2 half. It also found that sticky disks could not work on an EC2 guest. One defect is fixed in the same change, and the other went with the AMI built from GitHub's template: on 2026-11-04 a new fork of a new probe repository proved [the sticky-disk half](#sticky-disks-on-a-contract-4-image-2026-11-04) on a contract 4 image.

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
| `40214476753` | `workflow_dispatch` on `main` | the published-port measurement for [#163](https://github.com/junioryono/billet/issues/163) |
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
| `imds_in_container` | `000unreachable` | curl ran in a container in the job and had no answer from the metadata service in 5 s (code `000`, then its failure's `unreachable`): one hop |
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
- The first default-branch job got a cold store and wrote to it. It was published as `snap-00adf0207995e4eb6` under `…scoped/untrusted/<test organization>/<probe repository>/amd64/docker`: an untrusted pool's own namespace, which no trusted key reaches.
- Every later job attached the pool's newest generation at the time it started, warm. The second default-branch job and both attempts of the fork's first run (`40214025831`) attached `snap-00adf0207995e4eb6`, and so did the `workflow_dispatch` job.
- The `workflow_dispatch` job's writes were published as `snap-0db26ddb05277b740`, which `40217214502` attached. A dispatch on the default branch is a writing event under ADR-013.
- The node logged a publication for four jobs: the first default-branch job, the dispatch, `40217214502` and `40220843182`. It logged neither a publication nor an abandonment for the second default-branch job (`40213645726`), whose store was settled; an authority billet could not prove writes nothing and logs nothing, so this record cannot say which held.
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
- **Every attach after `40217214502`** used its generation, `snap-00039f729a12da102`, until the last default-branch job published `snap-07f71fa4c68e5af24`: the fork's writes never became one.
- **The rule:** a fork's pull request is not a writing event, and its head repository is not the pool's, so `server.DecideCacheAuthority` refuses it publication (ADR-013).

## What the run found about sticky disks

**Node did not trust the cache endpoint's issuer. Fixed here.**
- **The failure:** the first runs' `stickydisk` steps failed with "unable to verify the first certificate" and ran cold.
- **The cause:** the AMI installs the cache endpoint's issuer in the distribution's trust store, and the image-store helper's curl reads that store. The sticky-disk action is Node, whose `https` trusts only the roots it was built with.
- **The fix:** the action now adds `/etc/ssl/certs/ca-certificates.crt` to what Node trusts by default (its bundled roots and `NODE_EXTRA_CA_CERTS`) for an HTTPS request. From then on its cache call succeeded and the node attached the disk; the evidence records the sticky disk `cold`, not `unused`.

**On this contract 3 image, the runner could not mount it.**
- **The failure:** the action formats and mounts the disk with `sudo`. On this guest that failed with "sudo: a password is required".
- **The cause:** the image gives the `runner` account no sudo, on purpose (`internal/provider/ec2/build.go`: running untrusted jobs as root "would give away the only isolation this backend has above the instance boundary").
- **The consequence:** sticky disks could not work on an EC2 guest as then built. `actions/stickydisk`, and `setup-docker-builder` built on it, ran cold. A fork's sticky-disk write was discarded because it never happened, which proved nothing about the publication rule. On a contract 4 image the sticky disk mounts (below); `setup-docker-builder` did not run there.

**One more thing the image's comment does not account for.** The `runner` account is in the `docker` group, and membership of that group is root on the host: the #163 job rewrote `/etc/docker/daemon.json` and restarted Docker from a privileged container. On an EC2 guest the instance is the boundary, there is no instance profile, and the registration is consumed, so the root a job can reach is its own instance's. But the image's stated reason for withholding sudo did not hold. Contract 4 settles it the other way: nothing on an EC2 guest depends on the job not being root.

## Sticky disks on a contract 4 image (2026-11-04)

A fork's sticky-disk writes do not reach a later job of the pool: its re-run starts over on the same key, and neither it nor the next default-branch job finds what it wrote to the default branch's disk. That was #23's last open assertion on this backend. #23 asked that the post step say the write was discarded. It does not, and could not: the action prints the same line in every job, and the decision comes after it. What says so is the node's log and the re-run.

**What ran.** The same shape as above, with these differences:
- **The account:** a second account in the same AWS organization, in us-west-2, because the CI account had no vCPUs free.
- **The deployment:** a new one, `89b600d12409b1c83df0307ee4ceab99`, whose tags the instances carried.
- **The image:** `ami-07bee0c3cc239315f`, made that day by `billet ami build` from GitHub's template on the default builder. It took 37 min for the template and layer, 52 min for the snapshot and 6 min for the verifier, which stamped it `sh.billet.ami-contract=4`, with `sh.billet.components` empty because the build accepted no licensed component.
- **The binary:** billet with this change, the image store's bound and the compaction and log changes made alongside it, all merged locally for the run, so the `will not publish` line quoted below comes from that last change.
- **GitHub:** a new public probe repository and a new fork, and pull request #1. The probe below gained the `sudo`, `groups`, `main_disk_mounted` and `main_disk_fork_marker` lines, and on `pull_request` it writes a `fork-marker` file into the default branch's own disk.

Every job's isolation probes printed what the table above shows, and `PROBE sudo yes`. Every job's post steps printed "billet will publish this sticky-disk update once the job's result proves it ran on the default branch", the fork's too: the action cannot know, and the control plane decides from GitHub's record of the run.

| Run | Event | `probe-main`: hit, marker, fork marker | This run's own disk: hit, marker | Docker tags found first | What the node did |
|---|---|---|---|---|---|
| `41810117985` | `push` | `false`, none, none | `false`, none | none | published the image store and both disks |
| `41811088960`, attempt 1 | `pull_request` from the fork | `true`, run `41810117985`'s, none | `false`, none | `push-41810117985-1` | "will not publish", `proven=false` |
| `41811088960`, attempt 2 | `pull_request` from the fork | `true`, run `41810117985`'s, none | `false`, none | `push-41810117985-1` | "will not publish", `proven=false` |
| `41812703623` | `push` | `true`, run `41810117985`'s, none | `false`, none | `push-41810117985-1` | published the image store and both disks |

- **The pool's reads:** the fork's first attempt read the default branch's disk warm, with its marker.
- **The fork's own key:** only attempt 2's cell tests anything, since the other three are a key's first use. It uses the key attempt 1 wrote, `probe-own-pull_request-41811088960`, and found it cold and empty. `mke2fs` also ran again, giving filesystem `17e48dab…` where attempt 1's was `4fac5127…`, so what it mounted was a new volume rather than attempt 1's.
- **The default branch's own key and the store:** attempt 2 and the last push each found no `fork-marker` in `probe-main` and none of the fork's Docker tags. The last push did not open the fork's own key.
- **Why, from the control plane and the node:** the control plane logged `decided what a completed job's caches may publish … event=pull_request ref="" proven=false publish_default=false`, and for each push `ref=refs/heads/main proven=true publish_default=true`. That line names no clause. No failed read was logged, and the fork is what the code's head-repository check refuses, so the fork is the reason by reading rather than by the log. The node then logged `a completed job's caches will not publish … policy=default-branch succeeded=true proven=false … event=pull_request` for both fork attempts, and `published a cache a default-branch job wrote` for each of the three keys of both pushes, under `…scoped/untrusted/<test organization>/<probe repository>/amd64/` and `sticky/` beneath it.
- **From outside:** the instance of the fork's first attempt, described after it started and before the job's first step, had `b1t12-untrusted` alone, no `IamInstanceProfile`, `HttpTokens: required`, a hop limit of 1 and the deployment's tags. The run's evidence lists the four jobs as `trust: untrusted`, `provider: ec2`, `instance_type: c7i.xlarge`, sticky disks `cold` on the first push and `warm` after, and the compute barrier `proved clear`.

**How far the writes themselves are proved.** The job never read its disk writes back, so that they happened is inferred, soundly: each ran under `bash -e` straight after `mounted yes` was printed, and the job was green, so a failed write would have failed it. The post steps then unmounted, and their line shows the node accepted each disk's commit as clean and deferred it. The Docker tag has a real read-back, in each job's second listing. No line records the discard of a clone: it is shown by the `will not publish` line and by what later jobs found.

**Teardown.** `billet acceptance down` deleted the scale set and found no instance left, and its sweep found nothing carrying the deployment. Its cache purge stopped at S3's `AccessDenied`, as above, after removing the deployment's snapshots and volumes. The buckets, AMI and its snapshot, orchestrator, repository, fork and runner group were then removed, and the App key was shredded before the orchestrator was terminated.

## Teardown

- `billet acceptance down` deleted the scale set and found no instance left, and `acceptance sweep` found nothing carrying the deployment.
- The cache purge then stopped at S3: `DELETE returned AccessDenied`. The generated node policy grants no delete beside the store, and the acceptance role this record's earlier runs used carried one extra delete for exactly this (see [AWS acceptance](aws-acceptance.md)). The purge ran once more under the operator's own credentials, passed on stdin, and removed the run's seven S3 state objects. Afterwards no snapshot, volume or instance carried the deployment's owner tag.
- **A second `acceptance run` on the same workspace inherits the first one's seal.** Its listener says the deployment is "no longer taking new work" and assigns nothing until `billet resume`. One push job queued while the deployment was sealed was never assigned after the resume, and ran once it was cancelled and run again.

## The probe, exactly as the fork ran it

This is the 2026-11-04 version. The 2026-10-25 runs ran the same file without the lines the section above names. The file the 2026-11-04 runs ran differed from this in two ways, both slips of editing that change no result: it carried the results-origin step and the image-store step twice, so each job's second image-store listing also shows the tag its first wrote, and the table above reads the first; and it printed `ssl_cert_file` and `node_extra_ca_certs` through a different shell expression, with the same two values.

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
          echo "PROBE sudo $(sudo -n true 2>/dev/null && echo yes || echo no)"
          echo "PROBE groups $(id -nG)"

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
          echo "PROBE main_disk_mounted $(mountpoint -q probe-main && echo yes || echo no)"
          echo "PROBE main_disk_fork_marker $(cat probe-main/fork-marker 2>/dev/null || echo none)"
          if [ "${{ github.event_name }}" = push ] && mountpoint -q probe-main; then
            echo "written by run ${{ github.run_id }} on ${{ github.ref_name }}" > probe-main/marker
          fi
          # THE FORK WRITES TO THE DEFAULT BRANCH'S OWN KEY: a later run finding this
          # would mean a fork published into the pool.
          if [ "${{ github.event_name }}" = pull_request ] && mountpoint -q probe-main; then
            echo "written by the fork, run ${{ github.run_id }} attempt ${{ github.run_attempt }}" > probe-main/fork-marker
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

- **A trusted EC2 tier's instance profile,** which this tier never had, by design.
- **The node's cache listener refusing other guests.** The untrusted group let these guests reach the node's cache port, which is the path their cache uses. A guest is refused without its session bearer: ADR-013 measured that on the reference deployment, not here.
- **A fork using what it holds to go around the action.** A job is root on its instance and holds its session's bearer. Only the action's own path ran here, not a job calling the node's cache API directly: ADR-013's rule is that the node decides from the control plane's authority rather than from the guest, and this run did not attack it.
- **Reachability between two guests,** which was not probed.
- **A failed or cancelled job's discard.** Every job here succeeded.
- **A fork running beside a publishing push.** The jobs ran one after another.
- **Other events,** such as a pull request from the same repository, or `pull_request_target`.
- **Whether the last push's generations read back.** It published them, and no job read them, nor any key of the `probe-own` kind warm.
