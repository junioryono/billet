package ec2

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/guestassets"
	"github.com/junioryono/billet/internal/runnerimages"
	"github.com/junioryono/billet/scripts"
)

// sha256Hex is the lowercase hex digest of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// decodeUserData is a RunInstances UserData parameter as the instance reads it.
func decodeUserData(t *testing.T, encoded string) string {
	t.Helper()

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the user data is not base64: %v", err)
	}

	if !bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		return string(raw)
	}

	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the user data is not gzip: %v", err)
	}

	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("unpack the user data: %v", err)
	}

	return string(plain)
}

// mustBootstrap is a bootstrap for a staged payload, or the test fails.
func mustBootstrap(t *testing.T) string {
	t.Helper()

	script, err := bootstrapScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, nonce: verifyProbeNonce})
	if err != nil {
		t.Fatalf("bootstrapScript: %v", err)
	}

	return script
}

// payloadMembers reads a staged payload back: every member's header and body.
func payloadMembers(t *testing.T, archive []byte) (map[string]*tar.Header, map[string][]byte) {
	t.Helper()

	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("the payload is not gzip: %v", err)
	}

	tr := tar.NewReader(zr)
	headers, bodies := map[string]*tar.Header{}, map[string][]byte{}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("the payload is not a tarball: %v", err)
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}

		if _, dup := headers[hdr.Name]; dup {
			t.Errorf("the payload carries %s twice", hdr.Name)
		}

		headers[hdr.Name], bodies[hdr.Name] = hdr, body
	}

	return headers, bodies
}

// payloadMember is one member's body, or nil; the build fake reads the nonce out
// of build.env with it, the way the builder does.
func payloadMember(archive []byte, path string) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil
	}

	tr := tar.NewReader(zr)

	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil
		}

		if hdr.Name == strings.TrimPrefix(path, "/") {
			body, err := io.ReadAll(tr)
			if err != nil {
				return nil
			}

			return body
		}
	}
}

// THE PAYLOAD IS THE TREE, laid out where build-ami.sh finds it: every file the
// scripts package embeds at its path under the repository, with the mode the tree
// gives it, the declarations and the pin beside them, and the build's own
// settings, layer and unit. Nothing else, nothing outside the build's directory
// and the unit's path, and every member root's.
func TestThePayloadLaysTheBuildOutAsTheTreeDoes(t *testing.T) {
	t.Parallel()

	spec := BuildSpec{Arch: "arm64", nonce: verifyProbeNonce, Accept: []string{"firefox", "android-sdk"}}

	archive, digest, err := payloadArchive(spec, testLayer)
	if err != nil {
		t.Fatalf("payloadArchive: %v", err)
	}

	again, digestAgain, err := payloadArchive(spec, testLayer)
	if err != nil || digest != digestAgain || !bytes.Equal(archive, again) {
		t.Errorf("two archives of one build differ (%v): its digest is not a statement about it", err)
	}

	headers, bodies := payloadMembers(t, archive)

	want := map[string]struct {
		mode int64
		body []byte
	}{}

	tree, err := scripts.AMIBuild()
	if err != nil {
		t.Fatalf("AMIBuild: %v", err)
	}

	for _, f := range tree {
		mode := int64(0o644)
		if f.Executable {
			mode = 0o755
		}

		name := strings.TrimPrefix(amiRepoDir, "/") + "/scripts/" + f.Path
		if f.Path == scripts.AMIBuildService {
			name = strings.TrimPrefix(amiUnitPath, "/")
		}

		want[name] = struct {
			mode int64
			body []byte
		}{mode, f.Body}
	}

	for _, arch := range []string{"x64", "arm64"} {
		body, err := runnerimages.ToolsetBytesFor(arch)
		if err != nil {
			t.Fatalf("ToolsetBytesFor(%s): %v", arch, err)
		}

		file, err := runnerimages.ToolsetFileFor(arch)
		if err != nil {
			t.Fatalf("ToolsetFileFor(%s): %v", arch, err)
		}

		want["var/lib/billet-ami/repo/internal/runnerimages/"+file] = struct {
			mode int64
			body []byte
		}{0o644, body}
	}

	env, err := buildEnvFile(spec)
	if err != nil {
		t.Fatalf("buildEnvFile: %v", err)
	}

	for name, m := range map[string]struct {
		mode int64
		body []byte
	}{
		"var/lib/billet-ami/repo/internal/runnerimages/pinned.txt":        {0o644, runnerimages.PinnedFile()},
		"var/lib/billet-ami/repo/internal/guestassets/docker-daemon.json": {0o644, []byte(guestassets.DockerDaemonJSON)},
		"var/lib/billet-ami/build.env":                                    {0o600, []byte(env)},
		"var/lib/billet-ami/ec2-layer.sh":                                 {0o755, []byte(testLayer)},
	} {
		want[name] = m
	}

	for name, w := range want {
		hdr, ok := headers[name]
		if !ok {
			t.Errorf("the payload carries no %s", name)

			continue
		}

		if hdr.Mode != w.mode || hdr.Uid != 0 || hdr.Gid != 0 || hdr.Typeflag != tar.TypeReg {
			t.Errorf("%s is mode %o owned %d:%d type %c, want a regular file of mode %o owned by root",
				name, hdr.Mode, hdr.Uid, hdr.Gid, hdr.Typeflag, w.mode)
		}

		if !bytes.Equal(bodies[name], w.body) {
			t.Errorf("%s is not the bytes billet holds for it", name)
		}
	}

	// THE BUILD'S DIRECTORY IS ROOT'S ALONE, because GitHub's template opens /opt to
	// everyone and root runs what is in here after it.
	dir, ok := headers["var/lib/billet-ami/"]
	if !ok || dir.Typeflag != tar.TypeDir || dir.Mode != 0o700 || dir.Uid != 0 || dir.Gid != 0 {
		t.Errorf("the build's directory is %+v, want a directory of mode 700 owned by root", dir)
	}

	for name := range headers {
		if _, ok := want[name]; !ok && name != "var/lib/billet-ami/" {
			t.Errorf("the payload carries %s, which nothing names", name)
		}
	}

	// THE SETTINGS ARE THIS BUILD'S, and the accepted names are sorted, so the
	// build installs them in one order whatever the config said.
	for _, line := range []string{
		"BILLET_AMI_NONCE=" + verifyProbeNonce, "BILLET_AMI_SCOPE=ami-arm64",
		"BILLET_AMI_ACCEPT='android-sdk firefox'",
	} {
		if !strings.Contains(env, line+"\n") {
			t.Errorf("build.env does not say %s:\n%s", line, env)
		}
	}
}

