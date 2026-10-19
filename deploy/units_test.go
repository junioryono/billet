package deploy_test

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/version"
)

// plistValue reads the value a plist associates with a top-level key, by
// PARSING rather than by searching, and returns its ELEMENT KIND with it.
//
// A substring or regex search is satisfied by the same text appearing in an XML
// comment or under a different key — and these files are mostly comments,
// because the reasoning is the point of them. The first version of this test
// searched, which meant it would have passed with the real ExitTimeOut deleted
// as long as the number still appeared in the paragraph explaining it.
//
// The KIND matters because launchd is typed: <string>88200</string> under
// ExitTimeOut is not the integer launchd wants, and a test that only compares
// text cannot tell. A DUPLICATE top-level key is refused outright rather than
// resolved, because which one launchd honours is not something this test should
// be quietly deciding.
//
// Only the outermost dict is walked, and comments are ignored by the decoder,
// so what comes back is the value launchd would use.
func plistValue(t *testing.T, name, body, key string) (kind, value string) {
	t.Helper()

	decoder := xml.NewDecoder(strings.NewReader(body))

	var (
		depth   int
		inKey   bool
		keyText string
		found   bool
	)

	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("%s is not well-formed XML: %v", name, err)
		}

		switch el := tok.(type) {
		case xml.StartElement:
			switch {
			case el.Name.Local == "dict":
				depth++

			case depth != 1:
				// Nested in a sub-dict or array: not a top-level setting.

			case found && kind == "":
				// EXACTLY THE NEXT ELEMENT, consumed whole. Scanning forward for
				// the next non-empty character data instead meant an EMPTY value
				// fell through to the FOLLOWING key's value — so
				// `<key>ExitTimeOut</key><integer></integer>` picked up the next
				// setting's number and the type test passed on a plist launchd
				// would reject.
				kind = el.Name.Local

				var content struct {
					Text string `xml:",chardata"`
				}

				if err := decoder.DecodeElement(&content, &el); err != nil {
					t.Fatalf("%s: cannot read the value of %s: %v", name, key, err)
				}

				// <true/> and <false/> carry their value in the element name.
				if kind == "true" || kind == "false" {
					value = kind
				} else {
					value = strings.TrimSpace(content.Text)
				}

			case el.Name.Local == "key":
				inKey = true
				keyText = ""
			}

		case xml.CharData:
			if inKey {
				keyText += string(el)
			}

		case xml.EndElement:
			switch {
			case el.Name.Local == "dict":
				depth--

			case el.Name.Local == "key" && inKey:
				inKey = false

				if depth == 1 && strings.TrimSpace(keyText) == key {
					if found {
						t.Fatalf("%s declares %s more than once; which one launchd honours "+
							"is not for this test to decide", name, key)
					}

					found = true
				}
			}
		}
	}

	return kind, value
}

// A SHIPPED FILE IS PARSED BY ITS REAL PARSER, which is the rule that already
// covers billet.example.yaml and deploy/billet.yaml. launchd will not tell you
// a plist is malformed in any useful way — the job simply never loads — so a
// typo here is discovered on an operator's Mac rather than here.
//
// It has already earned that: `--config` inside an XML comment is illegal
// (`--` may not appear in one), `plutil -lint` accepts it anyway, and Go's
// decoder does not. That would have shipped a plist launchd refuses to load.
func TestTheLaunchAgentsAreWellFormedXML(t *testing.T) {
	for name, body := range map[string]string{
		deploy.NodeAgentName:    deploy.NodeAgent,
		deploy.ServerAgentName:  deploy.ServerAgent,
		deploy.UpgradeAgentName: deploy.UpgradeAgent,
		deploy.ImagesAgentName:  deploy.ImagesAgent,
	} {
		decoder := xml.NewDecoder(strings.NewReader(body))

		for {
			_, err := decoder.Token()
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				t.Errorf("%s is not well-formed XML: %v", name, err)

				break
			}
		}
	}
}

// The label inside the plist is what launchctl addresses, and the constant is
// what billet would name it by. A disagreement means a command that reports
// success against a job that does not exist.
func TestEachAgentCarriesTheLabelBilletNamesIt(t *testing.T) {
	for label, agent := range map[string]struct{ name, body string }{
		deploy.NodeAgentLabel:    {deploy.NodeAgentName, deploy.NodeAgent},
		deploy.ServerAgentLabel:  {deploy.ServerAgentName, deploy.ServerAgent},
		deploy.UpgradeAgentLabel: {deploy.UpgradeAgentName, deploy.UpgradeAgent},
		deploy.ImagesAgentLabel:  {deploy.ImagesAgentName, deploy.ImagesAgent},
	} {
		kind, got := plistValue(t, agent.name, agent.body, "Label")
		if kind != "string" {
			t.Errorf("%s declares Label as <%s>, want <string>", agent.name, kind)
		}

		if got != label {
			t.Errorf("%s declares Label %q, want %q", agent.name, got, label)
		}
	}
}

