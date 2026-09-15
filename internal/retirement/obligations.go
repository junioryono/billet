package retirement

import (
	"errors"
	"path/filepath"
	"strconv"
)

// RetainedInvocation is the pre-handoff node evidence recorded before intent.
// An old journal without it cannot prove that shutdown left this node intact.
type RetainedInvocation struct {
	InvocationID      string             `json:"invocation_id"`
	MainPID           string             `json:"main_pid"`
	Deployment        string             `json:"deployment"`
	Node              string             `json:"node"`
	Incarnation       string             `json:"incarnation"`
	Endpoint          string             `json:"endpoint"`
	Resources         []RetainedResource `json:"resources"`
	ConfigPath        string             `json:"config_path,omitempty"`
	Provider          string             `json:"provider,omitempty"`
	Services          []RetainedService  `json:"services,omitempty"`
	IdentityDir       string             `json:"identity_dir,omitempty"`
	ConfigReplacement *RetainedResource  `json:"config_replacement,omitempty"`
}

// RetainedService binds the guest network to its original active invocation.
type RetainedService struct {
	Unit         string `json:"unit"`
	InvocationID string `json:"invocation_id"`
	MainPID      string `json:"main_pid"`
}

// RetainedResource records object identity and ownership, not mutable directory
// contents. Runtime marks only recreatable registration/lock records; its zero
// value requires persistence. Registration also has its separate trusted reader.
type RetainedResource struct {
	Path         string `json:"path"`
	ResolvedPath string `json:"resolved_path,omitempty"`
	Runtime      bool   `json:"runtime,omitempty"`
	GuestNetwork bool   `json:"guest_network,omitempty"`
	Absent       bool   `json:"absent,omitempty"`
	Device       uint64 `json:"device,omitempty"`
	Inode        uint64 `json:"inode,omitempty"`
	Mode         uint32 `json:"mode,omitempty"`
	UID          uint32 `json:"uid,omitempty"`
	GID          uint32 `json:"gid,omitempty"`
}

// RemainingServiceActions is the service work left by the executable decision.
// It does not dispatch a phase or authorize an operation on the host.
func RemainingServiceActions(variant Variant, phase Phase, decision Decision) []Action {
	switch decision.Action {
	case ActionAwaitBackup, ActionStop, ActionArchive, ActionAdvanceArchived, ActionRewrite, ActionAdvanceRewritten, ActionRestart:
	default:
		return nil
	}
	var actions []Action
	if phase == PhaseIntent {
		actions = append(actions, ActionStop)
	}
	if variant == VariantRetainedNode {
		actions = append(actions, ActionRestart)
	}
	return actions
}

func (j *Journal) retainedInvocationWellFormed() error {
	original := j.RetainedInvocation
	if original == nil {
		return nil
	}
	if j.Variant != VariantRetainedNode || !transitionIDPattern.MatchString(original.InvocationID) ||
		original.Deployment != j.Deployment || original.Node == "" || original.Incarnation == "" || original.Endpoint == "" {
		return errors.New("journal carries invalid retained invocation evidence")
	}
	pid, err := strconv.ParseUint(original.MainPID, 10, 32)
	if err != nil || pid == 0 || len(original.Resources) == 0 {
		return errors.New("journal's retained invocation has no process or resources")
	}
	if original.ConfigPath != "" && (!filepath.IsAbs(original.ConfigPath) || filepath.Clean(original.ConfigPath) != original.ConfigPath) {
		return errors.New("journal carries an invalid retained configuration path")
	}
	if original.IdentityDir != "" && original.IdentityDir != j.IdentityDir {
		return errors.New("journal carries a different retained identity directory")
	}
	if replacement := original.ConfigReplacement; replacement != nil {
		if replacement.Path != original.ConfigPath || replacement.Absent || replacement.Runtime || replacement.GuestNetwork ||
			replacement.Inode == 0 || !filepath.IsAbs(replacement.ResolvedPath) || filepath.Clean(replacement.ResolvedPath) != replacement.ResolvedPath {
			return errors.New("journal carries invalid retained configuration replacement evidence")
		}
	}
	seen := make(map[string]bool)
	for _, resource := range original.Resources {
		if !filepath.IsAbs(resource.Path) || filepath.Clean(resource.Path) != resource.Path || seen[resource.Path] {
			return errors.New("journal carries an invalid or repeated retained resource path")
		}
		if resource.Runtime && resource.GuestNetwork {
			return errors.New("journal marks a required guest-network resource disposable")
		}
		seen[resource.Path] = true
		if resource.Absent && (resource.Device != 0 || resource.Inode != 0 || resource.Mode != 0 || resource.UID != 0 || resource.GID != 0) {
			return errors.New("journal's absent retained resource carries object identity")
		}
		if !resource.Absent && resource.Inode == 0 {
			return errors.New("journal's retained resource has no object identity")
		}
	}
	return nil
}