// EVERY SOURCE EACH PLAN COPIES IS IN THE PAYLOAD. The runner refuses a plan
// whose source is missing only when it reaches that step, hours into a build; the
// payload is where the omission can be seen first.
func TestThePayloadCarriesEverySourceThePlansCopy(t *testing.T) {
	t.Parallel()

	archive, _, err := payloadArchive(BuildSpec{Arch: "x64", nonce: verifyProbeNonce}, testLayer)
	if err != nil {
		t.Fatalf("payloadArchive: %v", err)
	}

	headers, bodies := payloadMembers(t, archive)
	upstream := "var/lib/billet-ami/repo/scripts/runner-images/upstream/"

	for _, plan := range []string{"plan.json", "plan-arm64.json"} {
		var steps []struct {
			ID      string   `json:"id"`
			Script  string   `json:"script"`
			Scripts []string `json:"scripts"`
			Sources []string `json:"sources"`
		}

		if err := json.Unmarshal(bodies["var/lib/billet-ami/repo/scripts/runner-images/"+plan], &steps); err != nil {
			t.Fatalf("%s in the payload: %v", plan, err)
		}

		checked := 0

		for _, step := range steps {
			for _, src := range append(append(append([]string{}, step.Sources...), step.Scripts...), step.Script) {
				if src == "" || strings.HasPrefix(src, "/") || strings.HasPrefix(src, "images/ubuntu/toolsets/") {
					continue
				}

				checked++

				if _, ok := headers[upstream+src]; ok {
					continue
				}

				// A DIRECTORY IS ITS FILES.
				found := false
				for name := range headers {
					if strings.HasPrefix(name, upstream+src+"/") {
						found = true

						break
					}
				}

				if !found {
					t.Errorf("%s step %s copies %s, which the payload does not carry", plan, step.ID, src)
				}
			}
		}

		if checked < 50 {
			t.Errorf("%s names only %d sources; the plan was not read", plan, checked)
		}
	}
}

// THE TEMPLATE RUNNER, RUN FROM THE PAYLOAD, ACCEPTS IT: its declaration against
// the pin it carries, its plan and its differences, up to the first thing it asks
// of the target. A layout that put one of them anywhere but where the runner looks
// is refused there, before a builder could reach it.
func TestTheRunnerAcceptsThePayloadsTree(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}

	for _, arch := range []string{"x64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			t.Parallel()

			archive, _, err := payloadArchive(BuildSpec{Arch: arch, nonce: verifyProbeNonce}, testLayer)
			if err != nil {
				t.Fatalf("payloadArchive: %v", err)
			}

			root := t.TempDir()
			headers, bodies := payloadMembers(t, archive)

			for name, hdr := range headers {
				if hdr.Typeflag == tar.TypeDir {
					continue
				}

				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}

				if err := forkSafeWriteFile(path, bodies[name]); err != nil {
					t.Fatal(err)
				}

				if err := os.Chmod(path, os.FileMode(hdr.Mode)); err != nil {
					t.Fatal(err)
				}
			}

			calls := filepath.Join(root, "calls")
			driver := filepath.Join(root, "driver")
			writeExecutableFile(t, driver, "#!/bin/sh\necho \"$*\" >>'"+calls+"'\nexit 9\n")

			scope, _, err := scopeFor(arch)
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), "bash",
				filepath.Join(root, "var", "lib", "billet-ami", "repo", "scripts", "run-runner-images.sh"))
			cmd.Env = append(os.Environ(), "BILLET_RI_TARGET="+driver, "BILLET_RI_IMAGE_VERSION=1",
				"BILLET_RI_OUT="+t.TempDir(), "BILLET_RI_SCOPE="+scope)

			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("a driver that refuses everything let the build finish:\n%s", out)
			}

			got, readErr := os.ReadFile(calls)
			if readErr != nil || !strings.HasPrefix(string(got), "exec mkdir -p /var/tmp/billet-ri") {
				t.Fatalf("the runner refused the payload's tree before reaching the target (%v):\n%s",
					readErr, out)
			}
		})
	}
}