// THE TWO PLATFORMS MUST PROMISE THE SAME DRAIN, and this is the assertion that
// keeps them together.
//
// billet's node answers SIGTERM by draining — it stops taking work and waits for
// the jobs already running, for as long as drain_timeout allows. Both service
// managers kill it when their own timer expires: systemd at TimeoutStopSec,
// launchd at ExitTimeOut. A SIGKILL through the middle of a drain leaves guests
// running with their leases renewed by nobody, so the two numbers are the same
// promise written twice, and lowering one without the other silently breaks it
// on that platform only.
//
// launchd's default is FIVE SECONDS — measured on macOS 26 by asking
// `launchctl print` about an agent that sets none, against a man page that says
// twenty — which is why ExitTimeOut being ABSENT is a failure here rather than a
// default worth inheriting.
func TestTheStopGraceIsTheSameOnBothPlatforms(t *testing.T) {
	for _, pair := range []struct {
		what      string
		unit      string
		agentName string
		agent     string
	}{
		{"node", deploy.NodeUnit, deploy.NodeAgentName, deploy.NodeAgent},
		{"server", deploy.ServerUnit, deploy.ServerAgentName, deploy.ServerAgent},
	} {
		systemd := timeoutStopSec(t, pair.what, pair.unit)

		kind, raw := plistValue(t, pair.agentName, pair.agent, "ExitTimeOut")
		if raw == "" {
			t.Fatalf("the %s agent has no ExitTimeOut, so launchd would SIGKILL it after "+
				"its own short default", pair.what)
		}

		// launchd is typed: a <string> here is not the integer it wants.
		if kind != "integer" {
			t.Errorf("the %s agent declares ExitTimeOut as <%s>, want <integer>",
				pair.what, kind)
		}

		launchd, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("the %s agent's ExitTimeOut is %q, not a number", pair.what, raw)
		}

		if systemd != launchd {
			t.Errorf("the %s drains for %ds under systemd and %ds under launchd; both are "+
				"the same promise, and the shorter one SIGKILLs a node mid-drain",
				pair.what, systemd, launchd)
		}

		// launchd reads zero as infinity, and its own man page warns that a job
		// with an infinite grace can stall system shutdown forever.
		if launchd == 0 {
			t.Errorf("the %s agent's ExitTimeOut is zero, which launchd reads as infinity",
				pair.what)
		}
	}
}

// THE NODE AGENT MUST CARRY A PATH, because a launch agent does not inherit a
// shell's. Measured: without it the node starts, registers, and then refuses all
// work with `exec: "tart": executable file not found in $PATH`, and softnet is
// resolved the same way — so the omission breaks untrusted isolation too.
func TestTheNodeAgentCarriesAPathThatCanFindTart(t *testing.T) {
	// PATH lives inside EnvironmentVariables, a nested dict, so the top-level
	// walk deliberately does not see it — searching is right here, and the
	// value is not a number that could plausibly appear in prose.
	if !strings.Contains(deploy.NodeAgent, "<key>PATH</key>") {
		t.Fatal("the node agent sets no PATH, so launchd's default applies and tart is invisible")
	}

	for _, prefix := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(deploy.NodeAgent, prefix) {
			t.Errorf("the node agent's PATH omits %s, where an installed tart lives", prefix)
		}
	}
}

var stopSecPattern = regexp.MustCompile(`(?m)^TimeoutStopSec=(\d+)\s*$`)

func timeoutStopSec(t *testing.T, what, unit string) int {
	t.Helper()

	m := stopSecPattern.FindStringSubmatch(unit)
	if m == nil {
		t.Fatalf("the %s unit has no TimeoutStopSec, so systemd would SIGKILL it mid-drain", what)
	}

	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("the %s unit's TimeoutStopSec is not a number: %v", what, err)
	}

	return n
}

