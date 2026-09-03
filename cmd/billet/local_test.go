package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
)

// stubInspect answers with a fixed report for the duration of a test.
func stubInspect(t *testing.T, report lifeops.Report) {
	t.Helper()

	prev := inspect
	inspect = func(context.Context, string, string) (lifeops.Report, error) { return report, nil }
	t.Cleanup(func() { inspect = prev })
}

// healthy is a host where everything is as it should be.
func healthy() lifeops.Report {
	unit := func(name string) lifeops.ServiceFacts {
		return lifeops.ServiceFacts{
			Name:                 name,
			LoadState:            "loaded",
			ActiveState:          "active",
			SubState:             "running",
			UnitFileState:        "enabled",
			Result:               "success",
			Type:                 "notify",
			User:                 "billet",
			FragmentPath:         "/usr/lib/systemd/system/" + name,
			MainPID:              1643,
			ExecStart:            "/usr/bin/billet",
			ExecStartCount:       1,
			ExecStartIsThisBuild: lifeops.Yes,
			RunningIsThisBuild:   lifeops.Yes,
			MatchesPackagedUnit:  lifeops.Yes,
			ReloadPending:        lifeops.No,
		}
	}

	return lifeops.Report{
		Binary: "/usr/bin/billet",
		// A HEALTHY HOST HAS A READABLE CONFIG TOO. Leaving this zero made the
		// report render UNREADABLE while every other line looked fine, which is
		// exactly the kind of thing the negative assertions below exist to
		// catch — and did.
		Config: lifeops.FileFacts{
			Path:   "/etc/billet/billet.yaml",
			Exists: lifeops.Yes,
			Mode:   0o640,
			Owner:  "root",
			Group:  "billet",
		},
		Server: unit(deploy.ServerUnitName),
		Node:   unit(deploy.NodeUnitName),
	}
}

// writeLocalConfig writes a config `status` can load, and returns its path.
func writeLocalConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")
	body := `node:
  name: probe-node
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + filepath.Join(dir, "node") + `
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return path
}

// A HOST WITH NEITHER SERVICE MANAGER IS TOLD SO BY NAME, rather than being
// handed an error from a systemctl or a launchctl that does not exist.
//
// THE GUARD SITS ABOVE THE DISPATCH so a subcommand added later inherits it,
// which is a property no subcommand can be relied on to remember — so it is
// asserted here for EVERY subcommand rather than for the one that happens to be
// convenient.
func TestLocalRefusesAHostWithNeitherServiceManager(t *testing.T) {
	prev := hostOS
	hostOS = "plan9"
	t.Cleanup(func() { hostOS = prev })

	cfg := writeLocalConfig(t)

	for _, sub := range []string{"status", "up", "down", "uninstall"} {
		t.Run(sub, func(t *testing.T) {
			err := cmdLocal(t.Context(), []string{sub, "--config", cfg})
			if err == nil {
				t.Fatalf("`billet local %s` acted on a host with no service manager", sub)
			}

			for _, want := range []string{"plan9", "billet server"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// AND macOS IS NO LONGER ONE OF THEM. billet manages launch agents there, so a
// Mac reaching this command must get past the platform check — the refusal that
// used to name darwin was the whole of the macOS lifecycle for a while, and its
// absence is the thing worth asserting.
func TestLocalAdmitsAMac(t *testing.T) {
	prev := hostOS
	hostOS = "darwin"
	t.Cleanup(func() { hostOS = prev })

	err := cmdLocal(t.Context(), []string{"nonsense", "--config", writeLocalConfig(t)})
	if err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}

	// PAST THE PLATFORM CHECK: what comes back names the subcommand rather than
	// the operating system.
	if strings.Contains(err.Error(), "darwin") {
		t.Errorf("a Mac was still refused by the platform guard: %v", err)
	}

	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("the error does not name the unknown subcommand: %v", err)
	}
}

// THE ORDINARY ANSWER, and — as importantly — the absence of every alarm. A
// renderer that printed MISMATCH unconditionally would pass a test that only
// looked for the good lines.
func TestLocalStatusReportsAHealthyHostAndRaisesNoAlarm(t *testing.T) {
	asLinux(t)
	stubInspect(t, healthy())

	var err error
	out := capture(t, func() { err = cmdLocalStatus(t.Context(), []string{"--config", writeLocalConfig(t)}) })
	if err != nil {
		t.Fatalf("status failed on a healthy host: %v\n%s", err, out)
	}

	for _, want := range []string{
		"binary   /usr/bin/billet",
		"server   active (enabled), pid 1643",
		"node     active (enabled), pid 1643",
		"(the packaged unit, unmodified)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the healthy report does not contain %q:\n%s", want, out)
		}
	}
	// EVERY ALARM, not a sample: a renderer that raised one unconditionally
	// would pass a test that only looked for the good lines.
	for _, unwanted := range []string{
		"MISMATCH", "UNCONFIRMED", "DROP-INS", "MASKED", "LINKED", "LOAD STATE",
		"AMBIGUOUS", "PENDING RELOAD", "RUNNING AN OLDER", "UNREADABLE", "MISSING",
		"differs from the packaged unit", "could not be compared",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a healthy host raised %q:\n%s", unwanted, out)
		}
	}
}

