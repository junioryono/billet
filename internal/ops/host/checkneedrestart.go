package host

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/regularfile"
)

// needrestartConfigLimit bounds one needrestart configuration file's read.
const needrestartConfigLimit = 1 << 20

// reportNeedrestart warns when needrestart is installed under root and no
// configuration file it loads (needrestart.conf, then conf.d/*.conf) carries
// the exclusion the package and the host role install. Unattended upgrades
// run needrestart, which restarts every service mapping an upgraded library,
// and a billet-node restart is a drain with no time limit. ADVISORY: it reports
// and decides nothing, and it recognises only the shipped line, so a host kept
// safe some other way (needrestart told never to restart) still reads as a
// warning that names the file to install.
func reportNeedrestart(w io.Writer, root string) {
	confDir := filepath.Join(root, "etc", "needrestart")

	installed := false
	for _, path := range []string{filepath.Join(root, "usr", "sbin", "needrestart"), confDir} {
		switch _, err := os.Lstat(path); {
		case err == nil:
			installed = true
		case !errors.Is(err, fs.ErrNotExist):
			_, _ = fmt.Fprintf(w, "restarts could not tell whether needrestart is installed: %v\n", err)

			return
		}
	}

	if !installed {
		return
	}

	entries, err := os.ReadDir(filepath.Join(confDir, "conf.d"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(w, "restarts could not tell whether needrestart excludes billet's services: %v\n", err)

		return
	}

	var dropIns []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}

		dropIns = append(dropIns, filepath.Join(confDir, "conf.d", entry.Name()))
	}
	sort.Strings(dropIns)

	files := make([]string, 0, 1+len(dropIns))
	files = append(files, filepath.Join(confDir, "needrestart.conf"))
	files = append(files, dropIns...)

	var unreadable []error
	for _, path := range files {
		body, err := regularfile.ReadFile(path, needrestartConfigLimit, regularfile.Options{})
		if errors.Is(err, fs.ErrNotExist) {
			// ABSENT ONLY WHEN THE NAME IS: a dangling symlink is a file
			// needrestart fails to load, not one it does without.
			if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
				continue
			}
		}

		if err != nil {
			unreadable = append(unreadable, err)

			continue
		}

		for line := range strings.SplitSeq(string(body), "\n") {
			if strings.TrimSpace(line) == deploy.NeedrestartExclusion {
				_, _ = fmt.Fprintf(w, "restarts needrestart leaves billet's services alone (%s)\n", path)

				return
			}
		}
	}

	if len(unreadable) > 0 {
		_, _ = fmt.Fprintf(w, "restarts could not tell whether needrestart excludes billet's services: %v\n",
			errors.Join(unreadable...))

		return
	}

	_, _ = fmt.Fprintf(w, "restarts WARNING: needrestart is installed and nothing excludes billet's services, so "+
		"an unattended library upgrade can restart billet-node, a drain with no time limit; put %q in %s "+
		"(the package and the host role install it)\n", deploy.NeedrestartExclusion, deploy.NeedrestartDropInPath)
}
