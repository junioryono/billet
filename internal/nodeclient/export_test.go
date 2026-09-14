package nodeclient

import "io"

// The record writer's two seams, for the fixtures in the external test
// package. Each returns the function that restores the default; the fixtures
// that use them are serial.

// SetInvocationIDForTest replaces how the record reads the process's systemd
// invocation.
func SetInvocationIDForTest(f func() string) func() {
	saved := invocationID
	invocationID = f

	return func() { invocationID = saved }
}

// SetInstallRecordForTest replaces the one call the record writer makes into
// the durable installer.
func SetInstallRecordForTest(f func(dir, name string, write func(io.Writer) error) (string, error)) func() {
	saved := installRecord
	installRecord = f

	return func() { installRecord = saved }
}

// DefaultInstallRecord is the production installation call, for a fixture
// that wraps it.
func DefaultInstallRecord() func(dir, name string, write func(io.Writer) error) (string, error) {
	return installRecord
}

// RecordFieldNames are the record's seven members, in the order the type
// declares them, for a fixture that pins the set by name.
var RecordFieldNames = []string{"schema", "node", "deployment", "incarnation", "invocation_id", "endpoint", "registered_at"}

// SetDrainServingDoneForTest installs the hook the drain's serving worker
// calls as it returns.
func SetDrainServingDoneForTest(f func()) func() {
	saved := drainServingDone
	drainServingDone = f

	return func() { drainServingDone = saved }
}
