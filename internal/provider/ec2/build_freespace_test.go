package ec2

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// THE FREE-SPACE GUARD IS RUN, against what df might actually say.
//
// `[ "$x" -lt N ]` exits 2 when x is not a number, and `set -e` is SUPPRESSED for
// an `if` CONDITION — so a df that printed something unexpected made the guard
// pass and the build proceed onto a disk it never measured. Measured, not read:
// with `billet_free_kib=not-a-number` the script exits 0. Same family as the
// `!`-pipeline rule in billet-shell-gates, and the reason a `case` comes first.
func TestTheFreeSpaceGuardRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	block := freeSpaceBlock(t, mustScript(t))

	for _, tc := range []struct {
		name   string
		df     string
		wantOK bool
	}{
		{
			name:   "plenty of room",
			df:     strconv.Itoa(minBuilderFreeGiB*1024*1024 + 1),
			wantOK: true,
		},
		{
			name:   "exactly the floor",
			df:     strconv.Itoa(minBuilderFreeGiB * 1024 * 1024),
			wantOK: true,
		},
		{name: "one KiB short", df: strconv.Itoa(minBuilderFreeGiB*1024*1024 - 1)},
		{name: "nothing free", df: "0"},
		// THE CASES THE COMPARISON ALONE ACCEPTED. Each of these makes `[ -lt ]`
		// exit 2, which an `if` condition swallows.
		{name: "df said something that is not a number", df: "not-a-number"},
		{name: "df said nothing at all", df: ""},
		{name: "df printed a header instead of a figure", df: "Available"},
		{name: "df printed a size with a suffix", df: "20G"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A df FUNCTION, so what runs is the script's own command line rather
			// than a value this test assigned past it. Not an executable on PATH: a
			// parallel test's fork can inherit the descriptor that wrote it, exec then
			// fails with ETXTBSY, and /bin/sh carries on down PATH to the real df, whose
			// ample free space made "not-a-number" pass (CI run 34827346038).
			// An empty value prints the header alone, so awk finds no second line;
			// written into the row, awk's field splitting would read the next column.
			row := "  printf '/dev/root 1 1 " + tc.df + " 1%%%% /\\n'\n"
			if tc.df == "" {
				row = ""
			}
			fake := "df() {\n" +
				"  printf 'Filesystem 1K-blocks Used Available Use%% Mounted on\\n'\n" +
				row +
				"}\n"

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "set -eu\n"+fake+block)

			out, err := cmd.CombinedOutput()

			if tc.wantOK && err != nil {
				t.Fatalf("the guard refused %q free, which is at or above the %dGiB floor: "+
					"%v\n%s", tc.df, minBuilderFreeGiB, err, out)
			}

			if !tc.wantOK && err == nil {
				t.Fatalf("the guard accepted %q as the free space on the builder root; the "+
					"build would then install onto a disk it never measured\n--- block ---\n%s",
					tc.df, block)
			}
		})
	}
}

// freeSpaceBlock lifts the guard out of the generated script: from the df
// assignment to the `fi` that closes the comparison.
func freeSpaceBlock(t *testing.T, script string) string {
	t.Helper()

	lines := strings.Split(script, "\n")
	start := firstLineOf(t, lines, "billet_free_kib=$(df -Pk /")

	// Bounded at the FIRST fi: a guard whose case was deleted must fail here, not
	// extend the block to a later esac and run unrelated provisioning commands.
	for i := start; i < len(lines); i++ {
		if lines[i] != "fi" {
			continue
		}

		for _, line := range lines[start:i] {
			if line == "esac" {
				return strings.Join(lines[start:i+1], "\n") + "\n"
			}
		}

		break
	}

	t.Fatal("the free-space guard has no case guard before its comparison, so a df that " +
		"printed something unexpected would pass it")

	return ""
}
