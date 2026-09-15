package lifeops

import (
	"crypto/sha256"
	"fmt"
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
	var entries []string
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

// All five values are required: a mask pathname alone proves no quiet state.
func operationQuietMask(props map[string][]string) bool {
	for _, property := range []string{"LoadState", "FragmentPath", "UnitFileState", "ActiveState", "Job"} {
		if len(props[property]) != 1 {
			return false
		}
	}
	return first(props, "LoadState") == "masked" && first(props, "FragmentPath") == "/dev/null" &&
		slices.Contains([]string{"masked", "masked-runtime"}, first(props, "UnitFileState")) &&
		first(props, "ActiveState") == "inactive" && first(props, "Job") == ""
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
