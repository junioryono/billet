package ec2

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// guestReadsWithoutAssignment is every name the boot script and the guest caches'
// library read bare, under -u, that neither assigns: each is set by something outside
// them, and says what.
var guestReadsWithoutAssignment = map[string]string{
	// The boot script exports it from the instance's user data before it runs the
	// entry point; a launch without it is refused before the guest starts.
	jitEnvVar: "the runner's registration, exported by the boot script",
}

// shellText is the part of a script the shell itself expands: comment lines, the bodies
// of quoted here-documents and single-quoted strings are text it writes or hands to
// another program, never expansions of its own.
func shellText(script string) string {
	var out []string
	heredoc := regexp.MustCompile(`<<-?'([A-Za-z_][A-Za-z0-9_]*)'`)
	quoted := regexp.MustCompile(`'[^'\n]*'`)
	end := ""
	for _, line := range strings.Split(script, "\n") {
		if end != "" {
			if strings.TrimLeft(line, "\t") == end {
				end = ""
			}

			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := heredoc.FindStringSubmatch(line); m != nil {
			end = m[1]
		}
		out = append(out, quoted.ReplaceAllString(line, "''"))
	}

	return strings.Join(out, "\n")
}

// unassignedReads is every name text reads bare (with no default) that it never assigns.
func unassignedReads(text string) []string {
	assigned := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)(?:^|[\s;(&|])(?:local\s+|export\s+|readonly\s+|declare\s+(?:-\w+\s+)*)?([A-Za-z_][A-Za-z0-9_]*)\+?=`).FindAllStringSubmatch(text, -1) {
		assigned[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in`).FindAllStringSubmatch(text, -1) {
		assigned[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`read\s+(?:-[a-zA-Z]+\s+)*([A-Za-z_][A-Za-z0-9_ ]*)`).FindAllStringSubmatch(text, -1) {
		for _, name := range strings.Fields(m[1]) {
			assigned[name] = true
		}
	}
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$(\{?)([A-Za-z_][A-Za-z0-9_]*)([:=+\-]?)`).FindAllStringSubmatch(text, -1) {
		braced, name, next := m[1] == "{", m[2], m[3]
		if braced && next != "" {
			continue
		}
		if !assigned[name] {
			found[name] = true
		}
	}
	var out []string
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}

// EVERY NAME THE GUEST'S BOOT READS IS ONE IT ASSIGNS, or one something else provides and
// this test names: the boot script and the library run under -u, and the library is the
// firecracker agent's shared regions, so a variable the agent sets outside them is
// unbound here. That stopped every runner of the images built on 2026-11-15
// (`registry_mirrors_ca_file: unbound variable`), and nothing else would have said so
// before a job sat queued.
func TestEveryNameTheGuestLibraryReadsIsAssigned(t *testing.T) {
	t.Parallel()
	library, err := guestCachesLibrary()
	if err != nil {
		t.Fatal(err)
	}
	reads := unassignedReads(shellText(entryPointScript() + "\n" + library))
	var unbound []string
	for _, name := range reads {
		if _, ok := guestReadsWithoutAssignment[name]; !ok {
			unbound = append(unbound, name)
		}
	}
	if len(unbound) > 0 {
		t.Errorf("the guest's boot reads %v, which nothing in it assigns: under -u the first one ends the "+
			"boot before the runner starts; declare it in entryPointScript or read it with a default", unbound)
	}
	// AND THE LIST HOLDS ONLY WHAT IS STILL READ, so a name cannot stay excused after its
	// read is gone and another takes its place unnoticed.
	for name := range guestReadsWithoutAssignment {
		if !slices.Contains(reads, name) {
			t.Errorf("%s is excused, and the boot no longer reads it bare", name)
		}
	}
}

// THE SCAN SEES WHAT THE SHELL EXPANDS AND NOTHING ELSE, and finds an unbound read in it.
func TestTheUnboundScanReadsOnlyTheShellsOwnExpansions(t *testing.T) {
	t.Parallel()
	script := "set -u\n" +
		"a=1\n" +
		"echo \"$a ${b:-} ${c-x}\"\n" +
		"# a comment naming $comment\n" +
		"jq '.x = $jqvar' file\n" +
		"cat >f <<'HOOK'\n" +
		"echo \"$hookvar\"\n" +
		"HOOK\n" +
		"for d in 1 2; do echo \"$d\"; done\n" +
		"read -r e f <x\n" +
		"echo \"$e $f ${unbound}\" \"$alsounbound\"\n"
	if got, want := unassignedReads(shellText(script)), []string{"alsounbound", "unbound"}; !slices.Equal(got, want) {
		t.Fatalf("the scan found %v, want %v", got, want)
	}
}