// THE PARSER IS TESTED ON MALFORMED INPUT, because the shipped files are valid
// and a parser that is wrong about them cannot be caught by reading them.
//
// Each case here passed against an earlier version of plistValue, which is why
// they are cases rather than reasoning.
func TestPlistValueReadsExactlyTheKeysOwnValue(t *testing.T) {
	const head = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
`
	const tail = `</dict>
</plist>
`

	for name, tc := range map[string]struct {
		body      string
		wantKind  string
		wantValue string
	}{
		// The value belonging to the NEXT key must not be attributed to this
		// one. A scan-forward-for-character-data parser returns 88200 here.
		"empty value": {
			body:      "<key>ExitTimeOut</key><integer></integer>\n<key>Other</key><integer>88200</integer>\n",
			wantKind:  "integer",
			wantValue: "",
		},
		// Neither must a MISSING value: a <key> where a value belongs is
		// malformed, and consuming it as the value is what makes that visible.
		"missing value": {
			body:      "<key>ExitTimeOut</key>\n<key>Other</key><integer>88200</integer>\n",
			wantKind:  "key",
			wantValue: "Other",
		},
		// A nested dict's key of the same name is not a top-level setting.
		"nested namesake": {
			body: "<key>Wrapper</key><dict><key>ExitTimeOut</key><integer>7</integer></dict>\n" +
				"<key>ExitTimeOut</key><integer>88200</integer>\n",
			wantKind:  "integer",
			wantValue: "88200",
		},
	} {
		t.Run(name, func(t *testing.T) {
			kind, value := plistValue(t, name, head+tc.body+tail, "ExitTimeOut")
			if kind != tc.wantKind || value != tc.wantValue {
				t.Errorf("plistValue = <%s>%q, want <%s>%q", kind, value, tc.wantKind, tc.wantValue)
			}
		})
	}
}

// The backup unit runs as the account that can read the deployment, and nothing
// enables the service directly.
//
// BOTH PROPERTIES ARE ABOUT WHAT IT MUST NOT DO. It must not run as root,
// because a root-created archive is one the control plane's own account cannot
// read — the same failure a root-run restore had, one directory over. And it
// must carry no [Install] section: a oneshot enabled at boot backs up once per
// boot and then never again, which reads exactly like a working schedule.
func TestTheBackupUnitRunsAsTheServiceAccountAndIsNotEnabledDirectly(t *testing.T) {
	for _, want := range []string{
		"User=billet",
		"Group=billet",
		"Type=oneshot",
		"StateDirectoryMode=0700",
	} {
		if !strings.Contains(deploy.BackupUnit, want) {
			t.Errorf("%s does not carry %q", deploy.BackupUnitName, want)
		}
	}

	if strings.Contains(deploy.BackupUnit, "[Install]") {
		t.Errorf("%s has an [Install] section: a oneshot enabled at boot runs once per boot and "+
			"then never again, which reads exactly like a working schedule", deploy.BackupUnitName)
	}

	// THE SAME FENCE THE SERVER HAS. The backup is the other thing that opens the
	// state directory, so on a host whose ledger lives on its own volume it must
	// wait for that mount the way billet-server.service does. IN [Unit]: systemd
	// reads RequiresMountsFor= only there and silently ignores it under
	// [Service], and a substring test alone passes the ignored placement.
	const fence = "\nRequiresMountsFor=/var/lib/billet/server\n"

	fenceAt := strings.Index(deploy.BackupUnit, fence)
	unitAt := strings.Index(deploy.BackupUnit, "\n[Unit]\n")
	serviceAt := strings.Index(deploy.BackupUnit, "\n[Service]\n")

	switch {
	case fenceAt < 0:
		t.Errorf("%s does not carry %q, so a failed ledger mount leaves the timer archiving "+
			"whatever is at the path", deploy.BackupUnitName, strings.TrimSpace(fence))
	case unitAt < 0 || serviceAt < 0:
		t.Errorf("%s lacks a [Unit] or a [Service] section", deploy.BackupUnitName)
	case fenceAt < unitAt || fenceAt > serviceAt:
		// BOTH BOUNDS: before [Unit] is outside every section, after [Service]
		// is the wrong one, and systemd ignores the directive in either place.
		t.Errorf("%s carries RequiresMountsFor= outside [Unit], where systemd ignores it",
			deploy.BackupUnitName)
	}

	// THE LEDGER'S ENVIRONMENT. A PostgreSQL controller's backup opens the
	// ledger through the connection string the server reads from this file, and
	// billet refuses an empty variable before it writes anything, so a backup
	// unit that does not import it fails on every scheduled run of such a host.
	envAt := strings.Index(deploy.BackupUnit, "\nEnvironmentFile=-/etc/billet/server.env\n")

	switch {
	case envAt < 0:
		t.Errorf("%s does not import /etc/billet/server.env, so a PostgreSQL controller's "+
			"backup has no connection string", deploy.BackupUnitName)
	case serviceAt < 0 || envAt < serviceAt:
		// IN [Service], where systemd reads it; under [Unit] it is ignored and
		// the import is a line that does nothing.
		t.Errorf("%s carries EnvironmentFile= outside [Service], where systemd ignores it",
			deploy.BackupUnitName)
	}

	// AND THE TIMER IS THE THING THAT IS ENABLED, so it needs one.
	if !strings.Contains(deploy.BackupTimer, "[Install]") {
		t.Errorf("%s has no [Install] section, so `systemctl enable` cannot reach it",
			deploy.BackupTimerName)
	}

	// DAILY AND SPREAD, and a test names both so a retuned schedule is a decision
	// somebody wrote down rather than an edit nobody noticed.
	for _, want := range []string{"OnCalendar=daily", "RandomizedDelaySec="} {
		if !strings.Contains(deploy.BackupTimer, want) {
			t.Errorf("%s does not carry %q", deploy.BackupTimerName, want)
		}
	}

	// A MISSED RUN IS RUN. Without this a host that was off overnight silently
	// skips a day, and a backup's failures are the ones nobody sees.
	if !strings.Contains(deploy.BackupTimer, "Persistent=true") {
		t.Errorf("%s is not Persistent, so a host that was off skips a backup silently",
			deploy.BackupTimerName)
	}

	// NOTHING IN EITHER FILE DELETES AN ARCHIVE. Every one holds a copy of the
	// GitHub App private key GitHub issues exactly once, and a unit that removes
	// credentials on a timer is not something to add quietly.
	for name, body := range map[string]string{
		deploy.BackupUnitName:  deploy.BackupUnit,
		deploy.BackupTimerName: deploy.BackupTimer,
	} {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}

			if strings.Contains(line, "rm ") || strings.Contains(line, "find ") {
				t.Errorf("%s removes files: %q", name, line)
			}
		}
	}
}

// THE IMAGE REFRESH FOLLOWS THE BACKUP'S SHAPE: a oneshot nothing enables at
// boot, a timer that is the thing enabled, run daily with the decision in the
// command, persistent so a host that was off does not wait another day, and as
// root because a pull maps RBD and boots a probe under the jailer.
func TestTheImageRefreshUnitIsAPersistentDailyTimerRunAsRoot(t *testing.T) {
	for _, want := range []string{
		"User=root",
		"Type=oneshot",
		"ExecStart=/usr/bin/billet images refresh --config /etc/billet/billet.yaml",
		"ReadWritePaths=/var/lib/billet",
	} {
		if !strings.Contains(deploy.ImagesRefreshUnit, want) {
			t.Errorf("%s does not carry %q", deploy.ImagesRefreshUnitName, want)
		}
	}

	if strings.Contains(deploy.ImagesRefreshUnit, "[Install]") {
		t.Errorf("%s has an [Install] section: a oneshot enabled at boot runs once per boot and "+
			"then never again, which reads exactly like a working schedule", deploy.ImagesRefreshUnitName)
	}

	for _, want := range []string{
		"[Install]",
		"OnCalendar=daily",
		"Persistent=true",
		"RandomizedDelaySec=",
		"Unit=" + deploy.ImagesRefreshUnitName,
	} {
		if !strings.Contains(deploy.ImagesRefreshTimer, want) {
			t.Errorf("%s does not carry %q", deploy.ImagesRefreshTimerName, want)
		}
	}
}

// A PRIVATE /tmp IS NOT THE HOST'S, so a unit with PrivateTmp=true may name no
// path under /tmp or /var/tmp as required in ReadWritePaths: systemd resolves
// the entry inside the service's fresh private directory, finds nothing and
// refuses to start the unit with 226/NAMESPACE. Measured 2026-09-22 under
// systemd 255.4 on ubuntu-24.04, with PrivateTmp=true and ProtectSystem=strict:
// ReadWritePaths=/var/tmp/billet-images exited 226 while the directory existed on
// the host; without the entry, or with a leading '-', the service started and
// wrote into its private /var/tmp. billet-images-refresh.service carried the
// entry and so never started once on a systemd host; the fleet it served kept a
// three-week-old guest image while the daily timer failed in the journal.
func TestNoPrivateTmpUnitRequiresAWritablePathUnderTmp(t *testing.T) {
	for name, unit := range map[string]string{
		deploy.ServerUnitName:        deploy.ServerUnit,
		deploy.NodeUnitName:          deploy.NodeUnit,
		deploy.BackupUnitName:        deploy.BackupUnit,
		deploy.UpgradeUnitName:       deploy.UpgradeUnit,
		deploy.ImagesRefreshUnitName: deploy.ImagesRefreshUnit,
	} {
		for _, path := range requiredPathsUnderPrivateTmp(unit) {
			t.Errorf("%s has PrivateTmp and requires %s in ReadWritePaths; systemd refuses to start it (226/NAMESPACE)", name, path)
		}
	}
}

// requiredPathsUnderPrivateTmp reads a unit as systemd assigns it (whitespace
// around the '=', comment lines skipped, the last PrivateTmp winning, systemd's
// boolean spellings) and returns the ReadWritePaths entries a private /tmp
// cannot satisfy: a required path strictly below /tmp or /var/tmp. The roots
// themselves exist in the private namespace, and a '-' entry may be missing.
func requiredPathsUnderPrivateTmp(unit string) []string {
	private := false

	var required []string

	for line := range strings.Lines(unit) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		switch key {
		case "PrivateTmp":
			switch strings.ToLower(value) {
			case "1", "yes", "y", "true", "t", "on":
				private = true
			default:
				private = false
			}
		case "ReadWritePaths":
			for path := range strings.FieldsSeq(value) {
				path = strings.Trim(path, `"`)
				if strings.HasPrefix(path, "-") {
					continue
				}

				path = strings.TrimPrefix(path, "+")
				if strings.HasPrefix(path, "/tmp/") || strings.HasPrefix(path, "/var/tmp/") {
					required = append(required, path)
				}
			}
		}
	}

	if !private {
		return nil
	}

	return required
}

