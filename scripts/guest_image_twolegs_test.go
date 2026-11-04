package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/imagesource"
)

// twoSetManifest is a manifest of the assets with SUFFIX, of ARCH and CONTRACT, with
// real digests of what addAarch64Set and publishDir write.
func twoSetManifest(suffix, arch, contract string) string {
	digest := func(name string) string {
		sum := sha256.Sum256([]byte(name))

		return hex.EncodeToString(sum[:])
	}
	rootfs, kernel := "rootfs"+suffix+".img.zst.part000", "vmlinux-billet"+suffix

	return fmt.Sprintf(`{"schema":2,"arch":%q,"guest_contract":%q,"built_at":"2026-11-01T00:00:00Z",`+
		`"runner_version":"2.337.0","rootfs_multipart":{"name":"rootfs%s.img.zst","sha256":"x","size":1,`+
		`"parts":[{"name":%q,"sha256":%q,"size":1}]},`+
		`"kernel":{"name":%q,"sha256":%q,"size":1,"version":"6.1.155"}}`,
		arch, contract, suffix, rootfs, digest(rootfs), kernel, digest(kernel))
}

// addAarch64Set writes into DIR a whole aarch64 set under the names ADR-022 gives it.
func addAarch64Set(t *testing.T, dir, manifest string) {
	t.Helper()

	// THE SOURCE ARCHIVE'S FIXTURE WRITES THE x86_64 NAMES, in a directory of its own
	// here, and the set's names are what the publisher holds a part list to.
	work := t.TempDir()
	writeSourceArchive(t, work, publishIndex, 0)

	sum := sha256.Sum256(readBytes(t, filepath.Join(work, "image-sources.tar")))
	stat, err := os.Stat(filepath.Join(work, "image-sources.tar"))
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string][]byte{
		"manifest-aarch64.json":           []byte(manifest),
		"manifest-aarch64.sigstore.json":  []byte("{}"),
		"rootfs-aarch64.img.zst.part000":  []byte("rootfs-aarch64.img.zst.part000"),
		"vmlinux-billet-aarch64":          []byte("vmlinux-billet-aarch64"),
		"image-sources-aarch64.tsv":       readBytes(t, filepath.Join(dir, "image-sources.tsv")),
		"image-sources-aarch64-index.tsv": readBytes(t, filepath.Join(work, "image-sources-index.tsv")),
		"image-sources-aarch64.tar":       readBytes(t, filepath.Join(work, "image-sources.tar")),
		"image-sources-aarch64.parts": fmt.Appendf(nil, "image-sources-aarch64.tar %s %d\n",
			hex.EncodeToString(sum[:]), stat.Size()),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// x64PublishDir is a whole x86_64 set, with a manifest whose contract the aarch64 set
// is held to.
func x64PublishDir(t *testing.T) string {
	t.Helper()

	return publishDir(t, twoSetManifest("", "x86_64", "10"), "rootfs.img.zst.part000", "vmlinux-billet")
}

// ONE RELEASE CARRIES BOTH SETS, and a set is published whole or the release is not
// made: a release is immutable, so what the channel says it carries cannot be added
// afterwards. Every refusal comes before the tag.
func TestTheGuestReleaseCarriesAWholeAarch64SetOrNothing(t *testing.T) {
	t.Parallel()

	good := twoSetManifest("-aarch64", "aarch64", "10")

	for name, tc := range map[string]struct {
		manifest string
		edit     func(t *testing.T, dir string)
		says     string
	}{
		"a whole set": {manifest: good},
		"another machine's manifest": {
			manifest: twoSetManifest("-aarch64", "x86_64", "10"),
			says:     "does not describe an aarch64 image",
		},
		"another contract": {
			manifest: twoSetManifest("-aarch64", "aarch64", "9"),
			says:     "speaks another guest contract",
		},
		"no signature": {
			manifest: good,
			edit: func(t *testing.T, dir string) {
				t.Helper()
				removeAll(t, filepath.Join(dir, "manifest-aarch64.sigstore.json"))
			},
			says: "no manifest-aarch64.sigstore.json",
		},
		"no kernel": {
			manifest: good,
			edit:     func(t *testing.T, dir string) { t.Helper(); removeAll(t, filepath.Join(dir, "vmlinux-billet-aarch64")) },
			says:     "the manifest names vmlinux-billet-aarch64 and the build did not produce it",
		},
		"a truncated part": {
			manifest: good,
			edit: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, filepath.Join(dir, "rootfs-aarch64.img.zst.part000"), "cut", 0o600)
			},
			says: "rootfs-aarch64.img.zst.part000 is not the file the manifest describes",
		},
		"no source list": {
			manifest: good,
			edit: func(t *testing.T, dir string) {
				t.Helper()
				removeAll(t, filepath.Join(dir, "image-sources-aarch64.parts"))
			},
			says: "no image-sources-aarch64.parts",
		},
		"the x86_64 source's name in its list": {
			manifest: good,
			edit: func(t *testing.T, dir string) {
				t.Helper()
				body := strings.Replace(string(readBytes(t, filepath.Join(dir, "image-sources-aarch64.parts"))),
					"image-sources-aarch64.tar", "image-sources.tar", 1)
				writeFile(t, filepath.Join(dir, "image-sources-aarch64.parts"), body, 0o600)
			},
			says: "image-sources-aarch64.parts does not begin with the whole archive",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := x64PublishDir(t)
			addAarch64Set(t, dir, tc.manifest)

			if tc.edit != nil {
				tc.edit(t, dir)
			}

			out, ok, calls := runPublisher(t, dir)
			if tc.says != "" {
				if ok || !strings.Contains(out, tc.says) {
					t.Fatalf("answered %v, want a refusal saying %q:\n%s", ok, tc.says, out)
				}

				if strings.Contains(calls, "git tag") {
					t.Errorf("the refusal came after the tag:\n%s", calls)
				}

				return
			}

			if !ok {
				t.Fatalf("the publisher refused a whole set:\n%s", out)
			}

			for _, asset := range []string{
				"manifest.json", "manifest.sigstore.json", "rootfs.img.zst.part000", "vmlinux-billet",
				"image-sources.tsv", "image-sources-index.tsv", "image-sources.parts", "image-sources.tar",
				"manifest-aarch64.json", "manifest-aarch64.sigstore.json", "rootfs-aarch64.img.zst.part000",
				"vmlinux-billet-aarch64", "image-sources-aarch64.tsv", "image-sources-aarch64-index.tsv",
				"image-sources-aarch64.parts", "image-sources-aarch64.tar",
			} {
				if !strings.Contains(calls, "arg "+filepath.Join(dir, asset)+"\n") {
					t.Errorf("the release does not carry %s:\n%s", asset, calls)
				}
			}
		})
	}
}

