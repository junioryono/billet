package guestassets_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nodeOrSkip is the runner's own node stood in for by the host's; the hook is
// plain JavaScript with no dependencies, so any recent node runs it.
func nodeOrSkip(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed on this development host")
	}
	return node
}

// hookHarness copies the wrapper beside a fake reference hook that records the
// request it was handed and exits with a chosen status, and creates the shim
// file the wrapper looks for.
func hookHarness(t *testing.T, upstreamStatus string) (index, record string) {
	t.Helper()
	dir := t.TempDir()
	body, err := os.ReadFile("container-hook.js")
	if err != nil {
		t.Fatal(err)
	}
	record = filepath.Join(dir, "record.json")
	upstream := "const fs=require('fs');let d='';process.stdin.on('data',c=>d+=c);" +
		"process.stdin.on('end',()=>{fs.writeFileSync(" + jsString(record) + ",d);process.exit(" + upstreamStatus + ");});"
	if err := os.WriteFile(filepath.Join(dir, "upstream.js"), []byte(upstream), 0o644); err != nil {
		t.Fatal(err)
	}
	index = filepath.Join(dir, "index.js")
	if err := os.WriteFile(index, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return index, record
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func runHook(t *testing.T, node, index string, request map[string]any, shim string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(request)
	cmd := exec.CommandContext(t.Context(), node, index)
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Env = append(os.Environ(), "BILLET_TEST_SHIM="+shim)
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run the hook: %v\n%s", err, out)
	}
	return code, out
}

// The wrapper adds exactly one system mount, the shim read-only at its own path,
// and hands the reference hook everything else untouched.
func TestTheContainerHookMountsTheShimIntoTheJobContainer(t *testing.T) {
	t.Parallel()
	node := nodeOrSkip(t)
	index, record := hookHarness(t, "0")
	shim := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n# billet-docker-shim\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim's production path is fixed in the wrapper; a test points it at a
	// file it made by editing the constant in its private copy.
	patchShimPath(t, index, shim)

	request := map[string]any{
		"command":      "prepare_job",
		"responseFile": "/tmp/x.json",
		"args": map[string]any{
			"container": map[string]any{
				"image": "docker:27-cli",
				"systemMountVolumes": []any{
					map[string]any{"sourceVolumePath": "/var/run/docker.sock", "targetVolumePath": "/var/run/docker.sock", "readOnly": false},
				},
			},
			"services": []any{},
		},
	}
	code, out := runHook(t, node, index, request, shim)
	if code != 0 {
		t.Fatalf("hook exited %d\n%s", code, out)
	}
	forwarded := readForwarded(t, record)
	mounts := forwarded["args"].(map[string]any)["container"].(map[string]any)["systemMountVolumes"].([]any)
	if len(mounts) != 2 {
		t.Fatalf("forwarded %d system mounts, want the socket and the shim: %v", len(mounts), mounts)
	}
	last := mounts[1].(map[string]any)
	if last["sourceVolumePath"] != shim || last["targetVolumePath"] != shim || last["readOnly"] != true {
		t.Errorf("the shim mount is %v, want %s read-only at its own path", last, shim)
	}
	if forwarded["responseFile"] != "/tmp/x.json" || forwarded["command"] != "prepare_job" {
		t.Errorf("the rest of the request did not arrive untouched: %v", forwarded)
	}
}

// NOTHING IS ADDED WHEN THERE IS NOTHING TO ADD: a cleanup, a job with no
// container, or a host whose shim is absent all reach the reference hook with
// the request exactly as it arrived. A mount of a missing file would make docker
// create a directory under that name in the container.
func TestTheContainerHookLeavesOtherRequestsAlone(t *testing.T) {
	t.Parallel()
	node := nodeOrSkip(t)

	for _, tc := range []struct {
		name    string
		request map[string]any
		shim    bool
	}{
		{"cleanup", map[string]any{"command": "cleanup_job", "args": map[string]any{}}, true},
		{"no job container", map[string]any{"command": "prepare_job", "args": map[string]any{"services": []any{}}}, true},
		{"shim absent", map[string]any{"command": "prepare_job", "args": map[string]any{"container": map[string]any{"image": "x"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index, record := hookHarness(t, "0")
			shim := filepath.Join(t.TempDir(), "docker")
			if tc.shim {
				if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			patchShimPath(t, index, shim)
			if code, out := runHook(t, node, index, tc.request, shim); code != 0 {
				t.Fatalf("hook exited %d\n%s", code, out)
			}
			forwarded := readForwarded(t, record)
			want, _ := json.Marshal(tc.request)
			got, _ := json.Marshal(forwarded)
			if string(got) != string(want) {
				t.Errorf("forwarded %s\nwant   %s", got, want)
			}
		})
	}
}

// The reference hook's verdict is the wrapper's verdict.
func TestTheContainerHookPropagatesTheReferenceHooksExitStatus(t *testing.T) {
	t.Parallel()
	node := nodeOrSkip(t)
	index, _ := hookHarness(t, "3")
	patchShimPath(t, index, filepath.Join(t.TempDir(), "absent"))
	if code, _ := runHook(t, node, index, map[string]any{"command": "cleanup_job", "args": map[string]any{}}, ""); code != 3 {
		t.Fatalf("hook exited %d, want the reference hook's 3", code)
	}
}

func patchShimPath(t *testing.T, index, shim string) {
	t.Helper()
	body, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	const production = "const SHIM = '/opt/billet/bin/docker';"
	if !strings.Contains(string(body), production) {
		t.Fatalf("the wrapper no longer declares %s; update this test with it", production)
	}
	patched := strings.Replace(string(body), production, "const SHIM = "+jsString(shim)+";", 1)
	if err := os.WriteFile(index, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readForwarded(t *testing.T, record string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the reference hook was never handed a request: %v", err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatalf("the reference hook was handed something that is not JSON: %v\n%s", err, body)
	}
	return forwarded
}
