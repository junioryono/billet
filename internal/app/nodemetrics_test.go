package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

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

func mappingsIn(states ...ceph.MappingState) ceph.MappingReport {
	var report ceph.MappingReport
	for _, s := range states {
		report.Mappings = append(report.Mappings, ceph.KernelMapping{Pool: "billet-cache", State: s})
	}

	return report
}

// THE GAUGE COUNTS EVERY MAPPING BY WHAT ACCOUNTS FOR IT, zeros included, each
// sample in the family the source declares with that family's label count, and
// judged against the sessions read for that scrape.
func TestTheMappingGaugeCountsEachStateAgainstTheSessionsOfThatScrape(t *testing.T) {
	t.Parallel()

	reader := &fakeMappings{report: mappingsIn(ceph.MappingRootDisk, ceph.MappingRootDisk,
		ceph.MappingInSession, ceph.MappingUnaccounted, ceph.MappingUnaccounted, ceph.MappingUnaccounted,
		ceph.MappingOther)}
	reads := 0
	judge := func(name string) bool { return name == "cache-v-named" }

	read := mappingSnapshot(true, func() (kernelMappingReader, error) { return reader, nil },
		func() (func(string) bool, error) {
			reads++

			return judge, nil
		})

	snap, err := read(t.Context())
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	for label, want := range map[string]float64{
		"root_disk": 2, "generation": 0, "session": 1, "recent": 0, "unaccounted": 3, "other": 1,
	} {
		if got, ok := value(t, snap, familyNodeRBDMappings, label); !ok || got != want {
			t.Errorf("%s{state=%q} = %v (present %v), want %v", familyNodeRBDMappings, label, got, ok, want)
		}
	}

	if reads != 1 || reader.judged == nil || !reader.judged("cache-v-named") || reader.judged("cache-v-other") {
		t.Error("the mappings were not judged against the sessions read for this scrape")
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
}

// A READ THAT FAILS ANYWHERE REPORTS NOTHING, never an empty table: the
// sessions, the client, the table, and a state the gauge has no label for.
func TestAnUnreadableMappingTableIsAnErrorNotZeros(t *testing.T) {
	t.Parallel()

	ok := func() (func(string) bool, error) { return func(string) bool { return false }, nil }

	for name, c := range map[string]struct {
		open    func() (kernelMappingReader, error)
		records func() (func(string) bool, error)
	}{
		"sessions": {
			open:    func() (kernelMappingReader, error) { return &fakeMappings{}, nil },
			records: func() (func(string) bool, error) { return nil, errors.New("permission denied") },
		},
		"client": {
			open:    func() (kernelMappingReader, error) { return nil, ceph.ErrNoClient },
			records: ok,
		},
		"table": {
			open:    func() (kernelMappingReader, error) { return &fakeMappings{err: errors.New("rbd: null")}, nil },
			records: ok,
		},
		"a state with no label": {
			open: func() (kernelMappingReader, error) {
				return &fakeMappings{report: mappingsIn(ceph.MappingUnaccounted, ceph.MappingHeld)}, nil
			},
			records: ok,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			snap, err := mappingSnapshot(true, c.open, c.records)(t.Context())
			if err == nil || snap != nil {
				t.Errorf("a failed read reported %v, %v", snap, err)
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

// evictionRecorder is a store that records the order an eviction pass asks it
// things in.
type evictionRecorder struct {
	calls    []string
	report   ceph.MappingReport
	sweepErr error
}

func (e *evictionRecorder) SweepKernelMappings(context.Context) (ceph.MappingReport, error) {
	e.calls = append(e.calls, "sweep")

	return e.report, e.sweepErr
}

func (e *evictionRecorder) Evict(context.Context, time.Duration) error {
	e.calls = append(e.calls, "evict")

	return nil
}

// logLines is a slog handler that keeps each record's message and attributes.
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

// THE KERNEL'S LEFTOVER MAPPINGS ARE RELEASED BEFORE EVICTION, which keeps every
// volume this host maps, so one pass can reclaim a leaked volume; what is
// released is said by name, and what is kept is warned about.
func TestAnEvictionPassSweepsTheKernelMappingsFirst(t *testing.T) {
	t.Parallel()

	store := &evictionRecorder{
		report: ceph.MappingReport{Mappings: []ceph.KernelMapping{
			{Name: "cache-v-1-released", State: ceph.MappingUnmapped},
			{Name: "cache-v-2-held", State: ceph.MappingHeld},
		}},
		sweepErr: errors.New("ceph: billet-cache/cache-v-2-held is held open"),
	}
	logs := &logLines{}

	evictPass(t.Context(), store, func(context.Context) error {
		store.calls = append(store.calls, "mirrors")

		return nil
	}, slog.New(logs))

	if got := strings.Join(store.calls, ","); got != "sweep,evict,mirrors" {
		t.Errorf("an eviction pass asked %s, want sweep,evict,mirrors", got)
	}

	all := strings.Join(logs.lines, "\n")
	if !strings.Contains(all, "INFO unmapped") || !strings.Contains(all, "cache-v-1-released") {
		t.Errorf("the released mapping was not named:\n%s", all)
	}
	if !strings.Contains(all, "WARN kept kernel mappings") || !strings.Contains(all, "cache-v-2-held") {
		t.Errorf("the kept mapping was not warned about:\n%s", all)
	}
}

// AND A STORE THAT MAPS NOTHING INTO THE KERNEL IS ONLY EVICTED.
func TestAnEvictionPassOfAStoreWithoutMappingsOnlyEvicts(t *testing.T) {
	t.Parallel()

	store := &onlyEvicts{}
	evictPass(t.Context(), store, func(context.Context) error { return nil }, slog.New(&logLines{}))

	if store.evicted != 1 {
		t.Errorf("the store was evicted %d times, want once", store.evicted)
	}
}

type onlyEvicts struct{ evicted int }

func (o *onlyEvicts) Evict(context.Context, time.Duration) error {
	o.evicted++

	return nil
}
