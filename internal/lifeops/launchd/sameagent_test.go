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
