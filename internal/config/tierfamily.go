package config

import (
	"reflect"
	"slices"
	"strings"
)

// SameFamily says other differs from t only in its name, its size and how many of it
// a deployment keeps, so moving a job between them changes how much room it has and
// nothing about what it runs on, how it starts, where, or under whose authority.
// `billet jobs rightsize` recommends within a family and `billet migrate workflows
// --rightsize` applies a recommendation only within one, so a report written before
// a tier changed is judged by the tier as it is.
func (t Tier) SameFamily(other Tier) bool { return t.FamilyDifference(other) == "" }

// FamilyDifference says why other is not of t's family, and is empty when it is.
//
// EVERY FIELD IS COMPARED BUT THOSE, so a field a later billet adds is one more that
// must match rather than one this forgot. The room a job has that nothing measures
// (its disk, its shared memory, its BuildKit cache mounts) is never made smaller. The
// workflows a trusted tier allows are each caller's to check for the workflow that
// matters (AllowsWorkflow), and a tier is not of its own family.
func (t Tier) FamilyDifference(other Tier) string {
	switch {
	case other.Label == t.Label:
		return "it is the same tier"
	case !reflect.DeepEqual(sizeless(t), sizeless(other)):
		return "a setting other than its name, size and capacity differs"
	case !roomKept(t.Disk, other.Disk) || !roomKept(t.SHM, other.SHM) ||
		!roomKept(t.BuildKitCacheMountLimit, other.BuildKitCacheMountLimit):
		return "it has less disk, shared memory or BuildKit cache mount room"
	}

	return ""
}

// AllowsWorkflow says the tier takes a job of workflow, named owner/name/path as
// tiers[].workflows names it, without the ref. An untrusted tier takes any. A trusted
// one lists the workflows its runner group admits, and a job moved there without being
// listed would queue forever; the ref is GitHub's to judge, so only the file is
// compared.
//
// THE REPOSITORY IS COMPARED WITHOUT CASE, AS GITHUB NAMES IT, AND THE FILE WITH:
// ci.yml and CI.yml are two files a repository can hold.
func (t Tier) AllowsWorkflow(workflow string) bool {
	if t.Trust.Effective() != WorkloadTrusted {
		return true
	}
	repository, file := splitWorkflow(workflow)

	return slices.ContainsFunc(t.Workflows, func(allowed string) bool {
		path, _, _ := strings.Cut(allowed, "@")
		allowedRepository, allowedFile := splitWorkflow(path)

		return strings.EqualFold(allowedRepository, repository) && allowedFile == file
	})
}

// splitWorkflow is owner/name/path's repository, owner/name, and its file.
func splitWorkflow(workflow string) (string, string) {
	owner, rest, _ := strings.Cut(workflow, "/")
	name, file, _ := strings.Cut(rest, "/")

	return owner + "/" + name, file
}

// sizeless is a tier with what may differ within a family cleared.
func sizeless(t Tier) Tier {
	t.Label, t.RunsOn, t.Declaration, t.Targets = "", "", "", TierTargets{}
	t.VCPU, t.Memory, t.Sizes, t.MemoryPerVCPU = 0, 0, nil, 0
	t.Disk, t.SHM, t.BuildKitCacheMountLimit = 0, 0, 0
	t.WarmPool, t.MaxConcurrent, t.Reserved, t.Workflows = 0, 0, 0, nil

	return t
}

// roomKept says other gives a job at least the room current does, where zero is the
// backend's default: a default equals only a default, since what it comes to is the
// backend's to say.
func roomKept(current, other ByteSize) bool {
	return other == current || (current != 0 && other > current)
}
