package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/retirement"
)

func settledNodeConfigFixture(t *testing.T) (*requestFixture, retirement.Journal) {
	t.Helper()
	f := newRequestFixture(t) // Uses the same PostgreSQL gate as sibling command tests.
	retainAndRestartANode(t, f)
	f.reserve(t)
	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	retiredAnswer(t, out, code)
	j := requireRetireJournal(t)
	if !j.Settled || !j.RowDone {
		t.Fatal("producer did not reach settled done")
	}
	var installed map[string]any
	mustOK(t, yaml.Unmarshal([]byte(mustRead(t, f.cfg)), &installed))
	node, ok := installed["node"].(map[string]any)
	if !ok {
		t.Fatal("settled configuration has no node mapping")
	}
	node["lock_dir"] = "/run/billet/locks"
	body, err := yaml.Marshal(installed)
	mustOK(t, err)
	writeFile(t, f.cfg, string(body), 0o600)
	setRetireEffect(t, f, nodeUnit, "User", "root")
	setRetireEffect(t, f, nodeUnit, "Group", "root")
	return f, j
}

func nodeConfigDocument(t *testing.T, j retirement.Journal, rendering string, operations retireNodeOperations) string {
	t.Helper()
	body, err := json.Marshal(operations)
	mustOK(t, err)
	in := retireNodeConfigInput{Schema: 1, Run: requestRun, Retiring: requestRetiring, TransitionID: j.Provenance.TransitionID,
		Rendering: rendering, RenderingSHA256: retirement.Digest([]byte(rendering)), Operations: body}
	body, err = json.Marshal(in)
	mustOK(t, err)
	return string(body)
}

func emptyNodeOperations() retireNodeOperations {
	return retireNodeOperations{Filesystem: []retireFilesystemOperation{}, Services: []retireServiceOperation{}, Units: []retireProposedUnit{}}
}

func runNodeConfigCheck(t *testing.T, f *requestFixture, j retirement.Journal, document string, extra ...string) (string, int) {
	t.Helper()
	args := []string{"--check-node-config", "--input", "-", "--run", requestRun, "--retiring-host", requestRetiring,
		"--transition", j.Provenance.TransitionID, "--expected-holder", requestRun, "--expected-guard", f.guard.record(t).ID}
	return f.run(t, document, append(args, extra...)...)
}

func forbidNodeConfigWrites(t *testing.T, f *requestFixture) {
	t.Helper()
	before := map[string]string{}
	absent := []string{}
	for _, path := range []string{f.cfg, retirement.JournalPath(), retirement.StatusPath(), retirement.ServiceAccountPath(), filepath.Join(f.guard.active(), guardRecordName)} {
		body, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			absent = append(absent, path)
			continue
		}
		mustOK(t, err)
		before[path] = string(body)
	}
	count := len(f.manager.operations)
	savedEvent, savedPublish, savedGuard := retireMutationEvent, retirement.Publishing, guardHook
	retireMutationEvent = func(event, path string) {
		if event != "admission" {
			t.Fatalf("read-only check attempted %s at %s", event, path)
		}
	}
	retirement.Publishing = func(path string) error {
		t.Fatalf("read-only check published %s", path)
		return nil
	}
	guardHook = func(op guardOp) error {
		if op.Kind == "mkdir" || op.Kind == "fsync" || op.Kind == "rename" || op.Kind == "chown" {
			t.Fatalf("read-only check changed guard storage: %+v", op)
		}
		return nil
	}
	t.Cleanup(func() {
		retireMutationEvent, retirement.Publishing, guardHook = savedEvent, savedPublish, savedGuard
		if len(f.manager.operations) != count {
			t.Fatalf("read-only check acted on a service: %v", f.manager.operations[count:])
		}
		for _, path := range absent {
			if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("read-only check recreated %s: %v", path, err)
			}
		}
		for path, body := range before {
			if mustRead(t, path) != body {
				t.Fatalf("read-only check changed %s", path)
			}
		}
	})
}