// THE READER HOLDS ACROSS THE SPELLINGS systemd accepts, so the rule cannot be
// passed by writing the same unit another way.
func TestRequiredPathsUnderPrivateTmpReadsTheUnitAsSystemdDoes(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		want       []string
	}{
		{"canonical", "[Service]\nPrivateTmp=true\nReadWritePaths=/var/lib/billet /var/tmp/billet-images\n", []string{"/var/tmp/billet-images"}},
		{"yes and spaces", "[Service]\nPrivateTmp = yes\nReadWritePaths = /var/lib /tmp/x\n", []string{"/tmp/x"}},
		{"optional", "[Service]\nPrivateTmp=true\nReadWritePaths=-/var/tmp/billet-images\n", nil},
		{"the roots exist", "[Service]\nPrivateTmp=true\nReadWritePaths=/tmp /var/tmp\n", nil},
		{"not private", "[Service]\nPrivateTmp=false\nReadWritePaths=/var/tmp/billet-images\n", nil},
		{"last one wins", "[Service]\nPrivateTmp=false\nPrivateTmp=on\nReadWritePaths=/var/tmp/x\n", []string{"/var/tmp/x"}},
		{"comment", "[Service]\nPrivateTmp=true\n# ReadWritePaths=/var/tmp/x\n", nil},
	} {
		if got := requiredPathsUnderPrivateTmp(tc.unit); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE NODE DECLARES ITS REGISTRATION DIRECTORY ONCE, IN [Service], AND THE
// SERVER DECLARES NONE. The registration record lives under
// billet/registration; a second assignment or a later empty one would reset
// the list, a placement under [Unit] would be ignored, a RuntimeDirectoryPreserve
// other than no would keep a record across a restart (the empty directory at a
// start is what the inspector's currency rule rests on), and a server unit
// declaring the same directory would re-own and remove the node's record.
func TestTheNodeUnitDeclaresItsRegistrationDirectoryAndTheServerDeclaresNone(t *testing.T) {
	const want = "RuntimeDirectory=billet/registration"

	lines := strings.Split(deploy.NodeUnit, "\n")
	section := ""

	var declared []string
	var preserve []string

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "["):
			section = line
		case strings.HasPrefix(line, "RuntimeDirectory="):
			declared = append(declared, section+" "+line)
		case strings.HasPrefix(line, "RuntimeDirectoryPreserve="):
			preserve = append(preserve, strings.TrimPrefix(line, "RuntimeDirectoryPreserve="))
		}
	}

	if len(declared) != 1 || declared[0] != "[Service] "+want {
		t.Errorf("%s declares %q, want exactly one %q in [Service]", deploy.NodeUnitName, declared, want)
	}

	if len(preserve) > 1 || (len(preserve) == 1 && preserve[0] != "no") {
		t.Errorf("%s sets RuntimeDirectoryPreserve=%v; only an absent or a `no` keeps the directory empty at a start", deploy.NodeUnitName, preserve)
	}

	if strings.Contains(deploy.ServerUnit, "RuntimeDirectory") {
		t.Errorf("%s declares a RuntimeDirectory; a server declaring the node's would re-own and remove its record", deploy.ServerUnitName)
	}
}

