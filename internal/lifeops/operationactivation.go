package lifeops

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// AdmitQuietActivation checks only direct triggers and upholders. Every source
// must be one of the quiet backup/updater services' own retirement timers, quiet
// after its stop. Waiting units defer activity/job judgment to backup handling;
// stopped and filesystem proofs pass no waiting units or timer exceptions.
func (i *Inspector) AdmitQuietActivation(ctx context.Context, units, exceptions []string, waiting ...string) error {
	names := []string{"Id", "Names", "LoadState", "ActiveState", "Job", "TriggeredBy", "UpheldBy"}
	observations := make(map[string]map[string][]string)
	var changes []string
	read := func(unit string) (map[string][]string, error) {
		props, err := i.properties(ctx, unit, names...)
		if err != nil {
			return nil, fmt.Errorf("operation-activation-unknown: %s: %w", unit, err)
		}
		if err := requireOperationProperties(unit, props, names); err != nil {
			return nil, err
		}
		if !slices.Contains([]string{"loaded", "masked", "not-found"}, first(props, "LoadState")) {
			return nil, fmt.Errorf("operation-activation-unknown: %s load state", unit)
		}
		if first(props, "LoadState") == "not-found" {
			if first(props, "ActiveState") != "inactive" || first(props, "Job") != "" {
				return nil, fmt.Errorf("operation-activation-unknown: absent %s has activity", unit)
			}
		} else if !slices.Contains(strings.Fields(first(props, "Names")), unit) || !operationUnitName(first(props, "Id")) {
			return nil, fmt.Errorf("operation-activation-unknown: %s names", unit)
		}
		if before, exists := observations[unit]; exists {
			if err := compareOperationEvidence(unit, operationEvidence{props: before}, operationEvidence{props: props}); err != nil {
				return nil, err
			}
			changes = append(changes, operationRuntimeChanges(unit, before, props)...)
		}
		observations[unit] = props
		return props, nil
	}
	check := func() error {
		for _, unit := range units {
			props, err := read(unit)
			if err != nil {
				return err
			}
			state := first(props, "ActiveState")
			if slices.Contains(waiting, unit) && !slices.Contains([]string{"active", "inactive", "activating", "failed"}, state) {
				return fmt.Errorf("operation-activation-unknown: %s backup activity", unit)
			}
			if !slices.Contains(waiting, unit) && (!slices.Contains([]string{"active", "inactive"}, state) || first(props, "Job") != "") {
				return fmt.Errorf("operation-reactivation: %s ActiveState=%q Job=%q", unit, state, first(props, "Job"))
			}
			for _, relation := range []string{"TriggeredBy", "UpheldBy"} {
				for _, source := range strings.Fields(first(props, relation)) {
					if relation != "TriggeredBy" || !operationTimerPair(source, unit) {
						return fmt.Errorf("operation-edge-outside-set: %s %s=%s is not its own retirement timer", unit, relation, source)
					}
					from, err := read(source)
					if err != nil {
						return err
					}
					if !slices.Contains(exceptions, source) && (first(from, "ActiveState") != "inactive" || first(from, "Job") != "") {
						return fmt.Errorf("operation-reactivation: %s ActiveState=%q Job=%q", source, first(from, "ActiveState"), first(from, "Job"))
					}
				}
			}
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return fmt.Errorf("operation-activation-changed: changes=[%s]: %w", strings.Join(changes, "; "), err)
	}
	return nil
}
