package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/retirement"
)

const (
	retireReasonInertMarker    = "retired-inert-marker"
	retireReasonInertDropIn    = "retired-inert-drop-in"
	retireReasonInertCondition = "retired-inert-condition"
	retireReasonInertReload    = "retired-inert-reload"
	retireReasonInertSource    = "retired-inert-source"
	retireReasonInertInstall   = "retired-inert-install"
	retireReasonInertProcess   = "retired-inert-process"
	retireDropInName           = "10-billet-retired.conf"
	upgradeServiceUnit         = "billet-upgrade.service"
)

// The zero value uses the real durable primitive; tests interrupt its steps.
var retireInertInstaller durablefile.Installer

var retireInertUnits = []string{serverUnit, backupTimerUnit, backupServiceUnit, upgradeTimerUnit, upgradeServiceUnit}

// The marker holds the pre-install source fingerprints. A pending reload alone
// cannot distinguish our interrupted write from a changed fragment or drop-in.
// Its name is the transition id under the existing protected retirement root.
type retireInertRecord struct {
	Schema     int               `json:"schema"`
	Transition string            `json:"transition_id"`
	Deployment string            `json:"deployment"`
	Sources    map[string]string `json:"sources"`
}

func retireInertMarker(j retirement.Journal) string {
	return filepath.Join(retirement.RetiredDir(), j.Provenance.TransitionID)
}

func retireInertBytes(j retirement.Journal) string {
	return "[Unit]\nConditionPathExists=!" + retireInertMarker(j) + "\n"
}

func retireInertDropIn(insp *lifeops.Inspector, unit string) (string, error) {
	dirs := insp.OperationUnitDirectories()
	if len(dirs) == 0 || !filepath.IsAbs(dirs[0]) {
		return "", errors.New("no persistent systemd directory")
	}
	return filepath.Join(dirs[0], unit+".d", retireDropInName), nil
}

func readRetireInertRecord(j retirement.Journal) (retireInertRecord, error) {
	var record retireInertRecord
	path := retireInertMarker(j)
	file, info, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
	if err != nil {
		return record, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return record, fmt.Errorf("cannot read bounded retirement marker: %w", err)
	}
	if len(body) > 1<<20 {
		return record, errors.New("retirement marker exceeds read bound")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() || info.Mode().Perm() != 0o600 {
		return record, errors.New("untrusted retirement inertness record")
	}
	if err := retirement.DecodeDocument(body, &record); err != nil {
		return record, err
	}
	if record.Schema != 1 || record.Transition != j.Provenance.TransitionID || record.Deployment != j.Deployment || len(record.Sources) == 0 || !reflect.DeepEqual(record.Sources, j.InertSources) {
		return record, errors.New("inertness record does not bind this retirement and its source evidence")
	}
	return record, nil
}

// Only a positively absent name can be repaired. Altered or unreadable records
// and drop-ins are evidence of interference, never permission to overwrite.
func installRetireInertFile(path string, body []byte, mode fs.FileMode) error {
	present, err := retireNamePresent(path)
	if err != nil {
		return err
	}
	if present {
		have, err := regularfile.ReadFile(path, 1<<20, regularfile.Options{NoFollow: true})
		if err != nil {
			return err
		}
		if !bytes.Equal(have, body) {
			return fmt.Errorf("existing retirement file differs: %s", path)
		}
	}
	installer := retireInertInstaller
	if err := installer.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	noteRetireMutation("inert-file", path)
	_, err = installer.Install(filepath.Dir(path), filepath.Base(path), mode, func(w io.Writer) error {
		_, err := w.Write(body)
		return err
	})
	return err
}

// Snapshot every candidate fragment and drop-in, including shadowed files.
// Only our destination and non-effective durablefile staging names are excluded.
func retireInertSources(ctx context.Context, insp *lifeops.Inspector) (map[string]string, error) {
	sources := make(map[string]string)
	for _, unit := range retireInertUnits {
		own, err := retireInertDropIn(insp, unit)
		if err != nil {
			return nil, err
		}
		props, err := insp.UnitProperties(ctx, unit, "LoadState", "FragmentPath", "DropInPaths")
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"LoadState", "FragmentPath", "DropInPaths"} {
			if len(props[key]) != 1 {
				return nil, fmt.Errorf("missing %s for %s", key, unit)
			}
		}
		loaded := strings.Fields(firstProp(props, "DropInPaths"))
		paths := slices.Clone(loaded)
		for _, path := range slices.Clone(paths) {
			if path == own {
				continue
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				paths = append(paths, filepath.Join(filepath.Dir(path), entry.Name()))
			}
		}
		if fragment := firstProp(props, "FragmentPath"); fragment != "" && fragment != "/dev/null" {
			paths = append(paths, fragment)
			loaded = append(loaded, fragment)
		}
		for _, dir := range insp.OperationUnitDirectories() {
			paths = append(paths, filepath.Join(dir, unit))
			for _, suffix := range []string{".d", ".wants", ".requires", ".upholds"} {
				root := filepath.Join(dir, unit+suffix)
				entries, err := os.ReadDir(root)
				if errors.Is(err, fs.ErrNotExist) {
					if present, statErr := retireNamePresent(root); statErr != nil || present {
						return nil, fmt.Errorf("unreadable unit directory %s", root)
					}
					continue
				}
				if err != nil {
					return nil, err
				}
				for _, entry := range entries {
					paths = append(paths, filepath.Join(root, entry.Name()))
				}
			}
		}
		for _, path := range paths {
			if path == own || (filepath.Dir(path) == filepath.Dir(own) && !slices.Contains(loaded, path) && durablefile.IsStagingName(filepath.Base(path))) {
				continue
			}
			if _, seen := sources[path]; seen {
				continue
			}
			info, err := os.Lstat(path)
			if errors.Is(err, fs.ErrNotExist) {
				sources[path] = "absent"
				continue
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return nil, err
				}
				sources[path+"/link"] = target
				if target == "/dev/null" {
					sources[path] = "masked"
					continue
				}
			}
			body, err := regularfile.ReadFile(path, 1<<20, regularfile.Options{})
			if err != nil {
				return nil, err
			}
			sources[path] = retirement.Digest(body)
		}
	}
	encoded, err := json.Marshal(sources)
	if err != nil || len(encoded) > 16<<10 {
		return nil, errors.New("retirement source evidence exceeds its journal budget")
	}
	return sources, nil
}

