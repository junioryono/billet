package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// componentCheck runs check-guest-component.sh MODE NAME ROOT, with `chroot` and
// `id` stood in for so `present` runs without root: the stand-in chroot runs its
// command with paths moved under the root, which follows no symlink the way a
// real chroot would but asks the same questions. Asked as the runner
// (--userspec=runner:runner), it refuses each path the tree's .closed-to-runner
// lists, as a mode the runner cannot pass would.
func componentCheck(t *testing.T, mode, name, root string) (string, int) {
	t.Helper()

	fakes := t.TempDir()
	for file, body := range map[string]string{
		"id": "#!/bin/sh\n[ \"$1\" = -u ] && { echo 0; exit 0; }\nexec /usr/bin/id \"$@\"\n",
		"chroot": "#!/bin/bash\nrunner=\ncase $1 in --userspec=runner:runner) runner=1; shift ;; --userspec=*) exit 125 ;; esac\n" +
			"root=$1; shift\nargs=()\nfor a in \"$@\"; do\n" +
			"\tif [ -n \"$runner\" ] && grep -qxF -- \"$a\" \"$root/.closed-to-runner\" 2>/dev/null; then exit 1; fi\n" +
			"\tcase $a in /*) args+=(\"$root$a\") ;; *) args+=(\"$a\") ;; esac\ndone\nexec \"${args[@]}\"\n",
	} {
		if err := forkSafeWriteFile(filepath.Join(fakes, file), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "bash", "check-guest-component.sh", mode, name, root)
	cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		ok := errors.As(err, &exit)
		if !ok {
			t.Fatalf("check-guest-component.sh did not run: %v", err)
		}
		status = exit.ExitCode()
	}

	return string(out), status
}

// completedTree is a root with everything NAME installs, as check-guest-component.sh
// lists it, and the variables it adds in the job's environment.
func completedTree(t *testing.T, name string) string {
	t.Helper()

	root := t.TempDir()
	write := func(path, body string, mode os.FileMode) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := forkSafeWriteFile(full, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	dir := func(path string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Join(root, path), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, path), mode); err != nil {
			t.Fatal(err)
		}
	}

	env := "ImageOS=ubuntu24\n"
	switch name {
	case "android-sdk":
		write("usr/local/lib/android/sdk/cmdline-tools/latest/bin/sdkmanager", "#!/bin/sh\n", 0o755)
		dir("usr/local/lib/android/sdk/platforms/android-35", 0o777)
		write("usr/local/lib/android/sdk/ndk/27.3.13750724/source.properties", "Pkg.Revision = 27.3.13750724\n", 0o644)
		write("usr/local/lib/android/sdk/ndk/28.2.13676358/source.properties", "Pkg.Revision = 28.2.13676358\n", 0o644)
		dir("usr/local/lib/android/sdk", 0o777)
		env += "ANDROID_HOME=/usr/local/lib/android/sdk\nANDROID_SDK_ROOT=/usr/local/lib/android/sdk\n" +
			"ANDROID_NDK=/usr/local/lib/android/sdk/ndk/27.3.13750724\n" +
			"ANDROID_NDK_HOME=/usr/local/lib/android/sdk/ndk/27.3.13750724\n" +
			"ANDROID_NDK_ROOT=/usr/local/lib/android/sdk/ndk/27.3.13750724\n" +
			"ANDROID_NDK_LATEST_HOME=/usr/local/lib/android/sdk/ndk/28.2.13676358\n"
	case "codeql":
		write("opt/hostedtoolcache/CodeQL/2.23.3/x64/codeql/codeql", "#!/bin/sh\necho 2.23.3\n", 0o755)
		write("opt/hostedtoolcache/CodeQL/2.23.3/x64/pinned-version", "", 0o644)
		write("opt/hostedtoolcache/CodeQL/2.23.3/x64.complete", "", 0o644)
	case "firefox":
		write("usr/bin/firefox", "#!/bin/sh\n", 0o755)
		write("usr/bin/geckodriver", "#!/bin/sh\n", 0o755)
		dir("usr/local/share/gecko_driver", 0o755)
		env += "GECKOWEBDRIVER=/usr/local/share/gecko_driver\n"
	case "google-chrome":
		for _, bin := range []string{"usr/bin/google-chrome", "usr/bin/chromedriver", "usr/bin/chromium"} {
			write(bin, "#!/bin/sh\n", 0o755)
		}
		for _, d := range []string{"opt/google/chrome", "usr/local/share/chromedriver-linux64", "usr/local/share/chromium"} {
			dir(d, 0o755)
		}
		env += "CHROME_BIN=/usr/bin/google-chrome\nCHROMEWEBDRIVER=/usr/local/share/chromedriver-linux64\n"
	case "microsoft-edge":
		write("usr/bin/microsoft-edge", "#!/bin/sh\n", 0o755)
		write("usr/bin/msedgedriver", "#!/bin/sh\n", 0o755)
		dir("usr/local/share/edge_driver", 0o755)
		env += "EDGEWEBDRIVER=/usr/local/share/edge_driver\n"
	}
	write("etc/billet-image-env", env, 0o644)

	return root
}

