// Package macoslimit is a node's record of how many macOS guests it may run: its
// nodes[].macos_vm_limit as the control plane last said it at an accepted registration,
// kept under node.state_dir so a command beside the node, such as a verification, reads
// the limit the control plane places against (ADR-021). A node's own file need not carry
// nodes:, which is the control plane's policy.
//
// A leaf: the standard library, durablefile and regularfile, nothing else of billet's.
// The node writes the record and the provider and the image commands read it, and none
// of them may import another.
package macoslimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
)

// FileName is the record's name under node.state_dir.
const FileName = "macos-limit.json"

const (
	schema = 1
	// limit bounds a read. The record is one number and two short strings.
	limit = 4 << 10
)

// Record is the limit as a node last received it.
type Record struct {
	Schema int `json:"schema"`
	// Deployment is the control plane that said it, so a node that moved to another
	// deployment never counts its guests against the old one's limit.
	Deployment string `json:"deployment"`
	// Carried says whether that control plane's wire carries the limit at all. False is
	// an older control plane and is never a limit of zero.
	Carried bool `json:"carried"`
	// Limit is meaningful only when Carried.
	Limit int `json:"limit"`
	// ReceivedAt is when the registration that carried it was accepted.
	ReceivedAt string `json:"received_at"`
}

// Path is the record's path under a node's state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// install is the one call the writer makes into the durable installer, a seam so a test
// can fail it.
var install = func(dir, name string, write func(io.Writer) error) (string, error) {
	return durablefile.Installer{}.Install(dir, name, 0o600, write)
}

// Write records the limit deployment's control plane said, or that it does not say when
// carried is false, replacing any earlier record whole.
func Write(stateDir, deployment string, carried bool, macOSLimit int, at time.Time) error {
	if deployment == "" {
		return errors.New("a node's macOS limit is recorded only with the deployment that said it")
	}

	if !carried {
		macOSLimit = 0
	}

	body, err := json.Marshal(Record{
		Schema:     schema,
		Deployment: deployment,
		Carried:    carried,
		Limit:      macOSLimit,
		ReceivedAt: at.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("encode this node's macOS limit: %w", err)
	}

	body = append(body, '\n')

	if _, err := install(stateDir, FileName, func(w io.Writer) error {
		_, err := w.Write(body)

		return err
	}); err != nil {
		return fmt.Errorf("record this node's macOS limit in %s: %w", stateDir, err)
	}

	return nil
}

// Remove deletes the record, so a reader answers that it cannot tell rather than read an
// earlier answer. A record already absent is not an error.
func Remove(stateDir string) error {
	err := os.Remove(Path(stateDir))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return fmt.Errorf("remove this node's macOS limit from %s: %w", stateDir, err)
}

// Read returns the limit recorded for deployment and whether there is one. No record, a
// record of another deployment and a control plane that does not say all answer false
// with no error. One that cannot be read or parsed is an error, never absence.
func Read(stateDir, deployment string) (int, bool, error) {
	body, err := regularfile.ReadFile(Path(stateDir), limit, regularfile.Options{NoFollow: true})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}

	if err != nil {
		return 0, false, fmt.Errorf("read this node's macOS limit: %w", err)
	}

	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return 0, false, fmt.Errorf("parse this node's macOS limit: %w", err)
	}

	if rec.Schema != schema {
		return 0, false, fmt.Errorf("this node's macOS limit has schema %d and this billet reads %d",
			rec.Schema, schema)
	}

	if rec.Deployment != deployment || !rec.Carried {
		return 0, false, nil
	}

	return rec.Limit, true, nil
}
