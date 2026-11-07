package launchd

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
)

// AN AGENT A RELEASE CHANGED ONLY IN ITS COMMENTS IS BILLET'S OWN. #720 reworded the
// images agent's header comment, and every Mac installed before it then refused the
// agent as an operator's edit (#787). The file here is the agent as it shipped before.
func TestAnAgentChangedOnlyInItsCommentIsTheSameAgent(t *testing.T) {
	t.Parallel()

	before, err := os.ReadFile("testdata/sh.billet.images.before-720.plist")
	if err != nil {
		t.Fatal(err)
	}

	if string(before) == deploy.ImagesAgent {
		t.Fatal("the fixture is the shipped agent byte for byte, so it proves nothing about comments")
	}

	if !sameAgent(string(before), deploy.ImagesAgent) {
		t.Error("the images agent shipped before #720 is refused, though only its comment changed")
	}
}

// EVERY SHIPPED AGENT: what launchd does not read is no difference, and everything it
// reads is one.
func TestAnAgentIsTheSameExceptForWhatLaunchdReads(t *testing.T) {
	t.Parallel()

	for name, shipped := range map[string]string{
		"server": deploy.ServerAgent, "node": deploy.NodeAgent,
		"upgrade": deploy.UpgradeAgent, "images": deploy.ImagesAgent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			same := map[string]string{
				"itself":                  shipped,
				"a reworded comment":      strings.Replace(shipped, "<!--", "<!-- an older wording, then:", 1),
				"an added comment":        strings.Replace(shipped, "<dict>", "<dict>\n  <!-- added -->", 1),
				"reindented":              strings.ReplaceAll(shipped, "\n  ", "\n\t"),
				"blank lines and the end": "\n" + strings.ReplaceAll(shipped, "</key>\n", "</key>\n\n") + "\n\n",
			}

			for what, installed := range same {
				if installed == shipped && what != "itself" {
					t.Fatalf("%s: the edit changed nothing, so it proves nothing", what)
				}

				if !sameAgent(installed, shipped) {
					t.Errorf("%s: refused as another agent", what)
				}
			}

			label := firstBetween(t, shipped, "<string>", "</string>")
			key := firstBetween(t, shipped, "<key>", "</key>")

			different := map[string]string{
				"a changed value":           strings.Replace(shipped, "<string>"+label+"</string>", "<string>"+label+".x</string>", 1),
				"a value with a space":      strings.Replace(shipped, "<string>"+label+"</string>", "<string>"+label+" </string>", 1),
				"a changed key":             strings.Replace(shipped, "<key>"+key+"</key>", "<key>"+key+"X</key>", 1),
				"an added key":              strings.Replace(shipped, "<dict>", "<dict>\n  <key>Disabled</key>\n  <true/>", 1),
				"an attribute":              strings.Replace(shipped, `<plist version="1.0">`, `<plist version="1.1">`, 1),
				"cut short":                 shipped[:len(shipped)/2],
				"unclosed":                  strings.Replace(shipped, "</plist>", "", 1),
				"trailing garbage":          shipped + "\n<",
				"not xml":                   "Label=" + label,
				"a value moved into a text": strings.Replace(shipped, "<string>"+label+"</string>", label, 1),
			}

			for what, installed := range different {
				if installed == shipped {
					t.Fatalf("%s: the edit changed nothing, so it proves nothing", what)
				}

				if sameAgent(installed, shipped) {
					t.Errorf("%s: accepted as billet's own agent", what)
				}
			}
		})
	}
}

// A VALUE OF ONLY WHITESPACE IS STILL A VALUE: the layout rule is for the text between
// a container's children, never for the text inside a string.
func TestAWhitespaceValueIsNotLayout(t *testing.T) {
	t.Parallel()

	const (
		spaced = `<plist version="1.0"><dict><key>Program</key><string> </string></dict></plist>`
		empty  = `<plist version="1.0"><dict><key>Program</key><string></string></dict></plist>`
		laid   = "<plist version=\"1.0\">\n  <dict>\n    <key>Program</key>\n    <string> </string>\n  </dict>\n</plist>\n"
	)

	if sameAgent(spaced, empty) {
		t.Error("a string of one space was read as an empty one")
	}

	if !sameAgent(spaced, laid) {
		t.Error("the same values laid out across lines were read as another agent")
	}
}