// THE NODE UNIT THIS BUILD SHIPS PROVIDES WHAT THIS BUILD REQUIRES OF IT. A
// host upgrade refuses a candidate whose requirements the loaded unit does not
// meet (#168), so a requirement the shipped unit does not meet either would
// refuse every upgrade onto this release, even on a freshly converged host.
func TestTheShippedNodeUnitMeetsTheNodesRuntimeRequirements(t *testing.T) {
	var declared []string
	for _, value := range serviceAssignments(deploy.NodeUnit, "RuntimeDirectory") {
		declared = append(declared, strings.Fields(value)...)
	}

	if len(deploy.NodeRuntimeRequirements) == 0 {
		t.Fatal("this build declares no runtime directory its node needs; the registration record needs one")
	}

	for _, req := range deploy.NodeRuntimeRequirements {
		if !slices.Contains(declared, req.Entry) {
			t.Errorf("%s declares RuntimeDirectory %q without %q, which this build's node writes into",
				deploy.NodeUnitName, declared, req.Entry)
		}

		if !version.IsRelease(req.Since) {
			t.Errorf("the requirement %q names %q as the release that first needs it, which is not a release",
				req.Entry, req.Since)
		}
	}
}

// NO UNIT'S STOP REMOVES THE HOST LOCK DIRECTORY. A unit removes the runtime
// directories it declares when it stops, and a lock in /run/billet/locks is held
// by processes that outlive a node restart: `billet images verify` holds its
// probe lock for the minutes a boot verification takes. A stop that removed the
// directory unlinked the lock's name under its holder, and the next verifier
// locked a fresh file at the same path and destroyed the first one's live probe
// (#175; measured on systemd 255.4, 2026-10-16: the second flock on the
// recreated name was granted). A declaration of the directory's parent removes
// it just the same, so billet itself and every prefix of the lock directory
// count, and a unit may declare one only with RuntimeDirectoryPreserve=yes,
// which the node cannot set (its registration record must not outlive it).
func TestNoUnitDeclaresTheHostLockDirectory(t *testing.T) {
	units := map[string]string{
		deploy.ServerUnitName:        deploy.ServerUnit,
		deploy.NodeUnitName:          deploy.NodeUnit,
		deploy.BackupUnitName:        deploy.BackupUnit,
		deploy.UpgradeUnitName:       deploy.UpgradeUnit,
		deploy.ImagesRefreshUnitName: deploy.ImagesRefreshUnit,
	}

	lockDir := strings.TrimPrefix(deploy.NodeLockDir, "/run/")

	for name, unit := range units {
		preserved := slices.Equal(serviceAssignments(unit, "RuntimeDirectoryPreserve"), []string{"yes"})

		for _, value := range serviceAssignments(unit, "RuntimeDirectory") {
			for _, entry := range strings.Fields(value) {
				covers := entry == lockDir || strings.HasPrefix(lockDir, entry+"/") || strings.HasPrefix(entry, lockDir+"/")
				if covers && !preserved {
					t.Errorf("%s declares RuntimeDirectory %q without RuntimeDirectoryPreserve=yes, so its stop "+
						"removes %s under the locks other processes hold in it", name, entry, deploy.NodeLockDir)
				}
			}
		}
	}
}