// AN x86_64 RELEASE ALONE IS WHAT IT ALWAYS WAS: no aarch64 asset in it, and no aarch64
// file asked for.
func TestTheGuestReleaseWithoutAnAarch64SetIsTheOneItAlwaysWas(t *testing.T) {
	t.Parallel()

	out, ok, calls := runPublisher(t, x64PublishDir(t))
	if !ok {
		t.Fatalf("the publisher refused an x86_64 release:\n%s", out)
	}

	if strings.Contains(calls, "aarch64") {
		t.Errorf("an x86_64 release names aarch64:\n%s", calls)
	}
}

// THE POINTER SAYS WHAT THE RELEASE CARRIES, and a reader takes it at its word: a list
// the reader refuses would stop every node of every machine, so this is held to the
// reader's own refusal rather than to a copy of its rules. Every field is one the reader
// requires of a schema 1 pointer.
func TestTheGuestChannelListsWhatTheReleaseCarries(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		files []string
		want  []string
	}{
		"x86_64 alone": {[]string{"manifest.json"}, []string{"x86_64"}},
		"both":         {[]string{"manifest.json", "manifest-aarch64.json"}, []string{"x86_64", "aarch64"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out := t.TempDir()
			for _, file := range tc.files {
				writeFile(t, filepath.Join(out, file), "{}", 0o600)
			}

			cmd := exec.CommandContext(t.Context(), "bash", "write-guest-channel.sh", out)
			cmd.Env = append(os.Environ(), "TAG=guest-20261104-000000")

			if said, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, said)
			}

			var pointer struct {
				Schema           int      `json:"schema"`
				Tag              string   `json:"tag"`
				PublishedAt      string   `json:"published_at"`
				ExpiresAt        string   `json:"expires_at"`
				ReleaseImmutable bool     `json:"release_immutable"`
				Architectures    []string `json:"architectures"`
			}

			if err := json.Unmarshal(readBytes(t, filepath.Join(out, "current.json")), &pointer); err != nil {
				t.Fatal(err)
			}

			if pointer.Schema != 1 || pointer.Tag != "guest-20261104-000000" || !pointer.ReleaseImmutable ||
				pointer.PublishedAt == "" || pointer.ExpiresAt <= pointer.PublishedAt {
				t.Errorf("the pointer is not one a reader takes: %+v", pointer)
			}

			if !slices.Equal(pointer.Architectures, tc.want) {
				t.Errorf("the pointer lists %v, want %v", pointer.Architectures, tc.want)
			}

			if err := imagesource.CheckChannelArchitectures(pointer.Architectures); err != nil {
				t.Errorf("a reader refuses what the writer wrote: %v", err)
			}
		})
	}

	// NO RELEASE, NO POINTER: a list with nothing behind it would still be signed.
	cmd := exec.CommandContext(t.Context(), "bash", "write-guest-channel.sh", t.TempDir())
	cmd.Env = append(os.Environ(), "TAG=guest-20261104-000000")

	if said, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(said), "no manifest.json") {
		t.Errorf("a directory holding no release answered %v:\n%s", err, said)
	}
}

