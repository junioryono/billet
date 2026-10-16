package app

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// fakeMappings is a kernel-mapping reader whose table is report, and which
// remembers the session judge it was handed.
type fakeMappings struct {
	report ceph.MappingReport
	err    error
	judged func(string) bool
}

func (f *fakeMappings) KernelMappings(_ context.Context, inSession func(string) bool) (ceph.MappingReport, error) {
	f.judged = inSession

	return f.report, f.err
}

// mappingsIn is a report holding n mappings in each state.
func mappingsIn(counts map[ceph.MappingState]int) ceph.MappingReport {
	var report ceph.MappingReport
	for state, n := range counts {
		for range n {
			report.Mappings = append(report.Mappings, ceph.KernelMapping{Pool: "billet-cache", State: state})
		}
	}

	return report
}

// THE GAUGE COUNTS EVERY MAPPING BY WHAT ACCOUNTS FOR IT, each state under its
// own label (a different count for each, so no two labels can trade places),
// each sample in the family the source declares with that family's label
// count, and judged against the sessions read for each scrape.
func TestTheMappingGaugeCountsEachStateAgainstTheSessionsOfThatScrape(t *testing.T) {
	t.Parallel()

	reader := &fakeMappings{report: mappingsIn(map[ceph.MappingState]int{
		ceph.MappingRootDisk: 6, ceph.MappingGeneration: 5, ceph.MappingInSession: 4,
		ceph.MappingRecent: 3, ceph.MappingUnaccounted: 2, ceph.MappingOther: 1,
	})}
	reads := 0
	named := "cache-v-first"

	read := mappingSnapshot(true, func() (kernelMappingReader, error) { return reader, nil },
		func() (func(string) bool, error) {
			reads++
			name := named

			return func(n string) bool { return n == name }, nil
		})

	snap, err := read(t.Context())
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	for label, want := range map[string]float64{
		"root_disk": 6, "generation": 5, "session": 4, "recent": 3, "unaccounted": 2, "other": 1,
	} {
		if got, ok := value(t, snap, familyNodeRBDMappings, label); !ok || got != want {
			t.Errorf("%s{state=%q} = %v (present %v), want %v", familyNodeRBDMappings, label, got, ok, want)
		}
	}

	declared := map[string]int{}
	for _, f := range nodeMappingFamilies {
		declared[f.Name] = len(f.Labels)
	}
	for family, samples := range snap {
		n, ok := declared[family]
		if !ok {
			t.Errorf("the source reports %s, which it does not declare", family)
		}
		for _, s := range samples {
			if len(s.Labels) != n {
				t.Errorf("%s sample %v has %d labels, the family declares %d", family, s.Labels, len(s.Labels), n)
			}
		}
	}

	// A SECOND SCRAPE READS THE SESSIONS AGAIN, and judges by what it read.
	named = "cache-v-second"
	if _, err := read(t.Context()); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if reads != 2 || !reader.judged("cache-v-second") || reader.judged("cache-v-first") {
		t.Errorf("the second scrape did not judge against the sessions it read (%d reads)", reads)
	}
}

// A READ THAT FAILS ANYWHERE REPORTS NOTHING, never an empty table: the
// sessions, the client, the table, and a state the gauge has no label for.
func TestAnUnreadableMappingTableIsAnErrorNotZeros(t *testing.T) {
	t.Parallel()

	sessionsErr, tableErr := errors.New("permission denied"), errors.New("rbd: null")
	ok := func() (func(string) bool, error) { return func(string) bool { return false }, nil }

	for name, c := range map[string]struct {
		open    func() (kernelMappingReader, error)
		records func() (func(string) bool, error)
		want    error
	}{
		"sessions": {
			open:    func() (kernelMappingReader, error) { return &fakeMappings{}, nil },
			records: func() (func(string) bool, error) { return nil, sessionsErr },
			want:    sessionsErr,
		},
		"client": {
			open:    func() (kernelMappingReader, error) { return nil, ceph.ErrNoClient },
			records: ok,
			want:    ceph.ErrNoClient,
		},
		"table": {
			open:    func() (kernelMappingReader, error) { return &fakeMappings{err: tableErr}, nil },
			records: ok,
			want:    tableErr,
		},
		"a state with no label": {
			open: func() (kernelMappingReader, error) {
				return &fakeMappings{report: mappingsIn(map[ceph.MappingState]int{
					ceph.MappingUnaccounted: 1, ceph.MappingHeld: 1,
				})}, nil
			},
			records: ok,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			snap, err := mappingSnapshot(true, c.open, c.records)(t.Context())
			if err == nil || snap != nil {
				t.Fatalf("a failed read reported %v, %v", snap, err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("the read failed with %v, want %v", err, c.want)
			}
		})
	}
}