func TestRetirementNodeConfigAdmitsQuietEntryWithoutDrainOrRegistration(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, activity := range []string{"active", "inactive", "failed"} {
		t.Run(activity, func(t *testing.T) {
			if activity != "active" {
				f.manager.set(nodeUnit, "ActiveState", activity)
				f.manager.set(nodeUnit, "MainPID", "0")
				f.manager.set(nodeUnit, "InvocationID", "")
				if err := os.Remove(registrationRecordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
			}
			t.Setenv("BILLET_STATE_DSN", "postgres://billet@127.0.0.1:1/unreachable?sslmode=disable")
			forbidNodeConfigWrites(t, f)
			operations := emptyNodeOperations()
			operations.Services = []retireServiceOperation{{Verb: "start", Unit: nodeUnit}}
			document := nodeConfigDocument(t, j, mustRead(t, f.cfg), operations)
			out, code := runNodeConfigCheck(t, f, j, document)
			var in retireNodeConfigInput
			mustOK(t, json.Unmarshal([]byte(document), &in))
			want := retirement.NodeConfigVerdict{Run: requestRun, Guard: f.guard.record(t).ID, Retiring: requestRetiring,
				Deployment: j.Deployment, TransitionID: j.Provenance.TransitionID, RenderingSHA256: in.RenderingSHA256,
				OperationsSHA256: retirement.Digest(in.Operations)}
			verdict, err := retirement.DecodeNodeConfigVerdict([]byte(out), code, want)
			if err != nil {
				t.Fatalf("ordinary entry refused: %v\n%s", err, out)
			}
			wantActivity := activity
			if activity != "active" {
				wantActivity = "quiet-" + activity
			}
			if verdict.NodeActivity != wantActivity || strings.Contains(out, "postconditions") {
				t.Fatalf("entry claimed a completed running-node proof: %s", out)
			}
		})
	}
}

// Reintroducing identity-directory protection in unconditional service Paths
// refuses the server's own shipped StateDirectory even with no planned work.
func TestRetirementNodeConfigAdmitsPackagedServiceDirectories(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, unit := range []string{serverUnit, nodeUnit} {
		body := mustRead(t, filepath.Join("..", "..", "deploy", unit))
		props, err := retireOperationInspector().UnitProperties(t.Context(), unit, "FragmentPath")
		mustOK(t, err)
		source := firstProp(props, "FragmentPath")
		body = strings.ReplaceAll(body, "/usr/bin/billet", installedBinary)
		body = strings.ReplaceAll(body, "/etc/billet/billet.yaml", f.cfg)
		writeFile(t, source, body, 0o644)
		for _, line := range strings.Split(body, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch key {
			case "StateDirectory", "RuntimeDirectory", "ReadOnlyPaths", "ReadWritePaths", "User", "Group":
				setRetireEffect(t, f, unit, key, value)
			case "KillMode":
				f.manager.set(unit, key, value)
			case "PrivateTmp":
				if value != "true" {
					t.Fatalf("shipped PrivateTmp changed: %s", value)
				}
				setRetireEffect(t, f, unit, key, "yes")
				setRetireEffect(t, f, unit, "RequiresMountsFor", "/var/tmp")
			}
		}
	}
	server, err := retireOperationInspector().UnitProperties(t.Context(), serverUnit, "StateDirectory", "RuntimeDirectory")
	mustOK(t, err)
	node, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "StateDirectory", "RuntimeDirectory")
	mustOK(t, err)
	if firstProp(server, "StateDirectory") != "billet/server" || firstProp(server, "RuntimeDirectory") != "" ||
		firstProp(node, "StateDirectory") != "billet/node" || firstProp(node, "RuntimeDirectory") != "billet/locks billet/registration" {
		t.Fatal("control lost the shipped directory declarations")
	}
	// The archive stays in the fixture; its old location models the packaged
	// host. The command only observes this pathname and must never create it.
	j.IdentityDir = "/var/lib/billet/server"
	j.Locator.IdentityDir = j.IdentityDir
	j.RetainedInvocation.IdentityDir = j.IdentityDir
	if _, err := os.Lstat(j.IdentityDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("packaged retired identity must be absent for this control: %v", err)
	}
	mustOK(t, j.Write(retireNow()))
	forbidNodeConfigWrites(t, f)
	for _, verb := range []string{"", "start"} {
		operations := emptyNodeOperations()
		if verb != "" {
			operations.Services = []retireServiceOperation{{Verb: verb, Unit: nodeUnit}}
		}
		out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
		if code != 0 || retireAnswer(t, out)["outcome"] != "admitted" {
			t.Fatalf("packaged service directories refused %q: %s", verb, out)
		}
	}
	if _, err := os.Lstat(j.IdentityDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("preflight created the retired identity: %v", err)
	}
}

