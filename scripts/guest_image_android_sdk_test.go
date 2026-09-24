package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE GUEST CARRIES THE ANDROID SDK, AND THE GATE REFUSES ONE THAT DOES NOT (#209).
//
// An Android build moved onto the fleet failed with "SDK location not found" on a
// job that runs only when app code changes, days after its runs-on moved.
func TestTheGateRefusesAnImageWithoutTheAndroidSDK(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                 string
		sdkmanager, platform bool
		env                  string
		wantFailed           string
	}{
		{"installed", true, true, "ANDROID_HOME=/usr/local/lib/android/sdk\n", "0"},
		{"no sdkmanager", false, true, "ANDROID_HOME=/usr/local/lib/android/sdk\n", "1"},
		{"no platform", true, false, "ANDROID_HOME=/usr/local/lib/android/sdk\n", "1"},
		{"no ANDROID_HOME", true, true, "JAVA_HOME=/usr/lib/jvm/x\n", "1"},
		{"ANDROID_HOME elsewhere", true, true, "ANDROID_HOME=/opt/android\n", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			sdk := filepath.Join(root, "usr", "local", "lib", "android", "sdk")
			bin := filepath.Join(sdk, "cmdline-tools", "latest", "bin")

			for _, dir := range []string{bin, filepath.Join(sdk, "platforms"), filepath.Join(root, "etc")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			if tc.sdkmanager {
				if err := os.WriteFile(filepath.Join(bin, "sdkmanager"), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			if tc.platform {
				if err := os.MkdirAll(filepath.Join(sdk, "platforms", "android-35"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			if err := os.WriteFile(filepath.Join(root, "etc", "billet-image-env"), []byte(tc.env), 0o644); err != nil {
				t.Fatal(err)
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\n" +
				"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
				checkImageFunction(t, "check_android_sdk") + "\n" +
				"check_android_sdk \"$1\"\necho \"FAILED=$FAILED\"\n"

			path := filepath.Join(t.TempDir(), "gate.sh")
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			out, err := exec.CommandContext(t.Context(), "bash", path, root).CombinedOutput()
			if err != nil {
				t.Fatalf("the check did not run to its verdict: %v\n%s", err, out)
			}

			if !strings.Contains(string(out), "FAILED="+tc.wantFailed+"\n") {
				t.Errorf("want FAILED=%s:\n%s", tc.wantFailed, out)
			}
		})
	}
}

// AND THE GATE ASKS IT, and the build accepts the licences at the one call that
// installs the toolcache: a switch set anywhere else installs nothing.
func TestTheGuestBuildInstallsAndTheGateAsksForTheAndroidSDK(t *testing.T) {
	t.Parallel()

	if !hasExactLine(readScriptFile(t, "check-guest-image.sh"), `check_android_sdk "$MNT"`) {
		t.Error(`check-guest-image.sh never runs check_android_sdk "$MNT", so an image without the SDK passes`)
	}

	source := readScriptFile(t, "build-guest-image.sh")

	call := strings.LastIndex(source, "\n\t\tbillet_install_toolcache\n")
	if call < 0 {
		t.Fatal("build-guest-image.sh has no billet_install_toolcache call, so this test is reading the wrong script")
	}

	start := strings.LastIndex(source[:call], "\tBILLET_TC_ROOT=")
	if start < 0 || !strings.Contains(source[start:call], "\t\tBILLET_TC_ANDROID_ACCEPT_LICENSES=yes \\") {
		t.Error("the toolcache call in build-guest-image.sh does not set BILLET_TC_ANDROID_ACCEPT_LICENSES=yes, " +
			"so install_android installs nothing")
	}
}