// A UNIT THAT TAKES THE HOST LOCK CAN WRITE ITS DIRECTORY, AND NEITHER REMOVES IT.
//
// ProtectSystem=strict leaves nothing under /run writable that a unit does not
// name. The node (the deployment lock) names it in ReadWritePaths, without a
// leading '-': a host whose directory is missing then refuses the start with
// 226/NAMESPACE naming the path, rather than starting a node that cannot lock
// (measured on systemd 255.4, 2026-10-16). The image refresh (the probe lock a
// promotion takes) declares it, 0750 like the tmpfiles entry so it re-owns
// nothing, and preserves it: the declaration makes it where something removed
// it, and the preserve keeps this oneshot's stop after every run from removing
// it under a running node's lock (measured on a node 2026-09-22: without the
// declaration the pull downloaded about 5.1 GiB, imported the generation and
// then failed to verify it on the lock).
func TestAUnitThatTakesTheHostLockCanWriteItsDirectory(t *testing.T) {
	var entries []string
	for _, value := range serviceAssignments(deploy.NodeUnit, "ReadWritePaths") {
		entries = append(entries, strings.Fields(value)...)
	}

	if !slices.Contains(entries, deploy.NodeLockDir) {
		t.Errorf("%s names ReadWritePaths %q, without %s as a required entry; under "+
			"ProtectSystem=strict it cannot take the host lock there", deploy.NodeUnitName, entries, deploy.NodeLockDir)
	}

	// THE ROLE'S COPY IS READ BY THE SAME PARSER: it renders no variables, so
	// the template is the unit, and unit-parity's line patterns cannot follow a
	// continuation or a second [Service] the way systemd does.
	role, err := os.ReadFile("../ansible_collections/junioryono/billet/roles/host/templates/billet-images-refresh.service.j2")
	if err != nil {
		t.Fatal(err)
	}

	// The template is the unit only while Jinja has nothing to render in it.
	for _, delimiter := range []string{"{{", "{%", "{#", "#jinja2:"} {
		if strings.Contains(string(role), delimiter) {
			t.Errorf("the role's refresh template carries the Jinja marker %q, so it is not the unit it renders",
				delimiter)
		}
	}

	for name, unit := range map[string]string{
		deploy.ImagesRefreshUnitName:  deploy.ImagesRefreshUnit,
		"the role's refresh template": string(role),
	} {
		if err := plainUnit(unit); err != nil {
			t.Errorf("%s: %v", name, err)
		}

		for key, want := range map[string]string{
			"RuntimeDirectory":         strings.TrimPrefix(deploy.NodeLockDir, "/run/"),
			"RuntimeDirectoryMode":     "0750",
			"RuntimeDirectoryPreserve": "yes",
		} {
			if got := serviceAssignments(unit, key); !slices.Equal(got, []string{want}) {
				t.Errorf("%s assigns %s=%q, want exactly %q in [Service]", name, key, got, want)
			}
		}
	}
}

// THE TMPFILES ENTRY MAKES THE LOCK DIRECTORY AS THE NODE UNIT USED TO, AND
// NEVER AGES IT. root:root 0750 is what RuntimeDirectoryMode gave the
// directory, so a host moving from the old units to this entry re-owns
// nothing; an age field other than '-' would let systemd-tmpfiles --clean
// remove a lock file a process holds, which is #175 again on a timer.
func TestTheTmpfilesEntryMakesTheLockDirectoryAndNeverAgesIt(t *testing.T) {
	var lines []string

	for line := range strings.Lines(deploy.Tmpfiles) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}

	want := []string{
		"d /run/billet 0755 root root -",
		"d " + deploy.NodeLockDir + " 0750 root root -",
	}

	if !slices.Equal(lines, want) {
		t.Errorf("billet-tmpfiles.conf declares %q, want exactly %q", lines, want)
	}
}

// THE PACKAGE INSTALLS THE ENTRY WHERE postinstall CREATES IT FROM, AND THE
// ROLE RENDERS THE SAME BYTES. A host converged by the role and one that
// installed the package carry one directory with one owner and mode; the
// role's template must carry no Jinja delimiter, or its render would differ
// from its source.
func TestThePackageAndTheRoleInstallOneTmpfilesEntry(t *testing.T) {
	release, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(release), "- src: deploy/billet-tmpfiles.conf\n        dst: "+deploy.TmpfilesPath+"\n") {
		t.Errorf(".goreleaser.yaml does not install deploy/billet-tmpfiles.conf at %s", deploy.TmpfilesPath)
	}

	role, err := os.ReadFile("../ansible_collections/junioryono/billet/roles/host/templates/billet-tmpfiles.conf.j2")
	if err != nil {
		t.Fatal(err)
	}

	if string(role) != deploy.Tmpfiles {
		t.Errorf("the role's tmpfiles template differs from deploy/billet-tmpfiles.conf:\nrole:\n%s\npackage:\n%s",
			role, deploy.Tmpfiles)
	}

	for _, delimiter := range []string{"{{", "{%", "{#"} {
		if strings.Contains(string(role), delimiter) {
			t.Errorf("the role's tmpfiles template carries the Jinja delimiter %q", delimiter)
		}
	}

	account, err := os.ReadFile("../ansible_collections/junioryono/billet/roles/host/tasks/account.yml")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(account), "src: billet-tmpfiles.conf.j2\n    dest: "+deploy.TmpfilesRolePath+"\n") {
		t.Errorf("the role's account.yml does not render billet-tmpfiles.conf.j2 at %s", deploy.TmpfilesRolePath)
	}
}

