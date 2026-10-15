package flows

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func switchRoot(t *testing.T, values map[string]string) (string, string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "proc", "sys", "net", "netfilter")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	for name, v := range values {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return root, dir
}

func readSwitch(t *testing.T, dir, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(raw))
}

// NOTHING IS WRITTEN UNLESS WRITING CAN MAKE THE HOST READY: a switch that is
// off beside one that is absent or holds something else is left off.
func TestEnableChangesNothingOnAHostItCannotMakeReady(t *testing.T) {
	t.Parallel()

	for name, values := range map[string]map[string]string{
		"timestamps absent": {"nf_conntrack_acct": "0\n"},
		"timestamps other":  {"nf_conntrack_acct": "0\n", "nf_conntrack_timestamp": "2\n"},
		"accounting other":  {"nf_conntrack_acct": "x\n", "nf_conntrack_timestamp": "0\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, dir := switchRoot(t, values)

			wrote := false
			on, err := enable(root, func(string, []byte, os.FileMode) error { wrote = true; return nil })
			if err != nil || len(on) != 0 || wrote {
				t.Fatalf("enable = %v, %v (wrote %v); want nothing turned on and nothing written", on, err, wrote)
			}

			for file, v := range values {
				if got := readSwitch(t, dir, file); got != strings.TrimSpace(v) {
					t.Errorf("%s reads %q, want it left at %q", file, got, strings.TrimSpace(v))
				}
			}
		})
	}
}

// A WRITE THAT FAILS AFTER ANOTHER SUCCEEDED ANSWERS BOTH: the switch it turned
// on, and the one it could not, whatever user the test runs as.
func TestEnableSaysWhatItTurnedOnWhenALaterWriteFails(t *testing.T) {
	t.Parallel()

	root, dir := switchRoot(t, map[string]string{"nf_conntrack_acct": "0\n", "nf_conntrack_timestamp": "0\n"})

	on, err := enable(root, func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == "nf_conntrack_timestamp" {
			return errors.New("read-only file system")
		}

		return os.WriteFile(path, data, mode)
	})

	if !slices.Equal(on, []string{"nf_conntrack_acct"}) || err == nil ||
		!strings.Contains(err.Error(), "turn on net.netfilter.nf_conntrack_timestamp: read-only file system") {
		t.Fatalf("enable = %v, %v; want acct turned on and the timestamp's failure named", on, err)
	}

	if readSwitch(t, dir, "nf_conntrack_acct") != "1" || readSwitch(t, dir, "nf_conntrack_timestamp") != "0" {
		t.Error("the switches do not read what enable answered")
	}
}
