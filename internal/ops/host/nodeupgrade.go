package host

import (
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
)

// CheckNodeStateDir refuses a node.state_dir too long to hold the node's upgrade
// answer socket, which is what stops the node at its start, for a caller that
// chooses the directory and can say so before anything is created.
func CheckNodeStateDir(stateDir string) error {
	return node.ExecUpgrader{AckDir: stateDir}.Check()
}

// nodeUpgrader carries the controller's ledger environment on a host running both roles.
func NodeUpgrader(cfg *config.Config, configPath string) (node.ExecUpgrader, error) {
	upgrader := node.ExecUpgrader{ConfigPath: configPath, AckDir: cfg.Node.StateDir}
	if cfg.Server != nil {
		upgrader.DSNEnv = cfg.Server.LedgerDSNEnv()
	}

	return upgrader, upgrader.Check()
}