// Comparing only resolved environment targets admits the pathname and
// intermediate-link deletions. Each case first admits the unchanged host.
func TestRetirementNodeConfigProtectsEnvironmentPathnameAndTraversal(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	mustOK(t, err)
	target := filepath.Join(root, "etc", "billet", "server.env")
	tree := filepath.Join(root, "opt", "env-tree")
	mustOK(t, os.MkdirAll(filepath.Dir(target), 0o700))
	mustOK(t, os.MkdirAll(tree, 0o700))
	writeFile(t, target, "", 0o600)
	path := filepath.Join(tree, "node.env")
	mustOK(t, os.Symlink(target, path))
	alias := filepath.Join(root, "env-alias")
	mustOK(t, os.Symlink(tree, alias))
	props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "FragmentPath")
	mustOK(t, err)
	source := firstProp(props, "FragmentPath")
	before := mustRead(t, source)
	for _, c := range []struct{ name, input, destination string }{
		{"lexical parent", path, tree},
		{"lexical leaf", path, path},
		{"intermediate target", filepath.Join(alias, "node.env"), tree},
		{"resolved target", path, target},
	} {
		t.Run(c.name, func(t *testing.T) {
			setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", c.input+" (ignore_errors=no)")
			writeFile(t, source, before+"[Service]\nEnvironmentFile="+c.input+"\n", 0o644)
			forbidNodeConfigWrites(t, f)
			operations := emptyNodeOperations()
			operations.Filesystem = []retireFilesystemOperation{{Kind: "recursive-delete", Path: filepath.Join(root, "unrelated")}}
			out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
			if code != 0 {
				t.Fatalf("retained environment control refused: %s", out)
			}
			operations.Filesystem[0].Path = c.destination
			out, code = runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
			if code != exitRefused || retireAnswer(t, out)["reason"] != retireReasonNodePath || !strings.Contains(out, "retaining an environment file grants no mutation") {
				t.Fatalf("environment pathname destruction admitted or hit another refusal: %s", out)
			}
			link, err := os.Readlink(path)
			mustOK(t, err)
			if link != target || mustRead(t, target) != "" {
				t.Fatal("read-only admission changed the environment input")
			}
		})
	}
}