// THE BOOTSTRAP FETCHES, CHECKS, UNPACKS AND STARTS, IN THAT ORDER, AND TRACES
// NOTHING. The URL is a credential and a trace writes it to the console and into
// cloud-init's log; the digest is checked before tar reads a byte; and the unit is
// started, never enabled, so only the planned reboot's boot resumes a build.
func TestTheBootstrapChecksThePayloadBeforeItUnpacksIt(t *testing.T) {
	t.Parallel()

	script, err := bootstrapScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, nonce: verifyProbeNonce})
	if err != nil {
		t.Fatalf("bootstrapScript: %v", err)
	}

	if strings.Contains(script, "set -x") || strings.Contains(script, "-eux") {
		t.Error("the bootstrap traces, so the presigned URL reaches the console")
	}

	order := []string{
		"billet_free_kib=$(df -Pk /",
		"curl -fsSL --retry 5 --retry-all-errors -o " + payloadPath + " '" + testPayloadURL + "'",
		"echo '" + testPayloadDigest + "  " + payloadPath + "' | sha256sum -c -",
		"tar --no-overwrite-dir -xzf " + payloadPath + " -C /",
		"grep -qx 'BILLET_AMI_NONCE=" + verifyProbeNonce + "' " + amiBuildEnv,
		"systemctl daemon-reload",
		"systemctl start --no-block " + amiUnitName,
	}

	last := -1

	for _, line := range order {
		i := strings.Index(script, line)
		if i < 0 {
			t.Fatalf("the bootstrap has no %q:\n%s", line, script)
		}

		if i < last {
			t.Errorf("%q runs before what precedes it in the list", line)
		}

		last = i
	}

	if strings.Contains(script, "systemctl enable") {
		t.Error("the bootstrap enables the build unit, so any boot would run it")
	}

	if _, err := packUserData(script); err != nil {
		t.Errorf("the bootstrap is not deliverable as user data: %v", err)
	}
}

// A BOOTSTRAP CANNOT BE RENDERED WITHOUT ITS PAYLOAD, OR AROUND A URL THAT WOULD
// END ITS QUOTE.
func TestTheBootstrapRefusesWhatItCannotQuote(t *testing.T) {
	t.Parallel()

	for name, spec := range map[string]BuildSpec{
		"nothing staged":   {nonce: verifyProbeNonce},
		"no digest":        {payloadURL: testPayloadURL, nonce: verifyProbeNonce},
		"a short digest":   {payloadURL: testPayloadURL, payloadSHA256: "abc", nonce: verifyProbeNonce},
		"a quote":          {payloadURL: testPayloadURL + "'; poweroff; '", payloadSHA256: testPayloadDigest, nonce: verifyProbeNonce},
		"a newline in it":  {payloadURL: testPayloadURL + "\npoweroff", payloadSHA256: testPayloadDigest, nonce: verifyProbeNonce},
		"a backslash":      {payloadURL: testPayloadURL + "\\", payloadSHA256: testPayloadDigest, nonce: verifyProbeNonce},
		"no url, a digest": {payloadSHA256: testPayloadDigest, nonce: verifyProbeNonce},
		"no nonce":         {payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest},
		"an injected nonce": {payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
			nonce: "'; poweroff; echo '"},
	} {
		if _, err := bootstrapScript(spec); err == nil {
			t.Errorf("%s: the bootstrap rendered", name)
		}
	}
}

