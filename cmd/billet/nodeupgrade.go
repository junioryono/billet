package main

import (
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
)

// nodeUpgrader carries the controller's ledger environment on a host running both roles.
func nodeUpgrader(cfg *config.Config, configPath string) (node.ExecUpgrader, error) {
	upgrader := node.ExecUpgrader{ConfigPath: configPath, AckDir: cfg.Node.StateDir}
	if cfg.Server != nil {
		upgrader.DSNEnv = cfg.Server.LedgerDSNEnv()
	}

	return upgrader, upgrader.Check()
}