// A NODE WITH NO CEPH SITE REPORTS NO MAPPINGS, and asks for none.
func TestANodeWithoutCephReportsNoMappings(t *testing.T) {
	t.Parallel()

	snap, err := mappingSnapshot(false,
		func() (kernelMappingReader, error) {
			t.Error("a node with no node.ceph opened a ceph client")

			return nil, errors.New("unreachable")
		},
		func() (func(string) bool, error) {
			t.Error("a node with no node.ceph read its cache sessions")

			return nil, errors.New("unreachable")
		})(t.Context())
	if err != nil || len(snap) != 0 {
		t.Errorf("a node with no node.ceph reported %v, %v", snap, err)
	}
}

// AND THE NODE'S SOURCE IS BUILT THAT WAY: none without node.ceph, and with it
// the node's own cache sessions, whose missing records are could-not-tell.
func TestTheNodesMappingSourceReadsItsOwnSessions(t *testing.T) {
	t.Parallel()

	without := (&Node{cfg: &config.Config{Node: &config.NodeConfig{StateDir: t.TempDir()}}}).KernelMappingMetrics()
	if snap, err := without.read(t.Context()); err != nil || len(snap) != 0 {
		t.Errorf("a node with no node.ceph reported %v, %v", snap, err)
	}

	with := (&Node{cfg: &config.Config{Node: &config.NodeConfig{
		StateDir: t.TempDir(), Cache: &config.NodeCacheConfig{},
		Ceph: &config.CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "billet-cache"},
	}}}).KernelMappingMetrics()
	if _, err := with.read(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a cache node whose custody records are missing read its mappings anyway: %v", err)
	}
}

// evictionRecorder is a store that records the order an eviction pass asks it
// things in, and whether the sweep held the stop's slot while it ran.
type evictionRecorder struct {
	calls    []string
	report   ceph.MappingReport
	sweepErr error
	sweeping chan struct{}
	held     bool
}

func (e *evictionRecorder) SweepKernelMappings(context.Context) (ceph.MappingReport, error) {
	e.calls = append(e.calls, "sweep")
	e.held = len(e.sweeping) == 1

	return e.report, e.sweepErr
}

func (e *evictionRecorder) Evict(context.Context, time.Duration) error {
	e.calls = append(e.calls, "evict")

	return nil
}

// logLines is a slog handler that keeps each record as one line.
type logLines struct{ lines []string }

func (l *logLines) Enabled(context.Context, slog.Level) bool { return true }

func (l *logLines) Handle(_ context.Context, r slog.Record) error {
	line := r.Level.String() + " " + r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.String()

		return true
	})
	l.lines = append(l.lines, line)

	return nil
}

func (l *logLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logLines) WithGroup(string) slog.Handler      { return l }

func (l *logLines) find(prefix string) string {
	for _, line := range l.lines {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}

	return ""
}

// THE KERNEL'S LEFTOVER MAPPINGS ARE RELEASED BEFORE EVICTION, which keeps every
// volume this host maps, so one pass can reclaim a leaked volume; what is
// released is said by name, and what is kept is warned about apart from it.
func TestAnEvictionPassSweepsTheKernelMappingsFirst(t *testing.T) {
	t.Parallel()

	sweeping := make(chan struct{}, 1)
	store := &evictionRecorder{
		report: ceph.MappingReport{Mappings: []ceph.KernelMapping{
			{Name: "cache-v-1-released", State: ceph.MappingUnmapped},
			{Name: "cache-v-2-held", State: ceph.MappingHeld},
		}},
		sweepErr: errors.New("ceph: billet-cache/cache-v-2-held is held open"),
		sweeping: sweeping,
	}
	logs := &logLines{}

	evictPass(t.Context(), store, func(context.Context) error {
		store.calls = append(store.calls, "mirrors")

		return nil
	}, sweeping, slog.New(logs))

	if got := strings.Join(store.calls, ","); got != "sweep,evict,mirrors" {
		t.Errorf("an eviction pass asked %s, want sweep,evict,mirrors", got)
	}
	if !store.held || len(sweeping) != 0 {
		t.Errorf("the sweep held the stop's slot %v while it ran, and it is %d full after", store.held, len(sweeping))
	}

	released := logs.find("INFO unmapped")
	if !strings.Contains(released, "cache-v-1-released") || strings.Contains(released, "cache-v-2-held") {
		t.Errorf("the released line does not name exactly the released mapping: %q", released)
	}
	if kept := logs.find("WARN kept kernel mappings"); !strings.Contains(kept, "cache-v-2-held") {
		t.Errorf("the kept mapping was not warned about: %q", kept)
	}
}