func TestRetirementNodeConfigPlantedReadersRefuseBeforeMutation(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, scenario := range []string{"combined missing traversal", "symlink traversal", "recursive parent", "authority file", "transaction root", "controller unit", "changed node name", "changed state directory", "changed rendering digest", "controller service", "unknown operation", "queued job", "missing job", "nonzero quiet pid", "pending reload", "unknown activity", "node account"} {
		t.Run(scenario, func(t *testing.T) {
			unitPath, effects := filepath.Join(f.unitsDir, nodeUnit), filepath.Join(f.unitsDir, nodeUnit+".effects")
			unitBefore, effectsBefore := mustRead(t, unitPath), mustRead(t, effects)
			t.Cleanup(func() {
				writeFile(t, unitPath, unitBefore, 0o644)
				writeFile(t, effects, effectsBefore, 0o644)
			})
			operations := emptyNodeOperations()
			rendering := mustRead(t, f.cfg)
			want := retireReasonNodePath
			missing := filepath.Join(filepath.Dir(j.IdentityDir), "new-reader")
			switch scenario {
			case "combined missing traversal":
				operations.Filesystem = append(operations.Filesystem, retireFilesystemOperation{Kind: "mkdir", Path: missing + "/../" + filepath.Base(j.IdentityDir) + "/../cache/cert.pem"})
			case "symlink traversal":
				alias := filepath.Join(t.TempDir(), "alias")
				mustOK(t, os.Symlink(j.IdentityDir, alias))
				operations.Filesystem = append(operations.Filesystem, retireFilesystemOperation{Kind: "write", Path: alias + "/cert.pem"})
			case "recursive parent":
				operations.Filesystem = append(operations.Filesystem, retireFilesystemOperation{Kind: "recursive-chown", Path: filepath.Dir(j.IdentityDir)})
			case "authority file":
				operations.Filesystem = append(operations.Filesystem, retireFilesystemOperation{Kind: "write", Path: retirement.StatusPath()})
			case "transaction root":
				operations.Filesystem = append(operations.Filesystem, retireFilesystemOperation{Kind: "delete", Path: upgradeRoot})
			case "controller unit":
				operations.Filesystem = append(operations.Filesystem, retireFilesystemOperation{Kind: "write", Path: "/etc/systemd/system/" + serverUnit})
			case "changed node name":
				rendering = strings.Replace(rendering, "node-a", "node-b", 1)
				want = retireReasonIdentity
			case "changed state directory":
				rendering = strings.Replace(rendering, "node-state", "other-state", 1)
				want = retireReasonConfig
			case "changed rendering digest":
				want = retireReasonInput
			case "controller service":
				operations.Services = append(operations.Services, retireServiceOperation{Verb: "start", Unit: serverUnit})
				want = retireReasonEffects
			case "unknown operation":
				operations.Filesystem = append(operations.Filesystem, retireFilesystemOperation{Kind: "interpret", Path: "/etc/node"})
				want = retireReasonInput
			case "queued job", "missing job":
				setRetireEffect(t, f, nodeUnit, "Job", "123 /org/freedesktop/systemd1/job/123")
				if scenario == "missing job" {
					body := mustRead(t, effects)
					writeFile(t, effects, strings.ReplaceAll(body, "Job=123 /org/freedesktop/systemd1/job/123\n", ""), 0o644)
				}
				want = "job"
			case "nonzero quiet pid":
				f.manager.set(nodeUnit, "ActiveState", "inactive")
				want = "settled-entry-node-process-present"
			case "pending reload":
				setRetireEffect(t, f, nodeUnit, "NeedDaemonReload", "yes")
				want = "settled-entry-node-unit-mismatch"
			case "unknown activity":
				f.manager.set(nodeUnit, "ActiveState", "future-state")
				want = "settled-entry-node-observation-unreadable"
			case "node account":
				setRetireEffect(t, f, nodeUnit, "User", "billet")
				want = "settled-entry-node-account"
			}
			document := nodeConfigDocument(t, j, rendering, operations)
			if scenario == "changed rendering digest" {
				document = strings.Replace(document, retirement.Digest([]byte(rendering)), strings.Repeat("0", 64), 1)
			}
			forbidNodeConfigWrites(t, f)
			out, code := runNodeConfigCheck(t, f, j, document)
			if code == 0 || !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
				t.Fatalf("%s reached ordinary admission or hit another refusal: %s", scenario, out)
			}
			for _, path := range []string{missing, j.IdentityDir} {
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("admission created a missing/protected component %s: %v", path, err)
				}
			}
		})
	}
}