func admitRetireInertReload(ctx context.Context, insp *lifeops.Inspector, j retirement.Journal, record retireInertRecord) *retireRefusal {
	sources, err := retireInertSources(ctx, insp)
	if err != nil {
		return retireUnknown(retireReasonInertSource, "read unit sources: "+err.Error(), "")
	}
	if !reflect.DeepEqual(sources, record.Sources) {
		return retireRefuse(retireReasonInertSource, "unit sources differ from the retirement record", "")
	}
	pending, err := insp.PendingReloadUnits(ctx)
	if err != nil {
		return retireUnknown(retireReasonInertReload, err.Error(), "")
	}
	for _, unit := range pending {
		if !slices.Contains(retireInertUnits, unit) {
			return retireRefuse(retireReasonInertReload, "unrelated pending reload: "+unit, "")
		}
		path, err := retireInertDropIn(insp, unit)
		if err != nil {
			return retireUnknown(retireReasonInertReload, err.Error(), "")
		}
		body, err := regularfile.ReadFile(path, 4096, regularfile.Options{NoFollow: true})
		if errors.Is(err, fs.ErrNotExist) {
			present, statErr := retireNamePresent(path)
			if statErr == nil && !present {
				// Repair of our missing name is allowed; the reload boundary
				// separately requires all five exact files to exist.
				continue
			}
		}
		if err != nil || string(body) != retireInertBytes(j) {
			return retireUnknown(retireReasonInertReload, "pending reload is not the exact retirement drop-in: "+unit, "")
		}
	}
	return nil
}

// The exception changes only the reload predicate. Every ordinary pre-mutation
// admission, including current inverse activation edges, still runs.
func admitRetireInertPreparation(ctx context.Context, m retireMode, j retirement.Journal) *retireRefusal {
	insp := retireOperationInspector()
	record := retireInertRecord{Sources: j.InertSources}
	if len(record.Sources) == 0 {
		return retireUnknown(retireReasonInertSource, "intent has no pre-install source evidence", "")
	}
	if r := admitRetireInertReload(ctx, insp, j, record); r != nil {
		return r
	}
	allow := func(ctx context.Context, unit string) (string, error) {
		if !slices.Contains(retireInertUnits, unit) {
			return "", fmt.Errorf("unrelated pending reload: %s", unit)
		}
		if r := admitRetireInertReload(ctx, insp, j, record); r != nil {
			return "", errors.New(r.Why)
		}
		return retireInertDropIn(insp, unit)
	}
	return admitRetireRemaining(ctx, m, j, allow)
}