// THE BOOTSTRAP IS EXECUTED, against a payload that is and is not the one staged,
// and settings that do and do not name this build. A digest check that a pipeline
// or a missing `-c` turned into a no-op passes the first and the second alike; only
// running both tells them apart.
func TestTheBootstrapRefusesAPayloadThatIsNotTheStagedOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		payload  string
		settings string
		wantRun  bool
	}{
		{name: "the staged payload", payload: "the payload\n", wantRun: true},
		{name: "another payload", payload: "someone else's payload\n"},
		// SETTINGS THAT DO NOT NAME THIS BUILD: build-ami.sh could not say anything
		// billet would read, so the unit is never started and the bootstrap says it.
		{name: "settings of another build", payload: "the payload\n",
			settings: "BILLET_AMI_NONCE=fedcba9876543210fedcba9876543210\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}

			calls := filepath.Join(dir, "calls")
			staged := filepath.Join(dir, "staged")
			writeFile(t, staged, tc.payload, 0o644)

			console := filepath.Join(dir, "console")

			for name, body := range map[string]string{
				// sleep ENDS THE FAILURE VERDICT'S LOOP, which on a builder runs until
				// billet terminates it.
				"sleep": "#!/bin/sh\nn=$(($(cat '" + dir + "/sleeps' 2>/dev/null || echo 0) + 1))\n" +
					"echo $n >'" + dir + "/sleeps'\n[ $n -lt 3 ] || kill -TERM $PPID\n",
				"curl":      "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && cp '" + staged + "' \"$2\"; shift; done\n",
				"tar":       "#!/bin/sh\necho tar >>'" + calls + "'\n",
				"systemctl": "#!/bin/sh\necho \"systemctl $*\" >>'" + calls + "'\n",
				"df": "#!/bin/sh\necho 'Filesystem 1024-blocks Used Available Capacity Mounted on'\n" +
					"echo '/dev/root 209715200 4194304 205520896 3% /'\n",
				"sha256sum": "#!/bin/sh\nexec shasum -a 256 \"$@\"\n",
			} {
				writeExecutableFile(t, filepath.Join(bin, name), body)
			}

			sum := sha256Hex([]byte("the payload\n"))

			script, err := bootstrapScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: sum,
				nonce: verifyProbeNonce})
			if err != nil {
				t.Fatal(err)
			}

			script = substitute(t, script, payloadPath, filepath.Join(dir, "payload.tar.gz"))
			script = substitute(t, script, "/dev/console", console)

			settings := tc.settings
			if settings == "" {
				settings = "BILLET_AMI_NONCE=" + verifyProbeNonce + "\nBILLET_AMI_SCOPE=ami-x64\n"
			}

			env := filepath.Join(dir, "build.env")
			writeFile(t, env, settings, 0o600)
			script = substitute(t, script, amiBuildEnv, env)

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
			out, runErr := cmd.CombinedOutput()

			// NO CALLS IS NO FILE: a refused payload reaches neither tar nor systemctl.
			got, err := os.ReadFile(calls)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read the calls: %v", err)
			}

			ran := strings.Contains(string(got), "tar") && strings.Contains(string(got), "systemctl start")

			if ran != tc.wantRun || (runErr == nil) != tc.wantRun {
				t.Errorf("unpacked and started %v, exited %v; want %v\n%s\n%s", ran, runErr, tc.wantRun, got, out)
			}

			// AND A REFUSAL IS A VERDICT BILLET READS, through its own parser.
			said, err := os.ReadFile(console)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}

			report, found := parseReport(string(said), verifyProbeNonce, newBuildSchema())
			switch {
			case tc.wantRun && found:
				t.Errorf("a bootstrap that succeeded announced %v", report)
			case !tc.wantRun && (!found || report[reportVerdictKey] != verdictFail ||
				report[reportStepKey] != bootstrapStep):
				t.Errorf("a refused payload announced %v (found %v), want a failure at %s",
					report, found, bootstrapStep)
			}
		})
	}
}

// A BUILD OWES WHAT WAS ACCEPTED AND ITS TEMPLATE INSTALLS: on arm64, whose
// template has no Android SDK, CodeQL, Chrome or Edge, Firefox alone.
func TestABuildOwesWhatItsTemplateOffers(t *testing.T) {
	t.Parallel()

	all := []string{"android-sdk", "codeql", "firefox", "google-chrome", "microsoft-edge"}

	for arch, want := range map[string][]string{"x64": all, "arm64": {"firefox"}} {
		got, err := offeredComponents(arch, all)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s owes %q (%v), want %q", arch, got, err, want)
		}
	}

	if got, err := offeredComponents("x64", nil); err != nil || len(got) != 0 {
		t.Errorf("a build that accepted nothing owes %q (%v)", got, err)
	}

	// THE INSTALLERS ARE check-guest-component.sh's, which the build asks.
	for name, installer := range componentInstallers {
		out, err := exec.CommandContext(t.Context(), "bash",
			filepath.Join("..", "..", "..", "scripts", "check-guest-component.sh"), "installer", name).Output()
		if err != nil || strings.TrimSpace(string(out)) != installer {
			t.Errorf("%s: billet says %s and check-guest-component.sh %q (%v)", name, installer, out, err)
		}
	}

	if len(componentInstallers) != len(all) {
		t.Errorf("billet knows %d installers for %d components", len(componentInstallers), len(all))
	}
}