// EVERY COMPONENT IS CHECKED BOTH WAYS FROM ONE LIST: a completed tree is present
// and not absent, and a tree that carries none of it is absent and not present.
func TestEachComponentIsCheckedPresentAndAbsentFromOneList(t *testing.T) {
	t.Parallel()

	names, err := exec.CommandContext(t.Context(), "bash", "check-guest-component.sh", "names").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(string(names)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			completed := completedTree(t, name)
			if out, status := componentCheck(t, "present", name, completed); status != 0 {
				t.Errorf("a completed %s is not present (status %d):\n%s", name, status, out)
			}
			if out, status := componentCheck(t, "absent", name, completed); status != 1 ||
				!strings.Contains(out, "billet may not redistribute (#584)") {
				t.Errorf("a completed %s reads as absent (status %d):\n%s", name, status, out)
			}

			bare := completedTree(t, "none")
			if out, status := componentCheck(t, "absent", name, bare); status != 0 {
				t.Errorf("a tree without %s is not absent (status %d):\n%s", name, status, out)
			}
			if out, status := componentCheck(t, "present", name, bare); status != 1 {
				t.Errorf("a tree without %s reads as present (status %d):\n%s", name, status, out)
			}
		})
	}
}

// ONE PIECE MISSING OR LEFT BEHIND IS NAMED, so a completion that stopped halfway
// is not present, and an image that kept one link of a component is not absent.
func TestOnePieceOfAComponentDecidesBothWays(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		component string
		damage    func(t *testing.T, root string)
		mode      string
		says      string
	}{
		"a driver missing": {"google-chrome", func(t *testing.T, root string) {
			t.Helper()

			removeAll(t, filepath.Join(root, "usr", "bin", "chromedriver"))
		}, "present", "/usr/bin/chromedriver is not an executable"},
		"a variable missing": {"microsoft-edge", func(t *testing.T, root string) {
			t.Helper()

			writeTreeFile(t, root, "etc/billet-image-env", "ImageOS=ubuntu24\n")
		}, "present", "does not say EDGEWEBDRIVER=/usr/local/share/edge_driver"},
		// A VERSIONED VARIABLE IS ASKED FOR TOO, by the path its value starts with.
		"an NDK variable pointing elsewhere": {"android-sdk", func(t *testing.T, root string) {
			t.Helper()

			env, err := os.ReadFile(filepath.Join(root, "etc", "billet-image-env"))
			if err != nil {
				t.Fatal(err)
			}

			writeTreeFile(t, root, "etc/billet-image-env", strings.Replace(string(env),
				"ANDROID_NDK_ROOT=/usr/local/lib/android/sdk/ndk/27.3.13750724", "ANDROID_NDK_ROOT=/opt/ndk", 1))
		}, "present", "does not set ANDROID_NDK_ROOT to a path under /usr/local/lib/android/sdk/ndk/"},
		// A PREFIX COMPARED AS TEXT IS STILL CLIMBED OUT OF WITH `..`.
		"an NDK variable climbing out of the SDK": {"android-sdk", func(t *testing.T, root string) {
			t.Helper()

			env, err := os.ReadFile(filepath.Join(root, "etc", "billet-image-env"))
			if err != nil {
				t.Fatal(err)
			}

			writeTreeFile(t, root, "etc/billet-image-env", strings.Replace(string(env),
				"ANDROID_NDK_HOME=/usr/local/lib/android/sdk/ndk/27.3.13750724",
				"ANDROID_NDK_HOME=/usr/local/lib/android/sdk/ndk/../../../../../../usr", 1))
		}, "present", "is not one directory directly under /usr/local/lib/android/sdk/ndk/"},
		// A DIRECTORY WITHOUT AN NDK IN IT IS NOT ONE.
		"an NDK directory with no NDK in it": {"android-sdk", func(t *testing.T, root string) {
			t.Helper()

			removeAll(t, filepath.Join(root, "usr", "local", "lib", "android", "sdk", "ndk", "27.3.13750724", "source.properties"))
		}, "present", "in which the runner reads no source.properties"},
		// ROOT READS WHAT THE RUNNER CANNOT: the NDK is a job's only if the runner
		// can read it.
		"an NDK closed to the runner": {"android-sdk", func(t *testing.T, root string) {
			t.Helper()

			writeTreeFile(t, root, ".closed-to-runner", "/usr/local/lib/android/sdk/ndk/27.3.13750724/source.properties\n")
		}, "present", "ANDROID_NDK names /usr/local/lib/android/sdk/ndk/27.3.13750724, in which the runner reads no source.properties"},
		// A LINK IS NOT THE NDK'S OWN DIRECTORY.
		"an NDK variable naming a link": {"android-sdk", func(t *testing.T, root string) {
			t.Helper()

			ndk := filepath.Join(root, "usr", "local", "lib", "android", "sdk", "ndk")
			removeAll(t, filepath.Join(ndk, "28.2.13676358"))
			if err := os.Symlink("27.3.13750724", filepath.Join(ndk, "28.2.13676358")); err != nil {
				t.Fatal(err)
			}
		}, "present", "ANDROID_NDK_LATEST_HOME names /usr/local/lib/android/sdk/ndk/28.2.13676358, which is not a directory"},
		// AND UNDER IT IS NOT ENOUGH: the NDK it names must be there.
		"an NDK variable naming an NDK that is not there": {"android-sdk", func(t *testing.T, root string) {
			t.Helper()

			removeAll(t, filepath.Join(root, "usr", "local", "lib", "android", "sdk", "ndk", "28.2.13676358"))
		}, "present", "ANDROID_NDK_LATEST_HOME names /usr/local/lib/android/sdk/ndk/28.2.13676358, which is not a directory"},
		"an SDK Gradle cannot write": {"android-sdk", func(t *testing.T, root string) {
			t.Helper()

			if err := os.Chmod(filepath.Join(root, "usr", "local", "lib", "android", "sdk"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "present", "is not writable by everyone"},
		"codeql without its pinned-version": {"codeql", func(t *testing.T, root string) {
			t.Helper()

			removeAll(t, filepath.Join(root, "opt", "hostedtoolcache", "CodeQL", "2.23.3", "x64", "pinned-version"))
		}, "present", "no x64/pinned-version"},
		"codeql that does not run": {"codeql", func(t *testing.T, root string) {
			t.Helper()

			writeTreeFile(t, root, "opt/hostedtoolcache/CodeQL/2.23.3/x64/codeql/codeql", "#!/bin/sh\nexit 1\n")
		}, "present", "does not run"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := completedTree(t, tc.component)
			tc.damage(t, root)
			out, status := componentCheck(t, tc.mode, tc.component, root)
			if status != 1 || !strings.Contains(out, tc.says) {
				t.Errorf("answered %d, want a failure saying %q:\n%s", status, tc.says, out)
			}
		})
	}

	// A DANGLING LINK LEFT IN THE IMAGE IS STILL THE COMPONENT'S.
	root := completedTree(t, "none")
	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/google/chrome/google-chrome", filepath.Join(root, "usr", "bin", "google-chrome")); err != nil {
		t.Fatal(err)
	}
	if out, status := componentCheck(t, "absent", "google-chrome", root); status != 1 ||
		!strings.Contains(out, "the image carries /usr/bin/google-chrome") {
		t.Errorf("a dangling link read as absent (%d):\n%s", status, out)
	}

	// AN NDK VARIABLE LEFT BEHIND STILL TELLS A JOB WHERE ANDROID IS, though the SDK
	// is gone.
	root = completedTree(t, "none")
	writeTreeFile(t, root, "etc/billet-image-env", "ImageOS=ubuntu24\nANDROID_NDK_LATEST_HOME=/usr/local/lib/android/sdk/ndk/28.2.13676358\n")
	if out, status := componentCheck(t, "absent", "android-sdk", root); status != 1 ||
		!strings.Contains(out, "names ANDROID_NDK_LATEST_HOME, which only android-sdk sets") {
		t.Errorf("a leftover NDK variable read as absent (%d):\n%s", status, out)
	}

	// A JOB ENVIRONMENT THAT CANNOT BE READ CANNOT SAY A VARIABLE IS ABSENT.
	root = completedTree(t, "none")
	removeAll(t, filepath.Join(root, "etc", "billet-image-env"))
	if out, status := componentCheck(t, "absent", "firefox", root); status != 1 ||
		!strings.Contains(out, "could not read") {
		t.Errorf("an unreadable job environment read as absent (%d):\n%s", status, out)
	}
}

