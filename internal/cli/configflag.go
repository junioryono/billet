package cli

import (
	"flag"
	"os"
	"path/filepath"
)

// DefaultConfigPath deliberately does NOT look in the working directory.
//
// A server started from an attacker-writable directory would otherwise silently
// adopt that directory's billet.yaml — which chooses the state directory, the
// GitHub App key path, and every tier's resources. For a process that is often
// run as root by a unit file, that is privileged config injection. Use --config
// to point anywhere else.
func DefaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "billet", "billet.yaml")
	}

	return "/etc/billet/billet.yaml"
}

// AddConfigFlag defines --config on fs, defaulting to DefaultConfigPath.
func AddConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", DefaultConfigPath(), "path to billet.yaml")
}