// THE PUBLISHED NAMES ARE THE ONES A NODE ASKS FOR. The suffix a leg builds with is what
// a reader derives from its machine, and a mismatch is a release with an image no node
// fetches.
func TestTheLegsPublishTheNamesTheReaderAsksFor(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Uses  string            `yaml:"uses"`
			If    string            `yaml:"if"`
			Needs any               `yaml:"needs"`
			With  map[string]string `yaml:"with"`
		} `yaml:"jobs"`
	}

	caller, _, _ := guestImageWorkflows(t)
	if err := yaml.Unmarshal([]byte(caller), &workflow); err != nil {
		t.Fatal(err)
	}

	for job, want := range map[string]struct{ machine, runsOn, image, suffix string }{
		"x64":   {"x86_64", "billet-image-builder-ubuntu-2404", "ubuntu-2404-x64", ""},
		"arm64": {"aarch64", "billet-image-builder-ubuntu-2404-arm64", "ubuntu-2404-arm64", "-aarch64"},
	} {
		leg, ok := workflow.Jobs[job]
		if !ok {
			t.Fatalf("guest-image.yml has no %s job", job)
		}

		if leg.Uses != "./.github/workflows/guest-image-build.yml" || leg.With["machine"] != want.machine ||
			leg.With["runs_on"] != want.runsOn || leg.With["image_name"] != want.image || leg.With["suffix"] != want.suffix {
			t.Errorf("the %s leg is %+v, want %+v", job, leg, want)
		}

		manifest, bundle := imagesource.ManifestNamesFor(want.machine)
		if manifest != "manifest"+want.suffix+".json" || bundle != "manifest"+want.suffix+".sigstore.json" {
			t.Errorf("a %s node asks for %s and %s, and its leg publishes the suffix %q", want.machine, manifest, bundle, want.suffix)
		}

		if want.machine == "aarch64" && !strings.Contains(readScriptFile(t, "publish-guest-release.sh"), `check_set `+want.suffix) {
			t.Errorf("the publisher does not check a set with the suffix %s", want.suffix)
		}
	}

	publish := workflow.Jobs["publish"].If
	for _, want := range []string{"always()", "needs.x64.result == 'success'", "needs.resolve.result == 'success'"} {
		if !strings.Contains(publish, want) {
			t.Errorf("the publish job's condition %q does not hold %q", publish, want)
		}
	}

	// aarch64 FAILING IS WHAT IT HAS A RULE FOR, so its result is no condition of the job.
	if strings.Contains(publish, "needs.arm64") {
		t.Errorf("the publish job's condition %q depends on the aarch64 build", publish)
	}
}

// THE PUBLISH JOB TAKES A SUCCEEDED aarch64 BUILD'S ARTIFACT, and carries only when it
// did not succeed: carried after a build that passed and whose artifact would not
// download, a release would publish an older image as if nothing had happened.
func TestOnlyAFailedAarch64BuildCarriesAnImage(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}

	caller, _, _ := guestImageWorkflows(t)
	if err := yaml.Unmarshal([]byte(caller), &workflow); err != nil {
		t.Fatal(err)
	}

	conditions := map[string]string{}

	for _, step := range workflow.Jobs["publish"].Steps {
		conditions[step.Name] = step.If
	}

	for name, want := range map[string]string{
		"Download the checked image":         "",
		"Download the checked aarch64 image": "needs.arm64.result == 'success'",
		"Carry the last good aarch64 image":  "needs.arm64.result != 'success'",
		"Publish the dated release":          "",
		"Advance the guest channel":          "",
	} {
		got, ok := conditions[name]
		if !ok {
			t.Errorf("the publish job has no step %q", name)
		} else if got != want {
			t.Errorf("step %q runs if %q, want %q", name, got, want)
		}
	}
}