func TestTheRetireNodeConfigFixturesAreTheCommandsOwn(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, activity := range []string{"active", "inactive", "failed"} {
		t.Run(activity, func(t *testing.T) {
			if activity != "active" {
				f.manager.set(nodeUnit, "ActiveState", activity)
				f.manager.set(nodeUnit, "MainPID", "0")
			}
			document := nodeConfigDocument(t, j, mustRead(t, f.cfg), emptyNodeOperations())
			out, code := runNodeConfigCheck(t, f, j, document)
			if code != 0 {
				t.Fatalf("producer refused: %s", out)
			}
			compareFixture(t, "retire-node-config", activity, normaliseReport(t, out, map[string]string{
				j.Deployment: retireTestIdentity, f.guard.record(t).ID: strings.Repeat("1", 32),
				j.Provenance.TransitionID: strings.Repeat("2", 32),
			}))
		})
	}
	fixtureSetIs(t, "retire-node-config", []string{"active", "inactive", "failed"})
}

func TestRetirementNodeConfigRequiresSettledBoundRecordsWithoutRepair(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, scenario := range []string{"status absent", "status damaged", "status stale", "unsettled", "row incomplete", "marker", "holder", "guard id", "transition", "archive mismatch", "identity recreated"} {
		t.Run(scenario, func(t *testing.T) {
			paths := []string{retirement.StatusPath(), retirement.JournalPath(), filepath.Join(f.guard.active(), guardRecordName)}
			saved := make(map[string]string)
			for _, path := range paths {
				saved[path] = mustRead(t, path)
			}
			t.Cleanup(func() {
				for path, body := range saved {
					mode := os.FileMode(0o600)
					if path == retirement.StatusPath() {
						mode = 0o644
					}
					writeFile(t, path, body, mode)
				}
			})
			guardID := f.guard.record(t).ID
			changeRecord := func(path, member string, value any) {
				var doc map[string]any
				mustOK(t, json.Unmarshal([]byte(mustRead(t, path)), &doc))
				doc[member] = value
				body, err := json.Marshal(doc)
				mustOK(t, err)
				writeFile(t, path, string(body), 0o600)
			}
			want := retireReasonJournal
			extra := []string{}
			switch scenario {
			case "status absent":
				mustOK(t, os.Remove(retirement.StatusPath()))
				want = retireReasonStatus
			case "status damaged":
				writeFile(t, retirement.StatusPath(), "{broken", 0o644)
				want = retireReasonStatus
			case "status stale":
				mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, j.Variant, retireNow()))
				want = retireReasonStatus
			case "unsettled":
				changeRecord(retirement.JournalPath(), "settled", false)
			case "row incomplete":
				changeRecord(retirement.JournalPath(), "row_done", false)
			case "marker":
				changeRecord(paths[2], "transition", map[string]any{"kind": "retirement", "id": j.Provenance.TransitionID})
				want = retireReasonGuard
			case "holder":
				changeRecord(paths[2], "holder", "another-run")
				want = retireReasonGuard
			case "guard id":
				changeRecord(paths[2], "id", strings.Repeat("e", 32))
				extra = []string{"--expected-guard", guardID}
				want = retireReasonGuard
			case "transition":
				extra = []string{"--transition", strings.Repeat("e", 32)}
				want = retireReasonInput
			case "archive mismatch":
				changeRecord(retirement.JournalPath(), "archive", j.Archive+"-other")
			case "identity recreated":
				mustOK(t, os.Mkdir(j.IdentityDir, 0o700))
				t.Cleanup(func() { mustOK(t, os.Remove(j.IdentityDir)) })
				want = retireReasonIdentity
			}
			forbidNodeConfigWrites(t, f)
			document := nodeConfigDocument(t, j, mustRead(t, f.cfg), emptyNodeOperations())
			out, code := runNodeConfigCheck(t, f, j, document, extra...)
			if code == 0 || retireAnswer(t, out)["reason"] != want {
				t.Fatalf("%s admitted or refused for the wrong record: %s", scenario, out)
			}
		})
	}
}

