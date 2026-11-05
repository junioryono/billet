# Naming and restricting where jobs connect

A node can tell you which names a job's traffic went to, and a tier can say which names its jobs may reach. Both are enforced and recorded on the host, because a job is root in its guest. Firecracker tiers only. The decision is [ADR-027](../reference/decisions/adr-027-host-enforced-egress.md).

## Names

With `node.monitoring.flows` on, the node watches each guest's DNS through a resolver of its own in front of dnsmasq, and `billet jobs show` prints names beside addresses:

```text
destinations 3 by total bytes, from the host's connection tracker (attributed by the guest's source address)
           address               sent    received  connections  names (from the node's resolver, best effort)
           140.82.112.3       12.0MiB      3.1MiB            4  github.com
           185.199.108.133     1.2KiB     88.4MiB            2  objects.githubusercontent.com
           203.0.113.9          640B        1.1KiB           1  no known name

egress     open: where it went is only recorded, and nothing was refused
queries    41 asked of the node's resolver
complete   yes
```

"no known name" is what the resolver can say of an address nobody asked it about: a hard-coded one, one from a resolver the job brought, or one asked before the node adopted the job. It is not a verdict on the address.

A node only watches an open tier's guests with `node.monitoring.flows` on. Without it their DNS goes to dnsmasq as it always did, and `jobs show` says no report was recorded.

## Restricting a tier

```yaml
tiers:
  - label: linux-4-locked
    provider: firecracker
    network:
      egress:
        allow:
          - registry.npmjs.org
          - "*.pythonhosted.org"
          - 203.0.113.0/24
```

`example.com` allows that name alone, `*.example.com` every name below it and not the name itself, and an address or prefix those addresses. IPv4 only. Without `network.egress` a tier is open, as a tier always was. `allow: []` allows only what every list includes.

**Every list includes** the node's own services on the guest's gateway and GitHub's runner endpoints, since a runner that cannot reach them cannot take a job:

```text
github.com                  api.github.com                codeload.github.com
*.actions.githubusercontent.com     *.blob.core.windows.net
objects.githubusercontent.com       objects-origin.githubusercontent.com
github-releases.githubusercontent.com  github-registry-files.githubusercontent.com
release-assets.githubusercontent.com
```

That is [GitHub's list](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#accessible-domains-by-function) less what a job may not need. A job that pushes to GitHub Packages or uses Git LFS lists `ghcr.io`, `*.pkg.github.com`, `pkg-containers.githubusercontent.com`, `github-cloud.githubusercontent.com` or `github-cloud.s3.amazonaws.com` itself, and one pulling through `node.registry_mirrors` lists the mirror.

**What a listed guest gets:**

| It tries | It gets |
|---|---|
| a name on the list | an answer, and that answer's addresses are open to it for the rest of the job |
| a name off the list | `REFUSED`, and the query goes no further, so a query cannot carry data out either |
| an address no listed name answered, not in a listed prefix | the connection refused at once (a TCP reset, or an ICMP error) |
| DNS to any resolver but its gateway, even one in a listed prefix | refused the same way |
| IPv6 | every packet dropped and counted |
| another guest on its bridge, by address or by MAC, even one in a listed prefix | refused, and no peer can reach it either |

**What is shown:**

```text
egress     listed: the tier's network.egress, GitHub's runner endpoints and the node's own services, enforced on the host
queries    120 asked of the node's resolver, 3 refused and never forwarded
refused    pastebin.example 2, 1 not named
denied     14 packets refused, 2 of them DNS to a resolver other than the node's: 203.0.113.7:443/tcp 12, 8.8.8.8:53/udp 2
ipv6       6 packets refused (a listed guest has no IPv6)
complete   yes
```

`complete: no` means these are a lower bound: the node's resolver did not see the whole job (it was adopted across a handover), more was asked than is kept, or a count could not be read.

## Before the first list

1. **Upgrade every node and then the control plane's tiers.** A listed tier is refused to a node older than wire 40, which would ignore the block and run the job open; the job waits for a node that can honour it.
2. **Converge the host role** from the same release. It adds one rule to the guest firewall, so redirected DNS is not dropped, and that is a change to guest networking, which drains the node once.
3. **Run `billet check`**, as root. The `egress` line says whether this host can apply a list, or why not, and it fails the check if a tier here has one and it cannot.

A node that cannot apply a list still starts, since it may never be sent one. It refuses a listed launch and says why in its log, and runs open guests unwatched. Placement knows a node's wire and not this, so on a fleet with such a node a listed job's launch can fail there and be reassigned: `billet check` on each node is what finds it first.

## When something is refused that should not be

Read what the job was refused in `billet jobs show <lease>`: a name in `refused` needs adding, or a CDN behind a listed name is answering from an address the guest asked for by another name. A tool that resolved a name off the list usually fails with "server can't find" or "connection refused" naming neither the list nor billet, so start there.

**A CNAME target does not need listing.** Listing `registry.npmjs.org` admits the addresses its answer ends in, whatever it passes through, and a stub that then asks for the name the chain led to is answered. A name no allowed answer named is judged by the list.

**A stock guest asks for things of its own.** Before a job's first step, the Ubuntu runner image asks for `ntp.ubuntu.com` (160 times in the minute measured), `entropy.ubuntu.com`, `api.snapcraft.io` and `registry.npmjs.org`. Those appear in `refused` on a job that succeeded; list them only if a job needs them.

**A shared address is shared.** If a listed name resolves to a CDN's address, that address is open to the job for any site it serves, and the list cannot tell them apart. See [what a list does not do](../reference/decisions/adr-027-host-enforced-egress.md#what-this-does-not-do).

## If something removes the rules

`nft flush ruleset`, or a firewall service that does the same, opens a listed guest until the node notices. It checks that each listed guest's rules are in force at every sweep (5 minutes), at every registration and when it starts, puts back what has gone, and logs an error naming the guest. The job's report then reads `complete: no`. What the guest's names had admitted is forgotten, so it has to ask again. If the rules cannot be put back the node says so and tries again at each of those. Converge the host as well, since the same flush removes the role's guard.

## While the node is down

A listed guest stays closed: what its names admitted stays admitted and everything else stays refused, and it resolves nothing until the next node process has a resolver for it, usually within seconds. On a clean stop that leaves jobs running, an open guest's DNS is handed back to dnsmasq first. After a crash it fails until the next process starts, and is handed back then.
