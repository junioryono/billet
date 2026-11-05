# ADR-027: A job's network is named and restricted on the host, by its interface and a resolver of its own

Accepted, 2026-11-04. Issue #540, and #510's workspaces build on it. Measurements are in [the egress record](../records/egress-allowlist.md).

## Context

With `node.monitoring.flows` a node totals each job's traffic by destination address from the connection tracker. It cannot say which names those addresses belong to. Nothing restricts where a job may connect beyond the host role's guard, which drops the internal ranges. A team defending its supply chain wants to read "this job talked to these domains" and to say "this tier may talk only to these".

The guest is root in its VM and runs whatever the workflow says, so nothing in it can be believed or relied on. Three facts about the host shape what can be built:

- **A guest chooses its own source address.** The flows package already says so of what it attributes. A policy keyed by address would be one a guest can step around.
- **DHCP is learned late.** The node reads a guest's address from dnsmasq's lease file some time after the guest asked, so an address cannot be what a rule is written against when the guest boots.
- **dnsmasq answers every guest on a bridge from one cache.** It can fill an nftables set from its answers (`nftset=`), and that set would be one per configuration, not one per job, and the configuration is the role's and not the tier's. Its logs name queries by source address, which the first fact rules out.

## Decision

**The filter is keyed by the guest's interface.** A Firecracker tap is a bridge port, so a guest's chain hangs off the `bridge` family's prerouting hook, where the input interface is the port: every frame the guest sends crosses it, and it cannot send from anywhere else. A workspace's veth is routed, so its chain hangs off the `inet` family's. The same rules, sets, resolver and record serve both, which is what #510 needs: `egress.Host.Attach` with `Kind: Routed`.

**A guest's resolver is its own socket.** DNS to the guest's gateway is marked by its chain and redirected, in the `ip` family's nat prerouting, to a port that serves that guest alone. What a socket hears is that guest's whatever source address it carries, so a name cannot be attributed to the wrong job or admitted into another job's set. The host's dnsmasq stays the upstream, with its cache, its refusal of private answers and its bound on a bad answer's life, and every other guest keeps using it directly.

**A flow belongs to the guest that opened it.** The redirect is chosen when a connection is created, and the connection tracker keeps it without regard to interfaces, so a guest forging another's source address and port would join that guest's connection and reach that guest's resolver. The redirect stamps its mark on the connection, and a packet is dropped unless its interface's mark and its connection's are one; that also stops a guest joining a connection with no redirect, which would reach dnsmasq directly. A reply goes out the owner's interface only, and a listed guest's source address is held to its subnet, because the resolver answers whoever a query says it is from, and its answer carries the query's bytes. A guest's connections are deleted when its resolver changes or goes, since their translation outlives the rule.

**A listed guest's port is cut off from the bridge's other ports.** The rules read IP addresses and a bridge delivers by MAC, so a frame to a peer's MAC holding an allowed public address would otherwise reach the peer, which could relay it under a broader policy.

**An address reaches the guest's set before its answer reaches the guest.** The resolver adds what a listed name answered, waits for the kernel, and only then answers, so a connect cannot beat the rule that admits it. The set's elements live as long as the guest, so a long connection is never cut by an element expiring, and they go with its chain.

**A name off the list is refused and never forwarded.** A query for `<data>.attacker.example` reaches that domain's nameserver whatever the answer is, so forwarding it and refusing the connection would leave a way out. The query is answered REFUSED and counted with its name.

**The node's own services stay reachable.** Anything to one of the host's addresses is left for the role's guard to judge by port, as it judges every guest's: the node's guest cache listens on one address, and a guest on the other bridge reaches it across the host. DNS is the exception, so another bridge's dnsmasq is not a way around the resolver.

**What a list refuses, it refuses at once.** A connection gets a TCP reset and anything else an ICMP error, rather than a drop: a job then fails in milliseconds with a reason, not after its client's timeout. TCP gets its own reset, which needs nothing of the guest's ICMP handling (the record has what was measured).

**IPv6 is refused, explicitly.** The role's bridges carry none (`LinkLocalAddressing=no`, no router advertisements), so a guest has no IPv6 path off the host anyway. A listed guest's chain drops every IPv6 frame and counts it, so it is not an unwatched path, and an IPv6 entry in a list is refused when the file loads rather than reading as a promise. An IPv6 answer is recorded and never admitted.

**A bridge port has no business at the IP layer.** A frame to a link-local MAC is handed up from the port without passing the bridge's prerouting hook, so no chain of a guest's sees it, and it arrives as a packet on the port, which no rule of the role's guard names. Left alone, what stands between that frame and dnsmasq is the host's `rp_filter`, which the role does not set, and for IPv6 nothing. So every guest with rules has its port named in a set that a raw-priority chain drops on: everything a guest legitimately sends the host arrives on the bridge's device instead.