func TestRetirementNodeConfigAdmitsFutureCredentialsAndProtectsEachReader(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, field := range []string{"cert", "key", "ca"} {
		t.Run(field, func(t *testing.T) {
			var doc map[string]any
			mustOK(t, yaml.Unmarshal([]byte(mustRead(t, f.cfg)), &doc))
			node, ok := doc["node"].(map[string]any)
			if !ok {
				t.Fatal("missing node")
			}
			tls, ok := node["tls"].(map[string]any)
			if !ok {
				t.Fatal("missing TLS")
			}
			missing := filepath.Join(t.TempDir(), "not-installed", field)
			tls[field] = missing
			rendering, err := yaml.Marshal(doc)
			mustOK(t, err)
			forbidNodeConfigWrites(t, f)
			out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, string(rendering), emptyNodeOperations()))
			if code != 0 {
				t.Fatalf("future %s had to exist before admission: %s", field, out)
			}
			if _, err := os.Lstat(filepath.Dir(missing)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("preflight prepared credentials: %v", err)
			}
			tls[field] = j.IdentityDir + "/" + field
			rendering, err = yaml.Marshal(doc)
			mustOK(t, err)
			out, code = runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, string(rendering), emptyNodeOperations()))
			if code != exitRefused || retireAnswer(t, out)["reason"] != retireReasonNodePath {
				t.Fatalf("future %s reader could recreate the controller: %s", field, out)
			}
		})
	}
}

func TestNodeConfigInspectionLockNeverPreparesMissingStorage(t *testing.T) {
	for _, missing := range []string{"root", "lock"} {
		t.Run(missing, func(t *testing.T) {
			f := newGuardFixture(t)
			if missing == "lock" {
				mustOK(t, os.Mkdir(f.root, 0o700))
			}
			lock, err := takeRetireInspectionLock()
			if lock != nil {
				lock.release()
				t.Fatal("inspection acquired a lock that did not exist")
			}
			if err == nil {
				t.Fatal("inspection did not report absent storage")
			}
			path := f.root
			if missing == "lock" {
				path = filepath.Join(f.root, txLockName)
			}
			if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("inspection created %s: %v", path, err)
			}
		})
	}
}

func TestRetirementNodeConfigPreservesEnvironmentBeforeUnitReplacement(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, scenario := range []string{"required present", "optional present", "optional absent", "required absent", "wrong optionality", "multiple", "malformed", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			effects := filepath.Join(f.unitsDir, nodeUnit+".effects")
			typed := filepath.Join(f.unitsDir, nodeUnit+".EnvironmentFiles.json")
			props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "FragmentPath")
			mustOK(t, err)
			source := firstProp(props, "FragmentPath")
			beforeEffects, beforeTyped, beforeSource := mustRead(t, effects), mustRead(t, typed), mustRead(t, source)
			t.Cleanup(func() {
				writeFile(t, effects, beforeEffects, 0o644)
				writeFile(t, typed, beforeTyped, 0o600)
				writeFile(t, source, beforeSource, 0o644)
			})
			path := filepath.Join(t.TempDir(), "node.env")
			optional := strings.HasPrefix(scenario, "optional")
			if !strings.HasSuffix(scenario, "absent") {
				writeFile(t, path, "", 0o600)
			}
			flag, prefix := "no", ""
			if optional {
				flag, prefix = "yes", "-"
			}
			setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", path+" (ignore_errors="+flag+")")
			body := beforeSource + "[Service]\nEnvironmentFile=" + prefix + path + "\n"
			if scenario == "wrong optionality" {
				body = beforeSource + "[Service]\nEnvironmentFile=-" + path + "\n"
			}
			writeFile(t, source, body, 0o644)
			switch scenario {
			case "multiple":
				setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", path+" (ignore_errors=no)\n"+path+".other (ignore_errors=yes)")
			case "malformed":
				writeFile(t, typed, `{"type":"a(sb)","data":[["/etc/node.env","false"]]}`, 0o600)
			case "unsupported":
				setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", "/etc/unsupported path (ignore_errors=yes)")
			}
			operations := emptyNodeOperations()
			operations.Units = []retireProposedUnit{{Unit: nodeUnit, Path: source, Contents: body, SHA256: retirement.Digest([]byte(body))}}
			operations.Filesystem = []retireFilesystemOperation{{Kind: "write", Path: source}}
			forbidNodeConfigWrites(t, f)
			out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
			if scenario == "required present" || scenario == "optional present" || scenario == "optional absent" {
				if code != 0 {
					t.Fatalf("supported environment refused: %s", out)
				}
			} else if code == 0 || (!strings.Contains(strings.ToLower(out), "environment") && !strings.Contains(out, "retained-input-unreadable")) {
				t.Fatalf("unsupported environment reached unit replacement admission: %s", out)
			}
			if mustRead(t, source) != body {
				t.Fatal("admission changed the installed unit bytes")
			}
			if strings.HasSuffix(scenario, "absent") {
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("admission created an absent environment file: %v", err)
				}
			}
		})
	}
}