// serviceAssignments reads a unit as systemd assigns it — comment lines skipped,
// whitespace around the '=' ignored, sections tracked — and returns every value
// assigned to one key inside [Service], in order.
//
// PARSED RATHER THAN SEARCHED, because a directive named in a COMMENT satisfies
// a substring search: the first version of the rule above passed against a unit
// with the preserve deleted, on the strength of the comment that explains why it
// has to be there. A lone carriage return ends a line and a trailing backslash
// continues one, as systemd's parser reads them: a continuation folds the next
// line into the assignment it ends, so a directive on that next line is no
// directive.
//
// The line handling follows systemd 255's config_parse for the plain form
// plainUnit admits: a comment line is skipped before any joining, a line
// continues when it ends in an UNESCAPED backslash (counted over the line as
// joined so far, before any trimming), the backslash becomes a space, the joined
// line is trimmed before it is read as a section or an assignment, and a
// continuation still open at the end of the file is read as it stands. Outside
// that form systemd reads differently in ways this does not follow.
func serviceAssignments(unit, key string) []string {
	var out []string

	section := ""

	parse := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			return
		}

		if strings.HasPrefix(line, "[") {
			section = line

			return
		}

		k, v, ok := strings.Cut(line, "=")
		if ok && section == "[Service]" && strings.TrimSpace(k) == key {
			out = append(out, strings.TrimSpace(v))
		}
	}

	unit = strings.ReplaceAll(strings.ReplaceAll(unit, "\r\n", "\n"), "\r", "\n")

	continuation, continued := "", false

	for line := range strings.Lines(unit) {
		line = strings.TrimSuffix(line, "\n")

		// A COMMENT LINE IS SKIPPED BEFORE ANY JOINING, inside a continuation
		// or not, so a backslash ending one continues nothing.
		if lead := strings.TrimLeft(line, " \t"); strings.HasPrefix(lead, "#") || strings.HasPrefix(lead, ";") {
			continue
		}

		if continued {
			line = continuation + line
		}

		escaped := false
		for _, c := range line {
			escaped = !escaped && c == '\\'
		}

		if escaped {
			continuation, continued = line[:len(line)-1]+" ", true

			continue
		}

		continuation, continued = "", false

		parse(line)
	}

	if continued {
		parse(continuation)
	}

	return out
}

