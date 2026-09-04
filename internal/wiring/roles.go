package wiring

import "github.com/junioryono/godi/v5"

// OperatorModules is the set a one-shot operator command builds: the config,
// the AWS chain, and the ledger in operator mode. A command that needs more
// (the allocator, the GitHub client, the issuing authority) passes the module
// in, so a status report never starts requiring a readable App key.
func OperatorModules(path ConfigPath, extra ...godi.ModuleOption) []godi.ModuleOption {
	return append([]godi.ModuleOption{
		CoreModule(Core{ConfigPath: path}),
		AWSModule(),
		LedgerModule(LedgerOperator),
	}, extra...)
}

// DecisionModules is the set the host's upgrade timer reads its instruction
// through: the one open that names no release and creates nothing.
func DecisionModules(path ConfigPath) []godi.ModuleOption {
	return []godi.ModuleOption{
		CoreModule(Core{ConfigPath: path}),
		AWSModule(),
		LedgerModule(LedgerDecision),
	}
}

// MaintenanceModules is the set the host transaction's probes build: the
// handle that crosses the maintenance fence and admits no writes.
func MaintenanceModules(path ConfigPath) []godi.ModuleOption {
	return []godi.ModuleOption{
		CoreModule(Core{ConfigPath: path}),
		AWSModule(),
		LedgerModule(LedgerProbe),
	}
}
