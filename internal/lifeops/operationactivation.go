package lifeops

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// The manager publishes inverse edges, including instantiated unit names. Walk
// these rather than guessing a trigger's destination from its filename. PartOf
// and Requisite do not themselves pull in a start in v255; their inverses are
// included conservatively so sources behind them must also be quiet.
var operationActivationReverse = []string{
	"TriggeredBy", "UpheldBy", "OnSuccessOf", "OnFailureOf", "WantedBy",
	"RequiredBy", "BoundBy", "RequisiteOf", "ConsistsOf",
}

// AdmitQuietActivation closes the reverse activation graph of each quiet unit,
// including aliases and instances. Armed sources, active completion handlers,
// queued jobs, unreadable evidence and an exceeded bound refuse. Exceptions
// name retirement timers before their stop; filesystem proofs pass none.
func (i *Inspector) AdmitQuietActivation(ctx context.Context, units, exceptions []string) error {
	names := append([]string{"Id", "Names", "LoadState", "ActiveState", "Job"}, operationActivationReverse...)
	observations := make(map[string]map[string][]string)
	// A unit reached later by a completion/upholder edge must be judged again
	// even if it was already visited through an ordinary dependency or alias.
	type visit struct {
		unit   string
		source bool
	}
	var queue []visit
	for _, unit := range units {
		queue = append(queue, visit{unit: unit})
	}
	seen := make(map[visit]bool)
	for len(queue) != 0 {
		v := queue[0]
		queue = queue[1:]
		if seen[v] {
			continue
		}
		seen[v] = true
		props, ok := observations[v.unit]
		if !ok {
			if !operationUnitName(v.unit) || len(observations) >= operationUnitLimit {
				return fmt.Errorf("operation-activation-unknown: invalid unit or traversal bound: %s", v.unit)
			}
			var err error
			props, err = i.properties(ctx, v.unit, names...)
			if err != nil {
				return fmt.Errorf("operation-activation-unknown: %s: %w", v.unit, err)
			}
			if err := requireOperationProperties(v.unit, props, names); err != nil {
				return err
			}
			observations[v.unit] = props
		}
		if !slices.Contains([]string{"loaded", "masked", "not-found"}, first(props, "LoadState")) ||
			!slices.Contains([]string{"active", "inactive"}, first(props, "ActiveState")) {
			return fmt.Errorf("operation-activation-unknown: %s has no settled activity evidence", v.unit)
		}
		aliases := strings.Fields(first(props, "Names"))
		if first(props, "LoadState") == "not-found" {
			if first(props, "ActiveState") != "inactive" || first(props, "Job") != "" {
				return fmt.Errorf("operation-activation-unknown: absent %s has activity", v.unit)
			}
		} else if !operationUnitName(first(props, "Id")) || !slices.Contains(aliases, v.unit) || !slices.Contains(aliases, first(props, "Id")) {
			return fmt.Errorf("operation-activation-unknown: %s has incomplete names", v.unit)
		}
		armed := v.source
		for _, suffix := range []string{".path", ".socket", ".timer", ".automount"} {
			armed = armed || strings.HasSuffix(v.unit, suffix)
		}
		excepted := strings.HasSuffix(v.unit, ".timer") && slices.Contains(exceptions, v.unit)
		if !excepted && (first(props, "Job") != "" || (armed && first(props, "ActiveState") != "inactive")) {
			return fmt.Errorf("operation-reactivation: %s in the reverse activation closure is not quiet", v.unit)
		}
		for _, alias := range aliases {
			queue = append(queue, visit{unit: alias, source: v.source})
		}
		for _, relation := range operationActivationReverse {
			source := relation == "TriggeredBy" || relation == "UpheldBy" || relation == "OnSuccessOf" || relation == "OnFailureOf"
			for _, unit := range strings.Fields(first(props, relation)) {
				queue = append(queue, visit{unit: unit, source: source})
			}
		}
	}
	for unit, before := range observations {
		after, err := i.properties(ctx, unit, names...)
		if err != nil || !reflect.DeepEqual(before, after) {
			return fmt.Errorf("operation-activation-changed: %s changed or could not be read", unit)
		}
	}
	return nil
}
