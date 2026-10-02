package guestassets_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func runHook(t *testing.T, node, index string, request map[string]any, env ...string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), node, index)
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Env = append(withoutHookInputs(os.Environ()), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run the hook: %v\n%s", err, out)
	}
	return code, out
}

// hookInputs are the variables container-hook.js reads. A test sets the ones it
// means; the ambient value of any of them would be a second, unseen input.
var hookInputs = []string{
	"BILLET_CONTAINER_SHIM", "GOCACHEPROG", "GOFLAGS", "BILLET_CACHE_ENDPOINT", "BILLET_CACHE_TOKEN",
}

// withoutHookInputs is the environment with every hook input removed.
//
// REMOVED, NOT INHERITED, because these tests run inside billet's own guests:
// since guest-20261001-030940 a CI guest carries the Go cache configuration, so
// an inherited GOCACHEPROG made the hook add the cache helper's mount to every
// request and the tests that count mounts failed (2026-10-02).
func withoutHookInputs(environ []string) []string {
	out := make([]string, 0, len(environ))

	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(hookInputs, name) {
			out = append(out, kv)
		}
	}

	return out
}

// The ambient environment must not decide what the hook does.
func TestTheHookHarnessRemovesEveryHookInput(t *testing.T) {
	t.Parallel()

	environ := []string{"PATH=/bin", "GOCACHEPROG=/x cache gocacheprog", "GOFLAGS=-count=1",
		"BILLET_CACHE_ENDPOINT=http://h", "BILLET_CACHE_TOKEN=t", "BILLET_CONTAINER_SHIM=1"}

	if got := withoutHookInputs(environ); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Fatalf("withoutHookInputs left %v, want only PATH", got)
	}

	body, err := os.ReadFile("container-hook.js")
	if err != nil {
		t.Fatal(err)
	}

	// EVERY VARIABLE THE HOOK NAMES is one this harness removes, so a new input
	// added to the hook cannot leak back in unnoticed.
	source := string(body)

	// A COMPUTED READ cannot be checked by name. The one the hook has, env[name]
	// inside `for (const name of GO_CACHE_ENV)`, reads exactly that list, which
	// is parsed below; any other is refused.
	for _, m := range regexp.MustCompile(`env\[([^'"\]][^\]]*)\]`).FindAllStringSubmatch(source, -1) {
		if m[1] != "name" || !strings.Contains(source, "for (const name of GO_CACHE_ENV)") {
			t.Fatalf("container-hook.js reads env[%s], a computed name this test cannot check", m[1])
		}
	}

	var read []string

	for _, m := range regexp.MustCompile(`env\.([A-Za-z_][A-Za-z0-9_]*)|env\[['"]([^'"]+)['"]\]`).
		FindAllStringSubmatch(source, -1) {
		read = append(read, m[1]+m[2])
	}

	list := regexp.MustCompile(`GO_CACHE_ENV = \[([^\]]*)\]`).FindStringSubmatch(source)
	if list == nil {
		t.Fatal("container-hook.js no longer declares GO_CACHE_ENV; update this test's reading of its inputs")
	}

	for _, m := range regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(list[1], -1) {
		read = append(read, m[1])
	}

	slices.Sort(read)
	read = slices.Compact(read)

	want := slices.Clone(hookInputs)
	slices.Sort(want)

	if !slices.Equal(read, want) {
		t.Fatalf("container-hook.js reads %v; the harness removes %v. They must be the same set", read, want)
	}
}