// plainUnit refuses a unit outside the plain form both refresh copies are
// written in, where serviceAssignments reads every line as systemd 255 does:
// printable ASCII and newlines only (no carriage return, NUL, BOM, tab or
// Unicode space, each of which systemd's reader and Go's trimming treat
// differently), a final newline, no indented line, no line ending in a
// backslash (a comment line included, since systemd joins what follows it), and
// every line opening with '[' a bare section header. FAILING CLOSED, because
// emulating each of systemd's line rules was measured to leave the next one
// open: six review rounds each found another way to hide an override.
func plainUnit(unit string) error {
	if !strings.HasSuffix(unit, "\n") {
		return errors.New("the unit does not end in a newline")
	}

	for i, line := range strings.Split(strings.TrimSuffix(unit, "\n"), "\n") {
		// A comment line is skipped whole, so it may carry prose beyond ASCII;
		// a control byte ends or splits a line anywhere.
		comment := strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";")

		for _, c := range []byte(line) {
			if c < 0x20 || c == 0x7f || c > 0x7e && !comment {
				return fmt.Errorf("line %d carries the byte %#x, outside printable ASCII", i+1, c)
			}
		}

		switch {
		case strings.HasPrefix(line, " "):
			return fmt.Errorf("line %d is indented", i+1)
		case strings.HasSuffix(strings.TrimRight(line, " "), `\`):
			return fmt.Errorf("line %d ends in a backslash, which continues it", i+1)
		case strings.HasPrefix(line, "[") && !sectionHeader.MatchString(line):
			return fmt.Errorf("line %d opens with '[' and is not a bare section header: %q", i+1, line)
		}
	}

	return nil
}

var sectionHeader = regexp.MustCompile(`^\[[A-Za-z]+\]$`)

// THE PLAIN FORM REFUSES EVERY SHAPE THAT HID AN OVERRIDE FROM A READER.
func TestPlainUnitRefusesWhatSystemdReadsDifferently(t *testing.T) {
	for _, tc := range []struct{ name, unit string }{
		{"a comment that continues", "[Service]\n# why \\\nRuntimeDirectoryPreserve\\\n=no\n"},
		{"an escaped backslash", "[Service]\nX-Note=\\\\\nRuntimeDirectoryPreserve=no\n"},
		{"a carriage return", "[Service]\nX=1\r[Install]\n"},
		{"a NUL", "[Service]\n# note\x00RuntimeDirectoryPreserve=no\n"},
		{"a byte-order mark", "[Service]\n\ufeffRuntimeDirectoryPreserve=no\n"},
		{"a non-breaking space", "[Service]\nRuntimeDirectoryPreserve\u00a0=yes\n"},
		{"a tab", "[Service]\nRuntimeDirectoryPreserve\t=yes\n"},
		{"an indented line", "[Service]\n  RuntimeDirectoryPreserve = no\n"},
		{"a header with a comment", "[Service] # why\nRuntimeDirectoryPreserve=yes\n"},
		{"no final newline", "[Service]\nRuntimeDirectoryPreserve=yes"},
		{"a byte-order mark before a comment", "[Service]\n\ufeff# why\nRuntimeDirectoryPreserve=yes\n"},
		{"a NUL inside a comment", "[Service]\n# why\x00\nRuntimeDirectoryPreserve=yes\n"},
	} {
		if plainUnit(tc.unit) == nil {
			t.Errorf("%s: admitted %q", tc.name, tc.unit)
		}
	}

	if err := plainUnit("# a unit — with prose\n[Unit]\nDescription=x\n\n[Service]\nRuntimeDirectoryPreserve=yes\n"); err != nil {
		t.Errorf("a plain unit was refused: %v", err)
	}
}

// THE READER IS TESTED ON WHAT FOOLED ITS PREDECESSOR.
func TestServiceAssignmentsReadsAssignmentsAndNotProse(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		want       []string
	}{
		{"canonical", "[Service]\nRuntimeDirectoryPreserve=yes\n", []string{"yes"}},
		{"spaces", "[Service]\nRuntimeDirectoryPreserve = yes \n", []string{"yes"}},
		{"a comment is not an assignment", "[Service]\n# RuntimeDirectoryPreserve=yes keeps it\n", nil},
		{"another section is not [Service]", "[Unit]\nRuntimeDirectoryPreserve=yes\n", nil},
		{"back in [Service]", "[Unit]\nX=1\n[Service]\nRuntimeDirectoryPreserve=no\n", []string{"no"}},
		{"every assignment, in order", "[Service]\nRuntimeDirectoryPreserve=yes\nRuntimeDirectoryPreserve=no\n", []string{"yes", "no"}},
		{"a longer key is not this one", "[Service]\nRuntimeDirectoryPreserveMore=yes\n", nil},
		{"indented, in a second [Service]", "[Service]\nRuntimeDirectoryPreserve=yes\n[Install]\n[Service]\n  RuntimeDirectoryPreserve = no\n",
			[]string{"yes", "no"}},
		{"folded into the line before", "[Service]\nEnvironment=A=1 \\\nRuntimeDirectoryPreserve=yes\n", nil},
		{"a lone carriage return ends a line", "[Service]\nX=1\r[Install]\nRuntimeDirectoryPreserve=yes\n", nil},
		{"a continuation ends", "[Service]\nEnvironment=A=1 \\\n  B=2\nRuntimeDirectoryPreserve=yes\n", []string{"yes"}},
		{"an escaped backslash does not continue", "[Service]\nRuntimeDirectoryPreserve=yes\nX-Note=\\\\\nRuntimeDirectoryPreserve=no\n",
			[]string{"yes", "no"}},
		{"a space after the backslash does not continue", "[Service]\nX-Note=a\\ \nRuntimeDirectoryPreserve=no\n",
			[]string{"no"}},
		{"a joined line is trimmed before it is a section", "[Unit]\n\\\n[Service]\nRuntimeDirectoryPreserve=no\n",
			[]string{"no"}},
		{"a comment inside a continuation is skipped", "[Service]\nRuntimeDirectoryPreserve=\\\n# why\nno\n", []string{"no"}},
		{"a continuation open at the end of the file", "[Service]\nRuntimeDirectoryPreserve=no\\", []string{"no"}},
		{"a continuation open at the last newline", "[Service]\nRuntimeDirectoryPreserve=no\\\n", []string{"no"}},
	} {
		if got := serviceAssignments(tc.unit, "RuntimeDirectoryPreserve"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE CONSTANTS ARE THE UNITS' OWN BOUNDS: a caller that starts or stops a
// unit under deploy.UnitStartTimeout or deploy.UnitStopTimeout waits exactly
// as long as systemd would, plus its own margin, and a unit edited without
// the constant following it fails here.
func TestTheUnitBoundConstantsAreTheUnitsOwn(t *testing.T) {
	for _, pair := range []struct{ what, unit string }{
		{"node", deploy.NodeUnit},
		{"server", deploy.ServerUnit},
	} {
		if got := time.Duration(timeoutStopSec(t, pair.what, pair.unit)) * time.Second; got != deploy.UnitStopTimeout {
			t.Errorf("the %s unit's TimeoutStopSec is %s and deploy.UnitStopTimeout is %s", pair.what, got,
				deploy.UnitStopTimeout)
		}

		m := regexp.MustCompile(`(?m)^TimeoutStartSec=(\d+)$`).FindStringSubmatch(pair.unit)
		if m == nil {
			t.Fatalf("the %s unit has no TimeoutStartSec", pair.what)
		}

		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}

		if got := time.Duration(n) * time.Second; got != deploy.UnitStartTimeout {
			t.Errorf("the %s unit's TimeoutStartSec is %s and deploy.UnitStartTimeout is %s", pair.what, got,
				deploy.UnitStartTimeout)
		}
	}
}
