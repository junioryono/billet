package lifeops

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// AdmitQuietActivation requires every trigger and upholder to be inactive with
// no queued job. Exceptions name sources the admitted sequence itself stops;
// a stopped proof or post-stop filesystem mutation must pass no exceptions.
func (i *Inspector) AdmitQuietActivation(ctx context.Context, units, exceptions []string) error {
	type observation struct {
		unit  string
		names []string
		props map[string][]string
	}
	var observations []observation
	read := func(unit string, names ...string) (map[string][]string, error) {
		if !operationUnitName(unit) || len(observations) >= operationUnitLimit {
			return nil, fmt.Errorf("operation-activation-unknown: invalid unit or traversal bound: %s", unit)
		}
		props, err := i.properties(ctx, unit, names...)
		if err != nil {
			return nil, fmt.Errorf("operation-activation-unknown: %s: %w", unit, err)
		}
		if err := requireOperationProperties(unit, props, names); err != nil {
			return nil, err
		}
		observations = append(observations, observation{unit: unit, names: names, props: props})
		return props, nil
	}
	for _, unit := range units {
		props, err := read(unit, "TriggeredBy", "UpheldBy")
		if err != nil {
			return err
		}
		for _, relation := range []string{"TriggeredBy", "UpheldBy"} {
			for _, source := range strings.Fields(first(props, relation)) {
				if slices.Contains(exceptions, source) {
					continue
				}
				from, err := read(source, "LoadState", "ActiveState", "Job")
				if err != nil {
					return err
				}
				if !slices.Contains([]string{"loaded", "masked", "not-found"}, first(from, "LoadState")) ||
					first(from, "ActiveState") != "inactive" || first(from, "Job") != "" {
					return fmt.Errorf("operation-reactivation: %s %s=%s is not quiet", unit, relation, source)
				}
			}
		}
	}
	for _, before := range observations {
		after, err := i.properties(ctx, before.unit, before.names...)
		if err != nil || !reflect.DeepEqual(before.props, after) {
			return fmt.Errorf("operation-activation-changed: %s changed or could not be read", before.unit)
		}
	}
	return nil
}