// ACCEPTED NAMES ARE BILLET'S OWN, sorted and once each, or the build is refused
// before anything is bought.
func TestTheAcceptedComponentsAreChecked(t *testing.T) {
	t.Parallel()

	got, err := checkAccept([]string{"google-chrome", "firefox", "google-chrome"})
	if err != nil || !slices.Equal(got, []string{"firefox", "google-chrome"}) {
		t.Errorf("checkAccept = %q, %v; want firefox and google-chrome once each", got, err)
	}

	if _, err := checkAccept([]string{"firefox", "netscape"}); err == nil {
		t.Error("a component billet does not install was accepted")
	}

	for _, c := range config.ImageComponents() {
		if _, ok := componentPaths[string(c)]; !ok {
			t.Errorf("the verifier has no paths for %s, which a build may install", c)
		}
	}
}

// A REPORTED COMPONENTS LIST IS HELD TO WHAT WAS ACCEPTED: a subset, once each,
// each version one the completer could have read.
func TestAReportedComponentsListIsHeldToTheAcceptance(t *testing.T) {
	t.Parallel()

	accept := []string{"firefox", "google-chrome"}

	for value, ok := range map[string]bool{
		"":                                    true,
		"firefox:140.0.4":                     true,
		"firefox:140.0.4,google-chrome:1.2.3": true,
		"firefox:1:140.0.4":                   true,
		"android-sdk:12.0":                    false,
		"firefox":                             false,
		"firefox:":                            false,
		"firefox:1,firefox:2":                 false,
		"firefox:1 2":                         false,
		"firefox:140.0.4,":                    false,
	} {
		err := checkBuildComponents(value, accept)
		if (err == nil) != ok {
			t.Errorf("checkBuildComponents(%q) = %v, want accepted %v", value, err, ok)
		}
	}
}

// EVERY PART OF THE BUILD A VERDICT MAY NAME IS ONE build-ami.sh ASSIGNS, and the
// reverse: a label the script writes that billet does not know is a failure billet
// reads as no verdict at all, and the wait runs to its deadline.
func TestTheBuildStepsAreTheScriptsOwn(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "ami", "build-ami.sh"))
	if err != nil {
		t.Fatalf("read build-ami.sh: %v", err)
	}

	var got []string

	for _, m := range regexp.MustCompile(`(?m)^\s*at=([a-z]+)$`).FindAllStringSubmatch(string(raw), -1) {
		if !slices.Contains(got, m[1]) {
			got = append(got, m[1])
		}
	}

	if strings.Contains(string(raw), "verdict verdict=ok step=done") {
		got = append(got, "done")
	}

	if strings.Contains(mustBootstrap(t), "'"+reportStepKey+"="+bootstrapStep+"'") {
		got = append(got, bootstrapStep)
	}

	slices.Sort(got)

	want := slices.Clone(buildSteps)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("build-ami.sh names the parts %q and billet reads %q", got, want)
	}

	for _, key := range []string{buildBegin, buildEnd, buildTemplateStepKey, buildComponentsKey} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("build-ami.sh never writes %s", key)
		}
	}
}

// A TEMPLATE STEP IS NAMED FROM BILLET'S OWN PLAN, by its number, and a number the
// plan does not have names nothing.
func TestATemplateStepIsNamedFromThePlan(t *testing.T) {
	t.Parallel()

	for _, arch := range []string{"x64", "arm64"} {
		first, ok := planStepName(arch, "1")
		if !ok || first == "" {
			t.Errorf("%s: step 1 has no name", arch)
		}

		for _, n := range []string{"0", "-1", "", "x", "100000"} {
			if name, ok := planStepName(arch, n); ok {
				t.Errorf("%s: step %q was named %q", arch, n, name)
			}
		}
	}

	if _, ok := planStepName("riscv64", "1"); ok {
		t.Error("an architecture with no plan named a step")
	}
}

// A FAILURE VERDICT ENDS THE WAIT AT ONCE, NAMING THE PART AND THE TEMPLATE STEP
// FROM BILLET'S PLAN, and nothing is imaged.
func TestAFailedBuildIsReportedWithItsStep(t *testing.T) {
	t.Parallel()

	b := &buildFake{
		stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
		buildVerdict: verdictFail, buildStep: "template", buildTemplateStep: "3",
	}

	f := newFakeEC2(t)
	f.respond = b.serve(f)

	p := newTestProvider(t, f, nil)

	_, err := p.BuildImage(t.Context(), verifyingBuild())
	if err == nil {
		t.Fatal("a failed build produced an image")
	}

	name, ok := planStepName("x64", "3")
	if !ok {
		t.Fatal("the plan has no step 3")
	}

	if !strings.Contains(err.Error(), "failed in template, at step 3 of GitHub's template ("+name+")") {
		t.Errorf("the failure does not name its step: %v", err)
	}

	if n := f.countOf("CreateImage"); n != 0 {
		t.Errorf("%d images were registered from a failed build", n)
	}
}

