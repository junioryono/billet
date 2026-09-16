package lifeops

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// CI on Ubuntu 24.04/systemd 255.4 (2026-09-15) observed unequal reads of
// unchanged units after timer disable/reload and on a server-only host.
// Compare policy, not manager telemetry. Relationship arrays are unordered;
// TriggeredBy and enablement are rejudged from the fresh graph after reload.
func compareOperationEvidence(unit string, before, after operationEvidence) error {
	properties := append(slices.Clone(operationUnitProperties), operationRelations...)
	properties = append(properties, operationExecutionProperties...)
	properties = append(properties, "Where", "What", "Type", "PIDFile")
	for _, property := range properties {
		switch property {
		case "ActiveState", "Job", "UnitFileState", "TriggeredBy":
			continue
		}
		a, b := before.props[property], after.props[property]
		if property == "Names" || slices.Contains(operationRelations, property) || property == "RequiresMountsFor" {
			a, b = operationPropertySet(a), operationPropertySet(b)
		}
		if !slices.Equal(a, b) {
			return fmt.Errorf("operation-evidence-changed: %s %s before=%q after=%q", unit, property, before.props[property], after.props[property])
		}
	}
	if !reflect.DeepEqual(before.sources, after.sources) {
		return fmt.Errorf("operation-evidence-changed: %s definition sources before=%+v after=%+v", unit, operationSourceValues(before.sources), operationSourceValues(after.sources))
	}
	if !slices.Equal(before.privateTrees, after.privateTrees) {
		return fmt.Errorf("operation-evidence-changed: %s PrivateTmp trees before=%q after=%q", unit, before.privateTrees, after.privateTrees)
	}
	return nil
}

func operationPropertySet(values []string) []string {
	entries := make([]string, 0, len(values))
	for _, value := range values {
		entries = append(entries, strings.Fields(value)...)
	}
	slices.Sort(entries)
	return slices.Compact(entries)
}

func operationRuntimeChanges(unit string, before, after map[string][]string) []string {
	var changes []string
	for _, property := range []string{"ActiveState", "Job", "UnitFileState", "TriggeredBy"} {
		if !slices.Equal(before[property], after[property]) {
			changes = append(changes, fmt.Sprintf("%s %s before=%q after=%q", unit, property, before[property], after[property]))
		}
	}
	return changes
}

// Ubuntu 24.04/systemd 255.4 CI (2026-09-15) reports the mask's own
// FragmentPath. All five values and the actual /dev/null link prove quietness.
func operationQuietMask(props map[string][]string) (bool, error) {
	for _, property := range []string{"LoadState", "FragmentPath", "UnitFileState", "ActiveState", "Job"} {
		if len(props[property]) != 1 {
			return false, nil
		}
	}
	if first(props, "LoadState") != "masked" ||
		!slices.Contains([]string{"masked", "masked-runtime"}, first(props, "UnitFileState")) ||
		first(props, "ActiveState") != "inactive" || first(props, "Job") != "" {
		return false, nil
	}
	path := first(props, "FragmentPath")
	if path == "/dev/null" {
		return true, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("operation-mask-unreadable: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	target, err := os.Readlink(path)
	if err != nil {
		return false, fmt.Errorf("operation-mask-unreadable: readlink %s: %w", path, err)
	}
	return target == "/dev/null", nil
}

// Source bodies can contain environment credentials; identify byte changes by
// digest while reporting pathname, identity and permissions directly.
func operationSourceValues(sources []operationSource) []operationSource {
	values := slices.Clone(sources)
	for n := range values {
		values[n].Body = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(values[n].Body)))
	}
	return values
}
