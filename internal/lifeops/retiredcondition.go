package lifeops

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/regularfile"
)

// OperationUnitDirectories shares the installation-path seam with retirement.
// The first directory is the persistent administrator override directory.
func (i *Inspector) OperationUnitDirectories() []string {
	if i.operationUnitDirs != nil {
		return slices.Clone(i.operationUnitDirs)
	}
	return []string{"/etc/systemd/system", "/run/systemd/system", "/etc/systemd/system.control", "/run/systemd/system.control",
		"/run/systemd/transient", "/run/systemd/generator.early", "/run/systemd/generator", "/run/systemd/generator.late",
		"/usr/local/lib/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}
}

// RetiredConditionEvidence cannot be constructed with authority outside lifeops.
// A walk rechecks the condition and marker or persistent mask, not a permit.
type RetiredConditionEvidence struct {
	unit           string
	marker         string
	persistentMask bool
}

// ProveRetiredConditionEvidence observes a persistent mask or binds an effective
// condition to an existing regular marker. The caller binds marker contents.
func (i *Inspector) ProveRetiredConditionEvidence(ctx context.Context, unit, marker string) (RetiredConditionEvidence, error) {
	props, err := i.properties(ctx, unit, "LoadState", "UnitFileState", "FragmentPath", "NeedDaemonReload")
	if err != nil {
		return RetiredConditionEvidence{}, err
	}
	proof := RetiredConditionEvidence{unit: unit, persistentMask: first(props, "LoadState") == "masked"}
	if err := proof.proveProperties(props); err != nil {
		return RetiredConditionEvidence{}, err
	}
	if proof.persistentMask {
		return proof, nil
	}
	if err := i.ProveRetiredCondition(ctx, unit, marker); err != nil {
		return RetiredConditionEvidence{}, err
	}
	file, _, err := regularfile.Open(marker, regularfile.Options{NoFollow: true})
	if err != nil {
		return RetiredConditionEvidence{}, err
	}
	if err := file.Close(); err != nil {
		return RetiredConditionEvidence{}, err
	}
	return RetiredConditionEvidence{unit: unit, marker: marker}, nil
}

func (proof RetiredConditionEvidence) proveProperties(props map[string][]string) error {
	if err := requireOperationProperties(proof.unit, props, []string{"LoadState", "UnitFileState", "FragmentPath", "NeedDaemonReload"}); err != nil {
		return err
	}
	if first(props, "NeedDaemonReload") != "no" {
		return fmt.Errorf("retired-inert-reload: %s has a pending or unknown reload", proof.unit)
	}
	if proof.persistentMask {
		if first(props, "LoadState") != "masked" || first(props, "UnitFileState") != "masked" || first(props, "FragmentPath") != "/dev/null" {
			return fmt.Errorf("retired-inert-mask: %s is not persistently masked at /dev/null", proof.unit)
		}
	} else if first(props, "LoadState") != "loaded" {
		return fmt.Errorf("retired-inert-condition: %s is not loaded", proof.unit)
	}
	return nil
}

// ProveRetiredCondition reads Unit.Conditions, not the historical ConditionResult.
// systemctl 255 cannot print this structure; the typed bus reply preserves both
// boolean flags. TestRealSystemdRetiredConditions pins that rendering in CI.
func (i *Inspector) ProveRetiredCondition(ctx context.Context, unit, marker string) error {
	reply, err := i.operationTypedProperty(ctx, unit, "org.freedesktop.systemd1.Unit", "Conditions")
	if err != nil {
		return err
	}
	return proveRetiredConditionValue(reply, marker)
}

func proveRetiredConditionValue(reply operationTypedValue, marker string) error {
	var rows []json.RawMessage
	if reply.Type != "a(sbbsi)" || json.Unmarshal(reply.Data, &rows) != nil || rows == nil {
		return fmt.Errorf("retired-condition-malformed: Conditions is not a(sbbsi)")
	}
	count := 0
	for _, row := range rows {
		var fields []json.RawMessage
		var name, parameter *string
		var trigger, negate *bool
		var result *int32
		if json.Unmarshal(row, &fields) != nil || len(fields) != 5 ||
			json.Unmarshal(fields[0], &name) != nil || name == nil || !strings.HasPrefix(*name, "Condition") ||
			json.Unmarshal(fields[1], &trigger) != nil || trigger == nil ||
			json.Unmarshal(fields[2], &negate) != nil || negate == nil ||
			json.Unmarshal(fields[3], &parameter) != nil || parameter == nil ||
			json.Unmarshal(fields[4], &result) != nil || result == nil || *result < -1 || *result > 1 {
			return fmt.Errorf("retired-condition-malformed: invalid Conditions tuple")
		}
		if *name == "ConditionPathExists" {
			count++
			if *trigger || !*negate || *parameter != marker || !filepath.IsAbs(marker) {
				return fmt.Errorf("retired-condition-mismatch: marker is not the non-triggering negated condition")
			}
		}
	}
	if count != 1 {
		return fmt.Errorf("retired-condition-mismatch: require exactly one ConditionPathExists, got %d", count)
	}
	return nil
}

// PendingReloadUnits observes the complete loaded set before a global reload.
// A missing flag, failed listing or malformed reply supplies no permission.
func (i *Inspector) PendingReloadUnits(ctx context.Context) ([]string, error) {
	ctx, cancel := i.bounded(ctx)
	defer cancel()
	out, err := i.exec(ctx, []string{"list-units", "--all", "--output=json"})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Unit string `json:"unit"`
	}
	if json.Unmarshal(out, &rows) != nil || rows == nil || len(rows) == 0 {
		return nil, fmt.Errorf("retired-reload-unknown: no loaded unit inventory")
	}
	pending := []string{}
	seen := make(map[string]bool)
	for _, row := range rows {
		if !operationUnitName(row.Unit) || seen[row.Unit] {
			return nil, fmt.Errorf("retired-reload-unknown: invalid or repeated unit")
		}
		seen[row.Unit] = true
		props, err := i.properties(ctx, row.Unit, "NeedDaemonReload")
		if err != nil {
			return nil, err
		}
		if len(props["NeedDaemonReload"]) != 1 {
			return nil, fmt.Errorf("retired-reload-unknown: %s has no reload observation", row.Unit)
		}
		switch first(props, "NeedDaemonReload") {
		case "yes":
			pending = append(pending, row.Unit)
		case "no":
		default:
			return nil, fmt.Errorf("retired-reload-unknown: %s has an unknown reload flag", row.Unit)
		}
	}
	return pending, nil
}

// ReloadRetiredUnits is called only after journal-bound source reconciliation.
func (i *Inspector) ReloadRetiredUnits(ctx context.Context) error {
	ctx, cancel := i.bounded(ctx)
	defer cancel()
	_, err := i.exec(ctx, []string{"daemon-reload"})
	return err
}