// A SUCCESS VERDICT THAT IS NOT WHOLE IS NOT A SUCCESS: one that never reached
// `done`, and one whose components line the parser dropped, end the build before
// anything is imaged rather than reading as "installed nothing".
func TestAnIncompleteSuccessIsNotImaged(t *testing.T) {
	t.Parallel()

	for name, b := range map[string]*buildFake{
		"short of done":                 {buildVerdict: verdictOK, buildStep: "layer"},
		"no components":                 {omitBuildComponents: true},
		"components too long to render": {builtComponents: "firefox:" + strings.Repeat("1", 300)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b.stopReason, b.imageState = "Client.InstanceInitiatedShutdown", "available"

			f := newFakeEC2(t)
			f.respond = b.serve(f)

			spec := verifyingBuild()
			spec.Accept = []string{"firefox"}

			_, err := newTestProvider(t, f, nil).BuildImage(t.Context(), spec)
			if err == nil || !strings.Contains(err.Error(), "without finishing its report") {
				t.Fatalf("an incomplete success was answered %v", err)
			}

			if n := f.countOf("CreateImage"); n != 0 {
				t.Errorf("%d images were registered from an incomplete success", n)
			}
		})
	}
}

// A REPORTED COMPONENTS LIST IS REFUSED IN BILLET'S OWN WORDS: the machine's text
// reaches no error.
func TestAComponentsRefusalQuotesNothingTheMachineWrote(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"secret-token-123:1", "firefox:1,secret-token-123", "firefox:secret token"} {
		err := checkBuildComponents(value, []string{"firefox"})
		if err == nil {
			t.Fatalf("%q was accepted", value)
		}

		if strings.Contains(err.Error(), "secret") {
			t.Errorf("the refusal quotes the machine: %v", err)
		}
	}
}

// A BUILDER THAT STOPS ITSELF BEFORE BILLET HAS READ ITS SUCCESS IS NOT IMAGED:
// the stop is a base image's policy or a crash as readily as a finished build.
func TestAStopWithoutAVerdictIsNotImaged(t *testing.T) {
	t.Parallel()

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available", stopsUnread: true}

	f := newFakeEC2(t)
	f.respond = b.serve(f)

	p := newTestProvider(t, f, nil)

	if _, err := p.BuildImage(t.Context(), verifyingBuild()); err == nil ||
		!strings.Contains(err.Error(), "without billet having read its success verdict") {
		t.Errorf("a stop with no verdict read was answered %v", err)
	}

	if n := f.countOf("CreateImage"); n != 0 {
		t.Errorf("%d images were registered from a builder whose verdict was never read", n)
	}
}

// THE COMPONENTS THE BUILD REPORTS ARE THE ONES THE IMAGE MUST RECORD, AND THE
// ONES IT IS STAMPED WITH; a build that installed something it was not asked to
// is refused before anything is imaged.
func TestTheBuildsComponentsAreVerifiedAndStamped(t *testing.T) {
	t.Parallel()

	accepted := func() BuildSpec {
		spec := verifyingBuild()
		spec.Accept = []string{"firefox"}

		return spec
	}

	t.Run("recorded as built", func(t *testing.T) {
		t.Parallel()

		b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
			builtComponents: "firefox:140.0.4"}

		f := newFakeEC2(t)
		f.respond = b.serve(f)

		if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), accepted()); err != nil {
			t.Fatalf("BuildImage: %v", err)
		}

		if b.promotedComponents == nil || *b.promotedComponents != "firefox:140.0.4" {
			t.Errorf("the image was stamped with components %v, want firefox:140.0.4", b.promotedComponents)
		}
	})

	t.Run("recorded otherwise", func(t *testing.T) {
		t.Parallel()

		none := ""
		b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
			builtComponents: "firefox:140.0.4", recordedComponents: &none}

		f := newFakeEC2(t)
		f.respond = b.serve(f)

		if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), accepted()); err == nil {
			t.Fatal("an image recording other components than its build reported was stamped")
		}

		if b.promoted != "" {
			t.Errorf("the image was stamped %s", b.promoted)
		}
	})

	// OWED AND LEFT OUT: the template offers Firefox on x64 and the build reported
	// none, which is a deployment short of what it accepted.
	t.Run("accepted, offered and left out", func(t *testing.T) {
		t.Parallel()

		b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

		f := newFakeEC2(t)
		f.respond = b.serve(f)

		if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), accepted()); err == nil ||
			!strings.Contains(err.Error(), "this build owes") {
			t.Fatalf("a build that left out an owed component was answered %v", err)
		}

		if n := f.countOf("CreateImage"); n != 0 {
			t.Errorf("%d images were registered", n)
		}
	})

	t.Run("installed without acceptance", func(t *testing.T) {
		t.Parallel()

		b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
			builtComponents: "google-chrome:1.2.3"}

		f := newFakeEC2(t)
		f.respond = b.serve(f)

		if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), accepted()); err == nil {
			t.Fatal("a build that installed an unaccepted component produced an image")
		}

		if n := f.countOf("CreateImage"); n != 0 {
			t.Errorf("%d images were registered", n)
		}
	})
}

