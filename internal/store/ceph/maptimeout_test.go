package ceph

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A MAP GETS MapTimeout AND EVERYTHING ELSE KEEPS DefaultTimeout. A map waits for
// the kernel client and udev, which slow with the host's load: measured at 17.83s
// through a relaunch burst on 2026-10-03, past the ordinary bound.
func TestAMapIsBoundedByMapTimeoutAndNothingElseIs(t *testing.T) {
	t.Parallel()

	budgets := map[string][]time.Duration{}
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(func(ctx context.Context, _ string, args []string) ([]byte, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Errorf("%v ran with no deadline", args)

				return nil, nil
			}

			verb := "other"
			if slices.Contains(args, "device") && slices.Contains(args, "map") {
				verb = "map"
			}
			budgets[verb] = append(budgets[verb], time.Until(deadline))

			return []byte("/dev/rbd3\n"), nil
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.rbdMap(t.Context(), "billet-cache/x"); err != nil {
		t.Fatalf("rbdMap: %v", err)
	}
	if _, err := c.rbdCmd(t.Context(), false, "info", "billet-cache/x"); err != nil {
		t.Fatalf("rbdCmd: %v", err)
	}

	// EACH SEEN ONCE, AND EACH WITH ITS OWN BOUND less a second for scheduling, so
	// a missing call and a shortened bound both fail.
	if got := budgets["map"]; len(got) != 1 || got[0] < MapTimeout-time.Second || got[0] > MapTimeout {
		t.Errorf("map budgets %v, want one of about MapTimeout (%s)", got, MapTimeout)
	}
	if got := budgets["other"]; len(got) != 1 || got[0] < DefaultTimeout-time.Second || got[0] > DefaultTimeout {
		t.Errorf("ordinary rbd budgets %v, want one of about DefaultTimeout (%s)", got, DefaultTimeout)
	}
}

// EVERY MAP GOES THROUGH rbdMap. A map spelled out on rbdCmd would take the
// fifteen-second bound the measurement above rules out, and compile.
func TestNoMapBypassesRbdMap(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	maps := map[string]int{}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}

		count := strings.Count(string(body), `"device", "map"`)
		want := 0
		if name == "ceph.go" {
			want = 1
		}
		if count != want {
			t.Errorf("%s spells out `device map` %d time(s), want %d: every map goes through rbdMap",
				name, count, want)
		}

		maps[name] = strings.Count(string(body), "c.rbdMap(")
	}

	// AND THE THREE PLACES THAT MAP DO SO THROUGH IT, so a map moved out of
	// sight of the string search above still fails here.
	for _, name := range []string{"cache.go", "clone.go", "importer.go"} {
		if maps[name] == 0 {
			t.Errorf("%s no longer maps through rbdMap", name)
		}
	}
}
