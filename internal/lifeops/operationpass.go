package lifeops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// operationPass is discarded before the comparison reread and on return from
// AdmitOperations. It never caches a verdict or crosses a retirement step.
type operationPass struct {
	retained  []string
	shown     map[string]map[string][]string
	execution map[string]map[string][]string
	typed     map[string]map[string]operationTypedValue
}

type operationTypedValue struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func newOperationPass(retained []string) *operationPass {
	return &operationPass{retained: retained, shown: make(map[string]map[string][]string),
		execution: make(map[string]map[string][]string), typed: make(map[string]map[string]operationTypedValue)}
}

func cloneOperationProperties(props map[string][]string) map[string][]string {
	cloned := make(map[string][]string, len(props))
	for name, values := range props {
		cloned[name] = slices.Clone(values)
	}
	return cloned
}

func (p *operationPass) properties(ctx context.Context, i *Inspector, unit string, names []string) (map[string][]string, error) {
	props, ok := p.shown[unit]
	if !ok {
		all := append(slices.Clone(operationUnitProperties), operationRelations...)
		all = append(all, operationExecutionProperties...)
		all = append(all, "PIDFile", "KillMode", "Where", "What", "Type")
		if slices.Equal(names, []string{"Id", "Names", "LoadState", "ActiveState", "Job", "StopWhenUnneeded"}) {
			all = slices.Clone(names)
		}
		slices.Sort(all)
		all = slices.Compact(all)
		reader := *i
		reader.operationPass = nil
		var err error
		if slices.Contains(p.retained, unit) {
			// Property filters are literal in systemd 255. Only an unfiltered
			// show --all retains the inventory, including future path carriers.
			props, err = reader.properties(ctx, unit)
		} else {
			props, err = reader.properties(ctx, unit, strings.Join(all, ","))
		}
		if err != nil {
			return nil, err
		}
		p.shown[unit] = props
	}
	if len(names) == 0 {
		return cloneOperationProperties(props), nil
	}
	selected := make(map[string][]string, len(names))
	for _, name := range names {
		if values, ok := props[name]; ok {
			selected[name] = slices.Clone(values)
		}
	}
	return selected, nil
}

// GetAll with an empty interface reads Unit and Service in one typed call.
// systemd v255 bus-objects.c:1255 maps the empty interface to all vtables;
// busctl.c json_transform_dict_array renders a{sv} as an object of variants.
// https://github.com/systemd/systemd/blob/v255/src/libsystemd/sd-bus/bus-objects.c#L1255
func (p *operationPass) typedProperties(ctx context.Context, i *Inspector, unit string) (map[string]operationTypedValue, error) {
	if values, ok := p.typed[unit]; ok {
		return values, nil
	}
	bin := i.operationBusctl
	if bin == "" {
		bin = "busctl"
	}
	args := []string{"--json=short", "call", "org.freedesktop.systemd1", operationObjectPath(unit),
		"org.freedesktop.DBus.Properties", "GetAll", "s", ""}
	ctx, cancel := i.bounded(ctx)
	defer cancel()
	if i.observe != nil {
		i.observe(ctx, args)
	}
	out, err := i.run(ctx, bin, args)
	if err != nil {
		return nil, fmt.Errorf("retained-node-path-unknown: read %s typed properties: %w", unit, err)
	}
	var reply struct {
		Type string                           `json:"type"`
		Data []map[string]operationTypedValue `json:"data"`
	}
	if err := json.Unmarshal(out, &reply); err != nil || reply.Type != "a{sv}" || len(reply.Data) != 1 || reply.Data[0] == nil {
		return nil, fmt.Errorf("retained-node-path-unknown: %s has no complete typed property dictionary", unit)
	}
	p.typed[unit] = reply.Data[0]
	return reply.Data[0], nil
}
