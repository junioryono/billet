package scripts_test

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fetchFakes stand in for the network and the disk: gh answers the artifact
// listing from $FAKE/listing, curl copies $FAKE/artifact.zip to its -o path and
// records its arguments, and df reports $FAKE/free bytes available.
var fetchFakes = map[string]string{
	"gh": `#!/bin/sh
printf 'gh %s\n' "$*" >> "$FAKE/calls"
cat "$FAKE/listing"
`,
	"curl": `#!/bin/sh
printf 'curl %s\n' "$*" >> "$FAKE/calls"
while [ $# -gt 0 ]; do
	if [ "$1" = -o ]; then cp "$FAKE/artifact.zip" "$2"; exit 0; fi
	shift
done
exit 2
`,
	"df": `#!/bin/sh
echo "Avail"
cat "$FAKE/free"
`,
}

// artifactZip writes a zip holding the named files, each with its own name as its
// bytes, and returns its sha256.
func artifactZip(t *testing.T, path string, names ...string) string {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	for _, name := range names {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:])
}

func runFetch(t *testing.T, fake string) (string, string, error) {
	t.Helper()

	bin := filepath.Join(fake, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range fetchFakes {
		if err := forkSafeWriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(fake, "out")
	temp := filepath.Join(fake, "temp")
	if err := os.MkdirAll(temp, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "./fetch-guest-artifact.sh", out)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE="+fake,
		"GH_TOKEN=token-for-test", "GITHUB_REPOSITORY=junioryono/billet", "GITHUB_RUN_ID=77",
		"RUNNER_TEMP="+temp)
	output, err := cmd.CombinedOutput()

	return out, string(output), err
}

// THE ARTIFACT IS FETCHED THROUGH THE API AND HELD TO GITHUB'S DIGEST before it is
// extracted: the one artifact of that name is downloaded by its id, with the token
// on the API request, and its files land in the directory.
func TestTheGuestArtifactIsFetchedWhole(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	digest := artifactZip(t, filepath.Join(fake, "artifact.zip"), "manifest.json", "vmlinux-billet")
	writeFake(t, fake, "listing", fmt.Sprintf(`{"artifacts":[{"id":42,"name":"guest-image","expired":false,`+
		`"size_in_bytes":1000,"digest":"sha256:%s"}]}`, digest))
	writeFake(t, fake, "free", "10737418240\n")

	out, output, err := runFetch(t, fake)
	if err != nil {
		t.Fatalf("fetch: %v\n%s", err, output)
	}
	for _, name := range []string{"manifest.json", "vmlinux-billet"} {
		if got, err := os.ReadFile(filepath.Join(out, name)); err != nil || string(got) != name {
			t.Errorf("%s was not extracted whole (%v): %q", name, err, got)
		}
	}
	calls, err := os.ReadFile(filepath.Join(fake, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repos/junioryono/billet/actions/runs/77/artifacts?name=guest-image",
		"Authorization: Bearer token-for-test",
		"https://api.github.com/repos/junioryono/billet/actions/artifacts/42/zip"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("the fetch did not ask for %q:\n%s", want, calls)
		}
	}
}

// A FETCH THAT CANNOT BE PROVED WHOLE EXTRACTS NOTHING: a zip whose digest is not
// GitHub's, an artifact with no digest, two artifacts of the name, or too little
// room each refuse, and the last three before anything is downloaded.
func TestTheGuestArtifactFetchRefusesWhatItCannotProve(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, listing, free, clause string
		downloads                   bool
	}{
		{"a zip that is not GitHub's", `{"artifacts":[{"id":42,"name":"guest-image","expired":false,` +
			`"size_in_bytes":1000,"digest":"sha256:` + strings.Repeat("0", 64) + `"}]}`,
			"10737418240", "is not the one GitHub describes", true},
		{"no digest", `{"artifacts":[{"id":42,"name":"guest-image","expired":false,"size_in_bytes":1000}]}`,
			"10737418240", "published no sha256", false},
		{"two artifacts of the name", `{"artifacts":[` +
			`{"id":42,"name":"guest-image","expired":false,"size_in_bytes":1000,"digest":"sha256:x"},` +
			`{"id":43,"name":"guest-image","expired":false,"size_in_bytes":1000,"digest":"sha256:x"}]}`,
			"10737418240", "exactly one", false},
		{"too little room", `{"artifacts":[{"id":42,"name":"guest-image","expired":false,` +
			`"size_in_bytes":10737418240,"digest":"sha256:` + strings.Repeat("0", 64) + `"}]}`,
			"10737418240", "free to hold", false},
	} {
		fake := t.TempDir()
		artifactZip(t, filepath.Join(fake, "artifact.zip"), "manifest.json", "vmlinux-billet")
		writeFake(t, fake, "listing", tc.listing)
		writeFake(t, fake, "free", tc.free+"\n")

		out, output, err := runFetch(t, fake)
		if err == nil || !strings.Contains(output, tc.clause) {
			t.Errorf("%s: the fetch answered %v:\n%s", tc.name, err, output)
		}
		if _, err := os.Stat(filepath.Join(out, "vmlinux-billet")); err == nil {
			t.Errorf("%s: the artifact was extracted although it was not proved whole", tc.name)
		}
		calls, err := os.ReadFile(filepath.Join(fake, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(calls), "curl "); got != tc.downloads {
			t.Errorf("%s: downloaded = %v, want %v:\n%s", tc.name, got, tc.downloads, calls)
		}
	}
}