**A chain an allowed name led to may be followed.** A real guest's stub asked for the names an allowed endpoint's chain passed through (in the record). Refusing that keeps nothing out, since the chain's addresses are already admitted, and a guest does not choose what an allowed name's answer says; it would only show an operator a refusal of a name their list allowed. A name no allowed answer named is still refused, and what a guest may follow is bounded.

**GitHub's runner endpoints are in every list.** A runner that cannot reach them cannot take a job, and a list that forgot one reads as a queue nobody can explain. `config.GitHubRunnerEndpoints` is GitHub's own list, read and dated, of what essential operations, downloading actions, logs, artifacts, caches, runner updates, OIDC tokens and release assets need; it leaves out what a job may not need (Packages, LFS, Dependabot, VNet), which a tier names itself.

**Failing closed belongs to a list, and failing open to a watcher.** A listed guest's rules outlive its node's process and refuse everything until the next process's resolver is up, and a list that cannot be applied refuses the launch. An open guest is only watched, so a node that cannot watch runs it unwatched and says so, and a node leaving it running hands its DNS back to dnsmasq. A crash cannot do that, so an open guest's DNS fails until the next process starts; that is the cost of watching it, and only a node with `node.monitoring.flows` on pays it.

**A report of its own.** What the host saw of a job's network is reported once, as its compute goes, on a route and in a row beside the usage report and apart from it, because a list is enforced on a node that samples nothing.

## Consequences

- The role's guard gains one rule, so redirected DNS is not dropped: a filter hook's drop in one table cannot be overruled from another. The first converge with it drains the node, as any change to the guest firewall does. A node whose guard lacks the rule says so, refuses a listed launch and runs open guests unwatched.
- The wire moves to 40. A listed tier is refused to an older node, which would ignore the block and run the job open.
- A listed job's DNS costs one process start for each answer holding an address not yet in its set (4 ms, measured), and nothing for the rest.

## What this does not do

- **DNS over HTTPS to an allowed name** is not seen. The resolver sees what is asked of it; a job that brings its own resolver to an address the list allows has its traffic carried, and its names go unrecorded.
- **An allowed address is an address.** A CDN's address answers for every site on it, so a list that allows one name on a shared address allows connecting to that address for any of them. SNI is not read: it is the guest's to write.
- **`*.blob.core.windows.net` is every Azure storage account**, and GitHub names no narrower host for logs, artifacts and caches. A job holding a key to an account of its own can reach it. A deployment that cannot accept that needs a network control outside billet.
- **The node's own caches reach their origins.** The Git proxy, the npm cache and the Actions cache are the node's services on the gateway, so a listed job can ask them for what they serve whether or not those origins are on its list. They serve only their own origins, and each has its own trust rules (ADR-013, ADR-018, ADR-020).
- **The host's firewall is the host's.** `nft flush ruleset` opens a listed guest, as it does the role's own guard. The node checks that each listed guest's rules are in force, from the base chains' dispatch to the guest's last rule (each chain holds as many rules as its rendering would, and each map names it), at every sweep (5 minutes), at every registration and at its next start, and puts back what has gone, whether or not the guard came back with it; it says so, and the job's report says it is incomplete. A rule rewritten in place by hand keeps its count and is not noticed. Until then the guest is open. A restore that fails is tried again at each of those, and if the job ends first its report still says it had a list and is incomplete.
- **A guest can still harm a neighbour on its bridge.** It cannot reach a destination its list does not admit this way, but a listed guest can claim a neighbour's DNS tuple first by forging its source address, so that neighbour's query on that tuple is dropped, and any guest can claim a neighbour's MAC or address on the bridge (a forged ARP, or a link-local frame the bridge learns from before any hook) and be sent what the host meant for that neighbour. Both come from guests sharing one bridge and one connection tracker. Per-guest conntrack zones and a port that does not learn would close them; they change how every guest's traffic is tracked and bridged, so they are their own decision.
- **GitHub itself is a way out.** A job with a token of its own can write to `github.com` and `api.github.com`, which every list includes.
- **A cache is a channel.** A listed job can write to a cache an open job later reads, under the rules that govern publication (ADR-013): the list restricts where a guest connects, not what the node's services carry between jobs.
- **Two deployments on one host** choose marks without asking each other. The second's first attach then conflicts, which refuses a listed launch and runs an open guest unwatched: it fails closed.
- **Names are best effort.** A destination reached by a hard-coded address, a name asked before an adoption, or an answer from a resolver the job brought says "no known name". For a job adopted across a handover the resolver did not see what it asked before, which the report says; its flows are not followed across one either.
