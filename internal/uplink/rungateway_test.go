package uplink

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// A RUN ASKS THE GATEWAY IT IS NAMED, NONE WHEN TOLD, OR THE DEFAULT ROUTE'S;
// and a gateway it cannot look up is said and left out, never a reason not to
// shape. Not parallel: procRoute is the package's.
func TestARunAsksTheGatewayItIsGivenOrTheDefaultRoutes(t *testing.T) {
	table := filepath.Join(t.TempDir(), "route")
	if err := os.WriteFile(table, []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"eno2np1\t00000000\tFE01A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := procRoute
	procRoute = table
	t.Cleanup(func() { procRoute = old })

	logs := &records{}
	log := slog.New(logs)

	for name, c := range map[string]struct {
		opts Options
		want string
	}{
		"the default route's":           {Options{Log: log}, "192.168.1.254"},
		"the one named":                 {Options{Log: log, Gateway: "10.0.0.1"}, "10.0.0.1"},
		"none":                          {Options{Log: log, NoGateway: true}, ""},
		"none, though one is named too": {Options{Log: log, NoGateway: true, Gateway: "10.0.0.1"}, ""},
	} {
		if got := runGateway(c.opts, "eno2np1"); got != c.want {
			t.Errorf("%s: the run asks %q, want %q", name, got, c.want)
		}
	}

	if got := runGateway(Options{Log: log}, "wlan0"); got != "" || logs.count("not asking the gateway") != 1 {
		t.Errorf("an interface with no default route asked %q and said so %d times",
			got, logs.count("not asking the gateway"))
	}
}
