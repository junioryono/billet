package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
)

// uplinkSystemctl stands in for systemctl, answering `show` about the shaper's
// unit with the given states and refusing a restart, and records each call
// with the deadline it ran under.
type uplinkSystemctl struct {
	enabled, active string
	calls           [][]string
	deadlines       map[string]time.Time
}

func (s *uplinkSystemctl) run(ctx context.Context, _ string, args []string) ([]byte, error) {
	s.calls = append(s.calls, args)

	if d, ok := ctx.Deadline(); ok {
		s.deadlines[args[0]] = d
	}

	switch {
	case args[0] == "show" && args[len(args)-1] == uplinkUnit:
		return []byte("LoadState=loaded\nUnitFileState=" + s.enabled + "\nActiveState=" + s.active + "\n"), nil
	case args[0] == "restart":
		return nil, errors.New("Job for " + args[len(args)-1] + " failed")
	}

	return nil, errors.New("unexpected systemctl " + strings.Join(args, " "))
}

func uplinkHost(t *testing.T, s *uplinkSystemctl, out io.Writer) *systemdHost {
	t.Helper()

	s.deadlines = map[string]time.Time{}

	prev := newHostInspector
	newHostInspector = func() *lifeops.Inspector {
		return lifeops.NewInspector(lifeops.WithCommandRunner(s.run))
	}

	t.Cleanup(func() { newHostInspector = prev })

	env := cli.Env{Stdout: out, Stderr: io.Discard, Getenv: os.Getenv}

	return newSystemdHost(env, &config.Config{Node: &config.NodeConfig{}}, "/etc/billet/billet.yaml", "/tmp/staged", nil)
}

// THE TRANSACTION RESTARTS billet-uplink.service, under a bound of its own, and
// says so when the restart fails. Asserted at the systemctl the host runs, so a
// host that restarted another unit, or none, fails here.
func TestTheTransactionRestartsTheUplinkShaperUnderABound(t *testing.T) {
	s := &uplinkSystemctl{enabled: "enabled", active: "active"}
	h := uplinkHost(t, s, io.Discard)

	err := h.RestartUplink(t.Context())
	if err == nil || !strings.Contains(err.Error(), uplinkUnit) {
		t.Fatalf("a refused restart of the shaper answered %v; want an error naming %s", err, uplinkUnit)
	}

	if !slices.ContainsFunc(s.calls, func(c []string) bool {
		return slices.Equal(c, []string{"restart", "--", uplinkUnit})
	}) {
		t.Fatalf("the host never asked systemd to restart %s: %v", uplinkUnit, s.calls)
	}

	d, ok := s.deadlines["restart"]
	if !ok {
		t.Fatal("the shaper's restart ran under no deadline")
	}

	if remaining := time.Until(d); remaining > uplinkRestartBound || remaining < uplinkRestartBound-time.Minute {
		t.Fatalf("the shaper's restart ran under a deadline %s away; want %s", remaining.Round(time.Second),
			uplinkRestartBound)
	}
}

// A SHAPER AN OPERATOR DISABLED IS LEFT ALONE, and the transaction says so.
func TestTheTransactionLeavesADisabledUplinkShaperAlone(t *testing.T) {
	s := &uplinkSystemctl{enabled: "disabled", active: "active"}

	var out bytes.Buffer

	h := uplinkHost(t, s, &out)

	if err := h.RestartUplink(t.Context()); err != nil {
		t.Fatalf("a disabled shaper answered %v; want it left alone", err)
	}

	if slices.ContainsFunc(s.calls, func(c []string) bool { return c[0] == "restart" }) {
		t.Fatalf("a disabled shaper was restarted: %v", s.calls)
	}

	if want := "left " + uplinkUnit + " alone: not enabled"; !strings.Contains(out.String(), want) {
		t.Fatalf("the transaction said %q, want it to say %q", out.String(), want)
	}
}

// ON A MAC THERE IS NO SHAPER, and the transaction's step is nothing.
func TestALaunchdHostHasNoShaperToRestart(t *testing.T) {
	t.Parallel()

	if err := (&launchdHost{}).RestartUplink(t.Context()); err != nil {
		t.Fatalf("a Mac's shaper restart answered %v", err)
	}
}
