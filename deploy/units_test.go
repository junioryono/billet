package deploy_test

import (
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
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

// THE NODE DECLARES ITS TWO RUNTIME DIRECTORIES ONCE, IN [Service], AND THE
// SERVER DECLARES NONE. The registration record lives under
// billet/registration; a second assignment or a later empty one would reset
// the list, a placement under [Unit] would be ignored, a RuntimeDirectoryPreserve
// other than no would keep a record across a restart (the empty directory at a
// start is what the inspector's currency rule rests on), and a server unit
// declaring the same directory would re-own and remove the node's record.
func TestTheNodeUnitDeclaresItsRuntimeDirectoriesAndTheServerDeclaresNone(t *testing.T) {
	const want = "RuntimeDirectory=billet/locks billet/registration"

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

// A UNIT THAT TAKES THE HOST LOCK DECLARES THE DIRECTORY IT LIVES IN, AND ONLY
// ITS OWNER MAY REMOVE IT.
//
// /run/billet/locks is the host-wide collision domain, and ProtectSystem=strict
// leaves nothing under /run writable except a directory the unit itself
// declares. The image refresh takes that lock to boot-verify a generation before
// promoting it, so without the declaration a pull downloaded fifteen gigabytes,
// imported the generation and then failed on the lock, leaving the image
// unpromoted and @verified on the old one (measured on a node 2026-09-22).
//
// The preserve is the other half. A unit removes the runtime directories it
// declares when it stops, and a oneshot stops after every run, so a refresh
// without RuntimeDirectoryPreserve=yes would take the running node's lock
// directory with it. billet-node.service is the owner and must NOT preserve,
// which the test above pins; every other unit that declares the directory must.
func TestAUnitThatTakesTheHostLockDeclaresItWithoutTakingItAway(t *testing.T) {
	for name, unit := range map[string]string{
		deploy.ImagesRefreshUnitName: deploy.ImagesRefreshUnit,
	} {
		if got := serviceAssignments(unit, "RuntimeDirectory"); !slices.Equal(got, []string{"billet/locks"}) {
			t.Errorf("%s assigns RuntimeDirectory=%q; it takes the host lock, and under "+
				"ProtectSystem=strict a directory it does not declare cannot be opened, so the "+
				"pull it follows is wasted", name, got)
		}

		if got := serviceAssignments(unit, "RuntimeDirectoryPreserve"); !slices.Equal(got, []string{"yes"}) {
			t.Errorf("%s assigns RuntimeDirectoryPreserve=%q; a unit removes the runtime "+
				"directories it declares when it stops, and this one stops after every run, so "+
				"anything but yes takes %s's lock directory with it", name, got, deploy.NodeUnitName)
		}

		if got := serviceAssignments(unit, "RuntimeDirectoryMode"); !slices.Equal(got, []string{"0750"}) {
			t.Errorf("%s assigns RuntimeDirectoryMode=%q; %s creates that directory 0750, and a "+
				"different mode re-owns what the two units share", name, got, deploy.NodeUnitName)
		}
	}
}

// serviceAssignments reads a unit as systemd assigns it — comment lines skipped,
// whitespace around the '=' ignored, sections tracked — and returns every value
// assigned to one key inside [Service], in order.
//
// PARSED RATHER THAN SEARCHED, because a directive named in a COMMENT satisfies
// a substring search: the first version of the rule above passed against a unit
// with the preserve deleted, on the strength of the comment that explains why it
// has to be there.
func serviceAssignments(unit, key string) []string {
	var out []string

	section := ""

	for line := range strings.Lines(unit) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			section = line

			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok || section != "[Service]" || strings.TrimSpace(k) != key {
			continue
		}

		out = append(out, strings.TrimSpace(v))
	}

	return out
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
