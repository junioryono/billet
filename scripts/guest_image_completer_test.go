package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// completerFixture is a tree the image's completer runs against with its paths
// moved under it: GitHub's installers and test runner stood in for, the
// browsers' version commands on the PATH /etc/environment names, and the
// Android SDK's and CodeQL's version files under root.
type completerFixture struct {
	dir, complete, environment, imageEnv, bin, root, report, calls string
}

func newCompleterFixture(t *testing.T) completerFixture {
	t.Helper()

	dir := t.TempDir()
	fx := completerFixture{
		dir:         dir,
		complete:    filepath.Join(dir, "complete"),
		environment: filepath.Join(dir, "etc", "environment"),
		imageEnv:    filepath.Join(dir, "etc", "billet-image-env"),
		bin:         filepath.Join(dir, "usr-local-bin"),
		root:        filepath.Join(dir, "root"),
		report:      filepath.Join(dir, "report"),
		calls:       filepath.Join(dir, "calls"),
	}
	for _, d := range []string{
		filepath.Join(fx.complete, "scripts", "build"), filepath.Join(fx.complete, "scripts", "helpers"),
		filepath.Join(fx.complete, "installers"), filepath.Join(dir, "etc"), fx.bin, filepath.Join(dir, "browsers"),
		filepath.Join(fx.root, "usr", "local", "lib", "android", "sdk", "cmdline-tools", "latest"),
		filepath.Join(fx.root, "opt", "hostedtoolcache", "CodeQL", "2.23.3", "x64", "codeql"),
		filepath.Join(fx.root, "opt", "billet", "bin"), filepath.Join(fx.root, "opt", "google", "chrome"),
		filepath.Join(fx.root, "usr", "share", "doc"), filepath.Join(fx.root, "var", "lib", "apt", "lists"),
		filepath.Join(fx.root, "tmp"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// RUBY'S TOOLCACHE DIRECTORIES CARRY THE STICKY BIT, as configure-system.sh
	// leaves them; the leftovers are what cleanup.sh and the build remove.
	ruby := filepath.Join(fx.root, "opt", "hostedtoolcache", "Ruby", "3.3.1", "x64")
	if err := os.MkdirAll(ruby, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ruby, filepath.Dir(ruby)} {
		if err := os.Chmod(d, 0o1777); err != nil {
			t.Fatal(err)
		}
	}
	for _, leftover := range []string{"var/lib/apt/lists/archive_ubuntu_dists", "tmp/download", "opt/billet/bin/billet"} {
		if err := os.WriteFile(filepath.Join(fx.root, leftover), []byte("x\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	copyScript := func(from, to string, mode os.FileMode) {
		body := readScriptFile(t, from)
		if err := os.WriteFile(to, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	copyScript(filepath.Join("runner-images", "completer", "run"), filepath.Join(fx.complete, "run"), 0o755)
	copyScript(filepath.Join("runner-images", "write-image-env.sh"), filepath.Join(fx.complete, "write-image-env.sh"), 0o755)
	copyScript(filepath.Join("runner-images", "pam-environment.awk"), filepath.Join(fx.complete, "pam-environment.awk"), 0o644)

	// EACH INSTALLER RECORDS THE ENVIRONMENT IT RAN IN and adds a variable to
	// /etc/environment, as GitHub's do.
	for script, variable := range map[string]string{
		"install-android-sdk.sh":    "ANDROID_HOME=/usr/local/lib/android/sdk",
		"install-codeql-bundle.sh":  "",
		"install-firefox.sh":        "GECKOWEBDRIVER=/usr/local/share/gecko_driver",
		"install-google-chrome.sh":  "CHROMEWEBDRIVER=/usr/local/share/chromedriver-linux64",
		"install-microsoft-edge.sh": "EDGEWEBDRIVER=/usr/local/share/edge_driver",
	} {
		body := "#!/bin/sh\necho \"$(basename \"$0\") HELPER_SCRIPTS=$HELPER_SCRIPTS INSTALLER_SCRIPT_FOLDER=$INSTALLER_SCRIPT_FOLDER " +
			"DEBIAN_FRONTEND=$DEBIAN_FRONTEND HOME=$HOME EARLIER=${EARLIER:-} REPORT=$(cat " + fx.report + " | tr '\\n' ,)\" >>" + fx.calls + "\n" +
			"[ -e \"" + dir + "/fail-$(basename \"$0\")\" ] && exit 1\n"
		if variable != "" {
			body += "echo " + variable + " >>" + fx.environment + "\n"
		}
		body += "echo EARLIER=$(basename \"$0\") >>" + fx.environment + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(fx.complete, "scripts", "build", script), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	invoke := "#!/bin/sh\necho \"invoke_tests $*\" >>" + fx.calls + "\n[ -e \"" + dir + "/fail-tests-$1\" ] && exit 1\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fx.complete, "scripts", "helpers", "invoke-tests.sh"), []byte(invoke), 0o755); err != nil {
		t.Fatal(err)
	}

	apt := "#!/bin/sh\necho \"apt-get $*\" >>" + fx.calls + "\n[ -e \"" + dir + "/fail-apt-$1\" ] && exit 100\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "browsers", "apt-get"), []byte(apt), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, version := range map[string]string{
		"google-chrome": "155.0.8059.84",
		// WITH THE TRAILING SPACE Edge's own --version prints.
		"microsoft-edge": "Microsoft Edge 141.0.3537.71 ",
		"firefox":        "Mozilla Firefox 144.0",
	} {
		body := "#!/bin/sh\n[ -e \"" + dir + "/garble-" + name + "\" ] && { echo 'not a version!'; exit 0; }\necho '" + version + "'\n"
		if err := os.WriteFile(filepath.Join(dir, "browsers", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fx.root, "usr", "local", "lib", "android", "sdk", "cmdline-tools", "latest",
		"source.properties"), []byte("Pkg.Desc=Android SDK Command-line Tools\nPkg.Revision=19.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.root, "opt", "hostedtoolcache", "CodeQL", "2.23.3", "x64", "codeql", "codeql"),
		[]byte("#!/bin/sh\necho 2.23.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	environment := "PATH=\"" + fx.bin + ":" + filepath.Join(dir, "browsers") + ":/usr/bin:/bin\"\nImageOS=ubuntu24\n"
	if err := os.WriteFile(fx.environment, []byte(environment), 0o644); err != nil {
		t.Fatal(err)
	}

	return fx
}

// run runs the completer for components and returns its output.
func (fx completerFixture) run(t *testing.T, report bool, components ...string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), filepath.Join(fx.complete, "run"), components...)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"BILLET_COMPLETE_DIR=" + fx.complete,
		"BILLET_COMPLETE_ENVIRONMENT=" + fx.environment,
		"BILLET_COMPLETE_IMAGE_ENV=" + fx.imageEnv,
		"BILLET_COMPLETE_BIN=" + fx.bin,
		"BILLET_COMPLETE_ROOT=" + fx.root,
	}
	if report {
		cmd.Env = append(cmd.Env, "BILLET_COMPLETION_REPORT="+fx.report)
	}
	output, err := cmd.CombinedOutput()

	return string(output), err
}

func (fx completerFixture) reportLines(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile(fx.report)
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

// EVERY ACCEPTED COMPONENT IS INSTALLED AS GITHUB'S TEMPLATE RAN ITS STEP, its
// version read from what is on disk, and the job's environment is written again
// with what the installers added.
func TestTheCompleterInstallsEachComponentAndReportsItsVersion(t *testing.T) {
	t.Parallel()

	fx := newCompleterFixture(t)
	if output, err := fx.run(t, true, string(config.ComponentGoogleChrome), "android-sdk", "codeql", "firefox",
		"microsoft-edge"); err != nil {
		t.Fatalf("the completer exited %v:\n%s", err, output)
	}

	want := []string{
		"installed=google-chrome 155.0.8059.84",
		"installed=android-sdk 19.0",
		"installed=codeql 2.23.3",
		"installed=firefox 144.0",
		"installed=microsoft-edge 141.0.3537.71",
	}
	if got := fx.reportLines(t); !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}

	calls, err := os.ReadFile(fx.calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"install-google-chrome.sh HELPER_SCRIPTS=" + filepath.Join(fx.complete, "scripts", "helpers") +
			" INSTALLER_SCRIPT_FOLDER=" + filepath.Join(fx.complete, "installers") +
			" DEBIAN_FRONTEND=noninteractive HOME=/root EARLIER= REPORT=\n",
		// EACH STEP READS WHAT THE STEP BEFORE IT WROTE to /etc/environment, and
		// NOTHING IS REPORTED WHILE ANY STEP RUNS: the report is written once the
		// image is finished.
		"install-android-sdk.sh HELPER_SCRIPTS=",
		" EARLIER=install-google-chrome.sh REPORT=\n",
		"apt-get update\n",
		"apt-get clean\n",
		"invoke_tests Toolset Toolset.CodeQL\n",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("the calls do not include %q:\n%s", want, calls)
		}
	}

	imageEnv, err := os.ReadFile(fx.imageEnv)
	if err != nil {
		t.Fatalf("the job's environment was not written again: %v", err)
	}
	for _, want := range []string{"ImageOS=ubuntu24\n", "ANDROID_HOME=/usr/local/lib/android/sdk\n",
		"CHROMEWEBDRIVER=", "EDGEWEBDRIVER=", "GECKOWEBDRIVER="} {
		if !strings.Contains(string(imageEnv), want) {
			t.Errorf("the job's environment lacks %q:\n%s", want, imageEnv)
		}
	}
	if _, err := os.Lstat(filepath.Join(fx.bin, "invoke_tests")); !os.IsNotExist(err) {
		t.Errorf("invoke_tests was left on the image's PATH (%v)", err)
	}
}

// A COMPONENT THAT CANNOT BE PROVED INSTALLED IS REPORTED FAILED, whichever part
// failed, and the others are still installed: an installer that fails, CodeQL's
// toolset test failing, a version that cannot be read, and a name the image does
// not complete.
func TestTheCompleterReportsEachFailureByComponent(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		marker string
		failed string
	}{
		"an installer fails":            {"fail-install-firefox.sh", "firefox"},
		"codeql's toolset test fails":   {"fail-tests-Toolset", "codeql"},
		"a version cannot be read":      {"garble-microsoft-edge", "microsoft-edge"},
		"a name the image does not use": {"", "a-component"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fx := newCompleterFixture(t)
			if tc.marker != "" {
				if err := os.WriteFile(filepath.Join(fx.dir, tc.marker), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			components := []string{"firefox", "codeql", "microsoft-edge", "google-chrome"}
			if tc.marker == "" {
				components = append(components, tc.failed)
			}
			if output, err := fx.run(t, true, components...); err != nil {
				t.Fatalf("the completer exited %v:\n%s", err, output)
			}

			lines := fx.reportLines(t)
			if !slices.Contains(lines, "failed="+tc.failed) {
				t.Errorf("the report does not say %s failed: %q", tc.failed, lines)
			}
			for _, line := range lines {
				if strings.HasPrefix(line, "installed="+tc.failed+" ") {
					t.Errorf("the report says %s installed: %q", tc.failed, lines)
				}
			}
			if !slices.Contains(lines, "installed=google-chrome 155.0.8059.84") {
				t.Errorf("one failure stopped the others: %q", lines)
			}
		})
	}
}

// A JOB ENVIRONMENT THAT CANNOT BE WRITTEN AGAIN FAILS EVERY COMPONENT, because a
// component installed with its variables unseen is not one a job can use.
func TestTheCompleterFailsEverythingWhenTheJobEnvironmentCannotBeWritten(t *testing.T) {
	t.Parallel()

	fx := newCompleterFixture(t)
	if err := os.WriteFile(fx.environment, []byte("PATH=\""+filepath.Join(fx.dir, "browsers")+":/usr/bin:/bin\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := fx.run(t, true, "google-chrome", "firefox"); err != nil {
		t.Fatalf("the completer exited %v:\n%s", err, output)
	}
	if got, want := fx.reportLines(t), []string{"failed=google-chrome", "failed=firefox"}; !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
}

// THE BROWSERS' PREREQUISITE IS INSTALLED BEFORE THEIR INSTALLER RUNS, because a
// deployment may accept Chrome alone, without the Firefox that gives GitHub's
// build the real libasound2; and a prerequisite that cannot be installed fails
// the component without running its installer.
func TestTheCompleterInstallsWhatGitHubsOrderGivesTheBrowsers(t *testing.T) {
	t.Parallel()

	for browser, installer := range map[string]string{
		"google-chrome":  "install-google-chrome.sh ",
		"microsoft-edge": "install-microsoft-edge.sh ",
	} {
		t.Run(browser, func(t *testing.T) {
			t.Parallel()

			fx := newCompleterFixture(t)
			if output, err := fx.run(t, true, browser, "codeql"); err != nil {
				t.Fatalf("the completer exited %v:\n%s", err, output)
			}
			calls, err := os.ReadFile(fx.calls)
			if err != nil {
				t.Fatal(err)
			}
			ran := string(calls)
			prerequisite := strings.Index(ran, "apt-get install -y --no-install-recommends libasound2t64\n")
			installed := strings.Index(ran, installer)
			if prerequisite < 0 || installed < 0 || prerequisite > installed {
				t.Fatalf("libasound2t64 was not installed before %s's installer ran:\n%s", browser, ran)
			}
			if strings.Count(ran, "libasound2t64") != 1 {
				t.Fatalf("a component that needs no prerequisite was given one:\n%s", ran)
			}

			fx = newCompleterFixture(t)
			if err := os.WriteFile(filepath.Join(fx.dir, "fail-apt-install"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if output, err := fx.run(t, true, browser, "codeql"); err != nil {
				t.Fatalf("the completer exited %v:\n%s", err, output)
			}
			if got, want := fx.reportLines(t), []string{"failed=" + browser, "installed=codeql 2.23.3"}; !slices.Equal(got, want) {
				t.Fatalf("the report is %q, want %q", got, want)
			}
			calls, err = os.ReadFile(fx.calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), installer) {
				t.Fatalf("%s's installer ran without its prerequisite:\n%s", browser, calls)
			}
		})
	}
}

// A FINISH THAT FAILS FAILS EVERY COMPONENT, those already installed included:
// the image is not the one GitHub's template leaves until it is finished.
func TestTheCompleterFailsEverythingWhenTheFinishFails(t *testing.T) {
	t.Parallel()

	fx := newCompleterFixture(t)
	if err := os.WriteFile(filepath.Join(fx.dir, "fail-apt-clean"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := fx.run(t, true, "google-chrome", "firefox"); err != nil {
		t.Fatalf("the completer exited %v:\n%s", err, output)
	}
	if got, want := fx.reportLines(t), []string{"failed=google-chrome", "failed=firefox"}; !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
}

// AN APT THAT CANNOT UPDATE ITS LISTS PREPARES NOTHING: every component is
// reported failed and no installer runs, because the published image keeps no
// lists to install from.
func TestTheCompleterFailsEverythingWhenAptCannotUpdate(t *testing.T) {
	t.Parallel()

	fx := newCompleterFixture(t)
	if err := os.WriteFile(filepath.Join(fx.dir, "fail-apt-update"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := fx.run(t, true, "google-chrome", "codeql"); err != nil {
		t.Fatalf("the completer exited %v:\n%s", err, output)
	}
	if got, want := fx.reportLines(t), []string{"failed=google-chrome", "failed=codeql"}; !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
	calls, err := os.ReadFile(fx.calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "install-") {
		t.Fatalf("an installer ran with no apt lists:\n%s", calls)
	}
}

// THE IMAGE IS LEFT AS GITHUB'S TEMPLATE LEAVES IT: what the components installed
// under /opt and /usr/share writable by everyone, Ruby's toolcache keeping the
// sticky bit Ruby 4 needs, billet's own /opt/billet untouched, and apt's lists
// and the scratch GitHub's cleanup removes gone.
func TestTheCompleterLeavesTheImageAsTheTemplateDoes(t *testing.T) {
	t.Parallel()

	fx := newCompleterFixture(t)
	if output, err := fx.run(t, true, "google-chrome"); err != nil {
		t.Fatalf("the completer exited %v:\n%s", err, output)
	}
	mode := func(path string) os.FileMode {
		t.Helper()

		info, err := os.Stat(filepath.Join(fx.root, path))
		if err != nil {
			t.Fatal(err)
		}

		return info.Mode()
	}
	for path, want := range map[string]os.FileMode{
		"opt/google/chrome":                  os.ModeDir | 0o777,
		"usr/share/doc":                      os.ModeDir | 0o777,
		"opt/hostedtoolcache/Ruby/3.3.1/x64": os.ModeDir | os.ModeSticky | 0o777,
		"opt/hostedtoolcache/Ruby/3.3.1":     os.ModeDir | os.ModeSticky | 0o777,
		"opt/billet/bin":                     os.ModeDir | 0o755,
		"opt/billet/bin/billet":              0o755,
	} {
		if got := mode(path); got != want {
			t.Errorf("%s is %v, want %v", path, got, want)
		}
	}
	for _, gone := range []string{"var/lib/apt/lists/archive_ubuntu_dists", "tmp/download"} {
		if _, err := os.Stat(filepath.Join(fx.root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s was left in the image (%v)", gone, err)
		}
	}
}

// THE PERMISSIONS ARE configure-system.sh'S OWN, line for line, so a pin bump that
// changes what GitHub's image leaves writable fails here.
func TestTheCompletersPermissionsAreTheTemplatesOwn(t *testing.T) {
	t.Parallel()

	upstream := readScriptFile(t, filepath.Join("runner-images", "upstream", "images", "ubuntu", "scripts", "build",
		"configure-system.sh"))
	run := readScriptFile(t, filepath.Join("runner-images", "completer", "run"))
	for template, completer := range map[string]string{
		`chmod -R 777 /usr/share`: `chmod -R 777 "$root/usr/share" || finished=0`,
		`chmod -R 777 /opt`:       `find "$root/opt" -mindepth 1 -maxdepth 1 ! -name billet -exec chmod -R 777 {} + || finished=0`,
		`    find /opt/hostedtoolcache/Ruby -type d -exec chmod +t {} +`: `	find "$root/opt/hostedtoolcache/Ruby" -type d -exec chmod +t {} + || finished=0`,
	} {
		if !hasExactLine(upstream, template) {
			t.Errorf("configure-system.sh no longer says %q; the completer's permissions follow it", template)
		}
		if !hasExactLine(run, completer) {
			t.Errorf("the completer does not say %q", completer)
		}
	}
}

// NO REPORT IS NAMED, SO NOTHING CAN BE SAID, and the completer says so by its
// status rather than installing anything.
func TestTheCompleterRefusesToRunWithoutAReport(t *testing.T) {
	t.Parallel()

	fx := newCompleterFixture(t)
	output, err := fx.run(t, false, "google-chrome")
	if err == nil || !strings.Contains(output, "BILLET_COMPLETION_REPORT names no report") {
		t.Fatalf("answered %v, want a refusal:\n%s", err, output)
	}
	if _, statErr := os.Stat(fx.calls); !os.IsNotExist(statErr) {
		t.Fatalf("an installer ran without a report (%v)", statErr)
	}
}

// ONE LIST OF COMPONENTS: what the completer installs, what differences.tsv skips
// with a reason naming #584, what the build declares, and config.ImageComponents
// are the same five, each with its installer.
func TestTheCompletedComponentsAreConfigsList(t *testing.T) {
	t.Parallel()

	want := make([]string, 0, len(config.ImageComponents()))
	for _, c := range config.ImageComponents() {
		want = append(want, string(c))
	}

	run := readScriptFile(t, filepath.Join("runner-images", "completer", "run"))
	cases := regexp.MustCompile(`(?m)^\t([a-z][a-z-]*)\) echo (install-[a-z-]+\.sh) ;;$`).FindAllStringSubmatch(run, -1)
	var completed []string
	scripts := map[string]bool{}
	for _, c := range cases {
		completed = append(completed, c[1])
		scripts[c[2]] = true
	}
	slices.Sort(completed)
	if !slices.Equal(completed, want) {
		t.Errorf("the completer installs %q, want config's %q", completed, want)
	}

	// THE ONE LIST IS check-guest-component.sh's, which the build and the gate read.
	names, err := exec.CommandContext(t.Context(), "bash", "check-guest-component.sh", "names").Output()
	if err != nil {
		t.Fatalf("check-guest-component.sh names: %v", err)
	}
	if got := strings.Fields(string(names)); !slices.Equal(got, want) {
		t.Errorf("check-guest-component.sh names %q, want %q", got, want)
	}
	build := readScriptFile(t, "build-guest-image.sh")
	if !strings.Contains(build, `COMPONENTS=$("$SCRIPT_DIR/check-guest-component.sh" names`) {
		t.Error("build-guest-image.sh does not read its components from check-guest-component.sh")
	}

	differences := readScriptFile(t, filepath.Join("runner-images", "differences.tsv"))
	skipped := map[string]bool{}
	for _, line := range strings.Split(differences, "\n") {
		if fields := strings.Split(line, "\t"); len(fields) == 3 && fields[0] == "skip" && strings.Contains(fields[2], "#584") {
			skipped[fields[1]] = true
		}
	}
	for script := range scripts {
		if !skipped[script] {
			t.Errorf("the completer runs %s, which differences.tsv does not skip for #584", script)
		}
		if !strings.Contains(build, script) {
			t.Errorf("build-guest-image.sh does not install %s into the completer", script)
		}
	}
	for script := range skipped {
		if !scripts[script] {
			t.Errorf("differences.tsv skips %s for #584 and the completer does not run it", script)
		}
	}
}
