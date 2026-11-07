package scripts_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// addAarch64Set writes into DIR a whole aarch64 set under the names ADR-022 gives it,
// its source in sourceParts parts.
func addAarch64Set(t *testing.T, dir, manifest string, sourceParts int) {
	t.Helper()

	// THE SOURCE ARCHIVE'S FIXTURE WRITES THE x86_64 NAMES, in a directory of its own
	// here, and the set's names are what the publisher holds a part list to.
	work := t.TempDir()
	writeSourceArchive(t, work, publishIndex, sourceParts)

	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		body := readBytes(t, filepath.Join(work, entry.Name()))
		if entry.Name() == "image-sources.parts" {
			body = []byte(strings.ReplaceAll(string(body), "image-sources.tar", "image-sources-aarch64.tar"))
		}

		name := strings.Replace(entry.Name(), "image-sources", "image-sources-aarch64", 1)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for name, body := range map[string][]byte{
		"manifest-aarch64.json":          []byte(manifest),
		"manifest-aarch64.sigstore.json": []byte("{}"),
		"rootfs-aarch64.img.zst.part000": []byte("rootfs-aarch64.img.zst.part000"),
		"vmlinux-billet-aarch64":         []byte("vmlinux-billet-aarch64"),
		"image-sources-aarch64.tsv": []byte("package\tarchitecture\tversion\tsource\tsource_version\torigin\tlicense\n" +
			"bash\tarm64\t5.2.21-2ubuntu4\tbash\t5.2.21-2ubuntu4\tports.ubuntu.com\tgnu\n"),
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
		parts    int
		edit     func(t *testing.T, dir string)
		says     string
	}{
		"a whole set":           {manifest: good},
		"a whole set, in parts": {manifest: good, parts: 2},
		"a source part cut short": {
			manifest: good,
			parts:    2,
			edit: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, filepath.Join(dir, "image-sources-aarch64.tar.part001"), "cut", 0o600)
			},
			says: "image-sources-aarch64.tar.part001 is not the part image-sources-aarch64.parts describes",
		},
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
			addAarch64Set(t, dir, tc.manifest, tc.parts)

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
				"image-sources-aarch64.parts", "manifest-aarch64.origin",
			} {
				if !strings.Contains(calls, "arg "+filepath.Join(dir, asset)+"\n") {
					t.Errorf("the release does not carry %s:\n%s", asset, calls)
				}
			}

			// A SET NEW IN THIS RELEASE IS RECORDED AS BUILT BY IT, so a later carry can say so.
			if got, want := string(readBytes(t, filepath.Join(dir, "manifest-aarch64.origin"))),
				"guest-20260821-000000 "+strings.Repeat("a", 40)+"\n"; got != want {
				t.Errorf("the set's origin is %q, want %q", got, want)
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

	var uploaded []string

	for line := range strings.SplitSeq(calls, "\n") {
		// THE NOTES ARE gh's --notes-file, not an asset.
		if path, isArg := strings.CutPrefix(line, "arg "); isArg && filepath.IsAbs(path) && filepath.Base(path) != "release-notes.md" {
			uploaded = append(uploaded, filepath.Base(path))
		}
	}

	if want := []string{
		"manifest.json", "manifest.sigstore.json", "image-sources.tsv", "image-sources-index.tsv",
		"image-sources.parts", "image-sources.tar", "vmlinux-billet", "rootfs.img.zst.part000",
	}; !slices.Equal(uploaded, want) {
		t.Errorf("the release uploads %v, want %v", uploaded, want)
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

			raw := readBytes(t, filepath.Join(out, "current.json"))

			// THE READER'S OWN JUDGEMENT, at the moment it was written and just before it
			// lapses: its window is one a reader takes, and ten days of it.
			channel, err := imagesource.ParseChannel(raw, time.Now())
			if err != nil {
				t.Fatalf("a reader refuses what the writer wrote: %v\n%s", err, raw)
			}

			if channel.Tag != "guest-20261104-000000" {
				t.Errorf("the pointer names %q", channel.Tag)
			}

			if channel.Architectures == nil || !slices.Equal(*channel.Architectures, tc.want) {
				t.Errorf("the pointer lists %v, want %v", channel.Architectures, tc.want)
			} else if err := imagesource.CheckChannelArchitectures(*channel.Architectures); err != nil {
				t.Errorf("a reader refuses the list the writer wrote: %v", err)
			}

			if _, err := imagesource.ParseChannel(raw, time.Now().Add(9*24*time.Hour)); err != nil {
				t.Errorf("the pointer lapses within nine days: %v", err)
			}

			if _, err := imagesource.ParseChannel(raw, time.Now().Add(11*24*time.Hour)); err == nil {
				t.Error("the pointer outlives ten days, which replays a build two weeks old as current")
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

	// THE WHOLE CONDITION, because clauses found in it would pass an `||` between them.
	// Not always(), which publishes a run a person stopped; x86_64 gates; aarch64's result
	// is no part of it, because its failure is what this job has a rule for.
	if got, want := workflow.Jobs["publish"].If, "${{ !cancelled() && needs.resolve.result == 'success' && "+
		"needs.x64.result == 'success' && github.ref == 'refs/heads/main' && "+
		"(github.event_name != 'workflow_dispatch' || inputs.publish) && "+
		"needs.resolve.outputs.source_is_release == 'true' }}"; got != want {
		t.Errorf("the publish job's condition is\n%s\nwant\n%s", got, want)
	}
}

// EACH LEG BUILDS WHAT WAS RESOLVED, and the workflow's own lines say the names it writes:
// a leg fed the wrong commit builds another tree under this release's name, and these lines
// are what the real builds ran.
func TestALegBuildsTheResolvedTreeUnderItsOwnNames(t *testing.T) {
	t.Parallel()

	var caller struct {
		Jobs map[string]struct {
			With map[string]string `yaml:"with"`
		} `yaml:"jobs"`
	}

	body, build, _ := guestImageWorkflows(t)
	if err := yaml.Unmarshal([]byte(body), &caller); err != nil {
		t.Fatal(err)
	}

	for _, job := range []string{"x64", "arm64"} {
		with := caller.Jobs[job].With
		if with["source_sha"] != "${{ needs.resolve.outputs.source_sha }}" ||
			with["source_tag"] != "${{ needs.resolve.outputs.source_tag }}" ||
			with["runner_version"] != "${{ inputs.runner_version }}" {
			t.Errorf("the %s leg is not given what was resolved: %v", job, with)
		}
	}

	for _, want := range []string{
		"KERNEL: ${{ github.workspace }}/out/vmlinux-billet${{ inputs.suffix }}",
		"ROOTFS: ${{ github.workspace }}/out/rootfs${{ inputs.suffix }}.img.zst",
		"SOURCE_TAG: ${{ inputs.source_tag }}",
		"SOURCE_SHA: ${{ inputs.source_sha }}",
		"ref: ${{ inputs.source_sha }}",
		"IMAGE_NAME: ${{ inputs.image_name }}",
		"SUFFIX: ${{ inputs.suffix }}",
		"runs-on: ${{ inputs.runs_on }}",
		"key: guest-kernel-${{ inputs.machine }}-",
		`scripts/write-image-manifest.sh > "out/manifest$SUFFIX.json"`,
		`-o "out/rootfs$SUFFIX.img.zst" "$WORK/$IMAGE_NAME.ext4"`,
		`cp out/vmlinux-billet "out/vmlinux-billet$SUFFIX"`,
		`printf 'image-sources%s.tar %s %s\n' "$SUFFIX"`,
		"name: guest-image${{ inputs.suffix }}",
		"out/manifest${{ inputs.suffix }}.json",
		"out/rootfs${{ inputs.suffix }}.img.zst*",
		"out/vmlinux-billet${{ inputs.suffix }}",
		"out/image-sources${{ inputs.suffix }}.tar*",
		"out/release-notes${{ inputs.suffix }}.md",
	} {
		if !strings.Contains(build, want) {
			t.Errorf("guest-image-build.yml does not say %s", want)
		}
	}

	// NO STEP MAY GO RED AND LEAVE THE LEG GREEN: a gate that does is decoration, in yaml.
	if strings.Contains(build, "continue-on-error") || strings.Contains(body, "continue-on-error") {
		t.Error("a guest image workflow carries continue-on-error")
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

	steps := map[string]workflowStep{}

	for _, step := range workflow.Jobs["publish"].Steps {
		steps[step.Name] = step
	}

	for name, want := range map[string]struct{ cond, run string }{
		"Download the checked image":         {"", "scripts/fetch-guest-artifact.sh out"},
		"Download the checked aarch64 image": {"needs.arm64.result == 'success'", "scripts/fetch-guest-artifact.sh out"},
		"Carry the last good aarch64 image":  {"needs.arm64.result != 'success'", "scripts/carry-guest-set.sh out"},
		"Publish the dated release":          {"", "scripts/publish-guest-release.sh out"},
	} {
		got, ok := steps[name]
		if !ok {
			t.Errorf("the publish job has no step %q", name)

			continue
		}

		if got.If != want.cond || strings.TrimSpace(got.Run) != want.run {
			t.Errorf("step %q runs %q if %q, want %q if %q", name, got.Run, got.If, want.run, want.cond)
		}
	}

	// A CARRIED MANIFEST KEEPS THE SIGNATURE IT WAS PUBLISHED WITH: signed again it would
	// say this run vouches for an image it did not build.
	sign := steps["Sign the manifest"].Run
	check := strings.Index(sign, `if [ "$ARM64_RESULT" = success ]; then`)
	signed := strings.Index(sign, "--bundle out/manifest-aarch64.sigstore.json")

	if check < 0 || signed < check {
		t.Errorf("the aarch64 manifest is signed whatever its leg did:\n%s", sign)
	}

	// A LAPSED CHANNEL IS CARRIED FROM ONLY BY A PERSON'S INPUT, which a scheduled run has none of.
	if !strings.Contains(caller, "CARRY_EXPIRED_CHANNEL: ${{ inputs.carry_expired_channel }}") {
		t.Error("the carry step does not take its input from the dispatch")
	}

	// THE NOTES ARE ONE RELEASE'S, the carried set's origin among them.
	notes := steps["Write the release notes"].Run
	for _, want := range []string{"out/release-notes-aarch64.md", "out/release-notes-carried.md"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the release's notes leave out %s:\n%s", want, notes)
		}
	}
}

// carryFixture is what carry-guest-set.sh meets: a channel, the release it names and the
// tools that reach them, each stubbed and each failing on a call it does not expect.
type carryFixture struct {
	lsRemote      int    // what git ls-remote exits with
	noPointer     bool   // the branch holds no current.json
	pointer       string // the channel's pointer
	draft         bool   // the release it names is mutable
	unset         bool   // the release has no aarch64 set
	manifest      string // its aarch64 manifest
	verifyChannel int    // what the pointer's check exits with
	verifyImage   int    // what the manifest's check exits with
	noOrigin      bool   // the release's set has no record of what built it
	origin        string // that record, when it is not the default
	drop          string // an asset the release has lost
	failDownload  bool   // the download of the set, after its manifest, fails
	carryExpired  bool   // a person dispatched with carry_expired_channel
	noFetch       bool   // the fetch of the channel's branch fails
}

const carriedOrigin = "guest-20261012-000000 " + "0123456789abcdef0123456789abcdef01234567" + "\n"

// runCarry runs the script against a fixture and returns its output, whether it succeeded,
// and the directory it worked on.
func runCarry(t *testing.T, fx carryFixture) (string, bool, string) {
	t.Helper()

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	release := filepath.Join(dir, "release")
	bin := filepath.Join(dir, "bin")

	for _, path := range []string{bin, release} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// A WHOLE x86_64 PUBLICATION, AND THE RELEASE'S REAL aarch64 SET, so what a carry leaves
	// is what the real publisher goes on to judge.
	if err := os.CopyFS(out, os.DirFS(x64PublishDir(t))); err != nil {
		t.Fatal(err)
	}

	if !fx.unset {
		addAarch64Set(t, release, fx.manifest, 2)

		switch {
		case fx.noOrigin:
		case fx.origin != "":
			writeFile(t, filepath.Join(release, "manifest-aarch64.origin"), fx.origin, 0o600)
		default:
			writeFile(t, filepath.Join(release, "manifest-aarch64.origin"), carriedOrigin, 0o600)
		}
	}

	if fx.drop != "" {
		removeAll(t, filepath.Join(release, fx.drop))
	}

	writeFile(t, filepath.Join(dir, "pointer"), fx.pointer, 0o600)

	// FETCH_HEAD HOLDS NOTHING UNTIL THE CHANNEL'S OWN BRANCH IS FETCHED, as in a checkout,
	// whose FETCH_HEAD is otherwise whatever came last.
	git := "#!/bin/bash\n[ \"$1\" = -C ] && shift 2\ncase \"$*\" in\n" +
		fmt.Sprintf("'ls-remote --exit-code --heads origin refs/heads/guest-channel') exit %d ;;\n", fx.lsRemote) +
		fmt.Sprintf("'fetch --quiet origin refs/heads/guest-channel') [ %t = false ] || exit 1; : >\"$FAKE/fetched\" ;;\n", fx.noFetch)
	if fx.noPointer {
		git += "'show FETCH_HEAD:current.json') exit 128 ;;\n"
	} else {
		git += "'show FETCH_HEAD:current.json') [ -e \"$FAKE/fetched\" ] || exit 128; cat \"$FAKE/pointer\" ;;\n" +
			"'show FETCH_HEAD:current.sigstore.json') [ -e \"$FAKE/fetched\" ] || exit 128; echo '{\"bundle\":1}' ;;\n"
	}

	git += "*) echo \"unexpected git $*\" >&2; exit 99 ;;\nesac\n"
	writeFile(t, filepath.Join(bin, "git"), git, 0o700)

	// EVERY CALL IS ONE THE SCRIPT IS EXPECTED TO MAKE, of the release the pointer names; a
	// download honours its patterns, and a pattern matching nothing is silent, as gh's is.
	gh := "#!/bin/bash\nprintf '%s\\n' \"$*\" >>\"$FAKE/gh-calls\"\n" +
		"case \"$*\" in *'--repo junioryono/billet'*) ;; *) echo 'unexpected repository' >&2; exit 99 ;; esac\n" +
		"case \"$1 $2 $3\" in\n" +
		"'release view guest-20261026-000000')\n" +
		fmt.Sprintf("  immutable=%t\n", !fx.draft) +
		"  names=$(cd \"$FAKE/release\" && ls | jq -R '{name: .}' | jq -s .)\n" +
		"  jq -n --argjson a \"$names\" --argjson i \"$immutable\" '{assets: $a, isDraft: false, isImmutable: $i}' ;;\n" +
		"'release download guest-20261026-000000')\n" +
		fmt.Sprintf("  case \"$*\" in *image-sources-aarch64*) [ %t = false ] || exit 1 ;; esac\n", fx.failDownload) +
		"  shift 3\n  patterns=()\n  while [ $# -gt 0 ]; do\n" +
		"    case \"$1\" in --dir) d=$2 ;; --pattern) patterns+=(\"$2\") ;; esac\n    shift\n  done\n" +
		"  for f in \"$FAKE\"/release/*; do\n    for p in \"${patterns[@]}\"; do\n" +
		"      case \"${f##*/}\" in $p) cp \"$f\" \"$d/\" ;; esac\n    done\n  done ;;\n" +
		"*) echo \"unexpected gh $*\" >&2; exit 99 ;;\nesac\n"
	writeFile(t, filepath.Join(bin, "gh"), gh, 0o700)

	// WHAT WAS VERIFIED IS KEPT, to hold it to what is left in the release.
	goStub := "#!/bin/bash\nprintf '%s\\n' \"$*\" >>\"$FAKE/go-calls\"\n" +
		"[ \"$1 $3 $4\" = '-C run ./scripts/verifyguestset' ] || { echo \"unexpected go $*\" >&2; exit 99; }\n" +
		"case \"$5\" in\n" +
		fmt.Sprintf("channel | channel-expired) printf '%%s\\n' \"$5\" >\"$FAKE/asked\"; [ -s \"$7\" ] || exit 97; cp \"$6\" \"$FAKE/verified-pointer\"; cp \"$7\" \"$FAKE/verified-pointer-bundle\"; exit %d ;;\n", fx.verifyChannel) +
		fmt.Sprintf("manifest) [ \"$6\" = aarch64 ] || exit 98; cp \"$7\" \"$FAKE/verified-manifest\"; cp \"$8\" \"$FAKE/verified-bundle\"; exit %d ;;\n", fx.verifyImage) +
		"*) echo \"unexpected verification $5\" >&2; exit 99 ;;\nesac\n"
	writeFile(t, filepath.Join(bin, "go"), goStub, 0o700)

	cmd := exec.CommandContext(t.Context(), "bash", "carry-guest-set.sh", out)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE="+dir,
		"GH_TOKEN=token-for-test", "GITHUB_REPOSITORY=junioryono/billet",
		fmt.Sprintf("CARRY_EXPIRED_CHANNEL=%t", fx.carryExpired))

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

	good := twoSetManifest("-aarch64", "aarch64", "10")

	for name, tc := range map[string]struct {
		fx      carryFixture
		ok      bool
		carried bool
		says    string
	}{
		"a proved set of this contract": {
			fx: carryFixture{pointer: pointer, manifest: good},
			ok: true, carried: true, says: "carried the aarch64 set of guest-20261026-000000",
		},
		"a channel never published": {
			fx: carryFixture{lsRemote: 2},
			ok: true, says: "has never been published",
		},
		// A BRANCH THAT IS THERE WITH NO POINTER ON IT IS DAMAGE, not a channel never published.
		"a channel with no pointer": {
			fx:   carryFixture{noPointer: true},
			says: "holds no current.json",
		},
		"a channel it could not ask about": {
			fx:   carryFixture{lsRemote: 128},
			says: "could not ask whether the guest channel exists",
		},
		"a branch that will not fetch": {
			fx:   carryFixture{pointer: pointer, manifest: good, noFetch: true},
			says: "could not fetch the guest channel",
		},
		// A LAPSED POINTER IS REFUSED BY THE TOOL, whose own test holds that, and the refusal
		// names the one way past it.
		"a pointer a node would refuse": {
			fx:   carryFixture{pointer: pointer, manifest: good, verifyChannel: 1},
			says: "carry_expired_channel",
		},
		"a person recovering a lapsed channel": {
			fx: carryFixture{pointer: pointer, manifest: good, carryExpired: true},
			ok: true, carried: true, says: "carried the aarch64 set",
		},
		"a release that is not locked": {
			fx:   carryFixture{pointer: pointer, manifest: good, draft: true},
			says: "not an immutable, published release",
		},
		"a release with no aarch64 set": {
			fx: carryFixture{pointer: pointer, unset: true},
			ok: true, says: "carries no aarch64 set",
		},
		"another contract": {
			fx: carryFixture{pointer: pointer, manifest: twoSetManifest("-aarch64", "aarch64", "9")},
			ok: true, says: "speaks guest contract 9",
		},
		"a manifest the policy refuses": {
			fx:   carryFixture{pointer: pointer, manifest: good, verifyImage: 1},
			says: "the aarch64 manifest of guest-20261026-000000 is not one",
		},
		"a set with no record of what built it": {
			fx:   carryFixture{pointer: pointer, manifest: good, noOrigin: true},
			says: "no manifest-aarch64.origin",
		},
		"a record that names nothing": {
			fx:   carryFixture{pointer: pointer, manifest: good, origin: "not a record\n"},
			says: "does not name a release and a commit",
		},
		"a set that will not download": {
			fx:   carryFixture{pointer: pointer, manifest: good, failDownload: true},
			says: "could not download the aarch64 set",
		},
		"a channel naming no guest release": {
			fx:   carryFixture{pointer: `{"tag":"v0.29.0"}`},
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
			out := filepath.Join(dir, "out")

			_, err := os.Stat(filepath.Join(out, "manifest-aarch64.json"))
			if carried := err == nil; carried != tc.carried {
				t.Errorf("a manifest in the release's directory = %v, want %v:\n%s", carried, tc.carried, said)
			}

			if !tc.carried {
				return
			}

			// ASKED AS A NODE WOULD ASK, unless a person said otherwise.
			want := "channel\n"
			if tc.fx.carryExpired {
				want = "channel-expired\n"
			}

			if got := string(readBytes(t, filepath.Join(dir, "asked"))); got != want {
				t.Errorf("the pointer was asked about as %q, want %q", got, want)
			}

			if got := string(readBytes(t, filepath.Join(dir, "verified-pointer-bundle"))); got != "{\"bundle\":1}\n" {
				t.Errorf("the pointer was verified against %q", got)
			}

			// WHAT WAS PROVED IS WHAT IS PUBLISHED, byte for byte.
			if got, want := readBytes(t, filepath.Join(out, "manifest-aarch64.json")), readBytes(t, filepath.Join(dir, "verified-manifest")); !bytes.Equal(got, want) {
				t.Error("the manifest left in the release is not the one that was verified")
			}

			if got, want := readBytes(t, filepath.Join(out, "manifest-aarch64.sigstore.json")), readBytes(t, filepath.Join(dir, "verified-bundle")); !bytes.Equal(got, want) {
				t.Error("the bundle left in the release is not the one that was verified")
			}

			if got, want := readBytes(t, filepath.Join(dir, "verified-pointer")), []byte(pointer); !bytes.Equal(got, want) {
				t.Errorf("what was verified of the channel is %q", got)
			}

			// THE RELEASE IT ORIGINATED IN, NOT THE ONE IT WAS LAST CARRIED BY: that one's tag
			// points at an x86_64 build's commit.
			notes := string(readBytes(t, filepath.Join(out, "release-notes-carried.md")))
			for _, want := range []string{"carried forward from `guest-20261026-000000`", "provenance is", "`guest-20261012-000000`", "0123456789abcdef"} {
				if !strings.Contains(notes, want) {
					t.Errorf("the notes do not say %q:\n%s", want, notes)
				}
			}

			// AND THE REAL PUBLISHER TAKES THE WHOLE SET, leaving what built it as it was.
			out2, published, calls := runPublisher(t, out)
			if !published {
				t.Fatalf("the publisher refused what was carried:\n%s", out2)
			}

			for _, asset := range []string{
				"manifest-aarch64.json", "manifest-aarch64.sigstore.json", "manifest-aarch64.origin",
				"rootfs-aarch64.img.zst.part000", "vmlinux-billet-aarch64", "image-sources-aarch64.tsv",
				"image-sources-aarch64-index.tsv", "image-sources-aarch64.parts",
				"image-sources-aarch64.tar.part000", "image-sources-aarch64.tar.part001",
			} {
				if !strings.Contains(calls, "arg "+filepath.Join(out, asset)+"\n") {
					t.Errorf("the release does not carry %s:\n%s", asset, calls)
				}
			}

			if got := string(readBytes(t, filepath.Join(out, "manifest-aarch64.origin"))); got != carriedOrigin {
				t.Errorf("publishing it rewrote what built it: %q", got)
			}
		})
	}

	// AN ASSET THE RELEASE HAS LOST STOPS THE PUBLICATION, at the publisher: a pattern
	// matching nothing is silent, so nothing before it can tell.
	for _, lost := range []string{"vmlinux-billet-aarch64", "image-sources-aarch64.tar.part001"} {
		said, ok, dir := runCarry(t, carryFixture{pointer: pointer, manifest: good, drop: lost})
		if !ok {
			t.Fatalf("the carry itself refused:\n%s", said)
		}

		if out, published, _ := runPublisher(t, filepath.Join(dir, "out")); published || !strings.Contains(out, lost) {
			t.Errorf("a set without %s was published:\n%s", lost, out)
		}
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
