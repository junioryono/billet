package gocacheprog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain lets the test binary be the GOCACHEPROG a real go command runs.
func TestMain(m *testing.M) {
	if os.Getenv("BILLET_GOCACHEPROG_TEST_HELPER") == "1" {
		err := Run(context.Background(), Config{
			Dir:      os.Getenv("BILLET_GOCACHE_DIR"),
			Endpoint: os.Getenv("BILLET_CACHE_ENDPOINT"),
			Token:    os.Getenv("BILLET_CACHE_TOKEN"),
			Log:      os.Stderr,
		}, os.Stdin, os.Stdout)
		if err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeNode is the node's content-addressed cache, in memory, counting what it
// served.
type fakeNode struct {
	mu      sync.Mutex
	objects map[string][]byte
	refuse  bool
	casHits atomic.Int64
	puts    atomic.Int64
	// redone counts action results with an output put that the node already
	// held: work a served build should not have repeated. The go command
	// rewrites some empty marker entries on every build, and those are not work.
	redone atomic.Int64
}

func (f *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer token" {
		http.Error(w, "unauthorised", http.StatusUnauthorized)

		return
	}
	if f.refuse {
		http.Error(w, "off", http.StatusForbidden)

		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/v1/cas/go/")
	if !ok {
		http.NotFound(w, r)

		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		body, found := f.objects[key]
		if !found {
			http.NotFound(w, r)

			return
		}
		if strings.HasPrefix(key, "cas/") {
			f.casHits.Add(1)
		}
		if _, err := w.Write(body); err != nil {
			return
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable", http.StatusBadRequest)

			return
		}
		if _, held := f.objects[key]; held && strings.HasPrefix(key, "ac/") &&
			!bytes.Contains(body, []byte(digest(nil))) {
			f.redone.Add(1)
		}
		f.objects[key] = body
		f.puts.Add(1)
	}
}

// probeModule is a small main package; the same directory is built twice,
// because the directory is part of the go command's action ids.
func probeModule(t *testing.T) string {
	t.Helper()

	module := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":  "module example.com/probe\n\ngo 1.24\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\nfunc main() { fmt.Println(strings.ToUpper(\"probe\")) }\n",
	} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return module
}

func build(t *testing.T, node *httptest.Server, module, dir string) {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(t.TempDir(), "probe"), ".")
	cmd.Dir = module
	cmd.Env = append(os.Environ(),
		"GOCACHEPROG="+executable,
		"BILLET_GOCACHEPROG_TEST_HELPER=1",
		"BILLET_GOCACHE_DIR="+dir,
		"BILLET_CACHE_ENDPOINT="+node.URL,
		"BILLET_CACHE_TOKEN=token",
		"GOFLAGS=", "GOTOOLCHAIN=local", "GOWORK=off",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build through the helper failed: %v\n%s", err, output)
	}
}

// A REAL GO COMMAND BUILDS THROUGH THE HELPER, and a second build with an empty
// local directory is served from the node: the objects the first build put
// cross jobs, which is the point of the cache. A helper that only ever used its
// local directory would pass the first half of this and fail the second.
func TestTheGoCommandSharesObjectsThroughTheNode(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command to drive the helper")
	}
	t.Parallel()

	fake := &fakeNode{objects: map[string][]byte{}}
	node := httptest.NewServer(fake)
	t.Cleanup(node.Close)

	module := probeModule(t)
	build(t, node, module, t.TempDir())
	if fake.puts.Load() == 0 {
		t.Fatal("the first build put nothing on the node")
	}
	build(t, node, module, t.TempDir())
	if fake.casHits.Load() == 0 {
		t.Fatal("the second build, starting empty, was served nothing by the node")
	}
	if redone := fake.redone.Load(); redone != 0 {
		t.Fatalf("the second build redid %d actions the node already held", redone)
	}
}

// exchange drives the helper over the protocol directly and returns its answers
// after the capabilities announcement.
func exchange(t *testing.T, cfg Config, requests ...any) []response {
	t.Helper()

	var in bytes.Buffer
	encoder := json.NewEncoder(&in)
	for _, request := range requests {
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Run(t.Context(), cfg, &in, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	decoder := json.NewDecoder(&out)
	var announce response
	if err := decoder.Decode(&announce); err != nil {
		t.Fatal(err)
	}
	if announce.ID != 0 || !slices.Equal(announce.KnownCommands, []string{"get", "put", "close"}) {
		t.Fatalf("the helper announced %+v, want get, put and close under ID 0", announce)
	}
	var answers []response
	for decoder.More() {
		var answer response
		if err := decoder.Decode(&answer); err != nil {
			t.Fatal(err)
		}
		answers = append(answers, answer)
	}

	return answers
}

// AN OBJECT THE NODE SERVES IS KEPT ONLY WHEN IT IS WHAT ITS ENTRY NAMES. A
// node (or anything between it and the guest) serving other bytes under an
// output id is a miss, never a DiskPath the go command links from.
func TestAPoisonedObjectIsAMiss(t *testing.T) {
	t.Parallel()

	action := bytes.Repeat([]byte{7}, 32)
	good := []byte("the real object")
	output := digest(good)
	for name, poison := range map[string]struct {
		served []byte
		size   int
	}{
		"other bytes of the same size": {[]byte("the fake object"), len(good)},
		"a truncated object":           {good[:4], len(good)},
		"an entry misstating its size": {good, len(good) + 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeNode{objects: map[string][]byte{
				"ac/" + hex.EncodeToString(action): entry{output: output, size: int64(poison.size), at: time.Unix(1, 0)}.encode(),
				"cas/" + output:                    poison.served,
			}}
			node := httptest.NewServer(fake)
			t.Cleanup(node.Close)
			answers := exchange(t, Config{Dir: t.TempDir(), Endpoint: node.URL, Token: "token"},
				request{ID: 1, Command: "get", ActionID: action}, request{ID: 2, Command: "close"})
			if len(answers) != 2 || answers[0].ID != 1 || !answers[0].Miss || answers[0].DiskPath != "" {
				t.Fatalf("a poisoned object was answered %+v", answers)
			}
		})
	}
}

// A LOCAL OBJECT THAT NO LONGER MATCHES ITS ENTRY IS A MISS, not a DiskPath to
// a file of the wrong length.
func TestADamagedLocalObjectIsAMiss(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	action, body := bytes.Repeat([]byte{5}, 32), []byte("an object")
	sum := sha256.Sum256(body)
	exchange(t, Config{Dir: dir},
		request{ID: 1, Command: "put", ActionID: action, OutputID: sum[:], BodySize: int64(len(body))}, body,
		request{ID: 2, Command: "close"})
	if err := os.WriteFile(filepath.Join(dir, "o", hex.EncodeToString(sum[:])), body[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	answers := exchange(t, Config{Dir: dir},
		request{ID: 1, Command: "get", ActionID: action}, request{ID: 2, Command: "close"})
	if !answers[0].Miss {
		t.Fatalf("a truncated local object was answered %+v", answers[0])
	}

	// AND A REMOTE HIT FOR THE SAME ACTION REPLACES IT rather than naming the
	// damaged file again.
	fake := &fakeNode{objects: map[string][]byte{
		"ac/" + hex.EncodeToString(action): entry{output: hex.EncodeToString(sum[:]), size: int64(len(body)),
			at: time.Unix(1, 0)}.encode(),
		"cas/" + hex.EncodeToString(sum[:]): body,
	}}
	node := httptest.NewServer(fake)
	t.Cleanup(node.Close)
	answers = exchange(t, Config{Dir: dir, Endpoint: node.URL, Token: "token"},
		request{ID: 1, Command: "get", ActionID: action}, request{ID: 2, Command: "close"})
	got, err := os.ReadFile(answers[0].DiskPath)
	if answers[0].Miss || err != nil || !bytes.Equal(got, body) {
		t.Fatalf("a remote hit over a damaged local object answered %+v with %q (%v)", answers[0], got, err)
	}
}

// A BUSY NODE IS A MISS, NOT A REASON TO STOP ASKING: the next request is
// served once the node has room.
func TestABusyNodeIsAMissAndIsAskedAgain(t *testing.T) {
	t.Parallel()

	action := bytes.Repeat([]byte{3}, 32)
	body := []byte("served later")
	var busy atomic.Int64
	busy.Store(1)
	objects := map[string][]byte{
		"ac/" + hex.EncodeToString(action): entry{output: digest(body), size: int64(len(body)), at: time.Unix(1, 0)}.encode(),
		"cas/" + digest(body):              body,
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if busy.Add(-1) >= 0 {
			http.Error(w, "busy", http.StatusTooManyRequests)

			return
		}
		key, _ := strings.CutPrefix(r.URL.Path, "/v1/cas/go/")
		if _, err := w.Write(objects[key]); err != nil {
			return
		}
	}))
	t.Cleanup(node.Close)
	answers := exchange(t, Config{Dir: t.TempDir(), Endpoint: node.URL, Token: "token"},
		request{ID: 1, Command: "get", ActionID: action},
		request{ID: 2, Command: "get", ActionID: action},
		request{ID: 3, Command: "close"})
	if !answers[0].Miss || answers[1].Miss {
		t.Fatalf("answers = %+v, want a miss while busy and a hit after", answers[:2])
	}
}

// A PUT IS ANSWERED WITH AN ABSOLUTE PATH HOLDING THE BODY, and a later get in
// the same process finds it without the node.
func TestAPutIsServedLocallyByPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	action, body := bytes.Repeat([]byte{9}, 32), []byte("an object")
	sum := sha256.Sum256(body)
	answers := exchange(t, Config{Dir: dir},
		request{ID: 1, Command: "put", ActionID: action, OutputID: sum[:], BodySize: int64(len(body))}, body,
		request{ID: 2, Command: "get", ActionID: action},
		request{ID: 3, Command: "close"})
	if len(answers) != 3 {
		t.Fatalf("answers = %+v", answers)
	}
	for _, answer := range answers[:2] {
		got, err := os.ReadFile(answer.DiskPath)
		if !filepath.IsAbs(answer.DiskPath) || err != nil || !bytes.Equal(got, body) {
			t.Fatalf("answer %d names %q (%v)", answer.ID, answer.DiskPath, err)
		}
	}
	if get := answers[1]; get.Miss || !bytes.Equal(get.OutputID, sum[:]) || get.Size != int64(len(body)) {
		t.Fatalf("the get after a put answered %+v", get)
	}
}

// A NODE THAT REFUSES LEAVES A WORKING BUILD with a local cache.
func TestARefusingNodeLeavesAWorkingLocalCache(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command to drive the helper")
	}
	t.Parallel()

	node := httptest.NewServer(&fakeNode{objects: map[string][]byte{}, refuse: true})
	t.Cleanup(node.Close)
	dir := t.TempDir()
	build(t, node, probeModule(t), dir)
	entries, err := os.ReadDir(filepath.Join(dir, "o"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("the local cache holds nothing after a build: %v", err)
	}
}
