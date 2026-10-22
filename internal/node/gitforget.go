package node

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// gitMirrorsDirectory is where the node keeps its git mirrors, under its state
// directory.
const gitMirrorsDirectory = "git-mirrors"

// ForgetGitMirrors marks every mirror of owner/repo the node keeps under
// stateDir, in every trust class, broken: from then on nothing is served from
// it or fetched into it, and the node removes it, with no fetch being served
// from it, at the repository's next advertisement or its next reap. Names are
// matched as GitHub matches them, ignoring case. It returns the mirrors it
// marked, by trust class, owner and name, and those it marked before any error.
//
// THE NODE REMOVES IT, NOT THIS: the running node holds the mirror while it
// serves from it, and only its own claim waits for that before the mirror goes
// and a new one is fetched in its place. An answer already being sent when the
// mark lands is let finish.
//
// A LINK IS REFUSED, NEVER FOLLOWED OR PASSED OVER: the node serves through
// the path whatever it points at, so a link skipped would be a mirror reported
// absent and still served, and one followed could reach outside the mirrors.
func ForgetGitMirrors(stateDir, owner, repo string) ([]string, error) {
	for _, segment := range []string{owner, repo} {
		if !gitNameSegment(segment) {
			return nil, fmt.Errorf("%q is not a GitHub owner or repository name", segment)
		}
	}
	root := filepath.Join(stateDir, gitMirrorsDirectory)
	trusts, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var forgotten []string
	for _, trust := range trusts {
		if trust.Name() == gitDiscardDirectory {
			continue
		}
		if trust.Type()&fs.ModeSymlink != 0 {
			return forgotten, fmt.Errorf("%s is a link; nothing behind it is forgotten", filepath.Join(root, trust.Name()))
		}
		if !trust.IsDir() {
			continue
		}
		owners, err := os.ReadDir(filepath.Join(root, trust.Name()))
		if err != nil {
			return forgotten, err
		}
		for _, held := range owners {
			if !gitNameSegment(held.Name()) || !strings.EqualFold(held.Name(), owner) {
				continue
			}
			ownerPath := filepath.Join(root, trust.Name(), held.Name())
			if held.Type()&fs.ModeSymlink != 0 {
				return forgotten, fmt.Errorf("%s is a link; nothing behind it is forgotten", ownerPath)
			}
			if !held.IsDir() {
				continue
			}
			repos, err := os.ReadDir(ownerPath)
			if err != nil {
				return forgotten, err
			}
			for _, mirror := range repos {
				// THE SUFFIX IN ANY CASE: on a filesystem that ignores case,
				// api.GIT is the mirror the node serves as api.git.
				entry := mirror.Name()
				if len(entry) <= len(".git") || !strings.EqualFold(entry[len(entry)-len(".git"):], ".git") {
					continue
				}
				name := entry[:len(entry)-len(".git")]
				if !gitNameSegment(name) || !strings.EqualFold(name, repo) {
					continue
				}
				path := filepath.Join(ownerPath, mirror.Name())
				if mirror.Type()&fs.ModeSymlink != 0 {
					return forgotten, fmt.Errorf("%s is a link; it is not forgotten", path)
				}
				if !mirror.IsDir() {
					continue
				}
				if err := markBroken(path); err != nil {
					return forgotten, err
				}
				forgotten = append(forgotten, trust.Name()+"/"+held.Name()+"/"+name)
			}
		}
	}

	return forgotten, nil
}

// gitNameSegment is whether name is an owner or repository name the proxy
// keeps a mirror under: ASCII, so matching it ignoring case folds nothing
// else, and never a leading dot.
func gitNameSegment(name string) bool {
	return gitSegment.MatchString(name) && !strings.HasPrefix(name, ".")
}
