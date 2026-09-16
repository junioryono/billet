package lifeops

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// EnvironmentFile preserves the manager's optionality for each loaded input.
type EnvironmentFile struct {
	Path         string
	IgnoreErrors bool
}

// EnvironmentFiles requires a typed Service.EnvironmentFiles a(sb) reply.
// systemd 255's show printer emits no line for an empty array, even with --all:
// https://github.com/systemd/systemd/blob/v255/src/systemctl/systemctl-show.c#L1169-L1184.
func (i *Inspector) EnvironmentFiles(ctx context.Context, unit string) ([]EnvironmentFile, error) {
	reply, err := i.operationTypedProperty(ctx, unit, "org.freedesktop.systemd1.Service", "EnvironmentFiles")
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if reply.Type != "a(sb)" || json.Unmarshal(reply.Data, &rows) != nil || rows == nil {
		return nil, fmt.Errorf("operation-array-unknown: %s EnvironmentFiles has no typed array", unit)
	}
	files := make([]EnvironmentFile, 0, len(rows))
	for _, row := range rows {
		var fields []json.RawMessage
		var path *string
		var optional *bool
		if json.Unmarshal(row, &fields) != nil || len(fields) != 2 ||
			json.Unmarshal(fields[0], &path) != nil || path == nil || !filepath.IsAbs(*path) ||
			json.Unmarshal(fields[1], &optional) != nil || optional == nil {
			return nil, fmt.Errorf("operation-array-unknown: %s EnvironmentFiles has an invalid entry", unit)
		}
		files = append(files, EnvironmentFile{Path: *path, IgnoreErrors: *optional})
	}
	return files, nil
}

func (i *Inspector) operationTypedProperty(ctx context.Context, unit, iface, property string) (operationTypedValue, error) {
	bin := i.operationBusctl
	if bin == "" {
		bin = "busctl"
	}
	ctx, cancel := i.bounded(ctx)
	defer cancel()
	args := []string{"--json=short", "get-property", "org.freedesktop.systemd1", operationObjectPath(unit), iface, property}
	if i.observe != nil {
		i.observe(ctx, args)
	}
	out, err := i.run(ctx, bin, args)
	if err != nil {
		return operationTypedValue{}, fmt.Errorf("operation-array-unreadable: %s %s: %w", unit, property, err)
	}
	var reply operationTypedValue
	if json.Unmarshal(out, &reply) != nil || reply.Type == "" || len(reply.Data) == 0 || string(reply.Data) == "null" {
		return operationTypedValue{}, fmt.Errorf("operation-array-unknown: %s %s has no typed value", unit, property)
	}
	return reply, nil
}