// carryFixture is what carry-guest-set.sh meets: a channel, a previous release and the
// tools that reach them, each stubbed and each answer chosen by a case.
type carryFixture struct {
	pointer  string // the channel's pointer, or "" for a channel gh cannot read
	notFound bool   // gh answers the channel with GitHub's 404
	assets   string // the previous release's asset names, one a line
	manifest string // its aarch64 manifest
	verify   int    // what the signature check exits with
	noAssets bool   // the second download fails
}

// runCarry runs the script against a fixture and returns its output, whether it
// succeeded, and the directory it worked on.
func runCarry(t *testing.T, fx carryFixture) (string, bool, string) {
	t.Helper()

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	bin := filepath.Join(dir, "bin")

	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(out, "manifest.json"), twoSetManifest("", "x86_64", "10"), 0o600)
	writeFile(t, filepath.Join(dir, "pointer"), fx.pointer, 0o600)
	writeFile(t, filepath.Join(dir, "assets"), fx.assets, 0o600)
	writeFile(t, filepath.Join(dir, "aarch64-manifest"), fx.manifest, 0o600)

	apiAnswer := `printf '%s' "$(base64 <"$FAKE/pointer" | tr -d '\n')"`

	switch {
	case fx.notFound:
		apiAnswer = `echo 'gh: Not Found (HTTP 404)' >&2; exit 1`
	case fx.pointer == "":
		apiAnswer = `echo 'gh: connection reset by peer' >&2; exit 1`
	}

	gh := "#!/bin/bash\nprintf '%s\\n' \"$*\" >>\"$FAKE/gh-calls\"\ncase \"$1 $2\" in\n" +
		"'api '*) " + apiAnswer + " ;;\n" +
		"'release view') cat \"$FAKE/assets\" ;;\n" +
		"'release download')\n" +
		"  while [ $# -gt 0 ]; do [ \"$1\" = --dir ] && d=$2; shift; done\n" +
		"  case \"$d\" in\n" +
		"  */out)\n" +
		fmt.Sprintf("    [ %t = false ] || exit 1\n", fx.noAssets) +
		"    for f in rootfs-aarch64.img.zst.part000 vmlinux-billet-aarch64 image-sources-aarch64.tsv; do echo x >\"$d/$f\"; done ;;\n" +
		"  *) cp \"$FAKE/aarch64-manifest\" \"$d/manifest-aarch64.json\"; echo '{}' >\"$d/manifest-aarch64.sigstore.json\" ;;\n" +
		"  esac ;;\nesac\n"
	writeFile(t, filepath.Join(bin, "gh"), gh, 0o700)
	writeFile(t, filepath.Join(bin, "go"), fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >\"$FAKE/go-call\"\nexit %d\n", fx.verify), 0o700)

	cmd := exec.CommandContext(t.Context(), "bash", "carry-guest-set.sh", out)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE="+dir,
		"GH_TOKEN=token-for-test", "GITHUB_REPOSITORY=junioryono/billet")

	said, err := cmd.CombinedOutput()

	return string(said), err == nil, dir
}