// WHAT APPLE'S PLIST PARSER REFUSES IS NOT THE SAME AGENT, though an XML reading of it
// would be: a CDATA or an entity spelling of a value, a comment inside a value element,
// a non-breaking space between elements.
func TestASpellingLaunchdRefusesIsAnotherAgent(t *testing.T) {
	t.Parallel()

	// <true></true>, not <true/>, so a comment inside it is all that differs.
	const shipped = "<plist version=\"1.0\">\n<dict>\n  <key>ExitTimeOut</key>\n  <integer>300</integer>\n" +
		"  <key>RunAtLoad</key>\n  <true></true>\n</dict>\n</plist>\n"

	if !sameAgent(shipped, shipped) {
		t.Fatal("the fixture is not the same as itself")
	}

	for what, installed := range map[string]string{
		"a CDATA integer":                   strings.Replace(shipped, "<integer>300</integer>", "<integer><![CDATA[300]]></integer>", 1),
		"an entity in an integer":           strings.Replace(shipped, "<integer>300</integer>", "<integer>&#51;00</integer>", 1),
		"a comment inside a value":          strings.Replace(shipped, "<true></true>", "<true><!-- note --></true>", 1),
		"a comment splitting a value":       strings.Replace(shipped, "<integer>300</integer>", "<integer>3<!-- x -->00</integer>", 1),
		"a non-breaking space between them": strings.Replace(shipped, "</integer>\n", "</integer> \n", 1),
		"an unterminated comment":           shipped + "<!-- ",
		"an unterminated element":           shipped + "<dict",
		"larger than any agent":             shipped + "<!--" + strings.Repeat("x", maxAgentBytes) + "-->",
	} {
		if sameAgent(installed, shipped) {
			t.Errorf("%s: accepted as billet's own agent", what)
		}
	}
}

// A ROLLBACK REMOVES ONLY WHAT THIS RUN WROTE, byte for byte: a comment an operator added
// since is theirs, though the agent is still billet's for every other purpose.
func TestARollbackLeavesAnAgentAnOperatorCommentedOn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := &Converger{agentsDir: dir}
	path := c.agentPath(deploy.ImagesAgentLabel)

	commented := strings.Replace(deploy.ImagesAgent, "<dict>", "<dict>\n  <!-- why we keep this -->", 1)
	if err := os.WriteFile(path, []byte(commented), 0o600); err != nil {
		t.Fatal(err)
	}

	if !sameAgent(commented, deploy.ImagesAgent) {
		t.Fatal("the commented agent is not billet's own, so this proves nothing about a rollback")
	}

	if err := c.removeAgent(deploy.ImagesAgentLabel); err == nil {
		t.Error("a rollback removed an agent an operator had commented on since this run wrote it")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the operator's agent is gone: %v", err)
	}

	// AN EDIT PAST WHAT IS READ is still an edit: the file is the agent this run wrote,
	// then layout up to the read's end, then the operator's comment.
	padded := deploy.ImagesAgent + strings.Repeat(" ", maxAgentBytes+1-len(deploy.ImagesAgent)) +
		"<!-- operator edit -->"
	if err := os.WriteFile(path, []byte(padded), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.removeAgent(deploy.ImagesAgentLabel); err == nil {
		t.Error("a rollback removed an agent whose edit lay past the part it read")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the operator's padded agent is gone: %v", err)
	}

	if err := os.WriteFile(path, []byte(deploy.ImagesAgent), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.removeAgent(deploy.ImagesAgentLabel); err != nil {
		t.Fatalf("a rollback of exactly what this run wrote: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the agent this run wrote survived its rollback: %v", err)
	}
}

func firstBetween(t *testing.T, s, open, closing string) string {
	t.Helper()

	_, rest, ok := strings.Cut(s, open)
	if !ok {
		t.Fatalf("no %s in the agent", open)
	}

	value, _, ok := strings.Cut(rest, closing)
	if !ok {
		t.Fatalf("no %s in the agent", closing)
	}

	return value
}
