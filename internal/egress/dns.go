package egress

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	dnsHeaderLen = 12
	// maxQuery is the largest query a guest's resolver is sent past. A real one
	// is a name and an EDNS option; a large one is only a way to smuggle bytes.
	maxQuery = 4096
	// maxName is a name's length in its textual form, the bound DNS itself sets.
	maxName = 253

	flagResponse   = 0x8000
	flagOpcodeMask = 0x7800
	flagRecursion  = 0x0100
	flagAvailable  = 0x0080

	classInternet = 1
)

// question is the one thing a query asks.
type question struct {
	id uint16
	// name is lower case with no terminal dot.
	name string
	// raw is the question section as the query spelled it, for an answer that
	// echoes it.
	raw []byte
}

// readQuestion reads a query's question, the one input to whether it is
// forwarded, and says false for anything but one plain internet-class query.
//
// READ HERE RATHER THAN BY dnsmessage, because the name is what a policy judges,
// so what counts as a name is the policy's rule and not a library's: dnsmessage
// admits a label of any bytes but a dot. A label here is only letters, digits, a
// hyphen or an underscore, which also keeps every name this package records
// printable: a guest chooses these bytes, and they end up on an operator's terminal.
func readQuestion(q []byte) (question, bool) {
	if len(q) < dnsHeaderLen || len(q) > maxQuery {
		return question{}, false
	}

	flags := binary.BigEndian.Uint16(q[2:])
	if flags&flagResponse != 0 || flags&flagOpcodeMask != 0 {
		return question{}, false
	}

	// NOTHING BUT THE QUESTION: an answer or authority section in a query is a
	// place to carry bytes, and the additional section holds EDNS and no more.
	if binary.BigEndian.Uint16(q[4:]) != 1 || binary.BigEndian.Uint16(q[6:]) != 0 ||
		binary.BigEndian.Uint16(q[8:]) != 0 {
		return question{}, false
	}

	var name strings.Builder

	at := dnsHeaderLen

	for {
		if at >= len(q) {
			return question{}, false
		}

		size := int(q[at])
		at++

		if size == 0 {
			break
		}

		// A COMPRESSION POINTER HAS THE TOP BITS SET, and no client compresses
		// the only name in a query; nothing past 63 is a label at all.
		if size > 63 || at+size > len(q) {
			return question{}, false
		}

		for _, c := range q[at : at+size] {
			if !labelByte(c) {
				return question{}, false
			}
		}

		if name.Len() > 0 {
			name.WriteByte('.')
		}

		name.WriteString(strings.ToLower(string(q[at : at+size])))
		at += size

		if name.Len() > maxName {
			return question{}, false
		}
	}

	// THE TYPE AND THE CLASS.
	if at+4 > len(q) || binary.BigEndian.Uint16(q[at+2:]) != classInternet {
		return question{}, false
	}

	at += 4

	return question{id: binary.BigEndian.Uint16(q), name: name.String(), raw: q[dnsHeaderLen:at]}, true
}

func labelByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// refusal is the answer to a query that is not forwarded, with rcode, echoing
// its question when it could be read, or nil for bytes with no header to answer.
func refusal(q []byte, read question, readable bool, rcode dnsmessage.RCode) []byte {
	if len(q) < dnsHeaderLen {
		return nil
	}

	section := read.raw
	if !readable {
		section = nil
	}

	out := make([]byte, dnsHeaderLen, dnsHeaderLen+len(section))
	copy(out, q[:2])

	flags := binary.BigEndian.Uint16(q[2:])&(flagOpcodeMask|flagRecursion) | flagResponse | flagAvailable
	binary.BigEndian.PutUint16(out[2:], flags|uint16(rcode))

	if readable {
		binary.BigEndian.PutUint16(out[4:], 1)
	}

	return append(out, section...)
}

// answered is what a resolver's answer said.
type answered struct {
	// names is the name the question asked and every name it passed through on
	// the way to an address, each lower case with no terminal dot.
	names []string
	addrs []netip.Addr
}

var errNotTheAnswer = errors.New("egress: the upstream's answer is not to the question asked")

// readAnswer reads what a resolver answered to read. An answer that does not
// parse, or that answers another question, is an error: on a listed guest an
// address taken from one could be anything.
func readAnswer(resp []byte, read question) (answered, error) {
	var p dnsmessage.Parser

	header, err := p.Start(resp)
	if err != nil {
		return answered{}, err
	}

	if header.ID != read.id || !header.Response {
		return answered{}, errNotTheAnswer
	}

	questions, err := p.AllQuestions()
	if err != nil {
		return answered{}, err
	}

	if len(questions) != 1 || canonicalName(questions[0].Name) != read.name {
		return answered{}, errNotTheAnswer
	}

	out := answered{names: []string{read.name}}

	for {
		h, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return out, nil
		}

		if err != nil {
			return answered{}, err
		}

		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return answered{}, err
			}

			out.addrs = append(out.addrs, netip.AddrFrom4(r.A))
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return answered{}, err
			}

			out.addrs = append(out.addrs, netip.AddrFrom16(r.AAAA).Unmap())
		case dnsmessage.TypeCNAME:
			r, err := p.CNAMEResource()
			if err != nil {
				return answered{}, err
			}

			if name := canonicalName(r.CNAME); printableName(name) {
				out.names = append(out.names, name)
			}
		default:
			if err := p.SkipAnswer(); err != nil {
				return answered{}, err
			}
		}
	}
}

func canonicalName(n dnsmessage.Name) string {
	return strings.ToLower(strings.TrimSuffix(n.String(), "."))
}

// printableName reports whether a name a resolver answered may be kept: only
// bytes readQuestion admits and dots. An answer's names are the authoritative
// server's to choose, and they reach a terminal too.
func printableName(name string) bool {
	if name == "" || len(name) > maxName {
		return false
	}

	for i := range len(name) {
		if c := name[i]; c != '.' && !labelByte(c) {
			return false
		}
	}

	return true
}
