# The egress allowlist

What [ADR-028](../decisions/adr-028-host-enforced-egress.md)'s rules did in a real Firecracker guest and on a real kernel, measured on 2026-11-04. Each rule below was run, not read; what was not run is listed at the end.

## Where it ran

One `m8i.4xlarge` in AWS `us-east-2` (nested virtualization, `/dev/kvm` present), Ubuntu 24.04, kernel `7.0.0-1014-aws`, nft 1.0.9, Firecracker 1.16.1, converged by the host role from this branch with a single-OSD Ceph. It ran `billet server` and `billet node` over loopback, against the test organization's App, with the signed guest image `ubuntu-2404-x64` (booted and verified on that host, guest kernel 6.1.155) and two tiers: one open, with `node.monitoring.flows` on, and one with `network.egress.allow: [example.com]`. The instance was destroyed afterwards.

## In a real guest

The listed tier's `command` ran these from inside the guest, as root, before starting its runner. A guest's own report is worth little, so each result was reported by asking the node's resolver for a name, which the host records and a guest cannot edit afterwards. The job was GitHub's run 41854061158 of the test organization's private jobs repository, on the final binary, and it succeeded.

| The guest tried | It got | The host recorded |
|---|---|---|
| `curl https://example.com` | connected | `example.com` beside the address it reached, 1.4 KiB sent and 5.8 KiB received |
| `dig iana.org` at its gateway | `REFUSED` | `iana.org` under refused, never forwarded |
| `curl https://1.1.1.1` | refused at once, curl's exit 7 | `1.1.1.1:443/tcp 1` under denied |
| `dig @8.8.8.8 example.com`, UDP and TCP | no answer | `8.8.8.8:53/udp 1` and `8.8.8.8:53/tcp 1`, both counted as DNS to another resolver |
| `curl -6 https://example.com` | failed, curl's exit 7 | 10 IPv6 packets refused |

`billet jobs show` on that job:

```text
egress     listed: the tier's network.egress, GitHub's runner endpoints and the node's own services, enforced on the host
queries    219 asked of the node's resolver, 186 refused and never forwarded
denied     3 packets refused, 2 of them DNS to a resolver other than the node's: 1.1.1.1:443/tcp 1, 8.8.8.8:53/tcp 1, 8.8.8.8:53/udp 1
ipv6       10 packets refused (a listed guest has no IPv6)
complete   yes
```

The open tier's job, run 41854068644, reached eleven destinations and was refused nothing, and its 37 queries named them the same way.

**What a stock guest asks for.** Of the 186 refusals, 160 were `ntp.ubuntu.com`, in a guest that lived a little over a minute, then `entropy.ubuntu.com` 8, `api.snapcraft.io` 6 and `registry.npmjs.org` 4. None of them stopped the job, and each is noise a reader of `refused` has to expect.

**What the first run found.** The first run of this job (run 41821627681) refused `glb-c0e95bd587389a.github.com` 7 times, `glb-db52c2cf8be544.github.com` 3 times and `blob.blz25prdstrz09a.trafficmanager.net` once. Those names are the chains of GitHub's own allowed endpoints, each already in the answers the guest had been given, so the guest's stub was asking for where a chain led; the likely reason is an answer that stopped short of the type it wanted, which was not traced further. The job succeeded anyway, but a refusal of something the list allowed misleads whoever reads it. Those names are now followed, and the final run refused none of them.

**A flush while a guest ran.** During a listed job (run 41854624757) the dispatch chain was flushed by hand. The node's next sweep logged an error naming the guest and put the rule back, 3 s later because of when the flush happened to land; the sweep runs every 5 minutes, so that is the bound. The job succeeded, its report read `complete: no`, and afterwards no guest, port element or policy file was left on the host. The guest was open for those 3 s, and what it did then was not probed from inside it.

## On the kernel

The tests in `internal/egress/realegress_linux_test.go` run under `BILLET_TEST_EGRESS_NETNS=1` on real bridges and veth pairs, with the role's guard in place, in a network namespace that is thrown away (`unshare -n`): they change routes and sysctls, so never on a shared host. Each passed on this kernel. Each guard these rules rest on was then broken once to see a test fail, against this kernel where the failure is one a kernel can show; the two a test did not notice are named below.

