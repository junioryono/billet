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

	budgets := map[string]time.Duration{}
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
			budgets[verb] = time.Until(deadline)

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

	if got := budgets["map"]; got <= DefaultTimeout || got > MapTimeout {
		t.Errorf("a map ran with %s left, want more than DefaultTimeout (%s) and at most MapTimeout (%s)",
			got, DefaultTimeout, MapTimeout)
	}
	if got := budgets["other"]; got > DefaultTimeout {
		t.Errorf("an ordinary rbd call ran with %s left, want at most DefaultTimeout (%s)", got, DefaultTimeout)
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
	}
}
