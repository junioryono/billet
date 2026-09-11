package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// THE FIXTURES FROM THEIR PRODUCERS: every JSON a role consumes is committed
// under the collection's tests/fixtures/<command>/ by the Go test that runs
// the command over a planted shape, with the host-specific spellings (paths,
// identifiers, digests, times) replaced by fixed ones so the file is the same
// on every machine. A gate's fake answers from those files and never from a
// shape written by hand, and this comparison proves the committed bytes are
// what the command prints today (BILLET_UPDATE_FIXTURES=1 rewrites them).

// fixturesRoot is the collection's fixture directory.
var fixturesRoot = filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "tests", "fixtures")

// updateFixtures says whether this run writes the fixtures instead of
// comparing them.
func updateFixtures() bool { return os.Getenv("BILLET_UPDATE_FIXTURES") == "1" }

// compareFixture compares the normalised output of a producer with the
// committed fixture <command>/<name>.json, or writes it under
// BILLET_UPDATE_FIXTURES=1. The output must be JSON.
func compareFixture(t *testing.T, command, name, out string) {
	t.Helper()

	var parsed any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("%s/%s: the answer is not JSON: %v\n%s", command, name, err, out)
	}

	dir := filepath.Join(fixturesRoot, command)
	path := filepath.Join(dir, name+".json")

	if updateFixtures() {
		mustOK(t, os.MkdirAll(dir, 0o755))
		mustOK(t, os.WriteFile(path, []byte(out), 0o644))

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s/%s: %v (run with BILLET_UPDATE_FIXTURES=1 to write the fixtures)", command, name, err)
	}

	if string(want) != out {
		t.Errorf("%s/%s: the fixture differs from the command's answer:\n--- fixture\n%s\n--- command\n%s", command, name,
			want, out)
	}
}

// fixtureSetIs proves the committed set under <command>/ is exactly the
// producers' names: a stale file a gate might still read, or a file no
// producer writes, fails.
func fixtureSetIs(t *testing.T, command string, names []string) {
	t.Helper()

	if updateFixtures() {
		return
	}

	entries, err := os.ReadDir(filepath.Join(fixturesRoot, command))
	if err != nil {
		t.Fatalf("%s: %v", command, err)
	}

	var got []string

	for _, e := range entries {
		got = append(got, strings.TrimSuffix(e.Name(), ".json"))
	}

	want := append([]string(nil), names...)
	sort.Strings(got)
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: the committed fixtures are %v, the producers write %v", command, got, want)
	}
}

// inodeOf is a file's inode, through a checked assertion.
func inodeOf(t *testing.T, info os.FileInfo) uint64 {
	t.Helper()

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s carries no Stat_t", info.Name())
	}

	return st.Ino
}

// asReport is a known maybe's receipt report.
func asReport(t *testing.T, m maybe) receiptReport {
	t.Helper()

	if !m.known {
		t.Fatalf("the receipt is unknown: %s", m.why)
	}

	r, ok := m.value.(receiptReport)
	if !ok {
		t.Fatalf("the receipt is a %T", m.value)
	}

	return r
}

// epochOf is a JSON number as an epoch.
func epochOf(t *testing.T, v any) int64 {
	t.Helper()

	n, ok := v.(float64)
	if !ok {
		t.Fatalf("epoch %v is a %T", v, v)
	}

	return int64(n)
}

// mustMarshal encodes v or fails the test.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()

	body, err := json.Marshal(v)
	mustOK(t, err)

	return body
}

// fixtureNames is the sorted key set of a shapes map.
func fixtureNames[V any](shapes map[string]V) []string {
	names := make([]string, 0, len(shapes))

	for name := range shapes {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
