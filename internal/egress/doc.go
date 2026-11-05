// Package egress is the host side of a guest's network policy: where a job's
// guest may connect, what names it asked for, and what was refused (ADR-027).
//
// The guest is untrusted, so nothing here runs in it or believes it. Two things
// make what it does unforgeable:
//
// THE FILTER IS KEYED BY THE GUEST'S INTERFACE, never its address. A guest
// chooses its own source address, and every packet it sends crosses its own
// interface: a Firecracker tap is a bridge port, so its chain hangs off the
// bridge family's prerouting, where the port is the input interface; a
// workspace's veth is routed, so its chain hangs off the inet family's.
//
// THE RESOLVER IS PER GUEST. DNS the guest sends to its gateway is marked by the
// chain and redirected to a socket that serves that guest alone, so what a socket
// hears is that guest's, and an address reaches the guest's set before its answer
// reaches the guest: a connect cannot beat the rule that admits it. The host's
// dnsmasq stays the upstream, with its cache and its refusal of private answers.
//
// ONE MECHANISM FOR A TIER AND A WORKSPACE. Host attaches a Policy to an
// interface; the rules, the sets, the resolver and the record are the same for
// both kinds, and only the hook differs.
//
// A LIST FAILS CLOSED AND AN OBSERVER FAILS OPEN. A listed guest whose resolver is
// gone resolves nothing, and its rules outlive the process. An open guest is only
// watched, so a node that cannot watch runs it unwatched and says so, and a node
// that stops hands its DNS back to dnsmasq.
package egress