// A BUILD STAGES ITS PAYLOAD AND REMOVES IT, AND NAMES THE STAGED DIGEST IN ITS
// USER DATA: the bootstrap fetches what was uploaded and nothing else.
func TestABuildStagesWhatItsBootstrapFetches(t *testing.T) {
	t.Parallel()

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.serve(f)

	if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), verifyingBuild()); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	staged := f.stagedPayload(t)

	var builder string

	for _, call := range f.runInstancesCalls() {
		if call.Get("ImageId") == "ami-base" {
			builder = call.Get("UserData")
		}
	}

	script := decodeUserData(t, builder)
	if !strings.Contains(script, sha256Hex(staged)) {
		t.Errorf("the bootstrap does not check the digest of what was staged:\n%s", script)
	}

	f.mu.Lock()
	removed := f.removed
	f.mu.Unlock()

	if removed != 1 {
		t.Errorf("the staged payload was removed %d times, want once", removed)
	}

	if len(payloadMember(staged, amiLayerPath)) == 0 {
		t.Error("the staged payload carries no EC2 layer")
	}
}

// THE SUCCESS IS READ ONCE: the dwell repeats it until the builder powers off, and
// a repeat that arrives damaged is not a reason to terminate a build that finished.
func TestADamagedRepeatOfTheSuccessDoesNotEndTheBuild(t *testing.T) {
	t.Parallel()

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
		dwellPolls: 3, damagedRepeats: true}

	f := newFakeEC2(t)
	f.respond = b.serve(f)

	if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), verifyingBuild()); err != nil {
		t.Fatalf("a damaged repeat of a success already read ended the build: %v", err)
	}
}

// A FAILURE SAID AFTER THE SUCCESS STILL ENDS THE BUILD: a builder rebooted during
// its dwell runs the bootstrap again, and nothing it says next is ignored because a
// success came first.
func TestAFailureAfterTheSuccessEndsTheBuild(t *testing.T) {
	t.Parallel()

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
		dwellPolls: 3, failsAfterSuccess: true}

	f := newFakeEC2(t)
	f.respond = b.serve(f)

	if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), verifyingBuild()); err == nil ||
		!strings.Contains(err.Error(), "failed in "+bootstrapStep) {
		t.Fatalf("a failure after the success was answered %v", err)
	}

	if n := f.countOf("CreateImage"); n != 0 {
		t.Errorf("%d images were registered from a builder that failed after its success", n)
	}
}

// A FEW FAILED POLLS IN A ROW ARE A NETWORK THAT WENT AWAY, NOT A BUILD THAT
// FAILED; past maxBuildPollFailures the wait gives up, and nothing is imaged.
func TestAFewFailedPollsDoNotEndABuild(t *testing.T) {
	t.Parallel()

	short := strings.Repeat("R", maxBuildPollFailures-1)

	for _, tc := range []struct {
		reads, describes string
		wantOK           bool
	}{
		{reads: short, wantOK: true},
		{reads: short + "R"},
		// IN A ROW: an answered read between two runs of refusals starts the count
		// again, however many there are in all.
		{reads: short + "Q" + short, wantOK: true},
		// AND A REFUSED STATE IS A FAILED POLL TOO.
		{describes: short, wantOK: true},
		{describes: short + "R"},
	} {
		b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
			consoleReads: tc.reads, describeReads: tc.describes}

		f := newFakeEC2(t)
		f.respond = b.serve(f)

		_, err := newTestProvider(t, f, nil).BuildImage(t.Context(), verifyingBuild())
		if (err == nil) != tc.wantOK {
			t.Errorf("console reads %q, describes %q: the build answered %v, want success %v",
				tc.reads, tc.describes, err, tc.wantOK)
		}

		if n := f.countOf("CreateImage"); (n == 1) != tc.wantOK {
			t.Errorf("console reads %q, describes %q: %d images were registered", tc.reads, tc.describes, n)
		}
	}
}

// THE VERDICT build-ami.sh PRINTS IS ONE BILLET READS: the script's own verdict
// function, executed, through the parser and schema awaitBuild uses, rather than two
// copies of one format that agree only because both were typed the same way.
func TestTheBuildsVerdictRoundTrips(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "ami", "build-ami.sh"))
	if err != nil {
		t.Fatalf("read build-ami.sh: %v", err)
	}

	source := string(raw)

	start := strings.Index(source, "verdict() {\n")
	if start < 0 {
		t.Fatal("build-ami.sh defines no verdict function")
	}

	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatal("build-ami.sh's verdict function is not closed")
	}

	fn := source[start : start+end+3]

	for _, tc := range []struct {
		args []string
		want map[string]string
	}{
		{
			args: []string{"verdict=ok", "step=done", "template_step=", "components=firefox:158.0.1"},
			want: map[string]string{reportVerdictKey: verdictOK, reportStepKey: "done",
				buildTemplateStepKey: "", buildComponentsKey: "firefox:158.0.1"},
		},
		{
			args: []string{"verdict=fail", "step=template", "template_step=42", "components="},
			want: map[string]string{reportVerdictKey: verdictFail, reportStepKey: "template",
				buildTemplateStepKey: "42", buildComponentsKey: ""},
		},
	} {
		console := filepath.Join(t.TempDir(), "console")
		script := "set -eu\nnonce=" + verifyProbeNonce + "\nconsole=" + console +
			"\ndwell_rounds=1\ndwell_seconds=0\nsleep() { exit 0; }\n" + fn + "verdict \"$@\"\n"

		args := append([]string{"-c", script, "verdict"}, tc.args...)
		if out, err := exec.CommandContext(t.Context(), "bash", args...).CombinedOutput(); err != nil {
			t.Fatalf("the verdict function failed: %v\n%s", err, out)
		}

		said, err := os.ReadFile(console)
		if err != nil {
			t.Fatalf("the verdict function wrote no console: %v", err)
		}

		got, found := parseReport(string(said), verifyProbeNonce, newBuildSchema())
		if !found || len(got) != len(tc.want) {
			t.Fatalf("billet read %v (found %v) from:\n%s", got, found, said)
		}

		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("billet read %s=%q, the script said %q", k, got[k], v)
			}
		}
	}
}

