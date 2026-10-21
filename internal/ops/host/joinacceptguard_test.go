package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// THE READ IS MADE UNDER THE LOCK: the stand-in edits the file as it is taken,
// so a read made before it would find the file changed and refuse, and one
// made under it merges the edit in. A dry run takes no lock at all.
func TestJoinAcceptReadsTheConfigUnderTheLock(t *testing.T) {
	f := newJoinFixture(t)

	out, err := runJoinLocked(t, func() {
		raw := mustRead(t, f.configPath)
		mustOK(t, os.WriteFile(f.configPath, []byte("# written under the lock\n"+raw), 0o600))
	}, "accept", "--config", f.configPath, f.proposal(t, f.deployment(t)))
	if err != nil {
		t.Fatalf("join accept: %v\n%s", err, out)
	}
	if !strings.Contains(mustRead(t, f.configPath), "# written under the lock") {
		t.Errorf("the merge did not start from the file as it was under the lock")
	}

	locksTaken = 0
	if _, err := runJoin(t, "", "accept", "--config", f.configPath, "--dry-run", f.proposal(t, f.deployment(t))); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if locksTaken != 0 {
		t.Errorf("a dry run took the lifecycle lock %d times", locksTaken)
	}
}

// A CONTROL PLANE WITH NO DEPLOYMENT IDENTITY YET is not the one a proposal
// bound to a deployment was made against.
func TestJoinAcceptRefusesWhenThisControlPlaneHasNoIdentity(t *testing.T) {
	f := newJoinFixture(t)
	token := f.proposal(t, f.deployment(t))
	mustOK(t, os.Remove(filepath.Join(f.stateDir, "deployment-id")))
	before := mustRead(t, f.configPath)

	_, err := runJoin(t, "", "accept", "--config", f.configPath, token)
	if err == nil || !strings.Contains(err.Error(), "no deployment identity yet") {
		t.Fatalf("a control plane with no identity answered %v", err)
	}
	if mustRead(t, f.configPath) != before {
		t.Errorf("a refused accept changed the config")
	}
}

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
			if after := mustRead(t, f.configPath); after != rendered {
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
	if err == nil || !strings.Contains(err.Error(), "changed after it was read") {
		t.Fatalf("a file changed under the merge answered %v", err)
	}
	if after := mustRead(t, f.configPath); after != string(edited) {
		t.Errorf("the edit made meanwhile was overwritten")
	}
	if locksTaken != 1 {
		t.Errorf("the lifecycle lock was taken %d times, want once", locksTaken)
	}
}
