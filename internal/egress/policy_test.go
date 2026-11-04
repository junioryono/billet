package egress

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestAListAllowsWhatItNamesAndNothingBeside(t *testing.T) {
	t.Parallel()

	p, err := Compile([]string{"Exact.example.com", "*.below.example.com", "203.0.113.0/24"})
	if err != nil {
		t.Fatal(err)
	}

	if !p.Listed() {
		t.Fatal("a compiled list is not listed")
	}

	for name, want := range map[string]bool{
		"exact.example.com":             true,
		"www.exact.example.com":         false,
		"example.com":                   false,
		"x.below.example.com":           true,
		"a.b.below.example.com":         true,
		"below.example.com":             false,
		"xbelow.example.com":            false,
		"exact.example.com.attacker.io": false,
		"below.example.com.attacker.io": false,
		"":                              false,
	} {
		if got := p.AllowsName(name); got != want {
			t.Errorf("AllowsName(%q) = %v, want %v", name, got, want)
		}
	}

	if len(p.prefixes) != 1 || p.prefixes[0] != netip.MustParsePrefix("203.0.113.0/24") {
		t.Errorf("prefixes = %v", p.prefixes)
	}
}

// EVERY LIST INCLUDES WHAT A RUNNER NEEDS, an empty one too, or a tier with a list
// would be a queue of jobs no runner can take.
func TestEveryListIncludesWhatARunnerNeeds(t *testing.T) {
	t.Parallel()

	p, err := Compile(nil)
	if err != nil {
		t.Fatal(err)
	}

	if !p.Listed() {
		t.Fatal("an empty list is not listed")
	}

	for _, entry := range config.GitHubRunnerEndpoints() {
		name := strings.Replace(entry, "*.", "some.", 1)
		if !p.AllowsName(name) {
			t.Errorf("%s is refused", name)
		}
	}

	if p.AllowsName("example.com") {
		t.Error("an empty list allows example.com")
	}
}

func TestAListThatCannotBeReadIsNotCompiled(t *testing.T) {
	t.Parallel()

	if _, err := Compile([]string{"example.com", "not a name"}); err == nil {
		t.Fatal("a bad entry compiled, and the guest would have run under part of its list")
	}
}

func TestAnOpenGuestMayResolveAnything(t *testing.T) {
	t.Parallel()

	p := Observe()
	if p.Listed() || !p.AllowsName("anything.example") {
		t.Fatalf("Observe: listed %v", p.Listed())
	}

	var none *Policy
	if none.Listed() || !none.AllowsName("anything.example") {
		t.Fatal("no policy read as a list")
	}
}