// A QUESTION THAT CANNOT BE ASKED IS STATUS 2, never an answer.
func TestTheComponentCheckRefusesWhatItCannotAsk(t *testing.T) {
	t.Parallel()

	root := completedTree(t, "none")
	for _, args := range [][]string{
		{"present", "a-component", root},
		{"absent", "a-component", root},
		{"absent", "firefox", filepath.Join(root, "missing")},
		{"present", "firefox"},
		{"sideways", "firefox", root},
	} {
		cmd := exec.CommandContext(t.Context(), "bash", append([]string{"check-guest-component.sh"}, args...)...)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		ok := errors.As(err, &exit)
		if !ok || exit.ExitCode() != 2 {
			t.Errorf("%v answered %v, want status 2:\n%s", args, err, out)
		}
	}
}

// THE GATE ASKS IT OF THE IMAGE IT PUBLISHES: none of the components, the
// completer in place, and the manifest declaring each. A check that is defined and
// never called passes every image.
func TestTheGateAsksForTheComponentsTheCompleterAndTheManifest(t *testing.T) {
	t.Parallel()

	gate := readScriptFile(t, "check-guest-image.sh")
	for _, call := range []string{`check_components_left_out "$MNT"`, `check_completer "$MNT"`, `check_manifest_components`} {
		if !hasExactLine(gate, call) {
			t.Errorf("check-guest-image.sh never runs %s", call)
		}
	}
}

