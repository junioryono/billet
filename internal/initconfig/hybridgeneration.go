package initconfig

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// THE GENERATION FINGERPRINT BINDS THE RENDERS TO THE ROOT THAT WROTE THE
// OUTPUTS (#62). The plan render mints it, the generated root declares it as an
// output, and every later render carries it on its command line and compares it
// with what `terraform output -json` says. A name and a region cannot do that:
// two generations sharing both, in different accounts, are otherwise
// indistinguishable.
//
// Sixty-four random bits in four groups, because a person reads it in the
// runbook and retypes or pastes it; the groups make a transposition visible.

// hybridGenerationPattern is the one shape a fingerprint has.
var hybridGenerationPattern = regexp.MustCompile(`^[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}$`)

// hybridGenerationDeclaration finds an output block named generation, its
// label quoted or bare (HCL allows both), in a root whose comments and
// heredocs are already masked.
var hybridGenerationDeclaration = regexp.MustCompile(`\boutput\s+(?:"` + HybridOutputGeneration + `"|` + HybridOutputGeneration + `\b)\s*\{`)

// hybridValueAttribute finds a block's value attribute, up to the token after
// the equals sign.
var hybridValueAttribute = regexp.MustCompile(`(?m)^\s*value\s*=\s*`)

// NewHybridGeneration mints a fingerprint.
func NewHybridGeneration() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint the generation fingerprint: %w", err)
	}

	h := hex.EncodeToString(b[:])

	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16], nil
}

// CheckHybridGeneration refuses anything that is not a fingerprint.
func CheckHybridGeneration(g string) error {
	if !hybridGenerationPattern.MatchString(g) {
		return fmt.Errorf("--generation %q is not a generation fingerprint (four groups of four "+
			"lowercase hex digits, as the runbook prints it)", g)
	}

	return nil
}

