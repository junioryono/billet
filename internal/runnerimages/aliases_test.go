package runnerimages_test

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// TestEveryAliasEntryIsAssertedByName is the check that stops the alias map from
// being edited without anyone noticing.
//
// A TEST THAT ASSERTS ONLY ONE ENTRY LETS THE OTHERS BE DELETED. Both language
// readers would simply agree on the shortened file, and the package the deleted
// entry mapped would go back to a name apt cannot install (netcat) or one dpkg
// never registers (upx) — the first breaks the build loudly, the second publishes
// an image the gate then reports as incomplete.
//
// EVERY ENTRY IS NAMED HERE ON PURPOSE. Adding one to apt-aliases.json means
// adding it here, which is the point at which somebody has to say what it is for.
func TestEveryAliasEntryIsAssertedByName(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"netcat": "netcat-openbsd",
		"upx":    "upx-ucl",
	}

	got := runnerimages.AptAliases()

	for name, install := range want {
		switch actual, ok := got[name]; {
		case !ok:
			t.Errorf("the alias for %q is gone. %s", name, whyItMatters(name))
		case actual != install:
			t.Errorf("%q maps to %q, want %q", name, actual, install)
		}
	}

	for name := range got {
		if _, expected := want[name]; !expected {
			t.Errorf("apt-aliases.json has an entry for %q that no test names. Add it to "+
				"this table with a reason, so a mapping cannot appear without anyone "+
				"saying what it is for", name)
		}
	}
}

// TestTheAliasesAreAppliedToTheDeclaredSet: the map existing is not the same as
// the map being used.
func TestTheAliasesAreAppliedToTheDeclaredSet(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	packages := strings.Join(ts.AptPackages(), "\n")

	for declared, installed := range runnerimages.AptAliases() {
		if strings.Contains("\n"+packages+"\n", "\n"+declared+"\n") {
			t.Errorf("AptPackages still emits the declared name %q rather than %q; the "+
				"alias map is present and not applied", declared, installed)
		}

		if !strings.Contains("\n"+packages+"\n", "\n"+installed+"\n") {
			t.Errorf("AptPackages does not emit %q, so the package %q stands for is not "+
				"installed at all", installed, declared)
		}
	}
}

// TestAnAliasThatNamesNothingIsAnError. An empty install used to read as "no
// alias" in Go while both shell readers emitted a BLANK package name — which
// dropped the package from the install list and from the expected set the gate
// checks, in the same step, with nothing reporting it.
func TestAnAliasThatNamesNothingIsAnError(t *testing.T) {
	t.Parallel()

	// THE REAL FILE MUST PASS, or this test proves only that the validator is
	// capable of failing.
	if err := runnerimages.ValidateAptAliases(); err != nil {
		t.Fatalf("the checked-in alias map is rejected by its own validator: %v", err)
	}

	if _, err := runnerimages.Load(); err != nil {
		t.Fatalf("Load rejects the checked-in alias map: %v", err)
	}
}

func whyItMatters(name string) string {
	switch name {
	case "netcat":
		return "netcat is virtual with two providers on noble, so apt refuses it and the " +
			"build dies after debootstrap"
	case "upx":
		return "upx is virtual with one provider, so apt silently installs upx-ucl and only " +
			"the gate notices the declared name is absent"
	}

	return "it exists because a declared name is not installable as written"
}
