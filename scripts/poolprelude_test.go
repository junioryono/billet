package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePreludeCurl writes the URL it was asked for into the file it was told to write,
// so a checksum taken of that URL passes only for the download that was wanted, and
// records every call.
const fakePreludeCurl = `out="" url=""
while [ "$#" -gt 0 ]; do
	case $1 in
	-o) out=$2; shift ;;
	https://*) url=$1 ;;
	esac
	shift
done
echo "$url" >>"$FAKE/curl.log"
printf '%s' "$url" >"$out"
`

// AN ARM64 POOL HOST INSTALLS ARM64'S BILLET, AND AN X64 HOST ITS OWN: the collection's
// prelude, run up to the binary's install with every command that would change the
// machine faked, fetches the binary named for the host's architecture, passes its
// checksum and reaches the install; an arm64 host named no binary of its own fetches the
// one named for both.
func TestThePoolPreludeFetchesTheHostsOwnArchitecturesBillet(t *testing.T) {
	t.Parallel()
	template, err := os.ReadFile("../ansible_collections/junioryono/billet/examples/pool-host-prelude/prelude.sh")
	if err != nil {
		t.Fatal(err)
	}
	const work = "work=/var/lib/billet-pool-prelude\n"
	if strings.Count(string(template), work) != 1 {
		t.Fatalf("the prelude no longer sets its work directory as %q once", work)
	}
	sum := func(url string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(url))) }
	const x64, arm = "https://example.test/billet-amd64", "https://example.test/billet-arm64"
	both := []string{"BILLET_URL=" + x64, "BILLET_SHA256=" + sum(x64),
		"BILLET_URL_ARM64=" + arm, "BILLET_SHA256_ARM64=" + sum(arm)}
	for _, c := range []struct {
		name, machine string
		env           []string
		want          string
	}{
		{"an arm64 host named both", "aarch64", both, arm},
		{"an x64 host named both", "x86_64", both, x64},
		{"an arm64 host named one", "aarch64", both[:2], x64},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newMatrixFakes(t, "sha256sum", "tar")
			f.command(t, "uname", `echo "`+c.machine+`"`)
			f.command(t, "apt-get", "exit 0")
			f.command(t, "curl", fakePreludeCurl)
			// THE STOP: the install is where the prelude would first change the machine.
			f.command(t, "install", `echo "$*" >>"$FAKE/install.log"; exit 1`)
			script := filepath.Join(t.TempDir(), "prelude.sh")
			body := strings.Replace(string(template), work, "work="+t.TempDir()+"\n", 1)
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			out, code := f.run(t, script, c.env)
			if code == 0 {
				t.Fatalf("the prelude ran past the faked install:\n%s", out)
			}
			fetched, err := os.ReadFile(filepath.Join(f.dir, "curl.log"))
			if err != nil || string(fetched) != c.want+"\n" {
				t.Fatalf("fetched %q (%v), want %s alone:\n%s", fetched, err, c.want, out)
			}
			installed, err := os.ReadFile(filepath.Join(f.dir, "install.log"))
			if err != nil || !strings.Contains(string(installed), "billet /usr/bin/billet") {
				t.Fatalf("the binary was not installed after its checksum (%q, %v):\n%s", installed, err, out)
			}
		})
	}
}