// A SET IS CARRIED ONLY WHEN IT IS PROVED, AND ITS ABSENCE IS ONLY EVER SAID ON EVIDENCE.
// A channel that cannot be read must stop the publication: published alone, the release
// would tell every arm64 node in a signed pointer that there is no image, on no evidence
// that there is none.
func TestALastGoodImageIsCarriedOnlyWhenItIsProved(t *testing.T) {
	t.Parallel()

	const pointer = `{"schema":1,"tag":"guest-20261026-000000","architectures":["x86_64","aarch64"]}`

	const set = "manifest.json\nmanifest-aarch64.json\nmanifest-aarch64.sigstore.json\n"

	good := twoSetManifest("-aarch64", "aarch64", "10")

	for name, tc := range map[string]struct {
		fx        carryFixture
		ok        bool
		carried   bool
		says      string
		noDownset bool
	}{
		"a proved set of this contract": {
			fx: carryFixture{pointer: pointer, assets: set, manifest: good},
			ok: true, carried: true, says: "carried the aarch64 set of guest-20261026-000000",
		},
		"no channel yet": {
			fx: carryFixture{notFound: true},
			ok: true, says: "does not exist yet",
		},
		"a channel it could not read": {
			fx:   carryFixture{},
			says: "could not read the guest channel",
		},
		"a release with no aarch64 set": {
			fx: carryFixture{pointer: pointer, assets: "manifest.json\n"},
			ok: true, says: "carries no aarch64 set",
		},
		"another contract": {
			fx: carryFixture{pointer: pointer, assets: set, manifest: twoSetManifest("-aarch64", "aarch64", "9")},
			ok: true, says: "speaks guest contract 9",
		},
		"a signature the policy refuses": {
			fx:   carryFixture{pointer: pointer, assets: set, manifest: good, verify: 1},
			says: "not one billet's own signing policy accepts",
		},
		"a set that will not download": {
			fx:   carryFixture{pointer: pointer, assets: set, manifest: good, noAssets: true},
			says: "could not download the aarch64 set",
		},
		"a channel naming no guest release": {
			fx:   carryFixture{pointer: `{"tag":"v0.29.0"}`, assets: set},
			says: "not a guest release",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			said, ok, dir := runCarry(t, tc.fx)
			if ok != tc.ok || !strings.Contains(said, tc.says) {
				t.Fatalf("succeeded = %v saying:\n%s\nwant %v saying %q", ok, said, tc.ok, tc.says)
			}

			// THE MANIFEST IN out IS WHAT THE PUBLISHER READS AS A SET THIS RELEASE CARRIES,
			// and write-guest-channel.sh as a machine it lists, so it is there only when
			// the whole set is.
			_, err := os.Stat(filepath.Join(dir, "out", "manifest-aarch64.json"))
			if carried := err == nil; carried != tc.carried {
				t.Errorf("a manifest in the release's directory = %v, want %v:\n%s", carried, tc.carried, said)
			}

			if !tc.carried {
				return
			}

			call, err := os.ReadFile(filepath.Join(dir, "go-call"))
			if err != nil {
				t.Fatalf("the signature was never checked: %v", err)
			}

			if !strings.Contains(string(call), "run ./scripts/verifyguestset aarch64 ") {
				t.Errorf("the check ran as %q", call)
			}

			notes := string(readBytes(t, filepath.Join(dir, "out", "release-notes-carried.md")))
			if !strings.Contains(notes, "carried forward from `guest-20261026-000000`") {
				t.Errorf("the notes do not name where the set came from:\n%s", notes)
			}
		})
	}
}

// WHAT THE WRITER SAYS OF AN aarch64 SET IS WHAT A NODE ON THAT MACHINE TAKES, by the
// reader's own parse and its own judgement: every asset carrying the machine's name, and a
// component list of Firefox alone under schema 3. A writer and a reader that disagree about
// a name are found by a release no node can pull.
func TestAnAarch64ManifestIsOneAnAarch64NodeTakes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	image := filepath.Join(dir, "rootfs-aarch64.img.zst")
	writeFile(t, image, strings.Repeat("x", 8192), 0o600)
	parts := strings.Join(runSplit(t, image, 4096), "\n")

	kernel := filepath.Join(dir, "vmlinux-billet-aarch64")
	if err := os.WriteFile(kernel, arm64Kernel(), 0o600); err != nil {
		t.Fatal(err)
	}

	info := filepath.Join(dir, "build-info.env")
	writeFile(t, info, "RUNNER_VERSION=2.337.0\nGUEST_CONTRACT=10\nARCH=aarch64\nLEFT_OUT=\"firefox\"\n", 0o600)

	components := filepath.Join(dir, "components.json")
	writeFile(t, components, `[{"name":"firefox","space":1272971264}]`, 0o600)

	cmd := exec.CommandContext(t.Context(), repoScript(t, "write-image-manifest.sh"))
	cmd.Env = append(os.Environ(), "ROOTFS="+image, "KERNEL="+kernel, "INFO="+info,
		"COMPONENTS_FILE="+components, "ROOTFS_PARTS="+parts)

	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("write-image-manifest.sh: %v", err)
	}

	manifest, err := imagesource.ParseManifest(raw)
	if err != nil {
		t.Fatalf("the reader refuses what the writer wrote: %v\n%s", err, raw)
	}

	if err := manifest.Usable("10", "aarch64"); err != nil {
		t.Errorf("an aarch64 node of that contract refuses it: %v", err)
	}

	if err := manifest.Usable("10", "x86_64"); err == nil {
		t.Error("an x86_64 node would take it")
	}

	if manifest.Kernel.Name != "vmlinux-billet-aarch64" || manifest.Arch != "aarch64" {
		t.Errorf("it names kernel %q of %q", manifest.Kernel.Name, manifest.Arch)
	}

	for _, part := range manifest.RootfsMultipart.Parts {
		if !strings.HasPrefix(part.Name, "rootfs-aarch64.img.zst.part") {
			t.Errorf("it names the part %q", part.Name)
		}
	}
}
