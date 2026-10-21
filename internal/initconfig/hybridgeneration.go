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

// hybridGenerationDeclaration finds an output block named generation in a root
// whose comments and heredocs are already masked.
var hybridGenerationDeclaration = regexp.MustCompile(`\boutput\s+"` + HybridOutputGeneration + `"\s*\{`)

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
		return "", false, fmt.Errorf("the root has an unterminated string or comment, so whether it " +
			"declares a generation cannot be read")
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
// scan reached the end without an unterminated string or block comment.
func maskHCL(src string) (string, string, []hclLiteral, bool) {
	out := []byte(src)
	var (
		literals []hclLiteral
		contents [][2]int
	)

	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}

	both := func() (string, string) {
		uncommented := string(out)
		for _, c := range contents {
			blank(c[0], c[1])
		}

		return uncommented, string(out)
	}

	for i := 0; i < len(src); {
		switch {
		case src[i] == '#' || strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			blank(i, i+end)
			i += end

		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				blank(i, len(src))
				uncommented, masked := both()

				return uncommented, masked, literals, false
			}
			blank(i, i+2+end+2)
			i += 2 + end + 2

		case strings.HasPrefix(src[i:], "<<"):
			if i = maskHeredoc(src, i, blank); i < 0 {
				uncommented, masked := both()

				return uncommented, masked, literals, false
			}

		case src[i] == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) || src[j] != '"' {
				// UNTERMINATED: no literal, and nothing after it is trusted.
				blank(i, len(src))
				uncommented, masked := both()

				return uncommented, masked, literals, false
			}
			literals = append(literals, hclLiteral{start: i, end: j})
			contents = append(contents, [2]int{i + 1, j})
			i = j + 1

		default:
			i++
		}
	}

	uncommented, masked := both()

	return uncommented, masked, literals, true
}

// maskHeredoc blanks a heredoc starting at src[i] ("<<" or "<<-" then an
// identifier and a newline) through its closing line, and returns the offset
// after it, or -1 for a heredoc that never closes; anything else at "<<" is
// left alone.
func maskHeredoc(src string, i int, blank func(int, int)) int {
	j := i + 2
	if j < len(src) && src[j] == '-' {
		j++
	}

	k := j
	for k < len(src) && (src[k] == '_' || src[k] >= 'a' && src[k] <= 'z' || src[k] >= 'A' && src[k] <= 'Z' || src[k] >= '0' && src[k] <= '9') {
		k++
	}

	ident := src[j:k]
	if ident == "" || k >= len(src) || src[k] != '\n' {
		return i + 2
	}

	for line := k + 1; line < len(src); {
		next := strings.IndexByte(src[line:], '\n')
		lineEnd := len(src)
		if next >= 0 {
			lineEnd = line + next
		}
		if strings.TrimSpace(src[line:lineEnd]) == ident {
			blank(k+1, line)

			return lineEnd
		}
		if next < 0 {
			break
		}
		line = lineEnd + 1
	}

	blank(k+1, len(src))

	return -1
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
