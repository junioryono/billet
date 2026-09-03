package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// EVERY ANDROID PACKAGE NAME CONTAINS A SEMICOLON, AND A SHELL READS THAT AS A
// COMMAND SEPARATOR.
//
// (This file is not named ..._android_test.go, and that is not a style choice:
// `android` is a GOOS, so Go reads a trailing _android as a build constraint and
// compiles the file only for GOOS=android. Named that way it reported "no tests to
// run" on every real platform -- a test that cannot fail because it does not
// exist.)
//
// sdkmanager's identifiers are `platforms;android-34`, `extras;google;m2repository`
// and so on. Passed through `sh -c "... --install $want"` they are not arguments,
// they are a SCRIPT: a real build ran `sdkmanager --install platforms`, then tried
// to execute `android-34` as a command, and the console filled with
//
//	sh: 1: android-34: not found
//	sh: 1: android-35: not found
//	installing the android packages failed
//
// after four minutes of downloads. The fix is to invoke sdkmanager directly rather
// than through a shell, and what this asserts is that one argument arrives per
// declared package — which is the property that was violated, and which no amount
// of reading the string would have settled.
func TestEveryAndroidPackageArrivesAsOneArgument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	catalogue := filepath.Join(dir, "catalogue")

	// A sdkmanager THAT RECORDS ITS ARGUMENTS, one per line. `printf '%s\n' "$@"`
	// is the whole point: it distinguishes an argument containing a semicolon from
	// two commands separated by one, which is precisely what went wrong.
	sdk := filepath.Join(dir, "sdk")
	mgr := filepath.Join(sdk, "cmdline-tools", "latest", "bin", "sdkmanager")

	if err := os.MkdirAll(filepath.Dir(mgr), 0o755); err != nil {
		t.Fatalf("make the fake sdk: %v", err)
	}

	body := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		// --list is asked first and its answer is what the selection reads.
		"  *--list*)\n" +
		"    echo '  platforms;android-34 | 3 | Android SDK Platform 34'\n" +
		"    echo '  platforms;android-35 | 2 | Android SDK Platform 35'\n" +
		"    echo '  build-tools;34.0.0   | 1 | Android SDK Build-Tools'\n" +
		"    echo '  build-tools;35.0.0   | 1 | Android SDK Build-Tools'\n" +
		"    echo '  ndk;27.0.12077973    | 1 | NDK'\n" +
		"    echo '  ndk;28.0.12674087    | 1 | NDK'\n" +
		"    echo '  ndk;29.0.13113456    | 1 | NDK'\n" +
		"    echo '  cmake;3.31.5         | 1 | CMake'\n" +
		"    echo '  cmake;4.1.2          | 1 | CMake'\n" +
		"    echo '  extras;android;m2repository | 1 | Support Repo'\n" +
		"    echo '  extras;google;m2repository  | 1 | Google Repo'\n" +
		"    echo '  extras;google;google_play_services | 1 | Play Services'\n" +
		// A NAME WITH A GLOB IN IT, because that is the only kind that can catch an
		// unquoted expansion. Every other identifier here is inert under pathname
		// expansion, so a test built only from realistic names cannot tell a quoted
		// array from an unquoted one — the mutation survived exactly that way.
		//
		// Upstream does not publish this today. The declaration is pinned and
		// digest-verified, which proves where a string came from and says nothing
		// about whether it is safe shell syntax; that is the rule aptPackageName and
		// toolcacheName already state, and this is it applied to sdkmanager.
		"    echo '  extras;billet;glob*test | 1 | Deliberate glob'\n" +
		"    exit 0 ;;\n" +
		"  *--licenses*) exit 0 ;;\n" +
		"esac\n" +
		"printf '%s\\n' \"$@\" > " + argv + "\n" +
		// AND IT REFUSES A PACKAGE IT DOES NOT PUBLISH. Without this the fake accepts
		// anything, so the test cannot tell a selection built from the catalogue from
		// one invented out of the declaration — and asking sdkmanager for a package it
		// has never heard of is a real failure a real build meets twenty minutes into
		// a download.
		//
		// A CATALOGUE FILE AND AN EXACT-MATCH LOOKUP, not a `case` pattern. The first
		// version listed the packages as case alternatives split across lines, which
		// is a shell syntax error (`|` does not continue a pattern list), and every
		// name in it needed its semicolons escaped — the very characters under test.
		"for a in \"$@\"; do\n" +
		"  [ \"$a\" = --install ] && continue\n" +
		"  grep -qxF \"$a\" " + catalogue + " || { echo \"not in catalogue: $a\" >&2; exit 1; }\n" +
		"done\n" +
		"exit 0\n"

	// THE SAME PACKAGES THE FAKE'S --list ANSWERS WITH. One list, so a fixture that
	// publishes one set and accepts another cannot exist.
	if err := os.WriteFile(catalogue, []byte(strings.Join([]string{
		"platforms;android-34", "platforms;android-35",
		"build-tools;34.0.0", "build-tools;35.0.0",
		"ndk;27.0.12077973", "ndk;28.0.12674087", "ndk;29.0.13113456",
		"cmake;3.31.5", "cmake;4.1.2",
		"extras;android;m2repository", "extras;google;m2repository",
		"extras;google;google_play_services", "extras;billet;glob*test",
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write the fake catalogue: %v", err)
	}

	if err := os.WriteFile(mgr, []byte(body), 0o700); err != nil {
		t.Fatalf("write the fake sdkmanager: %v", err)
	}

	toolset := filepath.Join(dir, "toolset.json")
	decl := `{"java":{"default":"17","versions":["17"]},
	  "android":{"cmdline-tools":"tools.zip","platform_min_version":"34",
	  "build_tools_min_version":"34.0.0","extra_list":["android;m2repository",
	  "google;m2repository","google;google_play_services","billet;glob*test"],
	  "additional_tools":["cmake;3.31.5","cmake;4.1.2"],
	  "ndk":{"default":"27","versions":["27","28","29"]}}}`

	if err := os.WriteFile(toolset, []byte(decl), 0o600); err != nil {
		t.Fatalf("write the declaration: %v", err)
	}

	env := filepath.Join(dir, "env")
	work := filepath.Join(dir, "work")

	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("make the work dir: %v", err)
	}

	// THE DOWNLOAD AND THE UNPACK ARE SHORT-CIRCUITED, not faked in detail: what
	// is under test is how the selected package list is HANDED to sdkmanager, and
	// a fixture that also had to serve a real zip would be testing curl.
	harness := "#!/usr/bin/env bash\nset -Eeuo pipefail\n" +
		"BILLET_TC_TOOLSET=" + toolset + "\n" +
		"BILLET_TC_ENV_FILE=" + env + "\n" +
		"BILLET_TC_WORK=" + work + "\n" +
		"BILLET_TC_DPKG=amd64\n" +
		"BILLET_TC_ANDROID_ACCEPT_LICENSES=yes\n" +
		"curl() { : ; }\n" +
		"billet_tc_run() {\n" +
		"  case \"$1\" in\n" +
		"    test) return 0 ;;\n" +
		"    mkdir|rm|mv|unzip) return 0 ;;\n" +
		"  esac\n" +
		"  \"$@\"\n" +
		"}\n" +
		guestImageFunction(t, "install_android") + "\n" +
		// The installer looks for sdkmanager under a fixed root; point it here.
		"install_android\n"

	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte(
		strings.Replace(harness, "local root=/usr/local/lib/android/sdk",
			"local root="+sdk, 1)), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	// THE ROOT IS REWRITTEN IN THE EXTRACTED FUNCTION, since it is a literal in
	// the installer rather than a parameter.
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read the harness: %v", err)
	}

	if err := os.WriteFile(script, []byte(strings.ReplaceAll(string(raw),
		"/usr/local/lib/android/sdk", sdk)), 0o700); err != nil {
		t.Fatalf("rewrite the harness: %v", err)
	}

	// A DECOY THE GLOB WOULD MATCH, IN THE DIRECTORY THE SCRIPT RUNS FROM.
	//
	// An unquoted expansion of `extras;billet;glob*test` is a no-op unless something
	// in the working directory matches it -- bash leaves an unmatched pattern
	// literal -- so without this file the quoted and unquoted forms behave
	// identically and the mutation survives. With it, an unquoted array turns the
	// declared package into this filename, which is precisely the substitution the
	// quoting prevents.
	if err := os.WriteFile(filepath.Join(dir, "extras;billet;globDECOYtest"),
		[]byte(""), 0o600); err != nil {
		t.Fatalf("write the glob decoy: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", script)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install_android: %v\n%s", err, out)
	}

	recorded, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("sdkmanager was never asked to install anything: %v\n%s", err, out)
	}

	args := strings.Split(strings.TrimSpace(string(recorded)), "\n")

	// THE COMPLETE SET, NOT A SAMPLE. Asserting seven of twelve let five packages
	// disappear with the test still green — two NDKs, a CMake and two extras — which
	// is the same partial-coverage shape as a gate that iterates what exists rather
	// than what was promised.
	//
	// EVERY NAME IS WHOLE. A name split at its semicolon arrives as a truncated
	// argument (`platforms` alone) and the pieces after it never arrive at all,
	// because the shell was running them.
	want := []string{
		"--install",
		"platforms;android-34",
		"platforms;android-35",
		"build-tools;34.0.0",
		"build-tools;35.0.0",
		"ndk;27.0.12077973",
		"ndk;28.0.12674087",
		"ndk;29.0.13113456",
		"cmake;3.31.5",
		"cmake;4.1.2",
		"extras;android;m2repository",
		"extras;google;m2repository",
		"extras;google;google_play_services",
		"extras;billet;glob*test",
	}

	got := map[string]int{}
	for _, a := range args {
		got[a]++
	}

	for _, w := range want {
		switch got[w] {
		case 1:
			delete(got, w)
		case 0:
			t.Errorf("sdkmanager was not asked for %q\nit received:\n  %s",
				w, strings.Join(args, "\n  "))
		default:
			t.Errorf("sdkmanager was asked for %q %d times", w, got[w])
			delete(got, w)
		}
	}

	// AND NOTHING ELSE. A leftover here is either a truncated name — `platforms` on
	// its own is exactly what the semicolon bug produced — or a package the
	// selection invented.
	for extra, n := range got {
		t.Errorf("sdkmanager received the unexpected argument %q (%d times); a bare "+
			"category name means a package was cut at its first semicolon\n"+
			"it received:\n  %s", extra, n, strings.Join(args, "\n  "))
	}
}
