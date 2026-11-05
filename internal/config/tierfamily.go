package config

import "reflect"

// SameFamily says other differs from t only in its name, its size and how many of it
// a deployment keeps, so moving a job between them changes how much room it has and
// nothing about what it runs on, how it starts, where, or under whose authority.
// `billet jobs rightsize` recommends within a family and `billet migrate workflows
// --rightsize` applies a recommendation only within one, so a report written before
// a tier changed is judged by the tier as it is.
//
// EVERY FIELD IS COMPARED BUT THOSE, so a field a later billet adds is one more that
// must match rather than one this forgot. The room a job has that nothing measures
// (its disk, its shared memory, its BuildKit cache mounts) is never made smaller. The
// workflows a trusted tier allows are each caller's to check for the workflow that
// matters, and a tier is not of its own family.
func (t Tier) SameFamily(other Tier) bool {
	return other.Label != t.Label && reflect.DeepEqual(sizeless(t), sizeless(other)) &&
		roomKept(t.Disk, other.Disk) && roomKept(t.SHM, other.SHM) &&
		roomKept(t.BuildKitCacheMountLimit, other.BuildKitCacheMountLimit)
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
