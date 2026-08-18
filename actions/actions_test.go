package actions_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type action struct {
	Name string `yaml:"name"`
	Runs struct {
		Using string `yaml:"using"`
		Main  string `yaml:"main"`
		Post  string `yaml:"post"`
		Steps []struct {
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"runs"`
}

func TestEveryActionMetadataNamesRunnableFiles(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dir := entry.Name()
		t.Run(dir, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(dir, "action.yml"))
			if err != nil {
				t.Fatalf("read action.yml: %v", err)
			}

			var metadata action
			if err := yaml.Unmarshal(body, &metadata); err != nil {
				t.Fatalf("parse action.yml: %v", err)
			}
			if metadata.Name == "" || metadata.Runs.Using == "" {
				t.Fatalf("action has no name or runtime: %+v", metadata)
			}

			for _, script := range []string{metadata.Runs.Main, metadata.Runs.Post} {
				if script == "" {
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, script)); err != nil {
					t.Errorf("%s does not exist: %v", script, err)
				}
			}
		})
	}
}

func TestJavaScriptActionsParseOnTheRunnersNodeRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed on this build host")
	}

	scripts, err := filepath.Glob("*/*.js")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	for _, script := range scripts {
		t.Run(script, func(t *testing.T) {
			if output, err := exec.CommandContext(t.Context(), node, "--check", script).CombinedOutput(); err != nil {
				t.Fatalf("node --check: %v\n%s", err, output)
			}
		})
	}
}

func TestTheBuilderUsesUpstreamBuildPushRatherThanVendoringIt(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("build-push-action", "action.yml"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !strings.Contains(string(body),
		"uses: docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a # v7") {
		t.Fatal("the wrapper no longer delegates to the reviewed immutable upstream build action")
	}
}

func TestBuilderCleanupReceivesEveryHyphenatedStateName(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed on this build host")
	}
	state := filepath.Join(t.TempDir(), "github-state")
	cmd := exec.CommandContext(t.Context(), node, filepath.Join("stop-docker-builder", "index.js"))
	cmd.Env = append(os.Environ(),
		"GITHUB_STATE="+state,
		"INPUT_CONTAINER=container",
		"INPUT_BUILDER=builder",
		"INPUT_STATE-PATH=/state",
		"INPUT_MOUNT-LIMIT-BYTES=4096",
		"INPUT_DISCARD-MARKER=/discard",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("record cleanup state: %v\n%s", err, output)
	}
	body, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("read cleanup state: %v", err)
	}
	for _, want := range []string{
		"state_path=/state\n", "mount_limit_bytes=4096\n", "discard_marker=/discard\n",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("cleanup state does not contain %q:\n%s", want, body)
		}
	}
}

func TestTheBuilderStarterIsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no executable mode bit")
	}

	info, err := os.Stat(filepath.Join("setup-docker-builder", "start.sh"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("start.sh is not executable")
	}
}

func TestBuilderCleanupReportsGrowthAndPrunesAnOversizedMount(t *testing.T) {
	t.Parallel()

	statePath := t.TempDir()
	discardMarker := filepath.Join(t.TempDir(), "billet-buildkit-discard")
	if err := os.WriteFile(discardMarker, nil, 0o600); err != nil {
		t.Fatalf("write publication guard: %v", err)
	}
	if err := os.WriteFile(filepath.Join(statePath, ".billet-buildkit-cachemounts.json"),
		[]byte(`{"cached mount /root/.cache from buildkit":{"size":1024}}`), 0o600); err != nil {
		t.Fatalf("write previous usage: %v", err)
	}

	output, calls := runBuilderCleanup(t, statePath, discardMarker, false, false)
	if !strings.Contains(output, "cached mount /root/.cache from buildkit") ||
		!strings.Contains(output, "grew 2 KiB") {
		t.Errorf("cleanup did not report per-mount growth:\n%s", output)
	}
	if !strings.Contains(calls, "prune --filter id==mount-1") {
		t.Errorf("cleanup did not prune the exact oversized record:\n%s", calls)
	}
	if strings.Contains(calls, "prune --filter id==mount-2") {
		t.Errorf("cleanup pruned a mount that stayed below its ceiling:\n%s", calls)
	}
	if _, err := os.Stat(discardMarker); !os.IsNotExist(err) {
		t.Errorf("successful enforcement left a discard marker: %v", err)
	}
}

func TestBuilderCleanupDiscardsThePublicationWhenACeilingCannotBeEnforced(t *testing.T) {
	t.Parallel()

	statePath := t.TempDir()
	discardMarker := filepath.Join(t.TempDir(), "billet-buildkit-discard")
	if err := os.WriteFile(discardMarker, nil, 0o600); err != nil {
		t.Fatalf("write publication guard: %v", err)
	}
	output, _ := runBuilderCleanup(t, statePath, discardMarker, true, false)
	if !strings.Contains(output, "will discard this cache update") {
		t.Errorf("cleanup did not explain the fail-safe discard:\n%s", output)
	}
	if _, err := os.Stat(discardMarker); err != nil {
		t.Fatalf("failed enforcement did not mark the sticky disk for discard: %v", err)
	}
}

func TestBuilderCleanupDiscardsThePublicationUntilTheContainerStops(t *testing.T) {
	t.Parallel()

	statePath := t.TempDir()
	discardMarker := filepath.Join(t.TempDir(), "billet-buildkit-discard")
	if err := os.WriteFile(discardMarker, nil, 0o600); err != nil {
		t.Fatalf("write publication guard: %v", err)
	}
	output, _ := runBuilderCleanup(t, statePath, discardMarker, false, true)
	if !strings.Contains(output, "stop the BuildKit container exited 1") {
		t.Errorf("cleanup did not explain why publication remained blocked:\n%s", output)
	}
	if _, err := os.Stat(discardMarker); err != nil {
		t.Fatalf("a running BuildKit container did not keep the publication guard: %v", err)
	}
}

func TestBuilderResetClearsOnlyTheMountedBuildKitState(t *testing.T) {
	t.Parallel()

	temporary := t.TempDir()
	statePath := filepath.Join(temporary, "billet-buildkit-state")
	if err := os.MkdirAll(filepath.Join(statePath, "nested"), 0o700); err != nil {
		t.Fatalf("create old state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "nested", "poisoned"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write old state: %v", err)
	}
	sibling := filepath.Join(temporary, "must-survive")
	if err := os.WriteFile(sibling, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	tools := t.TempDir()
	fakeDocker := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(tools, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	output := filepath.Join(temporary, "outputs")
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("setup-docker-builder", "start.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"RUNNER_TEMP="+temporary,
		"GITHUB_OUTPUT="+output,
		"GITHUB_RUN_ID=1",
		"GITHUB_RUN_ATTEMPT=1",
		"GITHUB_JOB=build",
		"BILLET_BUILDKIT_IMAGE=moby/buildkit:test",
		"BILLET_BUILDKIT_RESET=true",
	)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start reset builder: %v\n%s", err, combined)
	}
	if _, err := os.Stat(filepath.Join(statePath, "nested", "poisoned")); !os.IsNotExist(err) {
		t.Errorf("reset left old BuildKit state: %v", err)
	}
	if body, err := os.ReadFile(sibling); err != nil || string(body) != "keep" {
		t.Errorf("reset changed a sibling outside its mount: body %q error %v", body, err)
	}
}

func TestTheBuilderConfiguresEachUpstreamToItsOwnMirror(t *testing.T) {
	t.Parallel()

	temporary := t.TempDir()
	statePath := filepath.Join(temporary, "billet-buildkit-state")
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("create builder state: %v", err)
	}
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("setup-docker-builder", "start.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"RUNNER_TEMP="+temporary,
		"GITHUB_OUTPUT="+filepath.Join(temporary, "outputs"),
		"GITHUB_RUN_ID=1",
		"GITHUB_RUN_ATTEMPT=1",
		"GITHUB_JOB=build",
		"BILLET_BUILDKIT_IMAGE=moby/buildkit:test",
		"BILLET_BUILDKIT_RESET=false",
		`BILLET_REGISTRY_MIRRORS_JSON={"docker.io":"https://docker-cache.home.example","ghcr.io":"https://ghcr-cache.home.example","quay.io":"https://quay-cache.home.example"}`,
	)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start mirrored builder: %v\n%s", err, combined)
	}
	body, err := os.ReadFile(filepath.Join(temporary, "billet-buildkitd.toml"))
	if err != nil {
		t.Fatalf("read BuildKit config: %v", err)
	}
	for _, want := range []string{
		`[registry."docker.io"]`, `mirrors = ["docker-cache.home.example"]`,
		`[registry."ghcr.io"]`, `mirrors = ["ghcr-cache.home.example"]`,
		`[registry."quay.io"]`, `mirrors = ["quay-cache.home.example"]`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("BuildKit config does not contain %q:\n%s", want, body)
		}
	}
}

func TestTheTierMountLimitAndFailSafeDiscardAreWiredBetweenActions(t *testing.T) {
	t.Parallel()

	setup, err := os.ReadFile(filepath.Join("setup-docker-builder", "action.yml"))
	if err != nil {
		t.Fatalf("read setup action: %v", err)
	}
	if !strings.Contains(string(setup),
		"mount-limit-bytes: ${{ env.BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES }}") {
		t.Fatal("the tier's mount ceiling does not reach the cleanup action")
	}
	if strings.Count(string(setup), "discard-marker: ${{ runner.temp }}/billet-buildkit-discard") != 2 {
		t.Fatal("the builder and sticky-disk post steps do not share one runner-local discard marker")
	}

	cleanup, err := os.ReadFile(filepath.Join("stickydisk", "cleanup.js"))
	if err != nil {
		t.Fatalf("read sticky-disk cleanup: %v", err)
	}
	markerAt := bytes.Index(cleanup, []byte("STATE_discard_marker"))
	unmountAt := bytes.Index(cleanup, []byte(`["umount", target]`))
	if markerAt < 0 || unmountAt < 0 || markerAt >= unmountAt {
		t.Fatal("sticky-disk cleanup does not read the fail-safe marker before unmount hides it")
	}
	if !bytes.Contains(cleanup, []byte(`discard ? "discard" : "commit"`)) {
		t.Fatal("a failed BuildKit policy cannot discard the sticky-disk publication")
	}
	if !bytes.Contains(cleanup, []byte(`discard ? 2 * 60 * 1000 : 13 * 60 * 1000`)) {
		t.Fatal("the sticky-disk post request budgets commit time without delaying discard")
	}
	attach, err := os.ReadFile(filepath.Join("stickydisk", "index.js"))
	if err != nil {
		t.Fatalf("read sticky-disk attach: %v", err)
	}
	if !bytes.Contains(attach, []byte(`fs.writeFileSync(discardMarker`)) {
		t.Fatal("the sticky-disk action does not create its publication guard before the policy hook runs")
	}
	if !bytes.Contains(attach, []byte(`}, 13 * 60 * 1000);`)) {
		t.Fatal("sticky-disk attach does not leave the server time to compact legacy cache lineage")
	}
}

func runBuilderCleanup(
	t *testing.T,
	statePath, discardMarker string,
	failPrune, failRemove bool,
) (string, string) {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed on this build host")
	}

	tools := t.TempDir()
	calls := filepath.Join(tools, "calls")
	seen := filepath.Join(tools, "du-seen")
	before := filepath.Join(tools, "before.json")
	after := filepath.Join(tools, "after.json")
	usage := "[{\"id\":\"mount-1\",\"mutable\":true,\"inUse\":false,\"size\":3072,\"description\":\"cached mount /root/.cache from buildkit\",\"recordType\":\"exec.cachemount\"}," +
		"{\"id\":\"mount-2\",\"mutable\":true,\"inUse\":false,\"size\":512,\"description\":\"cached mount /go/pkg/mod from buildkit\",\"recordType\":\"exec.cachemount\"}]\n"
	if err := os.WriteFile(before, []byte(usage), 0o600); err != nil {
		t.Fatalf("write usage fixture: %v", err)
	}
	remaining := "[{\"id\":\"mount-2\",\"mutable\":true,\"inUse\":false,\"size\":512,\"description\":\"cached mount /go/pkg/mod from buildkit\",\"recordType\":\"exec.cachemount\"}]\n"
	if err := os.WriteFile(after, []byte(remaining), 0o600); err != nil {
		t.Fatalf("write post-prune fixture: %v", err)
	}

	fakeDocker := `#!/bin/sh
printf '%s\n' "$*" >> "$BILLET_FAKE_CALLS"
case " $* " in
  *" buildctl "*" du "*)
    if [ -f "$BILLET_FAKE_DU_SEEN" ]; then
      cat "$BILLET_FAKE_AFTER"
    else
      : > "$BILLET_FAKE_DU_SEEN"
      cat "$BILLET_FAKE_BEFORE"
    fi
    ;;
  *" buildctl "*" prune "*)
    if [ "$BILLET_FAKE_FAIL_PRUNE" = true ]; then
      echo refused >&2
      exit 1
    fi
    ;;
  *" rm --force "*)
    if [ "$BILLET_FAKE_FAIL_REMOVE" = true ]; then
      echo still-running >&2
      exit 1
    fi
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	fakeDU := "#!/bin/sh\nprintf '1.0K\\t%s\\n' \"$2\"\n"
	if err := os.WriteFile(filepath.Join(tools, "du"), []byte(fakeDU), 0o755); err != nil {
		t.Fatalf("write fake du: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), node, filepath.Join("stop-docker-builder", "cleanup.js"))
	cmd.Env = append(os.Environ(),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"STATE_container=builder-container",
		"STATE_builder=builder-name",
		"STATE_state_path="+statePath,
		"STATE_mount_limit_bytes=2048",
		"STATE_discard_marker="+discardMarker,
		"BILLET_FAKE_CALLS="+calls,
		"BILLET_FAKE_DU_SEEN="+seen,
		"BILLET_FAKE_BEFORE="+before,
		"BILLET_FAKE_AFTER="+after,
		"BILLET_FAKE_FAIL_PRUNE="+map[bool]string{true: "true", false: "false"}[failPrune],
		"BILLET_FAKE_FAIL_REMOVE="+map[bool]string{true: "true", false: "false"}[failRemove],
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("builder cleanup changed the job result: %v\n%s", err, output.String())
	}

	called, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read fake docker calls: %v", err)
	}

	return output.String(), string(called)
}