// THE ONE AN OPERATOR CANNOT SEE FROM ANYWHERE ELSE: the service is running
// happily from a binary that is not the one they just installed.
func TestLocalStatusNamesABinaryMismatch(t *testing.T) {
	asLinux(t)

	report := healthy()
	report.Binary = "/usr/local/bin/billet"
	report.Server.ExecStartIsThisBuild = lifeops.No
	report.Server.ExecStartWhy = "/usr/bin/billet is a different file from the running billet"
	stubInspect(t, report)

	var err error
	out := capture(t, func() { err = cmdLocalStatus(t.Context(), []string{"--config", writeLocalConfig(t)}) })
	if err != nil {
		t.Fatalf("status failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "BINARY MISMATCH: this unit would run /usr/bin/billet") {
		t.Errorf("the mismatch is not reported:\n%s", out)
	}
	if !strings.Contains(out, "different file") {
		t.Errorf("the mismatch does not say why:\n%s", out)
	}
	// The NODE was fine, and must still read as fine.
	if strings.Count(out, "BINARY MISMATCH") != 1 {
		t.Errorf("the mismatch was reported for a unit that has none:\n%s", out)
	}
}

// EVERY WAY A UNIT CAN BE SOMETHING OTHER THAN WHAT ITS FILE SAYS, each named
// in its own words rather than folded into one vague warning.
func TestLocalStatusNamesEachWayAUnitDiffers(t *testing.T) {
	asLinux(t)

	cases := []struct {
		name  string
		mutex func(*lifeops.ServiceFacts)
		want  string
	}{
		{"masked", func(s *lifeops.ServiceFacts) { s.LoadState = "masked" }, "MASKED"},
		{"linked", func(s *lifeops.ServiceFacts) { s.UnitFileState = "linked" }, "LINKED"},
		{"differs from the package", func(s *lifeops.ServiceFacts) { s.MatchesPackagedUnit = lifeops.No },
			"differs from the packaged unit"},
		{"a pending reload", func(s *lifeops.ServiceFacts) { s.ReloadPending = lifeops.Yes }, "PENDING RELOAD"},
		{"running an older binary", func(s *lifeops.ServiceFacts) {
			s.RunningIsThisBuild = lifeops.No
			s.RunningWhy = "pid 1643 is executing a different file from the running billet"
		}, "RUNNING AN OLDER BINARY"},
		{"drop-ins", func(s *lifeops.ServiceFacts) {
			s.DropInPaths = []string{"/etc/systemd/system/billet-server.service.d/z.conf"}
		}, "DROP-INS: /etc/systemd/system/billet-server.service.d/z.conf"},
		{"two ExecStarts", func(s *lifeops.ServiceFacts) { s.ExecStartCount = 2 }, "AMBIGUOUS"},
		{"unconfirmed binary", func(s *lifeops.ServiceFacts) {
			s.ExecStartIsThisBuild = lifeops.Unknown
			s.ExecStartWhy = "cannot stat /usr/bin/billet: permission denied"
		}, "BINARY UNCONFIRMED: cannot stat"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := healthy()
			tc.mutex(&report.Server)
			stubInspect(t, report)

			var err error
			out := capture(t, func() {
				err = cmdLocalStatus(t.Context(), []string{"--config", writeLocalConfig(t)})
			})
			if err != nil {
				t.Fatalf("status failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the report does not name it (%q):\n%s", tc.want, out)
			}
		})
	}
}

