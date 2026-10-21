package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// THE INVENTORY'S FILE IS RECOGNISED HOWEVER AN EDITOR LEFT IT: a byte-order
// mark, CRLF line ends, or a comment above the marker do not let an edit
// through that the next converge would undo.
func TestJoinAcceptRecognisesTheInventorysFileInEveryForm(t *testing.T) {
	for name, wrap := range map[string]func(string) string{
		"a byte-order mark": func(body string) string { return "\xef\xbb\xbf" + initconfig.RoleRenderedMarker + "\n" + body },
		"CRLF line ends": func(body string) string {
			return strings.ReplaceAll(initconfig.RoleRenderedMarker+"\n"+body, "\n", "\r\n")
		},
		"a comment above it": func(body string) string {
			return "# managed by site.yml\n\n" + initconfig.RoleRenderedMarker + "\n" + body
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newJoinFixture(t)
			raw, err := os.ReadFile(f.configPath)
			mustOK(t, err)
			rendered := wrap(string(raw))
			mustOK(t, os.WriteFile(f.configPath, []byte(rendered), 0o600))

			// THE FILE STILL LOADS, so the refusal is the marker's.
			if _, err := config.Parse("rendered", []byte(rendered)); err != nil {
				t.Fatalf("the wrapped config does not load: %v", err)
			}

			_, err = runJoin(t, "", "accept", "--config", f.configPath, f.proposal(t, f.deployment(t)))
			if err == nil || !strings.Contains(err.Error(), "the inventory's") {
				t.Fatalf("the inventory's file was edited: %v", err)
			}
			if after, _ := os.ReadFile(f.configPath); string(after) != rendered {
				t.Errorf("the inventory's file was changed")
			}
		})
	}
}

// A SYMLINKED CONFIG IS EDITED AT ITS TARGET, and the link stays a link: the
// write is a rename, and renaming over the link would replace it and leave the
// file it named untouched.
func TestJoinAcceptEditsASymlinkedConfigAtItsTarget(t *testing.T) {
	f := newJoinFixture(t)
	link := filepath.Join(t.TempDir(), "billet.yaml")
	mustOK(t, os.Symlink(f.configPath, link))

	if out, err := runJoin(t, "", "accept", "--config", link, f.proposal(t, f.deployment(t))); err != nil {
		t.Fatalf("join accept: %v\n%s", err, out)
	}

	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced (%v)", err)
	}

	cfg, err := config.Load(f.configPath)
	mustOK(t, err)
	if cfg.Server.MaxVCPU != 18 {
		t.Errorf("the target's ceiling is %d, want 18: the edit did not land on the file the link names",
			cfg.Server.MaxVCPU)
	}
}

// ONE EDIT AT A TIME: the accept holds the lifecycle lock, and a file changed
// between the merge and the write is refused, never overwritten with a merge
// of what it used to say.
func TestJoinAcceptRefusesAFileChangedUnderIt(t *testing.T) {
	f := newJoinFixture(t)

	merged := joinAcceptMerged
	t.Cleanup(func() { joinAcceptMerged = merged })

	var edited []byte
	joinAcceptMerged = func() {
		raw, err := os.ReadFile(f.configPath)
		mustOK(t, err)
		edited = append([]byte("# edited meanwhile\n"), raw...)
		mustOK(t, os.WriteFile(f.configPath, edited, 0o600))
	}

	locksTaken = 0
	_, err := runJoin(t, "", "accept", "--config", f.configPath, f.proposal(t, f.deployment(t)))
	if err == nil || !strings.Contains(err.Error(), "changed while the merge was made") {
		t.Fatalf("a file changed under the merge answered %v", err)
	}
	if after, _ := os.ReadFile(f.configPath); !bytes.Equal(after, edited) {
		t.Errorf("the edit made meanwhile was overwritten")
	}
	if locksTaken != 1 {
		t.Errorf("the lifecycle lock was taken %d times, want once", locksTaken)
	}
}