// The wrapper adds exactly one system mount, the shim read-only at its own path,
// and hands the reference hook everything else untouched.
func TestTheContainerHookMountsTheShimIntoTheJobContainer(t *testing.T) {
	t.Parallel()
	node := nodeOrSkip(t)
	index, record := hookHarness(t, "0")
	shim := filepath.Join(t.TempDir(), "docker")
	if err := forkSafeWriteFile(shim, []byte("#!/bin/sh\n# billet-docker-shim\n"), 0o755); err != nil {
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
	code, out := runHook(t, node, index, request, "BILLET_CONTAINER_SHIM=1")
	if code != 0 {
		t.Fatalf("hook exited %d\n%s", code, out)
	}
	var forwarded struct {
		Command      string `json:"command"`
		ResponseFile string `json:"responseFile"`
		Args         struct {
			Container struct {
				SystemMountVolumes []struct {
					Source   string `json:"sourceVolumePath"`
					Target   string `json:"targetVolumePath"`
					ReadOnly bool   `json:"readOnly"`
				} `json:"systemMountVolumes"`
			} `json:"container"`
		} `json:"args"`
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the reference hook was never handed a request: %v", err)
	}
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatalf("the reference hook was handed something that is not JSON: %v\n%s", err, body)
	}
	mounts := forwarded.Args.Container.SystemMountVolumes
	if len(mounts) != 2 {
		t.Fatalf("forwarded %d system mounts, want the socket and the shim: %v", len(mounts), mounts)
	}
	last := mounts[1]
	if last.Source != shim || last.Target != shim || !last.ReadOnly {
		t.Errorf("the shim mount is %+v, want %s read-only at its own path", last, shim)
	}
	if forwarded.ResponseFile != "/tmp/x.json" || forwarded.Command != "prepare_job" {
		t.Errorf("the rest of the request did not arrive untouched: %+v", forwarded)
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
			t.Parallel()
			index, record := hookHarness(t, "0")
			shim := filepath.Join(t.TempDir(), "docker")
			if tc.shim {
				if err := forkSafeWriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			patchShimPath(t, index, shim)
			if code, out := runHook(t, node, index, tc.request, "BILLET_CONTAINER_SHIM=1"); code != 0 {
				t.Fatalf("hook exited %d\n%s", code, out)
			}
			forwarded := readForwarded(t, record)
			want, err := json.Marshal(tc.request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(forwarded)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
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
	if code, _ := runHook(t, node, index, map[string]any{"command": "cleanup_job", "args": map[string]any{}}); code != 3 {
		t.Fatalf("hook exited %d, want the reference hook's 3", code)
	}
}

func patchShimPath(t *testing.T, index, shim string) {
	t.Helper()
	patchConstant(t, index, "SHIM", "/opt/billet/bin/docker", shim)
}

// patchConstant points one of the wrapper's fixed production paths at a file
// the test made, in the test's private copy.
func patchConstant(t *testing.T, index, name, production, replacement string) {
	t.Helper()
	body, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	declared := "const " + name + " = '" + production + "';"
	if !strings.Contains(string(body), declared) {
		t.Fatalf("the wrapper no longer declares %s; update this test with it", declared)
	}
	patched := strings.Replace(string(body), declared, "const "+name+" = "+jsString(replacement)+";", 1)
	if err := os.WriteFile(index, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A TIER WITHOUT THE GO CACHE GETS NEITHER THE HELPER NOR ITS ENVIRONMENT, even
// on an image that carries billet.
func TestTheContainerHookAddsNothingForATierWithoutTheGoCache(t *testing.T) {
	t.Parallel()
	node := nodeOrSkip(t)
	index, record := hookHarness(t, "0")
	billet := filepath.Join(t.TempDir(), "billet")
	if err := forkSafeWriteFile(billet, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	patchShimPath(t, index, filepath.Join(t.TempDir(), "absent"))
	patchConstant(t, index, "BILLET", "/opt/billet/bin/billet", billet)
	request := map[string]any{
		"command": "prepare_job",
		"args":    map[string]any{"container": map[string]any{"image": "golang:1.26"}},
	}
	if code, out := runHook(t, node, index, request, "GOCACHEPROG=", "BILLET_CACHE_TOKEN=token"); code != 0 {
		t.Fatalf("hook exited %d\n%s", code, out)
	}
	container := field[map[string]any](t, field[map[string]any](t, readForwarded(t, record), "args"), "container")
	if len(container) != 1 {
		t.Fatalf("a tier without the go cache had its container changed: %v", container)
	}
}

// A GO CACHE TIER'S JOB CONTAINER GETS THE HELPER AND ITS ENVIRONMENT, and a
// job that set GOCACHEPROG itself keeps its own value, which is how a workflow
// turns the cache off. Without interception the docker shim is not mounted.
func TestTheContainerHookGivesAJobContainerTheGoCacheHelper(t *testing.T) {
	t.Parallel()
	node := nodeOrSkip(t)

	for name, own := range map[string]map[string]any{
		"the job sets nothing":         {},
		"the job turned the cache off": {"GOCACHEPROG": ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			index, record := hookHarness(t, "0")
			dir := t.TempDir()
			shim, billet := filepath.Join(dir, "docker"), filepath.Join(dir, "billet")
			for _, file := range []string{shim, billet} {
				if err := forkSafeWriteFile(file, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			patchShimPath(t, index, shim)
			patchConstant(t, index, "BILLET", "/opt/billet/bin/billet", billet)

			request := map[string]any{
				"command": "prepare_job",
				"args": map[string]any{"container": map[string]any{
					"image": "golang:1.26", "environmentVariables": own,
				}},
			}
			code, out := runHook(t, node, index, request,
				"GOCACHEPROG="+billet+" cache gocacheprog", "GOFLAGS=-count=1",
				"BILLET_CACHE_ENDPOINT=http://172.31.0.1:7718", "BILLET_CACHE_TOKEN=token",
				"BILLET_CONTAINER_SHIM=")
			if code != 0 {
				t.Fatalf("hook exited %d\n%s", code, out)
			}
			container := field[map[string]any](t,
				field[map[string]any](t, readForwarded(t, record), "args"), "container")
			mounts := field[[]any](t, container, "systemMountVolumes")
			if len(mounts) != 1 {
				t.Fatalf("mounts = %v, want only the helper, read-only", mounts)
			}
			mount, ok := mounts[0].(map[string]any)
			if !ok || mount["sourceVolumePath"] != billet || mount["readOnly"] != true {
				t.Fatalf("mounts = %v, want only the helper, read-only", mounts)
			}
			variables := field[map[string]any](t, container, "environmentVariables")
			wantHelper := billet + " cache gocacheprog"
			if _, turnedOff := own["GOCACHEPROG"]; turnedOff {
				wantHelper = ""
			}
			for key, want := range map[string]string{
				"GOCACHEPROG": wantHelper, "GOFLAGS": "-count=1",
				"BILLET_CACHE_ENDPOINT": "http://172.31.0.1:7718", "BILLET_CACHE_TOKEN": "token",
			} {
				if variables[key] != want {
					t.Errorf("%s = %v, want %q", key, variables[key], want)
				}
			}
		})
	}
}

// field is object[key] as a T, failing the test when it is absent or not one.
func field[T any](t *testing.T, object map[string]any, key string) T {
	t.Helper()

	value, ok := object[key].(T)
	if !ok {
		t.Fatalf("%s is %T, not the %T the hook forwards: %v", key, object[key], value, object)
	}

	return value
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
