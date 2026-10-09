package endpoint

import "testing"

// THE CANONICAL SPELLING IS A FIXED POINT. Whatever spelling Parse accepts,
// the endpoint's String is accepted by ParseCanonical and by Parse under the
// same TLS state, and both read back the same endpoint: the one representation
// the node's request base, its record and the inspector's comparison share
// cannot drift between a write and the next read.
func FuzzEndpointParse(f *testing.F) {
	for _, seed := range []string{
		"10.0.0.4:7717", "plane.example:7717", "https://plane.example", "http://127.0.0.1",
		"[::1]:7717", "[::ffff:192.0.2.1]:8443", "[fe80::1%25eth0]:7717", "PLANE.Example.:443",
		"plane:", "https://plane/path", "user@plane:1", "plane:99999", "plane:0", "plane:07717",
		"https://[::1]", "", "://", "plane#", "plane?", "xn--nxasmq6b.example:1",
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, input string, tls bool) {
		e, err := Parse(input, tls)
		if err != nil {
			return
		}

		text := e.String()

		canonical, err := ParseCanonical(text)
		if err != nil {
			t.Fatalf("Parse(%q, %v) gave %q, which ParseCanonical refuses: %v", input, tls, text, err)
		}

		if canonical != e {
			t.Fatalf("Parse(%q, %v) = %+v but its canonical spelling %q reads back as %+v",
				input, tls, e, text, canonical)
		}

		again, err := Parse(text, tls)
		if err != nil || again != e {
			t.Fatalf("Parse(%q, %v) = %+v but Parse of its spelling %q = %+v, %v", input, tls, e, text, again, err)
		}
	})
}