// AND A STORE THAT MAPS NOTHING INTO THE KERNEL IS EVICTED AND ITS MIRRORS
// REAPED, with no slot taken.
func TestAnEvictionPassOfAStoreWithoutMappingsOnlyEvicts(t *testing.T) {
	t.Parallel()

	store := &onlyEvicts{}
	reaped := 0
	evictPass(t.Context(), store, func(context.Context) error {
		reaped++

		return nil
	}, make(chan struct{}), slog.New(&logLines{}))

	if store.evicted != 1 || reaped != 1 {
		t.Errorf("the store was evicted %d times and its mirrors reaped %d, want once each", store.evicted, reaped)
	}
}

type onlyEvicts struct{ evicted int }

func (o *onlyEvicts) Evict(context.Context, time.Duration) error {
	o.evicted++

	return nil
}

// A STOP WAITS FOR A SWEEP IN FLIGHT, AND NO LONGER THAN ITS BOUND: an unmap the
// process exits under is killed with the unit, and one cut short leaves the
// image's lock with the host's one kernel client.
func TestAStopWaitsForASweepInFlightWithinItsBound(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		sweeping := make(chan struct{}, 1)
		sweeping <- struct{}{}

		done := make(chan bool, 1)
		go func() { done <- awaitSweep(sweeping, time.Minute) }()

		time.Sleep(30 * time.Second)
		synctest.Wait()
		if len(done) != 0 {
			t.Fatal("the stop returned while a sweep still held the slot")
		}

		<-sweeping
		if !<-done {
			t.Error("the stop did not see the sweep end")
		}

		sweeping <- struct{}{}
		started := time.Now()
		if awaitSweep(sweeping, time.Minute) {
			t.Error("the stop said no sweep held the slot while one did")
		}
		if waited := time.Since(started); waited != time.Minute {
			t.Errorf("the stop waited %v for a sweep that never ended, want its bound", waited)
		}
	})
}

// THE NODE RUNS THAT PASS, ON THE CONTEXT ITS STOP CANCELS, AND ITS STOP WAITS
// FOR IT. startNodeCache needs a provider and a listener, so this reads it.
func TestTheNodeCacheRunsEvictPassAndItsStopWaitsForTheSweep(t *testing.T) {
	t.Parallel()

	start := nodeFunc(t, "nodecache.go", "startNodeCache")

	passes := 0
	ast.Inspect(start, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isIdent(call.Fun, "evictPass") || len(call.Args) != 5 {
			return true
		}
		if isIdent(call.Args[0], "evictCtx") && isIdent(call.Args[1], "storage") && isIdent(call.Args[3], "sweeping") {
			passes++
		}

		return true
	})
	if passes != 1 {
		t.Errorf("startNodeCache calls evictPass(evictCtx, storage, _, sweeping, _) %d times, want once", passes)
	}

	var stopped, awaited token.Pos
	ast.Inspect(start, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 4 {
			return true
		}
		stop, ok := ret.Results[2].(*ast.FuncLit)
		if !ok {
			return true
		}
		for _, stmt := range stop.Body.List {
			// A STATEMENT OF ITS OWN: a deferred cancel would run after the wait,
			// which would then wait out a pass still starting unmaps.
			if expr, ok := stmt.(*ast.ExprStmt); ok {
				if call, ok := expr.X.(*ast.CallExpr); ok && isIdent(call.Fun, "stopEviction") && !stopped.IsValid() {
					stopped = call.Pos()
				}
			}

			ast.Inspect(stmt, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok && isIdent(call.Fun, "awaitSweep") && len(call.Args) == 2 &&
					isIdent(call.Args[0], "sweeping") && isIdent(call.Args[1], "sweepStopWait") {
					awaited = call.Pos()
				}

				return true
			})
		}

		return true
	})
	if !stopped.IsValid() || !awaited.IsValid() || awaited < stopped {
		t.Error("startNodeCache's stop does not cancel the eviction and then awaitSweep(sweeping, sweepStopWait)")
	}
}

// ONLY THE NODE'S EVICTION PASS UNMAPS: `billet check` and the gauge read the
// mappings, and a read that swept would unmap from a command an operator runs to
// look.
func TestOnlyTheEvictionPassSweepsTheKernelMappings(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	var callers []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "testdata", "node_modules", "tools":
				return filepath.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(source), "SweepKernelMappings(") {
			return nil
		}

		parsed, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return err
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SweepKernelMappings" {
						callers = append(callers, filepath.ToSlash(path)+":"+fn.Name.Name)
					}
				}

				return true
			})
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}

	if len(callers) != 1 || !strings.HasSuffix(callers[0], "internal/app/nodecache.go:evictPass") {
		t.Errorf("SweepKernelMappings is called from %v, want only evictPass", callers)
	}
}
