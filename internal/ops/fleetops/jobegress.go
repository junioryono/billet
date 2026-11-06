package fleetops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/alloc"
)

// noEgressReport is what a job with no egress report says in place of one.
const noEgressReport = "no report recorded (one arrives as the job's compute goes, from a firecracker " +
	"node with node.monitoring.flows on or a tier with network.egress)"

// jobEgress is what the ledger holds of a job's network: a report, none, or a row
// it could not read, which is not none.
type jobEgress struct {
	row        *alloc.RecordedEgress
	unreadable string
}

func readJobEgress(ctx context.Context, a *alloc.Allocator, leaseID string) jobEgress {
	row, err := a.LeaseEgress(ctx, leaseID)

	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		return jobEgress{}
	case err != nil:
		return jobEgress{unreadable: "the ledger's row could not be read: " + strconv.Quote(err.Error())}
	}

	return jobEgress{row: &row}
}

// names is what each address answered for, or nil when nothing watched the job's
// resolver, in which case a destination has nothing to say about its name.
func (e jobEgress) names() map[string][]string {
	if e.row == nil {
		return nil
	}

	out := make(map[string][]string, len(e.row.Names))
	for _, n := range e.row.Names {
		out[n.Addr] = n.Names
	}

	return out
}

// destinationNames is an address's names as a table shows them. An address nobody
// answered for says so: it may have been reached by a hard-coded address, or by a
// name whose answer the resolver did not see.
func destinationNames(names map[string][]string, addr string) string {
	if len(names[addr]) == 0 {
		return "no known name"
	}

	return strings.Join(names[addr], ", ")
}

// egressFields is the job's network as labelled lines, the same for every format.
//
// A COUNT THAT IS A LOWER BOUND SAYS SO on its own line, and a job nothing watched
// says that rather than showing zeros, which would read as a job refused nothing.
func egressFields(e jobEgress, rec alloc.JobRecord) []pageField {
	switch {
	case e.unreadable != "":
		return []pageField{{Label: "egress", Value: e.unreadable}}
	case e.row == nil && rec.DetailRetiredAt != "":
		return []pageField{{Label: "egress", Value: detailRetired(rec)}}
	case e.row == nil:
		return []pageField{{Label: "egress", Value: noEgressReport}}
	}

	r := e.row

	if !r.Listed {
		return []pageField{
			{Label: "egress", Value: "open: where it went is only recorded, and nothing was refused"},
			{Label: "queries", Value: groupDigits(r.Queries) + " asked of the node's resolver"},
			egressComplete(r),
		}
	}

	fields := []pageField{
		{Label: "egress", Value: "listed: the tier's network.egress, GitHub's runner endpoints and the " +
			"node's own services, enforced on the host"},
		{Label: "queries", Value: fmt.Sprintf("%s asked of the node's resolver, %s refused and never forwarded",
			groupDigits(r.Queries), groupDigits(r.RefusedQueries))},
	}

	if r.RefusedQueries > 0 {
		fields = append(fields, pageField{Label: "refused", Value: refusedNames(r)})
	}

	fields = append(fields,
		pageField{Label: "denied", Value: deniedDestinations(r)},
		pageField{Label: "ipv6", Value: groupDigits(r.IPv6Packets) + " packets refused (a listed guest has no IPv6)"},
		egressComplete(r))

	return fields
}

func egressComplete(r *alloc.RecordedEgress) pageField {
	if r.Incomplete {
		return pageField{Label: "complete", Value: "no: the resolver did not see all of this job, or a count " +
			"could not be read, so these are a lower bound"}
	}

	return pageField{Label: "complete", Value: "yes"}
}

func refusedNames(r *alloc.RecordedEgress) string {
	var (
		named []string
		kept  int64
	)

	for _, n := range r.RefusedNames {
		named = append(named, n.Name+" "+groupDigits(n.Count))
		kept += n.Count
	}

	if rest := r.RefusedQueries - kept; rest > 0 {
		named = append(named, groupDigits(rest)+" not named")
	}

	return strings.Join(named, ", ")
}

func deniedDestinations(r *alloc.RecordedEgress) string {
	if r.DeniedPackets == 0 {
		return "no packets refused"
	}

	var named []string

	for _, d := range r.Denied {
		named = append(named, fmt.Sprintf("%s:%d/%s %s", d.Addr, d.Port, d.Protocol, groupDigits(d.Packets)))
	}

	out := groupDigits(r.DeniedPackets) + " packets refused"

	if r.OutsideResolverPackets > 0 {
		out += fmt.Sprintf(", %s of them DNS to a resolver other than the node's",
			groupDigits(r.OutsideResolverPackets))
	}

	if len(named) > 0 {
		out += ": " + strings.Join(named, ", ")
	}

	return out
}

// renderEgress writes the job's network section.
func renderEgress(w io.Writer, e jobEgress, rec alloc.JobRecord) {
	fmt.Fprintln(w)

	for _, f := range egressFields(e, rec) {
		fmt.Fprintf(w, "%-11s%s\n", f.Label, f.Value)
	}
}