// A BUILD WITH NO BUCKET IS REFUSED BEFORE ANYTHING IS BOUGHT.
func TestABuildWithoutAPayloadBucketIsRefused(t *testing.T) {
	t.Parallel()

	f := newFakeEC2(t)

	spec := verifyingBuild()
	spec.PayloadBucket = ""

	if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), spec); err == nil {
		t.Error("a build with no payload bucket went ahead")
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("%d instances were launched", n)
	}
}

// THE PAYLOAD UNPACKS AS ROOT'S: GNU tar, as the bootstrap runs it, makes the
// build's directory mode 700 from the archive's own header, so nothing GitHub's
// template opens up and no other account reaches what root runs from it. Only
// GNU tar has --no-overwrite-dir, so a machine without it skips.
func TestThePayloadUnpacksIntoARootOnlyDirectory(t *testing.T) {
	t.Parallel()

	version, err := exec.CommandContext(t.Context(), "tar", "--version").Output()
	if err != nil || !strings.Contains(string(version), "GNU tar") {
		t.Skip("no GNU tar here; the builder's tar is GNU's")
	}

	archive, _, err := payloadArchive(BuildSpec{Arch: "x64", nonce: verifyProbeNonce}, testLayer)
	if err != nil {
		t.Fatalf("payloadArchive: %v", err)
	}

	root := t.TempDir()

	// THE MODES AS ROOT KEEPS THEM: root's tar preserves an archive's permissions
	// by default, and an ordinary account's needs to be told to.
	cmd := exec.CommandContext(t.Context(), "tar", "--no-overwrite-dir", "--same-permissions", "-xzf", "-", "-C", root)
	cmd.Stdin = bytes.NewReader(archive)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}

	info, err := os.Stat(filepath.Join(root, "var", "lib", "billet-ami"))
	if err != nil {
		t.Fatalf("the payload made no build directory: %v", err)
	}

	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("the build's directory unpacked as %o, want 700", mode)
	}

	env, err := os.Stat(filepath.Join(root, "var", "lib", "billet-ami", "build.env"))
	if err != nil || env.Mode().Perm() != 0o600 {
		t.Errorf("build.env unpacked as %v (%v), want 600", env, err)
	}
}

// A KEY TWICE IS NO REPORT: billet writes each once, so a second is the machine's,
// and which of the two is billet's cannot be told.
func TestAReportWithAKeyTwiceIsNoReport(t *testing.T) {
	t.Parallel()

	block := buildBegin + " " + verifyProbeNonce + "\nverdict=ok\nstep=done\ntemplate_step=\n" +
		"components=\ncomponents=firefox:1\n" + buildEnd + " " + verifyProbeNonce + "\n"

	if report, found := parseReport(block, verifyProbeNonce, newBuildSchema()); found {
		t.Errorf("a block naming components twice was read as %v", report)
	}
}

// A FAILED VERIFICATION QUOTES NO COMPONENTS: unlike the report's other fields the
// record is held to a vocabulary only once the image has passed, so a failed
// report's record is the machine's text and stays out of billet's error.
func TestAFailedVerificationQuotesNoComponents(t *testing.T) {
	t.Parallel()

	secret := "secret-token-123"
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
		verdict: verdictFail, recordedComponents: &secret}

	f := newFakeEC2(t)
	f.respond = b.serve(f)

	err := newTestProvider(t, f, nil).VerifyImage(t.Context(), VerifySpec{Image: "ami-new"})
	if err == nil {
		t.Fatal("a failed verification stamped the image")
	}

	if strings.Contains(err.Error(), secret) {
		t.Errorf("the failure quotes the image's components record: %v", err)
	}

	if !strings.Contains(err.Error(), "docker_driver=overlay2") {
		t.Errorf("the failure no longer renders the report's other fields, so this test proves "+
			"nothing about the components alone: %v", err)
	}
}
