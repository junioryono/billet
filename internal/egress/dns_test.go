package egress

import (
	"encoding/binary"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// aQuery is what a stub sends: one question and an EDNS option.
func aQuery(t testing.TB, name string, qtype dnsmessage.Type) []byte {
	t.Helper()

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}

	if err := b.Question(dnsmessage.Question{
		Name: dnsmessage.MustNewName(name), Type: qtype, Class: dnsmessage.ClassINET,
	}); err != nil {
		t.Fatal(err)
	}

	if err := b.StartAdditionals(); err != nil {
		t.Fatal(err)
	}

	var opt dnsmessage.ResourceHeader
	if err := opt.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
		t.Fatal(err)
	}

	if err := b.OPTResource(opt, dnsmessage.OPTResource{}); err != nil {
		t.Fatal(err)
	}

	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// rawQuery is a query whose name is the labels given, byte for byte, which is how
// a guest can write what no library would.
func rawQuery(labels ...string) []byte {
	out := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}

	for _, l := range labels {
		out = append(out, byte(len(l)))
		out = append(out, l...)
	}

	return append(out, 0, 0, 1, 0, 1)
}

// anAnswer is what the resolver says to q: a chain of names, then addresses.
func anAnswer(t testing.TB, q []byte, chain []string, addrs ...netip.Addr) []byte {
	t.Helper()

	var p dnsmessage.Parser

	header, err := p.Start(q)
	if err != nil {
		t.Fatal(err)
	}

	question, err := p.Question()
	if err != nil {
		t.Fatal(err)
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}

	if err := b.Question(question); err != nil {
		t.Fatal(err)
	}

	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}

	owner := question.Name

	for _, next := range chain {
		target := dnsmessage.MustNewName(next)
		if err := b.CNAMEResource(dnsmessage.ResourceHeader{
			Name: owner, Class: dnsmessage.ClassINET, TTL: 60,
		}, dnsmessage.CNAMEResource{CNAME: target}); err != nil {
			t.Fatal(err)
		}

		owner = target
	}

	for _, addr := range addrs {
		h := dnsmessage.ResourceHeader{Name: owner, Class: dnsmessage.ClassINET, TTL: 60}

		if addr.Is4() {
			err = b.AResource(h, dnsmessage.AResource{A: addr.As4()})
		} else {
			err = b.AAAAResource(h, dnsmessage.AAAAResource{AAAA: addr.As16()})
		}

		if err != nil {
			t.Fatal(err)
		}
	}

	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func TestAPlainQueryIsReadAsWhatItAsks(t *testing.T) {
	t.Parallel()

	read, ok := readQuestion(aQuery(t, "GitHub.COM.", dnsmessage.TypeA))
	if !ok {
		t.Fatal("a stub's own query was not read")
	}

	if read.name != "github.com" {
		t.Fatalf("name = %q, want it lower case with no terminal dot", read.name)
	}

	if read.id != 0x1234 {
		t.Fatalf("id = %#x", read.id)
	}

	for _, name := range []string{"_dmarc.example.com.", "a-b.example.com.", "xn--bcher-kva.example."} {
		if _, ok := readQuestion(aQuery(t, name, dnsmessage.TypeTXT)); !ok {
			t.Errorf("%s was not read", name)
		}
	}
}

// A LABEL HOLDING A DOT WOULD READ AS THE LISTED NAME IT SPELLS, while asking about a
// top-level domain of that spelling; and a guest's other bytes end up on a terminal.
func TestAQueryWhoseNameIsNotWhatItSpellsIsNotRead(t *testing.T) {
	t.Parallel()

	for name, q := range map[string][]byte{
		"a dot inside a label": rawQuery("github.com"),
		"an escape sequence":   rawQuery("a\x1b[31m", "example", "com"),
		"a newline":            rawQuery("a\nb", "example", "com"),
		"a space":              rawQuery("a b", "example", "com"),
		"a NUL":                rawQuery("a\x00b", "example", "com"),
		"a high byte":          rawQuery("caf\xc3\xa9", "example", "com"),
	} {
		if read, ok := readQuestion(q); ok {
			t.Errorf("%s: read as %q", name, read.name)
		}
	}
}

