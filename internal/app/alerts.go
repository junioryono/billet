package app

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"

	"github.com/junioryono/billet/internal/alerts"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/server"
)

// maxAlertURLBytes is the longest address file read: the file is an address, and one
// this long is something else.
const maxAlertURLBytes = 4096

// AlertAddress reads one destination's address from where the config says it is.
//
// EVERY ERROR NAMES WHERE IT LOOKED AND NOTHING OF WHAT IT FOUND, because what it found
// may be the credential with a typo in it. The value is read here and nowhere else, so
// the type that redacts it is what every later reader holds.
//
// getenv is the environment it reads: the process's for the control plane, and a
// command's own for one an operator runs.
func AlertAddress(d *config.AlertDestination, getenv func(string) string) (alerts.URL, error) {
	var raw string

	if d.URLEnv != "" {
		raw = getenv(d.URLEnv)
		if raw == "" {
			return "", fmt.Errorf("the environment variable %s is empty; export it in the service's environment", d.URLEnv)
		}
	} else {
		read, err := readAlertURLFile(d.URLFile)
		if err != nil {
			return "", err
		}
		raw = read
	}

	address, err := alerts.ParseURL(raw)
	if err != nil {
		return "", fmt.Errorf("the address from %s is not usable: %w", alertAddressSource(d), err)
	}

	return address, nil
}

func alertAddressSource(d *config.AlertDestination) string {
	if d.URLEnv != "" {
		return "the environment variable " + d.URLEnv
	}

	return d.URLFile
}

// readAlertURLFile reads an address file through regularfile, so a FIFO or a device at
// the path is refused before anything is read, and refuses one readable beyond its
// owner, as the App key's reader does.
func readAlertURLFile(path string) (string, error) {
	f, info, err := regularfile.Open(path, regularfile.Options{})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("the file %s does not exist", path)
		}

		return "", fmt.Errorf("the file %s could not be opened: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf("the file %s is mode %04o; it holds a credential, so it may be readable only by "+
				"its owner", path, perm)
		}
	}

	body, err := regularfile.ReadAllLimited(f, path, maxAlertURLBytes)
	if err != nil {
		return "", fmt.Errorf("the file %s could not be read: %w", path, err)
	}

	return string(body), nil
}

// BuildAlerts parses `server.alerts` and reads each destination's address, for the
// control plane's alert loops. A config with no block builds an empty set, which
// starts no loop; one with destinations and no rule still delivers what they are
// owed.
//
// A DESTINATION WHOSE ADDRESS CANNOT BE HAD DOES NOT STOP THE CONTROL PLANE. Scheduling
// matters more than a notice, and a secret missing from a service's environment is an
// ordinary thing to find at 3am. It is logged at Error here, and every notice owed to
// it settles failed with the reason, where `billet alerts list` shows it.
func BuildAlerts(cfg *config.Config, log *slog.Logger) (*server.Alerts, error) {
	if cfg.Server == nil || cfg.Server.Alerts == nil {
		return &server.Alerts{}, nil
	}

	block := cfg.Server.Alerts

	set := &server.Alerts{Destinations: make(map[string]server.AlertDestination, len(block.Destinations))}

	for i := range block.Destinations {
		d := &block.Destinations[i]

		sender, problem := AlertSender(d, os.Getenv)
		if problem != nil {
			log.Error("an alert destination cannot be reached; rules naming it will record why nothing arrived",
				"destination", d.Name, "error", problem)
		}

		set.Destinations[d.Name] = server.AlertDestination{Sender: sender, Kind: d.Kind}
	}

	for i := range block.Rules {
		rule, err := alerts.NewRule(&block.Rules[i])
		if err != nil {
			return nil, fmt.Errorf("server.alerts.rules[%d]: %w", i, err)
		}

		set.Rules = append(set.Rules, rule)
	}

	return set, nil
}

// AlertSender is the sender for one destination. Its address being unavailable is
// returned beside a sender that says so on every notice, which is what a running
// control plane keeps and what `billet alerts test` reports.
func AlertSender(d *config.AlertDestination, getenv func(string) string) (*alerts.Sender, error) {
	address, err := AlertAddress(d, getenv)
	if err != nil {
		return alerts.UnusableSender(d.Name, err.Error()), err
	}

	return alerts.NewSender(d.Name, address), nil
}
