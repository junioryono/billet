// Package fleetops is the operator commands that act on the fleet a control
// plane runs: the leases it holds and why (`billet leases`, `billet jobs`),
// admission (`billet drain`, `billet resume`, `billet force-destroy`), the
// nodes it admits (`billet nodes`), its certificate authority (`billet ca`),
// and removing what billet made on GitHub and in AWS (`billet teardown`,
// `billet decommission`).
package fleetops