| Measured | Result |
|---|---|
| A frame to `01:80:c2:00:00:0e` with no rules, `rp_filter` 0 | reaches a socket on the gateway: it passes no bridge hook, and the guard does not name the port it arrives on |
| The same frame, `rp_filter` 2 | does not, which is Ubuntu's default and nothing the role sets |
| The same frame with a guest's rules, at either setting | does not, and the guest's own traffic is unaffected |
| A guest's rules taken away: the dispatch flushed, its own chain flushed, a whole table flushed, or the tables deleted | the guest is open, the node's check finds it, and the rules are back as they were, with the same rule counts; the job reads as incomplete |
| Part of a guest's rules gone at a restart: the nat table, the jobs map, the dispatch, or the bridge table | its rules come back whole under its own mark, off-list names are refused and never reach dnsmasq, what its names admitted stays admitted unless its set went, and its tap is free for the next guest |
| A socket held across a flush and a restart | answered by the new resolver: the mark kept in the policy file names the connections to delete |
| A guest nothing can serve (its policy file unreadable) | its DNS is dropped, rather than redirected to a port the kernel could give another guest's resolver |
| A forged frame to a neighbour's MAC, a frame of no protocol, a neighbour's source address | each dropped; the unfiltered control arrives |
| A refused connect, on the final rules | about 100 µs (Go's dialer, in a namespace), with the TCP reset rule and without it |
| One `nft` process start to add an address | 4 ms |

**A number that did not hold.** An early probe, with an earlier shape of the rules and `bash`'s `/dev/tcp`, took 1013 ms to be refused by a bare `reject` and 3 ms by a TCP reset, which is why the reset rule was written. On the final rules the difference does not reproduce, and what caused it was not found. The rule stays, since a reset is TCP's own refusal and costs nothing, but nothing here shows it is needed: breaking it is the one mutant besides the last `drop` that no test notices.

**The last `drop`** is not observable on this kernel: a frame of no protocol billet reads does not arrive with or without it, so `reject` appears to stop it first (not traced further). It stays because the rule's purpose is not to depend on what `reject` does with a frame it has no answer for.

## On kernel 6.8

Ubuntu 24.04's general-availability kernel, which a stock install boots: measured on 2026-11-07 on an `m7i.xlarge` in `us-east-2` running Ubuntu 24.04.5 with `linux-image-generic`, kernel `6.8.0-149-generic`, nft 1.0.9, and destroyed afterwards. `main` as of `227cdd82`, which carries #727, was built there and `internal/egress`'s unit tests and every real test above run five times in a thrown-away namespace: all passed, 0 failures in 5 runs. Each real test prepares its host as a node does, so the dry run of a whole listed guest's rules is accepted on 6.8 too.

6.8 behaves as 7.0 does where it matters most. The frame to `01:80:c2:00:00:0e` reached a socket on the gateway with no rules and `rp_filter` 0, and did not with 2, so the bridge-port rule is needed on 6.8 as well, and stops it there. Rules taken away and put back, a held socket moved across a flush and a restart, a guest nothing can serve, forged frames and a neighbour's connection all held as they do on 7.0.

A stock cloud image does not boot the generic kernel as it comes: Ubuntu's EC2 image boots without an initrd (`GRUB_FORCE_PARTUUID`), and the generic kernel loads its NVMe driver from one, so it panicked mounting the root filesystem until the initrd was put back. That is the image's, not billet's, and a host installed from Ubuntu's own installer boots it as it is.

## Real guests on kernel 6.8, seven at once

On 2026-11-09 the same probes ran in real Firecracker guests on 6.8: an `m8i.4xlarge` in `us-east-2` with nested virtualization, Ubuntu 24.04.5 booted on `linux-image-generic` (`6.8.0-149-generic`, `/dev/kvm` and `kvm_intel` present), converged by the host role from `main` as of `6085589b` with a single-OSD Ceph, booting the signed guest image `guest-20261108-091752` (runner 2.338.2), and destroyed afterwards. Seven jobs were queued at once from the test organization's private jobs repository, five on the listed tier and two on the open tier, and all seven succeeded. Each of the five listed guests reported, through names the host records, what the guest on 7.0 reported: `example.com` connected and was named beside its address, another name was `REFUSED`, a hard-coded address and an outside resolver over UDP and TCP were refused at once and counted (`1.1.1.1:443/tcp`, `8.8.8.8:53/udp`, `8.8.8.8:53/tcp`, 2 of them as DNS to another resolver), and 10 or 11 IPv6 packets were refused, every report `complete: yes`. The open guests' 43 and 44 queries were recorded and nothing was refused. Afterwards no guest, port element, policy file or cgroup was left on the host.

**What 6.8 changed was not the egress rules but a guest's teardown.** A microVM's cgroup answered EBUSY to its rmdir for a moment after its process was proved gone, which failed every teardown and with it the release of the lease, until a later one retried; billet now waits that out (see `removeEmptiedCgroup`, #821). With it, six of the seven jobs' teardowns met EBUSY, two to nine times each, and their cgroups were removed after 41 to 187 ms. The boot-verify of the image had failed the same way three times out of three before it, and passed with it.

**The link-local frame from a real tap.** Each guest also sent a crafted query as root, three times to the gateway's own MAC and three times to `01:80:c2:00:00:0e`, with the node's flow monitoring off so the open guests had no rules at all, and the host's resolver logging every query. Every frame to the gateway's MAC arrived: the open guests' at the host's resolver, the listed guests' at the node's, which refused them. No frame to the link-local MAC reached the host's resolver, from either. On a stock Ubuntu host the path is closed before billet's rule: `systemd-sysctl` gives every interface that appears `rp_filter` 2 (`/etc/sysctl.d/10-network-security.conf`, applied to each new tap by udev), whatever `default` says, which a namespace's veth does not get. The rule stays for a host configured otherwise, where the namespace tests above show it is what stops the frame.

Upgrading from tables without the bridge-port rule needed no converge: the node's next start added it, since its base script makes the tables what they should be in one transaction.

## What this did not prove

- **The link-local frame from a real guest, with the rule alone between it and the host.** On a real tap the host's own `rp_filter` closed the path first, and it was not lowered for a tap to show the rule doing so; the namespace tests show that on both kernels.
- **A guest harming a neighbour.** The ADR's list of what this does not do includes claiming a neighbour's DNS tuple first, and taking its MAC or address on the bridge. Those were read from the rules and the kernel's source and not run.
- **At scale.** Seven guests at once, on one host.
