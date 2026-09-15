package lifeops

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type operationSetupValue struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type operationSetupProperty struct {
	Name   string `xml:"name,attr"`
	Type   string `xml:"type,attr"`
	Access string `xml:"access,attr"`
}

// Enumerating the whole type interface closes the reader as well as the policy:
// a new manager property is read and refused, never silently left unqueried.
// v255 registers exec, kill and cgroup vtables on this same type interface:
// https://github.com/systemd/systemd/blob/v255/src/core/dbus.c
// https://github.com/systemd/systemd/blob/v255/src/core/dbus-execute.c
func (i *Inspector) operationSetup(ctx context.Context, unit string) (map[string]operationSetupValue, error) {
	// Transient services can retain caller-supplied stdio descriptors whose
	// identities are not exported by the Service properties. No complete
	// setup proof is possible for those objects.
	metadata, err := i.properties(ctx, unit, "Transient")
	if err != nil || len(metadata["Transient"]) != 1 || first(metadata, "Transient") != "no" {
		return nil, fmt.Errorf("operation-setup-unknown: %s transient descriptor evidence", unit)
	}
	iface := operationExecutionInterface(unit)
	bin := i.operationBusctl
	if bin == "" {
		bin = "busctl"
	}
	ctx, cancel := i.bounded(ctx)
	defer cancel()
	run := func(args []string) ([]byte, error) {
		if i.observe != nil {
			i.observe(ctx, args)
		}
		return i.run(ctx, bin, args)
	}
	out, err := run([]string{"--xml-interface", "introspect", "org.freedesktop.systemd1", operationObjectPath(unit), iface})
	if err != nil {
		return nil, fmt.Errorf("operation-setup-unreadable: %s inventory: %w", unit, err)
	}
	var node struct {
		Interfaces []struct {
			Name       string                   `xml:"name,attr"`
			Properties []operationSetupProperty `xml:"property"`
		} `xml:"interface"`
	}
	if err := xml.Unmarshal(out, &node); err != nil {
		return nil, fmt.Errorf("operation-setup-unknown: %s inventory: %w", unit, err)
	}
	var properties []operationSetupProperty
	for _, entry := range node.Interfaces {
		if entry.Name == iface {
			if properties != nil {
				return nil, fmt.Errorf("operation-setup-unknown: duplicate interface for %s", unit)
			}
			properties = entry.Properties
		}
	}
	if len(properties) == 0 || len(properties) > 1024 {
		return nil, fmt.Errorf("operation-setup-unknown: %s empty or excessive inventory", unit)
	}
	// v255 hides these obsolete aliases from both introspection and GetAll,
	// but still exposes them to explicit Get. Include that complete hidden
	// suffix from dbus-execute.c and dbus-service.c, not just the public XML.
	hidden := []operationSetupProperty{
		{Name: "Capabilities", Type: "s", Access: "read"},
		{Name: "ReadWriteDirectories", Type: "as", Access: "read"},
		{Name: "ReadOnlyDirectories", Type: "as", Access: "read"},
		{Name: "InaccessibleDirectories", Type: "as", Access: "read"},
		{Name: "IOScheduling", Type: "i", Access: "read"},
	}
	if strings.HasSuffix(unit, ".service") {
		hidden = append(hidden,
			operationSetupProperty{Name: "PermissionsStartOnly", Type: "b", Access: "read"},
			operationSetupProperty{Name: "StartLimitInterval", Type: "t", Access: "read"},
			operationSetupProperty{Name: "StartLimitBurst", Type: "u", Access: "read"},
			operationSetupProperty{Name: "StartLimitAction", Type: "s", Access: "read"},
			operationSetupProperty{Name: "FailureAction", Type: "s", Access: "read"},
			operationSetupProperty{Name: "RebootArgument", Type: "s", Access: "read"})
	}
	for _, property := range hidden {
		if !slices.ContainsFunc(properties, func(p operationSetupProperty) bool { return p.Name == property.Name }) {
			properties = append(properties, property)
		}
	}
	slices.SortFunc(properties, func(a, b operationSetupProperty) int { return strings.Compare(a.Name, b.Name) })
	args := []string{"--json=short", "get-property", "org.freedesktop.systemd1", operationObjectPath(unit), iface}
	for n, property := range properties {
		if property.Name == "" || property.Type == "" || property.Access != "read" || (n > 0 && property.Name == properties[n-1].Name) {
			return nil, fmt.Errorf("operation-setup-unknown: %s invalid property inventory", unit)
		}
		args = append(args, property.Name)
	}
	out, err = run(args)
	if err != nil {
		return nil, fmt.Errorf("operation-setup-unreadable: %s values: %w", unit, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(properties) {
		return nil, fmt.Errorf("operation-setup-unknown: %s incomplete values", unit)
	}
	values := make(map[string]operationSetupValue, len(properties))
	for n, property := range properties {
		var value operationSetupValue
		if err := json.Unmarshal([]byte(lines[n]), &value); err != nil || value.Type != property.Type || len(value.Data) == 0 {
			return nil, fmt.Errorf("operation-setup-unknown: %s %s invalid typed value", unit, property.Name)
		}
		var compact bytes.Buffer
		if string(value.Data) == "null" || json.Compact(&compact, value.Data) != nil {
			return nil, fmt.Errorf("operation-setup-unknown: %s %s has no value", unit, property.Name)
		}
		value.Data = slices.Clone(compact.Bytes())
		values[property.Name] = value
	}
	// A truncated inventory is not proof of an empty execution context.
	for _, name := range []string{"StandardInput", "StandardOutput", "StandardError", "User", "PrivateTmp", "LoadCredential", "RuntimeDirectory", "Delegate"} {
		if _, ok := values[name]; !ok {
			return nil, fmt.Errorf("operation-setup-unknown: %s missing %s", unit, name)
		}
	}
	return values, nil
}

func (w *operationWalk) admitSetup(ctx context.Context, effect Operation, ev operationEvidence) error {
	if operationExecutionInterface(effect.Unit) == "" || protectedOperationUnit(effect.Unit, w.protection.Units) {
		return nil
	}
	values, err := w.inspector.operationSetup(ctx, effect.Unit)
	if err != nil {
		return err
	}
	// service_collect_fds in v255 src/core/service.c takes activation sockets
	// from TriggeredBy. Those descriptors and namespace joins are cross-unit
	// setup, not a helper's program semantics.
	if first(ev.props, "JoinsNamespaceOf") != "" {
		return fmt.Errorf("operation-setup-unsupported: %s JoinsNamespaceOf", effect.Unit)
	}
	for _, source := range strings.Fields(first(ev.props, "TriggeredBy")) {
		if strings.HasSuffix(source, ".socket") {
			return fmt.Errorf("operation-setup-unsupported: %s socket descriptors", effect.Unit)
		}
	}
	for name, value := range values {
		if _, directory := operationDirectoryRoots[name]; directory {
			var entries []string
			if value.Type != "as" || json.Unmarshal(value.Data, &entries) != nil || string(value.Data) == "null" ||
				strings.Join(entries, " ") != first(ev.props, name) {
				return fmt.Errorf("operation-setup-unknown: %s %s disagrees with directory evidence", effect.Unit, name)
			}
			continue
		}
		if !operationSetupAllowed(name, value) {
			// Never include the value: credential properties may contain secrets.
			return fmt.Errorf("operation-setup-unsupported: %s %s", effect.Unit, name)
		}
	}
	if before, ok := w.setups[effect.Unit]; ok && !reflect.DeepEqual(before, values) {
		return fmt.Errorf("operation-setup-changed: %s", effect.Unit)
	}
	w.setups[effect.Unit] = values
	return nil
}

// All cases are explicit property names, never name prefixes or a fallback by
// type. Numeric process limits, scheduling knobs and observations do not open
// files or select other units. Directory modes apply only to admitted paths.
// Commands are intentionally outside the manager-effects threat boundary.
// Sources: systemd v255 src/core/dbus-{execute,service,kill,cgroup,unit}.c.
func operationSetupAllowed(name string, value operationSetupValue) bool {
	scalar := func(kind string, allowed ...string) bool {
		return value.Type == kind && slices.Contains(allowed, string(value.Data))
	}
	text := func(allowed ...string) bool {
		var s string
		return value.Type == "s" && json.Unmarshal(value.Data, &s) == nil && slices.Contains(allowed, s)
	}
	number := func(kind string) bool {
		if value.Type != kind {
			return false
		}
		if kind == "i" {
			_, err := strconv.ParseInt(string(value.Data), 10, 32)
			return err == nil
		}
		bits := 64
		if kind == "u" {
			bits = 32
		}
		_, err := strconv.ParseUint(string(value.Data), 10, bits)
		return err == nil
	}
	switch name {
	case "StandardInput":
		return text("null")
	case "StandardOutput", "StandardError":
		return text("journal", "inherit", "null")
	case "StandardInputFileDescriptorName", "StandardOutputFileDescriptorName", "StandardErrorFileDescriptorName",
		"RootDirectory", "RootImage", "RootHashPath", "RootHashSignaturePath", "RootVerity", "TTYPath", "LogNamespace",
		"PAMName", "UtmpIdentifier", "NetworkNamespacePath", "IPCNamespacePath", "DelegateSubgroup", "PIDFile",
		"BusName", "USBFunctionDescriptors", "USBFunctionStrings", "ControlGroup", "Capabilities", "RebootArgument":
		return text("")
	case "User", "Group":
		return text("", "root", "0")
	case "WorkingDirectory":
		return text("", "/")
	case "StartLimitAction", "FailureAction":
		return text("none")
	case "KeyringMode":
		// A private session keyring is created for this process; shared mode
		// links the user's keyring and is outside this isolated setup policy.
		return text("private")
	case "UtmpMode":
		return text("init")
	case "ProtectHome", "ProtectSystem", "RuntimeDirectoryPreserve":
		return text("no")
	case "ProtectProc":
		return text("default")
	case "ProcSubset":
		return text("all")
	case "Type":
		return text("simple", "exec", "oneshot", "idle")
	case "ExitType":
		return text("main")
	case "Restart":
		return text("no")
	case "RestartMode":
		return text("normal")
	case "NotifyAccess":
		return text("none")
	case "TimeoutStartFailureMode", "TimeoutStopFailureMode":
		return text("terminate", "kill")
	case "KillMode":
		return text("control-group", "mixed")
	case "OOMPolicy":
		return text("stop", "kill", "continue")
	case "FileDescriptorStorePreserve":
		return text("restart", "no")
	case "DevicePolicy":
		return text("auto")
	case "ManagedOOMSwap", "ManagedOOMMemoryPressure":
		return text("auto")
	case "ManagedOOMPreference":
		return text("none")
	case "MemoryPressureWatch":
		return text("auto", "off")
	case "Slice":
		return text("system.slice")
	case "PrivateTmp", "PrivateDevices", "PrivateNetwork", "PrivateUsers", "PrivateMounts", "PrivateIPC",
		"ProtectClock", "ProtectKernelTunables", "ProtectKernelModules", "ProtectKernelLogs", "ProtectControlGroups",
		"ProtectHostname", "DynamicUser", "RemoveIPC", "RootEphemeral", "MountAPIVFS", "TTYReset", "TTYVHangup",
		"TTYVTDisallocate", "Delegate", "CoredumpReceive", "SameProcessGroup", "NonBlocking", "PermissionsStartOnly":
		return scalar("b", "false")
	case "CPUAffinityFromNUMA", "CPUSchedulingResetOnFork", "SyslogLevelPrefix", "SetLoginEnvironment",
		"IgnoreSIGPIPE", "NoNewPrivileges", "LockPersonality", "MemoryDenyWriteExecute", "RestrictRealtime",
		"RestrictSUIDSGID", "MemoryKSM", "RootDirectoryStartOnly", "RemainAfterExit", "GuessMainPID", "SendSIGKILL",
		"SendSIGHUP", "CPUAccounting", "IOAccounting", "BlockIOAccounting", "MemoryAccounting", "TasksAccounting", "IPAccounting":
		return scalar("b", "false", "true")
	case "Environment", "PassEnvironment", "UnsetEnvironment", "ExtensionDirectories", "ImportCredential", "SupplementaryGroups",
		"ReadWriteDirectories", "ReadOnlyDirectories", "InaccessibleDirectories", "ReadWritePaths", "ReadOnlyPaths", "InaccessiblePaths", "ExecPaths", "NoExecPaths", "ExecSearchPath", "SystemCallArchitectures",
		"DelegateControllers", "DisableControllers", "IPIngressFilterPath", "IPEgressFilterPath":
		return scalar("as", "[]")
	case "EnvironmentFiles":
		return scalar("a(sb)", "[]")
	case "SetCredential", "SetCredentialEncrypted":
		return scalar("a(say)", "[]")
	case "LoadCredential", "LoadCredentialEncrypted", "RootImageOptions", "TemporaryFileSystem", "DeviceAllow", "BPFProgram":
		return scalar("a(ss)", "[]")
	case "StateDirectorySymlink", "RuntimeDirectorySymlink", "CacheDirectorySymlink", "LogsDirectorySymlink", "OpenFile":
		return scalar("a(sst)", "[]")
	case "BindPaths", "BindReadOnlyPaths":
		return scalar("a(ssbt)", "[]")
	case "MountImages":
		return scalar("a(ssba(ss))", "[]")
	case "ExtensionImages":
		return scalar("a(sba(ss))", "[]")
	case "RootHash", "RootHashSignature", "CPUAffinity", "NUMAMask", "StandardInputData", "AllowedCPUs", "StartupAllowedCPUs",
		"AllowedMemoryNodes", "StartupAllowedMemoryNodes":
		return scalar("ay", "[]")
	case "EffectiveCPUs", "EffectiveMemoryNodes":
		var mask []uint8
		return value.Type == "ay" && string(value.Data) != "null" && json.Unmarshal(value.Data, &mask) == nil
	case "LogExtraFields":
		return scalar("aay", "[]")
	case "LogFilterPatterns":
		return scalar("a(bs)", "[]")
	case "SELinuxContext", "AppArmorProfile", "SmackProcessLabel":
		return scalar("(bs)", `[false,""]`)
	case "SystemCallFilter", "SystemCallLog", "RestrictAddressFamilies", "RestrictFileSystems", "RestrictNetworkInterfaces":
		return scalar("(bas)", "[false,[]]")
	case "RestartPreventExitStatus", "RestartForceExitStatus", "SuccessExitStatus":
		return scalar("(aiai)", "[[],[]]")
	case "IODeviceWeight", "IOReadBandwidthMax", "IOWriteBandwidthMax", "IOReadIOPSMax", "IOWriteIOPSMax",
		"IODeviceLatencyTargetUSec", "BlockIODeviceWeight", "BlockIOReadBandwidth", "BlockIOWriteBandwidth":
		return scalar("a(st)", "[]")
	case "IPAddressAllow", "IPAddressDeny":
		return scalar("a(iayu)", "[]")
	case "SocketBindAllow", "SocketBindDeny":
		return scalar("a(iiqq)", "[]")
	case "NFTSet":
		return scalar("a(iiss)", "[]")
	case "FileDescriptorStoreMax", "NFileDescriptorStore", "MainPID", "ControlPID":
		return scalar("u", "0")
	case "TTYRows", "TTYColumns":
		// exec_context_init uses UINT_MAX; the v255 bus property is uint16.
		// Neither value touches a terminal with every TTY mechanism disabled.
		return scalar("q", "0", "65535")
	case "MountFlags":
		return scalar("t", "0")
	case "UMask", "RuntimeDirectoryMode", "StateDirectoryMode", "CacheDirectoryMode", "LogsDirectoryMode", "ConfigurationDirectoryMode",
		"StartLimitBurst", "RestartSteps", "UID", "GID", "NRestarts", "LogRateLimitBurst", "ManagedOOMMemoryPressureLimit", "ExecMainPID":
		return number("u")
	case "IOScheduling", "StatusErrno", "OOMScoreAdjust", "Nice", "IOSchedulingClass", "IOSchedulingPriority", "CPUSchedulingPolicy", "CPUSchedulingPriority",
		"NUMAPolicy", "SyslogPriority", "SyslogLevel", "SyslogFacility", "LogLevelMax", "SecureBits", "SystemCallErrorNumber",
		"KillSignal", "RestartKillSignal", "FinalKillSignal", "WatchdogSignal", "ReloadSignal", "ExecMainCode", "ExecMainStatus":
		return number("i")
	case "StartLimitInterval", "LimitCPU", "LimitCPUSoft", "LimitFSIZE", "LimitFSIZESoft", "LimitDATA", "LimitDATASoft", "LimitSTACK", "LimitSTACKSoft",
		"LimitCORE", "LimitCORESoft", "LimitRSS", "LimitRSSSoft", "LimitNOFILE", "LimitNOFILESoft", "LimitAS", "LimitASSoft",
		"LimitNPROC", "LimitNPROCSoft", "LimitMEMLOCK", "LimitMEMLOCKSoft", "LimitLOCKS", "LimitLOCKSSoft", "LimitSIGPENDING",
		"LimitSIGPENDINGSoft", "LimitMSGQUEUE", "LimitMSGQUEUESoft", "LimitNICE", "LimitNICESoft", "LimitRTPRIO", "LimitRTPRIOSoft",
		"LimitRTTIME", "LimitRTTIMESoft", "CoredumpFilter", "TimerSlackNSec", "CapabilityBoundingSet", "AmbientCapabilities",
		"LogRateLimitIntervalUSec", "RestrictNamespaces", "TimeoutCleanUSec", "RestartUSec", "RestartMaxDelayUSec", "RestartUSecNext",
		"TimeoutStartUSec", "TimeoutStopUSec", "TimeoutAbortUSec", "RuntimeMaxUSec", "RuntimeRandomizedExtraUSec", "WatchdogUSec",
		"WatchdogTimestamp", "WatchdogTimestampMonotonic", "ExecMainStartTimestamp", "ExecMainStartTimestampMonotonic",
		"ExecMainExitTimestamp", "ExecMainExitTimestampMonotonic", "CPUWeight", "StartupCPUWeight", "CPUShares", "StartupCPUShares",
		"CPUQuotaPerSecUSec", "CPUQuotaPeriodUSec", "IOWeight", "StartupIOWeight", "BlockIOWeight", "StartupBlockIOWeight",
		"DefaultMemoryLow", "DefaultStartupMemoryLow", "DefaultMemoryMin", "MemoryMin", "MemoryLow", "StartupMemoryLow",
		"MemoryHigh", "StartupMemoryHigh", "MemoryMax", "StartupMemoryMax", "MemorySwapMax", "StartupMemorySwapMax",
		"MemoryZSwapMax", "StartupMemoryZSwapMax", "MemoryLimit", "TasksMax", "MemoryPressureThresholdUSec",
		"ControlGroupId", "MemoryCurrent", "MemoryPeak", "MemorySwapCurrent", "MemorySwapPeak", "MemoryZSwapCurrent",
		"MemoryAvailable", "CPUUsageNSec", "TasksCurrent", "IPIngressBytes", "IPIngressPackets", "IPEgressBytes",
		"IPEgressPackets", "IOReadBytes", "IOReadOperations", "IOWriteBytes", "IOWriteOperations":
		return number("t")
	case "SyslogIdentifier", "StatusText", "Result", "ReloadResult", "CleanResult", "Personality",
		"RootImagePolicy", "MountImagePolicy", "ExtensionImagePolicy":
		// Observations or configuration of disabled mechanisms: images are
		// empty above, and personality/log metadata only affect this process.
		var s string
		return value.Type == "s" && json.Unmarshal(value.Data, &s) == nil
	case "ExecCondition", "ExecStartPre", "ExecStart", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost":
		return operationSetupCommands(value, "a(sasbttttuii)")
	case "ExecConditionEx", "ExecStartPreEx", "ExecStartEx", "ExecStartPostEx", "ExecReloadEx", "ExecStopEx", "ExecStopPostEx":
		return operationSetupCommands(value, "a(sasasttttuii)")
	default:
		return false
	}
}

func operationSetupCommands(value operationSetupValue, signature string) bool {
	var commands []json.RawMessage
	return value.Type == signature && string(value.Data) != "null" && json.Unmarshal(value.Data, &commands) == nil
}