// This reconciliation runs before remaining-operation admission on intent
// resumes, and before the stop step's first stop/disable. It cannot close over
// a different pending reload. Later phases only prove; they never repair.
func reconcileRetireInert(ctx context.Context, m retireMode, j retirement.Journal) *retireRefusal {
	if j.Variant != retirement.VariantRetainedNode || j.Phase != retirement.PhaseIntent {
		return nil
	}
	if j.RetainedInvocation == nil {
		return retireUnknown(retireReasonInertSource, "intent lacks retained invocation evidence", "")
	}
	if r := admitRetireInertPreparation(ctx, m, j); r != nil {
		return r
	}
	insp := retireOperationInspector()
	record, err := readRetireInertRecord(j)
	if errors.Is(err, fs.ErrNotExist) {
		present, statErr := retireNamePresent(retireInertMarker(j))
		if statErr != nil || present {
			return retireUnknown(retireReasonInertMarker, "marker absence is not proved", "")
		}
		if len(j.InertSources) == 0 {
			return retireUnknown(retireReasonInertSource, "intent has no pre-install source evidence", "")
		}
		record = retireInertRecord{Schema: 1, Transition: j.Provenance.TransitionID, Deployment: j.Deployment, Sources: j.InertSources}
		if r := admitRetireInertReload(ctx, insp, j, record); r != nil {
			return r
		}
		body, err := json.Marshal(record)
		if err != nil {
			return retireUnknown(retireReasonInertMarker, err.Error(), "")
		}
		if err := installRetireInertFile(retireInertMarker(j), body, 0o600); err != nil {
			return retireUnknown(retireReasonInertInstall, err.Error(), "")
		}
	} else if err != nil {
		return retireUnknown(retireReasonInertMarker, err.Error(), "")
	}
	// A previous rename may have returned before its directory was committed.
	if _, err := readRetireInertRecord(j); err != nil {
		return retireUnknown(retireReasonInertMarker, err.Error(), "")
	}
	if err := retireInertInstaller.MkdirAll(filepath.Dir(retireInertMarker(j)), 0o755); err != nil {
		return retireUnknown(retireReasonInertInstall, err.Error(), "")
	}
	if err := retireInertInstaller.SyncDirectory(filepath.Dir(retireInertMarker(j))); err != nil {
		return retireUnknown(retireReasonInertInstall, err.Error(), "")
	}
	// Proving an already loaded install avoids a second reload in the stop
	// step after resume has reconciled it. It never substitutes for quietness.
	// Positive unit absence exempts the proof, never the five-file install.
	complete := true
	for _, unit := range retireInertUnits {
		path, err := retireInertDropIn(insp, unit)
		if err != nil {
			return retireUnknown(retireReasonInertDropIn, err.Error(), "")
		}
		body, err := regularfile.ReadFile(path, 4096, regularfile.Options{NoFollow: true})
		complete = complete && err == nil && string(body) == retireInertBytes(j)
		if err == nil && string(body) == retireInertBytes(j) {
			if err := retireInertInstaller.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return retireUnknown(retireReasonInertInstall, err.Error(), "")
			}
			if err := retireInertInstaller.SyncDirectory(filepath.Dir(path)); err != nil {
				return retireUnknown(retireReasonInertInstall, err.Error(), "")
			}
		}
	}
	if r := proveRetireInert(ctx, j); complete && r == nil {
		return nil
	}
	if r := admitRetireInertReload(ctx, insp, j, record); r != nil {
		return r
	}
	for _, unit := range retireInertUnits {
		if r := admitRetireInertPreparation(ctx, m, j); r != nil {
			return r
		}
		path, err := retireInertDropIn(insp, unit)
		if err != nil {
			return retireUnknown(retireReasonInertInstall, err.Error(), "")
		}
		protected := slices.Concat(retireOperationProtection(j).Paths, []string{j.IdentityDir})
		if r := admitRetireFuturePath(path, true, protected); r != nil {
			return r
		}
		if err := installRetireInertFile(path, []byte(retireInertBytes(j)), 0o644); err != nil {
			return retireUnknown(retireReasonInertInstall, err.Error(), "")
		}
	}
	if r := admitRetireInertPreparation(ctx, m, j); r != nil {
		return r
	}
	for _, unit := range retireInertUnits {
		path, err := retireInertDropIn(insp, unit)
		if err != nil {
			return retireUnknown(retireReasonInertDropIn, err.Error(), "")
		}
		body, err := regularfile.ReadFile(path, 4096, regularfile.Options{NoFollow: true})
		if err != nil || string(body) != retireInertBytes(j) {
			return retireUnknown(retireReasonInertDropIn, "reload requires every exact retirement drop-in", "")
		}
	}
	noteRetireMutation("service-operation", "daemon-reload")
	if err := insp.ReloadRetiredUnits(ctx); err != nil {
		return retireUnknown(retireReasonInertReload, err.Error(), "")
	}
	return proveRetireInert(ctx, j)
}

