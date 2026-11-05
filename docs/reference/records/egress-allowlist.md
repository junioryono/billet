# The egress allowlist

What [ADR-024](../decisions/adr-024-host-enforced-egress.md)'s rules did in a real Firecracker guest and on a real kernel, measured on 2026-11-04. Each rule below was run, not read; what was not run is listed at the end.

## Where it ran

One `m8i.4xlarge` in AWS `us-east-2` (nested virtualization, `/dev/kvm` present), Ubuntu 24.04, kernel `7.0.0-1014-aws`, nft 1.0.9, Firecracker 1.16.1, converged by the host role from this branch with a single-OSD Ceph. It ran `billet server` and `billet node` over loopback, against the test organization's App, with the signed guest image `ubuntu-2404-x64` (booted and verified on that host, guest kernel 6.1.155) and two tiers: one open, with `node.monitoring.flows` on, and one with `network.egress.allow: [example.com]`. The instance was destroyed afterwards.

## In a real guest

The listed tier's `command` ran these from inside the guest, as root, before starting its runner. A guest's own report is worth little, so each result was reported by asking the node's resolver for a name, which the host records and a guest cannot edit afterwards. The job was GitHub's `billet-testbed/jobs` run 41854061158, on the final binary, and it succeeded.

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

Upgrading from tables without the bridge-port rule needed no converge: the node's next start added it, since its base script makes the tables what they should be in one transaction.

## What this did not prove

- **Kernel 6.8**, Ubuntu 24.04's GA kernel. Everything here ran on 7.0. `billet check` and every node's start dry-run a whole listed guest's rules against the kernel it has, so a kernel that refuses any of them says so and refuses a listed launch rather than running it open.
- **The link-local frame from a real guest.** It was sent from a veth port in a namespace, not from a Firecracker guest's tap. The kernel's source for the bridge treats the two alike (read, not run); the real guest's run proves only that the rule did not stop anything it should not.
- **A guest harming a neighbour.** The ADR's list of what this does not do includes claiming a neighbour's DNS tuple first, and taking its MAC or address on the bridge. Those were read from the rules and the kernel's source and not run.
- **At scale.** One or two guests at a time.