// A HOST WITH NO UNITS IS TOLD WHERE THEY COME FROM, rather than shown an
// empty line it has to interpret.
func TestLocalStatusSaysWhereTheUnitsComeFrom(t *testing.T) {
	asLinux(t)

	report := healthy()
	report.Server = lifeops.ServiceFacts{Name: deploy.ServerUnitName, LoadState: "not-found"}
	stubInspect(t, report)

	var err error
	out := capture(t, func() { err = cmdLocalStatus(t.Context(), []string{"--config", writeLocalConfig(t)}) })
	if err != nil {
		t.Fatalf("status failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "server   not installed") {
		t.Errorf("an absent unit is not reported as such:\n%s", out)
	}
	if !strings.Contains(out, "install the billet package") {
		t.Errorf("an absent unit does not say where units come from:\n%s", out)
	}
}

// A CONFIG THAT WILL NOT LOAD IS REPORTED, NOT FATAL — the half-built host is
// exactly when somebody runs this, and the units and binary are often what
// explain the config problem.
func TestLocalStatusStillReportsWhenTheConfigWillNotLoad(t *testing.T) {
	asLinux(t)
	stubInspect(t, healthy())

	dir := t.TempDir()
	bad := filepath.Join(dir, "billet.yaml")
	if err := os.WriteFile(bad, []byte("server:\n  listen: [not a string\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var err error
	out := capture(t, func() { err = cmdLocalStatus(t.Context(), []string{"--config", bad}) })
	if err != nil {
		t.Fatalf("an unreadable config made status fail outright: %v\n%s", err, out)
	}

	if !strings.Contains(out, "UNREADABLE") {
		t.Errorf("the config problem is not reported:\n%s", out)
	}
	// AND THE REST STILL RAN, which is the property being tested.
	if !strings.Contains(out, "server   active") {
		t.Errorf("a config problem suppressed the service report:\n%s", out)
	}
}

// WHAT `up` WOULD REFUSE IS VISIBLE FROM `status`. An operator whose `up`
// refused needs the command whose job is to report to say why — otherwise the
// two commands disagree about what is worth mentioning on a host.
func TestLocalStatusReportsWhatUpWouldRefuse(t *testing.T) {
	asLinux(t)

	report := healthy()
	report.Server.ExecExtra = map[string]string{"ExecStartPre": "/bin/sh -c whatever"}
	report.Server.Namespace = map[string]string{"RootDirectory": "/srv/elsewhere"}
	report.Server.Elevation = map[string]string{"SupplementaryGroups": "docker"}
	stubInspect(t, report)

	var err error
	out := capture(t, func() { err = cmdLocalStatus(t.Context(), []string{"--config", writeLocalConfig(t)}) })
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	for _, want := range []string{
		"ALSO RUNS: ExecStartPre=/bin/sh -c whatever",
		"REPLACES ITS FILESYSTEM: RootDirectory=/srv/elsewhere",
		"WIDENS ITS IDENTITY: SupplementaryGroups=docker",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not report %q:\n%s", want, out)
		}
	}

	// The NODE is clean, so nothing is attributed to it. Cut rather than index:
	// a missing marker would otherwise slice from -1, and the test would fail
	// for a reason that has nothing to do with what it is about.
	_, node, found := strings.Cut(out, "\nnode ")
	if !found {
		t.Fatalf("status printed no node section at all:\n%s", out)
	}
	if strings.Contains(node, "ALSO RUNS") || strings.Contains(node, "WIDENS") {
		t.Errorf("a clean unit was reported as carrying overrides:\n%s", node)
	}
}