// All five proof boundaries share this effective-condition proof. Activity and
// enablement are separate: installation precedes stop, and settled ordinary
// work may enable a unit whose condition still prevents every start.
// The masked fragment is the null device itself or a name for it; a symlink to
// anything else, or one that cannot be resolved, is not a mask.
func retireNullFragment(path string) bool {
	if path == os.DevNull {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == os.DevNull
}

func proveRetireInert(ctx context.Context, j retirement.Journal) *retireRefusal {
	if j.Variant != retirement.VariantRetainedNode {
		return nil
	}
	if _, err := readRetireInertRecord(j); err != nil {
		return retireUnknown(retireReasonInertMarker, err.Error(), "")
	}
	insp := retireOperationInspector()
	for _, unit := range retireInertUnits {
		props, err := insp.UnitProperties(ctx, unit, "LoadState", "NeedDaemonReload", "DropInPaths", "UnitFileState", "FragmentPath")
		if err != nil {
			return retireUnknown(retireReasonInertCondition, err.Error(), "")
		}
		// The mask's own two answers are required only where a mask is claimed,
		// so a unit that answers nothing for them keeps the refusal its own
		// missing evidence produces rather than a mask diagnostic.
		for _, key := range []string{"LoadState", "NeedDaemonReload", "DropInPaths"} {
			if len(props[key]) != 1 {
				return retireUnknown(retireReasonInertCondition, "missing or repeated "+key+": "+unit, "")
			}
		}
		if firstProp(props, "NeedDaemonReload") != "no" {
			return retireUnknown(retireReasonInertReload, "pending or unknown reload: "+unit, "")
		}
		if firstProp(props, "LoadState") == "not-found" {
			continue
		}
		// A PERSISTENTLY MASKED UNIT IS INERT WITHOUT THE DROP-IN, and more
		// strongly: systemd refuses to start a unit whose fragment is
		// /dev/null, by dependency or by hand. All three answers are required
		// together, because `masked-runtime` is gone at the next boot and a
		// mask whose fragment is a real file is not a mask at all.
		if firstProp(props, "LoadState") == "masked" {
			if len(props["UnitFileState"]) != 1 || len(props["FragmentPath"]) != 1 {
				return retireUnknown(retireReasonInertCondition, "mask evidence could not be observed: "+unit, "")
			}
			if firstProp(props, "UnitFileState") != "masked" || !retireNullFragment(firstProp(props, "FragmentPath")) {
				return retireUnknown(retireReasonInertCondition, "mask is not persistent or does not resolve to /dev/null: "+unit, "")
			}
			continue
		}
		if firstProp(props, "LoadState") != "loaded" {
			return retireUnknown(retireReasonInertCondition, "unit is neither loaded, persistently masked nor positively absent: "+unit, "")
		}
		path, err := retireInertDropIn(insp, unit)
		if err != nil {
			return retireUnknown(retireReasonInertDropIn, err.Error(), "")
		}
		if !slices.Contains(strings.Fields(firstProp(props, "DropInPaths")), path) {
			return retireRefuse(retireReasonInertDropIn, "retirement drop-in is not loaded: "+unit, "")
		}
		body, err := regularfile.ReadFile(path, 4096, regularfile.Options{NoFollow: true})
		if err != nil || string(body) != retireInertBytes(j) {
			return retireUnknown(retireReasonInertDropIn, "retirement drop-in is missing, unreadable or altered: "+unit, "")
		}
		if r := proveRetireNoConditionOverride(insp, unit, path, strings.Fields(firstProp(props, "DropInPaths"))); r != nil {
			return r
		}
		if err := insp.ProveRetiredCondition(ctx, unit, retireInertMarker(j)); err != nil {
			return retireUnknown(retireReasonInertCondition, unit+": "+err.Error(), "")
		}
	}
	if _, err := readRetireInertRecord(j); err != nil {
		return retireUnknown(retireReasonInertMarker, err.Error(), "")
	}
	return nil
}

func retireInertEvidence(ctx context.Context, j retirement.Journal) ([]lifeops.RetiredConditionEvidence, *retireRefusal) {
	if j.Variant != retirement.VariantRetainedNode {
		return nil, nil
	}
	if r := proveRetireInert(ctx, j); r != nil {
		return nil, r
	}
	var evidence []lifeops.RetiredConditionEvidence
	insp := retireOperationInspector()
	for _, unit := range retireInertUnits {
		props, err := insp.UnitProperties(ctx, unit, "LoadState")
		if err != nil || len(props["LoadState"]) != 1 {
			return nil, retireUnknown(retireReasonInertCondition, "unit load state could not be observed: "+unit, "")
		}
		if firstProp(props, "LoadState") == "not-found" {
			continue
		}
		proof, err := insp.ProveRetiredConditionEvidence(ctx, unit, retireInertMarker(j))
		if err != nil {
			return nil, retireUnknown(retireReasonInertCondition, err.Error(), "")
		}
		evidence = append(evidence, proof)
	}
	return evidence, nil
}

func proveRetireNoConditionOverride(insp *lifeops.Inspector, unit, own string, paths []string) *retireRefusal {
	directories := make([]string, 0, len(insp.OperationUnitDirectories())+len(paths))
	for _, root := range insp.OperationUnitDirectories() {
		directories = append(directories, filepath.Join(root, unit+".d"))
	}
	for _, path := range paths {
		directories = append(directories, filepath.Dir(path))
	}
	for _, dir := range directories {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			present, statErr := retireNamePresent(dir)
			if statErr != nil || present {
				return retireUnknown(retireReasonInertDropIn, "drop-in directory absence is not proved", "")
			}
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return retireUnknown(retireReasonInertDropIn, err.Error(), "")
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".conf") {
				paths = append(paths, filepath.Join(dir, entry.Name()))
			}
		}
	}
	for _, path := range paths {
		if path == own {
			continue
		}
		body, err := regularfile.ReadFile(path, 1<<20, regularfile.Options{})
		if err != nil {
			return retireUnknown(retireReasonInertDropIn, err.Error(), "")
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasSuffix(line, "\\") {
				return retireUnknown(retireReasonInertCondition, "continued drop-in directive is unsupported: "+path, "")
			}
			key, _, assignment := strings.Cut(strings.TrimSpace(line), "=")
			if assignment && strings.TrimSpace(key) == "ConditionPathExists" {
				return retireRefuse(retireReasonInertCondition, "another drop-in assigns ConditionPathExists: "+path, "")
			}
		}
	}
	return nil
}

func proveRetireInertProcesses(ctx context.Context, j retirement.Journal) *retireRefusal {
	if j.Variant != retirement.VariantRetainedNode {
		return nil
	}
	insp := retireOperationInspector()
	for _, unit := range retireInertUnits {
		service := strings.HasSuffix(unit, ".service")
		if r := retireQuietJob(ctx, insp, unit, service); r != nil {
			return r
		}
		if !service {
			continue
		}
		props, err := insp.UnitProperties(ctx, unit, "LoadState")
		if err != nil || len(props["LoadState"]) != 1 {
			return retireUnknown(retireReasonInertProcess, "unit load state could not be observed: "+unit, "")
		}
		if firstProp(props, "LoadState") == "not-found" {
			continue
		}
		if err := insp.ProveUnitProcessesGone(ctx, unit); err != nil {
			return retireUnknown(retireReasonInertProcess, err.Error(), "")
		}
	}
	return nil
}