// HybridGenerationOf reads the fingerprint a rendered terraform/main.tf
// declares. ok is false when the root declares none, which is a root written
// before the fingerprint existed or one an operator rewrote without it.
//
// A DECLARATION BILLET CANNOT READ IS AN ERROR, NEVER AN ABSENCE: a value that
// is not one plain string literal (null, a number, an expression, a template,
// an unterminated string), a second declaration, or a malformed fingerprint.
// Reading any of those as absent let a re-run of the plan render mint another
// fingerprint over the root, and let --generation pass a root that declares
// something else.
//
// NOT AN HCL PARSER, and the module carries none, so the scan is lexical: it
// sets aside comments (`#`, `//`, `/* */`), heredocs and the contents of string
// literals before it looks for the declaration, so a block commented out or
// quoted inside a description is not one, and a comment inside the declaration
// does not hide it.
func HybridGenerationOf(mainTF string) (string, bool, error) {
	// TWO VIEWS: names survive in uncommented (comments and heredocs blanked),
	// which is where the declaration is found; masked also blanks every
	// literal's contents, so a brace or an attribute inside a string is not
	// counted when the block is read.
	uncommented, masked, literals, complete := maskHCL(mainTF)
	if !complete {
		// AN UNTERMINATED STRING OR COMMENT HIDES EVERYTHING AFTER IT, so a
		// declaration there is could-not-tell, which is never "none".
		return "", false, fmt.Errorf("the root has an unterminated string, template, comment or " +
			"heredoc, or a << that is not a heredoc, so whether it declares a generation cannot be read")
	}

	// A STRING CANNOT HIDE ONE: inside a literal the quotes around the name
	// would have to be escaped, and an escaped quote does not match.
	live := hybridGenerationDeclaration.FindAllStringIndex(uncommented, -1)

	switch len(live) {
	case 0:
		return "", false, nil
	case 1:
	default:
		return "", false, fmt.Errorf("the root declares the %q output %d times, so which generation "+
			"it belongs to cannot be read", HybridOutputGeneration, len(live))
	}

	unreadable := fmt.Errorf("the root declares a %q output whose value is not the quoted "+
		"fingerprint billet renders, so which generation it belongs to cannot be read; restore "+
		"it as `value = \"xxxx-xxxx-xxxx-xxxx\"`, or move the root aside and render the plan again",
		HybridOutputGeneration)

	open := live[0][1] - 1
	end := closingBrace(masked, open)
	if end < 0 {
		return "", false, unreadable
	}

	values := hybridValueAttribute.FindAllStringIndex(masked[open+1:end], -1)
	if len(values) != 1 {
		return "", false, unreadable
	}

	start := open + 1 + values[0][1]
	lit, ok := literalAt(literals, start)
	if !ok {
		return "", false, unreadable
	}

	// NOTHING BUT THE LITERAL ON ITS LINE: `"a" + "b"` or a trailing operator is
	// an expression, whose value only terraform knows.
	if rest := strings.TrimSpace(lineRest(masked, lit.end+1)); rest != "" {
		return "", false, unreadable
	}

	value := mainTF[lit.start+1 : lit.end]
	if strings.ContainsAny(value, `\`) || strings.Contains(value, "${") || strings.Contains(value, "%{") {
		return "", false, unreadable
	}

	if err := CheckHybridGeneration(value); err != nil {
		return "", false, fmt.Errorf("the root's %q output: %w", HybridOutputGeneration, err)
	}

	return value, true, nil
}

// hclLiteral is one quoted string's span: the offsets of its opening and
// closing quotes.
type hclLiteral struct{ start, end int }

// maskHCL returns src with comments and heredoc bodies blanked (keeping
// newlines, so offsets and lines survive), the same with every quoted string
// literal's contents blanked as well, the literals' spans, and whether the
// whole file was settled: an unterminated string, template, comment or
// heredoc, or a `<<` that is not a heredoc, is not.
func maskHCL(src string) (string, string, []hclLiteral, bool) {
	s := &hclScan{src: src, out: []byte(src)}

	complete := true
	for i := 0; i < len(src); {
		next, ok := s.skip(i)
		if !ok {
			complete = false

			break
		}
		i = next
	}

	uncommented := string(s.out)
	for _, c := range s.contents {
		s.blank(c[0], c[1])
	}

	return uncommented, string(s.out), s.literals, complete
}

// hclScan is one lexical pass over a root.
type hclScan struct {
	src      string
	out      []byte
	literals []hclLiteral
	contents [][2]int
}

// blank replaces out[from:to] with spaces, keeping newlines.
func (s *hclScan) blank(from, to int) {
	for i := from; i < to && i < len(s.out); i++ {
		if s.out[i] != '\n' {
			s.out[i] = ' '
		}
	}
}

// skip reads the construct at i (a comment, a heredoc, a string, or one other
// byte) and returns the offset after it; false is a construct that cannot be
// settled.
func (s *hclScan) skip(i int) (int, bool) {
	src := s.src

	switch {
	case src[i] == '#' || strings.HasPrefix(src[i:], "//"):
		end := len(src)
		if nl := strings.IndexByte(src[i:], '\n'); nl >= 0 {
			end = i + nl
		}
		s.blank(i, end)

		return end, true

	case strings.HasPrefix(src[i:], "/*"):
		end := strings.Index(src[i+2:], "*/")
		if end < 0 {
			return 0, false
		}
		s.blank(i, i+2+end+2)

		return i + 2 + end + 2, true

	case strings.HasPrefix(src[i:], "<<"):
		return s.heredoc(i)

	case src[i] == '"':
		return s.str(i)
	}

	return i + 1, true
}

// str reads a quoted string at i, its templates included, and records it.
//
// A TEMPLATE IS AN EXPRESSION, and its strings are strings of their own:
// "${"/*"}" holds a quote that does not close the outer string, so a scan that
// stopped there would read the /* after it as a comment and swallow what
// follows.
func (s *hclScan) str(i int) (int, bool) {
	src := s.src

	for j := i + 1; j < len(src); {
		switch {
		case src[j] == '\n':
			return 0, false
		case src[j] == '\\':
			j += 2
		case src[j] == '"':
			s.literals = append(s.literals, hclLiteral{start: i, end: j})
			s.contents = append(s.contents, [2]int{i + 1, j})

			return j + 1, true
		case strings.HasPrefix(src[j:], "$${") || strings.HasPrefix(src[j:], "%%{"):
			j += 3
		case strings.HasPrefix(src[j:], "${") || strings.HasPrefix(src[j:], "%{"):
			next, ok := s.template(j + 2)
			if !ok {
				return 0, false
			}
			j = next
		default:
			j++
		}
	}

	return 0, false
}

// template reads a template's expression from just after its `${` or `%{` to
// just after the brace that closes it, nested braces, strings, comments and
// heredocs included.
func (s *hclScan) template(i int) (int, bool) {
	depth := 1

	for i < len(s.src) {
		switch s.src[i] {
		case '{':
			depth++
			i++
		case '}':
			depth--
			i++
			if depth == 0 {
				return i, true
			}
		default:
			next, ok := s.skip(i)
			if !ok {
				return 0, false
			}
			i = next
		}
	}

	return 0, false
}

// heredocHeader is a heredoc's opening: "<<" or "<<-", an identifier (which
// HCL lets carry hyphens), and the end of its line, CRLF included.
var heredocHeader = regexp.MustCompile(`^<<-?([A-Za-z_][A-Za-z0-9_-]*)\r?\n`)

// heredoc blanks a heredoc's body at i through its closing line and returns
// the offset after it. HCL has no other `<<`, so anything at "<<" that is not
// a heredoc header, and a heredoc that never closes, cannot be settled.
func (s *hclScan) heredoc(i int) (int, bool) {
	m := heredocHeader.FindStringSubmatch(s.src[i:])
	if m == nil {
		return 0, false
	}

	ident := m[1]
	body := i + len(m[0])

	for line := body; line < len(s.src); {
		lineEnd := len(s.src)
		if nl := strings.IndexByte(s.src[line:], '\n'); nl >= 0 {
			lineEnd = line + nl
		}
		// TrimSpace takes a CRLF's \r and the indentation "<<-" allows.
		if strings.TrimSpace(s.src[line:lineEnd]) == ident {
			s.blank(body, line)

			return lineEnd, true
		}
		if lineEnd == len(s.src) {
			break
		}
		line = lineEnd + 1
	}

	return 0, false
}

// literalAt is the literal that opens exactly at offset.
func literalAt(literals []hclLiteral, offset int) (hclLiteral, bool) {
	for _, l := range literals {
		if l.start == offset {
			return l, true
		}
	}

	return hclLiteral{}, false
}

// closingBrace is the offset of the brace closing the one at open, in masked
// text where comments and literals no longer carry braces, or -1.
func closingBrace(masked string, open int) int {
	depth := 0
	for i := open; i < len(masked); i++ {
		switch masked[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// lineRest is the text from offset to the end of its line.
func lineRest(s string, offset int) string {
	if offset >= len(s) {
		return ""
	}
	if end := strings.IndexByte(s[offset:], '\n'); end >= 0 {
		return s[offset : offset+end]
	}

	return s[offset:]
}
