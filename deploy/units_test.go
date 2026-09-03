package deploy_test

import (
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

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

	// AND THE TIMER IS THE THING THAT IS ENABLED, so it needs one.
	if !strings.Contains(deploy.BackupTimer, "[Install]") {
		t.Errorf("%s has no [Install] section, so `systemctl enable` cannot reach it",
			deploy.BackupTimerName)
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