// Each cache reader and the explicit parent-creation operand carries the full
// missing/../protected/../outside witness, before any directory can exist.
func TestRetirementNodeConfigRejectsCombinedCacheTLSParentTraversal(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	var doc map[string]any
	mustOK(t, yaml.Unmarshal([]byte(mustRead(t, f.cfg)), &doc))
	node, ok := doc["node"].(map[string]any)
	if !ok {
		t.Fatal("missing node")
	}
	node["provider"], node["site"] = "ec2", "aws-us-west-2"
	node["max_vcpu"], node["max_memory"] = 64, "256GiB"
	delete(node, "docker")
	node["ec2"] = map[string]any{"region": "us-west-2", "subnet_id": "subnet-0abc", "security_group_ids": []string{"sg-0abc"},
		"instance_types": []any{map[string]any{"type": "c7i.2xlarge", "vcpu": 8, "memory": "16GiB", "price_usd_per_hour": 0.34}}}
	node["ebs_s3"] = map[string]any{"region": "us-west-2", "availability_zone": "us-west-2a", "bucket": "billet-cache-example"}
	cache := map[string]any{"listen": "10.0.2.10:7718", "guest_endpoint": "https://cache.aws.example:7718",
		"tls_cert": "/etc/billet/future-cache.crt", "tls_key": "/etc/billet/future-cache.key"}
	node["cache"] = cache
	for _, field := range []string{"safe", "tls_cert", "tls_key", "parent operation"} {
		t.Run(field, func(t *testing.T) {
			missing := filepath.Join(filepath.Dir(j.IdentityDir), "new-cache")
			parent := missing + "/../" + filepath.Base(j.IdentityDir) + "/../cache"
			operations := emptyNodeOperations()
			if field == "parent operation" {
				operations.Filesystem = []retireFilesystemOperation{{Kind: "mkdir", Path: parent}}
			} else if field != "safe" {
				before := cache[field]
				cache[field] = parent + "/cert.pem"
				t.Cleanup(func() { cache[field] = before })
			}
			rendering, err := yaml.Marshal(doc)
			mustOK(t, err)
			forbidNodeConfigWrites(t, f)
			out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, string(rendering), operations))
			if field == "safe" {
				if code != 0 {
					t.Fatalf("safe future cache configuration refused: %s", out)
				}
			} else if code != exitRefused || retireAnswer(t, out)["reason"] != retireReasonNodePath || !strings.Contains(out, "traverses protected resource") {
				t.Fatalf("combined %s witness reached ordinary work or another refusal: %s", field, out)
			}
			for _, path := range []string{missing, j.IdentityDir} {
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("preflight created transient cache/controller storage %s: %v", path, err)
				}
			}
		})
	}
}