func TestAQueryThatIsNotOnePlainQuestionIsNotRead(t *testing.T) {
	t.Parallel()

	good := rawQuery("example", "com")
	if _, ok := readQuestion(good); !ok {
		t.Fatal("the query every case breaks was not read to begin with")
	}

	broken := func(change func([]byte) []byte) []byte { return change(slices.Clone(good)) }
	set16 := func(at int, v uint16) func([]byte) []byte {
		return func(q []byte) []byte {
			binary.BigEndian.PutUint16(q[at:], v)

			return q
		}
	}

	for name, q := range map[string][]byte{
		"nothing":             nil,
		"no question":         good[:dnsHeaderLen],
		"cut in its name":     good[:dnsHeaderLen+3],
		"no type or class":    good[:len(good)-4],
		"a response":          broken(set16(2, flagResponse)),
		"an update":           broken(set16(2, 5<<11)),
		"two questions":       broken(set16(4, 2)),
		"no question count":   broken(set16(4, 0)),
		"an answer section":   broken(set16(6, 1)),
		"an authority":        broken(set16(8, 1)),
		"the chaos class":     broken(set16(len(good)-2, 3)),
		"a name compressed":   append(slices.Clone(good[:dnsHeaderLen]), 0xc0, 0x0c, 0, 1, 0, 1),
		"a label over 63":     rawQuery(strings.Repeat("a", 64), "com"),
		"a name over 253":     rawQuery(slices.Repeat([]string{strings.Repeat("a", 63)}, 5)...),
		"larger than a query": append(slices.Clone(good), make([]byte, maxQuery)...),
	} {
		if read, ok := readQuestion(q); ok {
			t.Errorf("%s: read as %q", name, read.name)
		}
	}
}

func TestARefusalAnswersTheQueryAsked(t *testing.T) {
	t.Parallel()

	q := aQuery(t, "evil.example.", dnsmessage.TypeA)

	read, ok := readQuestion(q)
	if !ok {
		t.Fatal("not read")
	}

	var p dnsmessage.Parser

	header, err := p.Start(refusal(q, read, true, dnsmessage.RCodeRefused))
	if err != nil {
		t.Fatalf("a client could not parse the refusal: %v", err)
	}

	if header.ID != 0x1234 || !header.Response || header.RCode != dnsmessage.RCodeRefused ||
		!header.RecursionDesired {
		t.Fatalf("header = %+v", header)
	}

	questions, err := p.AllQuestions()
	if err != nil || len(questions) != 1 || questions[0].Name.String() != "evil.example." {
		t.Fatalf("questions = %+v, %v", questions, err)
	}

	// UNREADABLE, IT ECHOES NOTHING: a guest's bytes are not worth repeating.
	unreadable := refusal(rawQuery("a b"), question{}, false, dnsmessage.RCodeFormatError)
	if len(unreadable) != dnsHeaderLen || binary.BigEndian.Uint16(unreadable[4:]) != 0 {
		t.Fatalf("an unreadable query was answered with %v", unreadable)
	}

	if got := refusal([]byte{1, 2}, question{}, false, dnsmessage.RCodeFormatError); got != nil {
		t.Fatalf("bytes with no header were answered with %v", got)
	}
}

func TestAnAnswerIsReadForItsChainAndAddresses(t *testing.T) {
	t.Parallel()

	q := aQuery(t, "www.example.com.", dnsmessage.TypeA)

	read, ok := readQuestion(q)
	if !ok {
		t.Fatal("not read")
	}

	v4, v6 := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")

	got, err := readAnswer(anAnswer(t, q, []string{"Edge.CDN.example."}, v4, v6), read)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(got.names, []string{"www.example.com", "edge.cdn.example"}) {
		t.Errorf("names = %v", got.names)
	}

	if !slices.Equal(got.addrs, []netip.Addr{v4, v6}) {
		t.Errorf("addrs = %v", got.addrs)
	}
}