// gateRun runs the gate's named function against ROOT and MANIFEST, as the gate
// defines it, and returns its output and the FAILED it left.
func gateRun(t *testing.T, call, root, manifest string, functions ...string) (string, string) {
	t.Helper()

	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\nGATE_DIR=" + here + "\nMANIFEST=" + manifest + "\n" +
		"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
		gateArray(t, "COMPLETER_FILES") + "\n" + gateArray(t, "COMPLETER_EXECUTED") + "\n"
	for _, name := range functions {
		script += checkImageFunction(t, name) + "\n"
	}
	script += call + " \"$1\"\necho \"FAILED=$FAILED\"\n"
	path := filepath.Join(t.TempDir(), "gate.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "bash", path, root).CombinedOutput()
	if err != nil {
		t.Fatalf("the check did not run to its verdict: %v\n%s", err, out)
	}
	failed := "?"
	if at := strings.LastIndex(string(out), "FAILED="); at >= 0 {
		failed = strings.TrimSpace(string(out)[at+len("FAILED="):])
	}

	return string(out), failed
}

// THE GATE REFUSES AN IMAGE CARRYING ANY OF A COMPONENT, AND PASSES ONE CARRYING
// NONE, by asking check-guest-component.sh of every name it lists.
func TestTheGateRefusesAnImageCarryingAComponent(t *testing.T) {
	t.Parallel()

	if out, failed := gateRun(t, "check_components_left_out", completedTree(t, "none"), "",
		"check_components_left_out"); failed != "0" {
		t.Errorf("an image carrying no component failed:\n%s", out)
	}
	for _, name := range []string{"codeql", "firefox"} {
		out, failed := gateRun(t, "check_components_left_out", completedTree(t, name), "", "check_components_left_out")
		if failed != "1" || !strings.Contains(out, "the image carries part of "+name) {
			t.Errorf("an image carrying %s passed:\n%s", name, out)
		}
	}
}

// THE GATE REQUIRES THE COMPLETER, EVERY FILE OF IT, AND GITHUB'S TEST RUNNER
// READING THE TESTS BESIDE IT.
func TestTheGateRequiresTheCompleter(t *testing.T) {
	t.Parallel()

	installed := func(t *testing.T) string {
		t.Helper()

		root := t.TempDir()
		completer := filepath.Join(root, "opt", "billet", "complete")
		for _, file := range []string{"run", "write-image-env.sh", "pam-environment.awk", "installers/toolset.json",
			"scripts/build/install-android-sdk.sh", "scripts/build/install-codeql-bundle.sh",
			"scripts/build/install-firefox.sh", "scripts/build/install-google-chrome.sh",
			"scripts/build/install-microsoft-edge.sh",
			"scripts/helpers/install.sh", "scripts/helpers/etc-environment.sh", "scripts/helpers/os.sh",
			"scripts/helpers/invoke-tests.sh", "scripts/helpers/Common.Helpers.psm1",
			"scripts/tests/Android.Tests.ps1", "scripts/tests/Browsers.Tests.ps1", "scripts/tests/Toolset.Tests.ps1"} {
			writeTreeFile(t, completer, file, "x\n")
		}
		writeTreeFile(t, completer, "scripts/tests/Helpers.psm1",
			"    $testPath = Join-Path $PSScriptRoot \"${TestFile}.Tests.ps1\"\n")

		return root
	}

	if out, failed := gateRun(t, "check_completer", installed(t), "", "check_completer"); failed != "0" {
		t.Errorf("a whole completer failed:\n%s", out)
	}

	root := installed(t)
	removeAll(t, filepath.Join(root, "opt", "billet", "complete", "scripts", "build", "install-firefox.sh"))
	if out, failed := gateRun(t, "check_completer", root, "", "check_completer"); failed != "1" ||
		!strings.Contains(out, "lacks scripts/build/install-firefox.sh") {
		t.Errorf("a completer without Firefox's installer passed:\n%s", out)
	}

	root = installed(t)
	if err := os.Chmod(filepath.Join(root, "opt", "billet", "complete", "run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, failed := gateRun(t, "check_completer", root, "", "check_completer"); failed != "1" ||
		!strings.Contains(out, "run (executable)") {
		t.Errorf("a completer whose runner is not executable passed:\n%s", out)
	}

	root = installed(t)
	writeTreeFile(t, filepath.Join(root, "opt", "billet", "complete"), "scripts/tests/Helpers.psm1",
		"    $testPath = \"/imagegeneration/tests/${TestFile}.Tests.ps1\"\n")
	if out, failed := gateRun(t, "check_completer", root, "", "check_completer"); failed != "1" ||
		!strings.Contains(out, "still reads the tests from /imagegeneration") {
		t.Errorf("a completer reading the build's skipped tests passed:\n%s", out)
	}
}

// THE COMPLETER THE BUILD INSTALLS IS ONE THE GATE PASSES, modes and all: GitHub's
// sourced helpers are ordinary files in the template, and only what is executed
// has to be executable.
func TestTheGatePassesTheCompleterTheBuildInstalls(t *testing.T) {
	t.Parallel()

	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script := "#!/usr/bin/env bash\nset -euo pipefail\nSCRIPT_DIR=" + here + "\nCOMPONENTS='android-sdk codeql '\n" +
		"TOOLSET_FILE=" + filepath.Join(here, "..", "internal", "runnerimages", "toolset-2404.json") + "\n" +
		guestImageFunction(t, "install_completer") + "\ninstall_completer \"$1\"\n"
	path := filepath.Join(t.TempDir(), "install.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "bash", path, root).CombinedOutput(); err != nil {
		t.Fatalf("install_completer: %v\n%s", err, out)
	}
	if out, failed := gateRun(t, "check_completer", root, "", "check_completer"); failed != "0" {
		t.Errorf("the gate refuses the completer the build installs:\n%s", out)
	}
}

// THE GATE HOLDS THE MANIFEST TO WHAT THE IMAGE LEAVES OUT: every component, each
// with a positive whole space, under schema 3 and completion contract 1.
func TestTheGateHoldsTheManifestToWhatTheImageLeavesOut(t *testing.T) {
	t.Parallel()

	const all = `{"name":"android-sdk","space":1048576},{"name":"codeql","space":1048576},` +
		`{"name":"firefox","space":1048576},{"name":"google-chrome","space":1048576},` +
		`{"name":"microsoft-edge","space":1048576}`
	for name, tc := range map[string]struct {
		manifest, failed, says string
	}{
		"every component":    {`{"schema":3,"completion_contract":"1","components":[` + all + `]}`, "0", "declares every component"},
		"one missing":        {`{"schema":3,"completion_contract":"1","components":[{"name":"codeql","space":1}]}`, "1", "the image leaves out"},
		"none declared":      {`{"schema":2}`, "1", "the image leaves out"},
		"another contract":   {`{"schema":3,"completion_contract":"2","components":[` + all + `]}`, "1", "completion contract 1"},
		"a space of nothing": {`{"schema":3,"completion_contract":"1","components":[` + strings.Replace(all, `"space":1048576}`, `"space":0}`, 1) + `]}`, "1", "positive whole space"},
		"a fractional space": {`{"schema":3,"completion_contract":"1","components":[` + strings.Replace(all, `"space":1048576}`, `"space":1.5}`, 1) + `]}`, "1", "positive whole space"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := forkSafeWriteFile(path, []byte(tc.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			out, failed := gateRun(t, "check_manifest_components", t.TempDir(), path, "check_manifest_components")
			if failed != tc.failed || !strings.Contains(out, tc.says) {
				t.Errorf("FAILED=%s, want %s saying %q:\n%s", failed, tc.failed, tc.says, out)
			}
		})
	}
}

// gateArray is the gate's top-level array NAME, from its `NAME=(` to its `)`.
func gateArray(t *testing.T, name string) string {
	t.Helper()

	source := readScriptFile(t, "check-guest-image.sh")
	start := strings.Index(source, "\n"+name+"=(\n")
	if start < 0 {
		t.Fatalf("check-guest-image.sh has no top-level %s array", name)
	}
	end := strings.Index(source[start+1:], "\n)\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s", name)
	}

	return source[start+1 : start+1+end+2]
}

func removeAll(t *testing.T, path string) {
	t.Helper()

	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func writeTreeFile(t *testing.T, root, path, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkSafeWriteFile(filepath.Join(root, path), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// toolsetPathForTest is the pinned declaration, relative to scripts/.
const toolsetPathForTest = "../internal/runnerimages/toolset-2404.json"

// readScriptFile is a file under scripts/, or the test fails.
func readScriptFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}

// shellQuote single-quotes s for a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readBuildScript returns the guest build script.
func readBuildScript(t *testing.T) string {
	t.Helper()

	return readScriptFile(t, "build-guest-image.sh")
}

// guestImageFunction returns one shell function from build-guest-image.sh, from
// its definition to the brace that closes it.
func guestImageFunction(t *testing.T, name string) string {
	t.Helper()

	source := readScriptFile(t, "build-guest-image.sh")

	start := strings.Index(source, name+"() {")
	if start < 0 {
		t.Fatalf("build-guest-image.sh has no %s function", name)
	}

	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s in build-guest-image.sh", name)
	}

	return source[start : start+end+2]
}
