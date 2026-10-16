package host

import (
	"bytes"
	"errors"
	"go/ast"
	"io/fs"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

func mappingCheckOutput(report ceph.MappingReport, err error) string {
	var out bytes.Buffer

	printKernelMappings(cli.Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }}, report, err)

	return out.String()
}

// THE CHECK COUNTS EVERY MAPPING BY WHAT ACCOUNTS FOR IT AND NAMES THE ONES NO
// JOB HOLDS (#494), so a build-up is seen before it matters.
func TestTheCheckNamesTheMappingsNoJobHolds(t *testing.T) {
	t.Parallel()

	report := ceph.MappingReport{Mappings: []ceph.KernelMapping{
		{Pool: "billet-cache", Name: "billet-c5dd400d12e011d8c985f37afbe4e0ca", State: ceph.MappingRootDisk},
		{Pool: "billet-cache", Name: "cache-v-1792178999-f11661bb00919aa6541bada0", State: ceph.MappingInSession},
		{Pool: "billet-cache", Name: "cache-v-1791016000-000000000000000000000001", State: ceph.MappingUnaccounted},
		{Pool: "billet-cache", Name: "cache-v-1791016000-000000000000000000000002", State: ceph.MappingUnaccounted},
	}}

	out := mappingCheckOutput(report, nil)

	for _, want := range []string{
		"4 kernel mapping(s) here",
		"1 guest root disk(s)",
		"1 cache volume(s) a cache session names",
		"2 cache volume(s) no cache session names",
		"billet-cache/cache-v-1791016000-000000000000000000000001",
		"billet-cache/cache-v-1791016000-000000000000000000000002",
		"never forced",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the check does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cache-v-1792178999") {
		t.Errorf("the check names a volume a session holds as held by no job:\n%s", out)
	}
}

// MANY ARE COUNTED, NOT ALL NAMED.
func TestTheCheckNamesOnlyTheFirstUnaccountedMappings(t *testing.T) {
	t.Parallel()

	var report ceph.MappingReport
	for range mappingNamesShown + 3 {
		report.Mappings = append(report.Mappings, ceph.KernelMapping{
			Pool: "billet-cache", Name: "cache-v-1", State: ceph.MappingUnaccounted,
		})
	}

	out := mappingCheckOutput(report, nil)
	if got := strings.Count(out, "billet-cache/cache-v-1"); got != mappingNamesShown || !strings.Contains(out, "and 3 more") {
		t.Errorf("the check named %d mappings, want %d and the rest counted:\n%s", got, mappingNamesShown, out)
	}
}

// A TABLE IT COULD NOT READ IS SAID AS SUCH, NEVER AS NO MAPPINGS; and a host
// that maps nothing says so.
func TestTheCheckSaysWhenItCouldNotTellTheMappings(t *testing.T) {
	t.Parallel()

	out := mappingCheckOutput(ceph.MappingReport{}, errors.New("node: read cache custody directory: permission denied"))
	if !strings.Contains(out, "could not tell") || !strings.Contains(out, "permission denied") ||
		strings.Contains(out, "maps nothing") {
		t.Errorf("an unreadable table was not reported as could-not-tell:\n%s", out)
	}

	if out := mappingCheckOutput(ceph.MappingReport{}, nil); !strings.Contains(out, "maps nothing") {
		t.Errorf("an empty table was not said:\n%s", out)
	}
}

// CUSTODY RECORDS IT CANNOT READ ARE COULD-NOT-TELL, never "no session names
// anything", which would call every live job's volume held by no job.
func TestTheCheckReadsNoMappingsWithoutTheSessions(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Node: &config.NodeConfig{
		StateDir: t.TempDir(), Cache: &config.NodeCacheConfig{},
		Ceph: &config.CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "billet-cache"},
	}}

	_, err := readKernelMappings(t.Context(), cfg)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a node whose custody records are missing read its mappings anyway: %v", err)
	}
}

// AND `billet check` REPORTS THEM ON EVERY CEPH NODE, after the cluster answered:
// runCheck cannot reach its Ceph branch without a cluster, so this reads it.
func TestTheCheckReportsTheMappingsAfterTheCluster(t *testing.T) {
	t.Parallel()

	var cluster, mappings bool

	ast.Inspect(findFunc(t, "runCheck"), func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}

		for i, stmt := range block.List {
			ifs, ok := stmt.(*ast.IfStmt)
			if !ok || ifs.Init == nil {
				continue
			}

			ast.Inspect(ifs.Init, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok && calleeName(call) == "checkCephCluster" {
					cluster = true

					if i+1 < len(block.List) {
						if expr, ok := block.List[i+1].(*ast.ExprStmt); ok {
							next, ok := expr.X.(*ast.CallExpr)
							mappings = ok && calleeName(next) == "reportKernelMappings"
						}
					}
				}

				return true
			})
		}

		return true
	})

	if !cluster || !mappings {
		t.Errorf("runCheck does not call reportKernelMappings straight after checkCephCluster "+
			"(cluster check found %v, mappings after it %v)", cluster, mappings)
	}
}