// AN ANSWER TO ANOTHER QUESTION NEVER ADMITS AN ADDRESS.
func TestAnAnswerToAnotherQuestionIsRefused(t *testing.T) {
	t.Parallel()

	q := aQuery(t, "allowed.example.", dnsmessage.TypeA)

	read, ok := readQuestion(q)
	if !ok {
		t.Fatal("not read")
	}

	addr := netip.MustParseAddr("192.0.2.1")

	other := anAnswer(t, aQuery(t, "other.example.", dnsmessage.TypeA), nil, addr)
	if _, err := readAnswer(other, read); err == nil {
		t.Error("an answer to another name was read")
	}

	wrongID := anAnswer(t, q, nil, addr)
	binary.BigEndian.PutUint16(wrongID, 0x4321)

	if _, err := readAnswer(wrongID, read); err == nil {
		t.Error("an answer with another id was read")
	}

	if _, err := readAnswer(q, read); err == nil {
		t.Error("the query itself was read as its answer")
	}

	if _, err := readAnswer([]byte{1, 2, 3}, read); err == nil {
		t.Error("three bytes were read as an answer")
	}
}

// A CNAME IS THE AUTHORITATIVE SERVER'S TO CHOOSE, and a guest may hold that server,
// so one a terminal would act on is dropped and the rest of the answer kept.
func TestAnAnswersUnprintableNameIsNotKept(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		"edge.cdn.example":       true,
		"a_b-c.example":          true,
		"":                       false,
		"a b.example":            false,
		"a\x1b[0m.example":       false,
		"a\nb.example":           false,
		strings.Repeat("a", 254): false,
	} {
		if got := printableName(name); got != want {
			t.Errorf("printableName(%q) = %v, want %v", name, got, want)
		}
	}
}

// FuzzReadQuestion holds what a policy relies on of any bytes a guest sends: a name
// read is lower case, printable and bounded, and its section is inside the query.
func FuzzReadQuestion(f *testing.F) {
	f.Add(aQuery(f, "github.com.", dnsmessage.TypeA))
	f.Add(aQuery(f, "_dmarc.Example.COM.", dnsmessage.TypeTXT))
	f.Add(rawQuery("github.com"))
	f.Add(rawQuery("a\x1b[31m", "com"))
	f.Add(append(make([]byte, dnsHeaderLen), 0xc0, 0x0c))

	f.Fuzz(func(t *testing.T, q []byte) {
		read, ok := readQuestion(q)
		if !ok {
			return
		}

		if read.name != "" && !printableName(read.name) {
			t.Fatalf("read a name that is not printable: %q", read.name)
		}

		if read.name != strings.ToLower(read.name) || strings.HasSuffix(read.name, ".") {
			t.Fatalf("read a name not in its canonical form: %q", read.name)
		}

		if len(read.raw) > len(q)-dnsHeaderLen {
			t.Fatalf("a %d byte question section from a %d byte query", len(read.raw), len(q))
		}

		// AND ITS REFUSAL IS ALWAYS ONE A CLIENT CAN PARSE.
		var p dnsmessage.Parser
		if _, err := p.Start(refusal(q, read, true, dnsmessage.RCodeRefused)); err != nil {
			t.Fatalf("the refusal of %x does not parse: %v", q, err)
		}
	})
}

// FuzzReadAnswer holds that no bytes from upstream panic the reader, and that what
// it keeps of them is what a report may carry.
func FuzzReadAnswer(f *testing.F) {
	q := aQuery(f, "www.example.com.", dnsmessage.TypeA)
	f.Add(anAnswer(f, q, []string{"edge.example."}, netip.MustParseAddr("192.0.2.1")))
	f.Add(anAnswer(f, q, nil, netip.MustParseAddr("2001:db8::1")))
	f.Add(q)

	read, ok := readQuestion(q)
	if !ok {
		f.Fatal("not read")
	}

	f.Fuzz(func(t *testing.T, resp []byte) {
		got, err := readAnswer(resp, read)
		if err != nil {
			return
		}

		for _, name := range got.names {
			if !printableName(name) {
				t.Fatalf("kept a name that is not printable: %q", name)
			}
		}

		for _, addr := range got.addrs {
			if !addr.IsValid() {
				t.Fatalf("kept an address that is not one: %v", addr)
			}
		}
	})
}
