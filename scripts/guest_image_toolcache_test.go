package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonToolcacheUsesVendorChecksumBeforeDownloading(t *testing.T) {
	t.Parallel()

	const (
		version  = "3.14.7"
		tag      = "3.14.7-31064857500"
		filename = "python-3.14.7-linux-24.04-x64.tar.gz"
		artifact = "the vendor-published python archive"
	)

	releaseURL := "https://github.com/actions/python-versions/releases/tag/" + tag
	archiveURL := "https://github.com/actions/python-versions/releases/download/" + tag + "/" + filename
	hashesURL := "https://github.com/actions/python-versions/releases/download/" + tag + "/hashes.sha256"
	wantHash := fmt.Sprintf("%X", sha256.Sum256([]byte(artifact)))
	manifest := func(releaseURL, filename, archiveURL string, releases, assets int) string {
		file := fmt.Sprintf(`{"filename":%q,"download_url":%q,"platform":"linux","platform_version":"24.04","arch":"x64"}`, filename, archiveURL)
		files := strings.TrimSuffix(strings.Repeat(file+",", assets), ",")
		release := fmt.Sprintf(`{"version":%q,"release_url":%q,"files":[%s]}`, version, releaseURL, files)

		return "[" + strings.TrimSuffix(strings.Repeat(release+",", releases), ",") + "]"
	}
	validManifest := manifest(releaseURL, filename, archiveURL, 1, 1)
	validHashes := wantHash + " " + filename + "\n"

	for _, tc := range []struct {
		name         string
		manifest     string
		hashes       string
		artifact     string
		wantSuccess  bool
		wantError    string
		wantRequests []string
		wantArchive  bool
	}{
		{
			name:         "published hash matches",
			manifest:     validManifest,
			hashes:       validHashes,
			artifact:     artifact,
			wantSuccess:  true,
			wantRequests: []string{hashesURL, archiveURL},
			wantArchive:  true,
		},
		{
			name:         "published binary-mode hash matches",
			manifest:     validManifest,
			hashes:       wantHash + " *" + filename + "\n",
			artifact:     artifact,
			wantSuccess:  true,
			wantRequests: []string{hashesURL, archiveURL},
			wantArchive:  true,
		},
		{
			name:         "published hash does not match",
			manifest:     validManifest,
			hashes:       strings.Repeat("0", sha256.Size*2) + " " + filename + "\n",
			artifact:     artifact,
			wantError:    "did NOT match",
			wantRequests: []string{hashesURL, archiveURL},
			// NO ARCHIVE, THOUGH IT WAS FETCHED. wantRequests is what proves the
			// download happened; the file itself must not survive a failed check,
			// because anything that unpacks by PATH rather than by return value
			// would then get bytes the vendor's checksum rejected. This case used
			// to assert the opposite, which described the code rather than the
			// property.
			wantArchive: false,
		},
		{
			name:         "release omits the asset hash",
			manifest:     validManifest,
			hashes:       wantHash + " some-other-asset.tar.gz\n",
			wantError:    "no single published checksum",
			wantRequests: []string{hashesURL},
		},
		{
			name:         "release duplicates the asset hash",
			manifest:     validManifest,
			hashes:       validHashes + validHashes,
			wantError:    "no single published checksum",
			wantRequests: []string{hashesURL},
		},
		{
			name:         "release publishes an invalid digest",
			manifest:     validManifest,
			hashes:       "not-a-sha256 " + filename + "\n",
			wantError:    "no single published checksum",
			wantRequests: []string{hashesURL},
		},
		{
			name:         "checksum record has extra fields",
			manifest:     validManifest,
			hashes:       wantHash + " " + filename + " extra\n",
			artifact:     artifact,
			wantError:    "no single published checksum",
			wantRequests: []string{hashesURL},
		},
		{
			name:         "checksum record is tab separated",
			manifest:     validManifest,
			hashes:       wantHash + "\t" + filename + "\n",
			artifact:     artifact,
			wantError:    "no single published checksum",
			wantRequests: []string{hashesURL},
		},
		{
			name:         "checksum record is over spaced",
			manifest:     validManifest,
			hashes:       wantHash + "  " + filename + "\n",
			artifact:     artifact,
			wantError:    "no single published checksum",
			wantRequests: []string{hashesURL},
		},
		{
			name:         "checksum duplicates text and binary records",
			manifest:     validManifest,
			hashes:       validHashes + wantHash + " *" + filename + "\n",
			artifact:     artifact,
			wantError:    "no single published checksum",
			wantRequests: []string{hashesURL},
		},
		{
			name:      "manifest points at an untrusted release origin",
			manifest:  manifest("https://example.invalid/tag/"+tag, filename, archiveURL, 1, 1),
			hashes:    validHashes,
			wantError: "not from actions/python-versions",
		},
		{
			name:      "release tag is unsafe",
			manifest:  manifest(releaseURL+"/../other", filename, archiveURL, 1, 1),
			hashes:    validHashes,
			wantError: "unsafe tag or asset name",
		},
		{
			name:      "release tag is a dot segment",
			manifest:  manifest("https://github.com/actions/python-versions/releases/tag/.", filename, "https://github.com/actions/python-versions/releases/download/./"+filename, 1, 1),
			hashes:    validHashes,
			wantError: "unsafe tag or asset name",
		},
		{
			name:      "asset filename is unsafe",
			manifest:  manifest(releaseURL, "../"+filename, archiveURL, 1, 1),
			hashes:    validHashes,
			wantError: "unsafe tag or asset name",
		},
		{
			name:      "asset filename is a dot segment",
			manifest:  manifest(releaseURL, "..", "https://github.com/actions/python-versions/releases/download/"+tag+"/..", 1, 1),
			hashes:    wantHash + " ..\n",
			wantError: "unsafe tag or asset name",
		},
		{
			name:      "asset URL does not belong to the release",
			manifest:  manifest(releaseURL, filename, archiveURL+".replacement", 1, 1),
			hashes:    validHashes,
			wantError: "not the file published by its release",
		},
		{
			name:      "manifest duplicates the version",
			manifest:  manifest(releaseURL, filename, archiveURL, 2, 1),
			hashes:    validHashes,
			wantError: "no single linux 24.04 x64 release asset",
		},
		{
			name:      "manifest duplicates the platform asset",
			manifest:  manifest(releaseURL, filename, archiveURL, 1, 2),
			hashes:    validHashes,
			wantError: "no single linux 24.04 x64 release asset",
		},
		{
			name:      "manifest is malformed",
			manifest:  `[{`,
			hashes:    validHashes,
			wantError: "no single linux 24.04 x64 release asset",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			tools := filepath.Join(root, "tools")
			if err := os.Mkdir(tools, 0o700); err != nil {
				t.Fatalf("make fake tool directory: %v", err)
			}

			logPath := filepath.Join(root, "curl.log")
			outPath := filepath.Join(root, "python.tgz")
			manifestPath := filepath.Join(root, "manifest.json")
			if err := os.WriteFile(manifestPath, []byte(tc.manifest), 0o600); err != nil {
				t.Fatalf("write manifest fixture: %v", err)
			}

			writeExecutable(t, filepath.Join(tools, "curl"), `#!/bin/sh
set -eu
out=""
url=""
while [ "$#" -gt 0 ]; do
	case "$1" in
		-o) out="$2"; shift 2 ;;
		*) url="$1"; shift ;;
	esac
done
printf '%s\n' "$url" >>"$CURL_LOG"
case "$url" in
	*/hashes.sha256) printf '%s' "$HASHES" ;;
	*.tar.gz)
		if [ -n "$out" ]; then
			printf '%s' "$ARTIFACT" >"$out"
		else
			printf '%s' "$ARTIFACT"
		fi
		;;
	*) exit 44 ;;
esac
`)

			script := filepath.Join(root, "exercise-python-toolcache.sh")
			body := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				guestImageFunction(t, "fetch_verified") + "\n" +
				guestImageFunction(t, "python_release_tag") + "\n" +
				guestImageFunction(t, "python_release_checksum") + "\n" +
				guestImageFunction(t, "fetch_python_toolcache") + "\n" +
				"manifest=$(<\"$MANIFEST\")\n" +
				"fetch_python_toolcache \"$manifest\" \"$VERSION\" \"$OUT\"\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatalf("write shell exercise: %v", err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", script)
			cmd.Env = append(os.Environ(),
				"PATH="+tools+":"+os.Getenv("PATH"),
				// THE INSTALLERS ASK THE ARCHITECTURE OF EVERY VENDOR, and each
				// spells it differently, so the readers take it from here rather
				// than assuming x64 the way they used to.
				"BILLET_TC_ARCH=x64",
				"MANIFEST="+manifestPath,
				"VERSION="+version,
				"OUT="+outPath,
				"CURL_LOG="+logPath,
				"HASHES="+tc.hashes,
				"ARTIFACT="+tc.artifact,
			)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("fetch error = %v; want success %t\n%s", err, tc.wantSuccess, output)
			}
			if tc.wantError != "" && !strings.Contains(string(output), tc.wantError) {
				t.Fatalf("fetch output = %q, want diagnostic containing %q", output, tc.wantError)
			}

			rawLog, readErr := os.ReadFile(logPath)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read curl log: %v", readErr)
			}
			gotRequests := strings.Fields(string(rawLog))
			if strings.Join(gotRequests, "\n") != strings.Join(tc.wantRequests, "\n") {
				t.Fatalf("curl requests = %q, want %q", gotRequests, tc.wantRequests)
			}

			if tc.wantArchive {
				got, readErr := os.ReadFile(outPath)
				if readErr != nil {
					t.Fatalf("read downloaded archive: %v", readErr)
				}
				if string(got) != tc.artifact {
					t.Fatalf("downloaded archive = %q, want %q", got, tc.artifact)
				}
			} else if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
				t.Fatalf("an archive was downloaded without a trusted checksum: %v", statErr)
			}
		})
	}
}

// guestImageFunction returns one shell function from the build, wherever it
// lives.
//
// TWO FILES, AND EXACTLY ONE DEFINITION. The toolcache installers moved to
// internal/runnerimages/install-toolcache.sh so the EC2 backend runs the same
// code, and build-guest-image.sh sources it — so the pair is one program and a
// test must find a function in either. Finding it in BOTH is the failure this
// whole arrangement exists to prevent: two copies that drift, which is what a
// second hand-written implementation would have been.
func guestImageFunction(t *testing.T, name string) string {
	t.Helper()

	var found string

	for _, path := range []string{"build-guest-image.sh", toolcacheAssetPath} {
		source := readScriptFile(t, path)

		start := strings.Index(source, name+"() {")
		if start < 0 {
			continue
		}

		if found != "" {
			t.Fatalf("%s is defined in build-guest-image.sh AND in %s; two copies of one "+
				"installer is what sourcing a shared file exists to prevent",
				name, toolcacheAssetPath)
		}

		end := strings.Index(source[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not find the end of %s in %s", name, path)
		}

		found = source[start : start+end+2]
	}

	if found == "" {
		t.Fatalf("neither build-guest-image.sh nor %s has a %s function",
			toolcacheAssetPath, name)
	}

	return found
}
