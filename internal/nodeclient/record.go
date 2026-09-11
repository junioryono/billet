package nodeclient

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
)

// registrationRecord is what the node publishes about its own registration:
// evidence, written after every successful registration and never before one,
// for an inspector that has to say which endpoint this process dials and
// under which incarnation the control plane knows it. EXACTLY THESE SEVEN
// FIELDS; the inspector holds a record to this set by name.
type registrationRecord struct {
	Schema       int    `json:"schema"`
	Node         string `json:"node"`
	Deployment   string `json:"deployment"`
	Incarnation  string `json:"incarnation"`
	InvocationID string `json:"invocation_id"`
	Endpoint     string `json:"endpoint"`
	RegisteredAt string `json:"registered_at"`
}

// registrationRecordSchema is the record's schema number.
const registrationRecordSchema = 1

// invocationID is the systemd invocation this process runs under, read
// through a seam so a fixture can supply one where no manager is present. The
// default is the environment variable systemd sets for every process of a
// unit's runtime cycle, which a child inherits; presence is never proof of
// being the managed node, and the inspector compares the value with the
// unit's own.
var invocationID = func() string { return os.Getenv("INVOCATION_ID") }

// installRecord is THE ONE CALL the writer makes into the durable installer:
// the zero-value Installer, whose seams are the real operations. A fixture
// wraps this to observe the installer's steps or to fail one of them; nothing
// in production sets a seam.
var installRecord = func(dir, name string, write func(io.Writer) error) (string, error) {
	return durablefile.Installer{}.Install(dir, name, 0o600, write)
}

// publishRegistration writes the record for a registration the control plane
// accepted. An empty path (darwin, where no manager makes a runtime directory)
// writes nothing. The directory is examined by Lstat first: a symlink there,
// or anything but a directory, refuses the write, because the record is
// root-owned evidence under a root-owned directory the unit's RuntimeDirectory
// made, and a name that leads elsewhere is not that. The write itself goes
// through the durable installer's one ordering (mode, file sync, close,
// rename, directory sync), so an interruption before the rename leaves the
// previous record intact and nothing staged.
func publishRegistration(c *Client, opts LoopOptions) error {
	path := opts.RegistrationRecordPath
	if path == "" {
		return nil
	}

	dir := filepath.Dir(path)

	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("examine the registration directory %s: %w", dir, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the registration directory %s is a symlink, and the record is not written through one", dir)
	}

	if !info.IsDir() {
		return fmt.Errorf("the registration directory %s is not a directory (%s)", dir, info.Mode().Type())
	}

	rec := registrationRecord{
		Schema:       registrationRecordSchema,
		Node:         c.node,
		Deployment:   opts.Deployment,
		Incarnation:  c.incarnation,
		InvocationID: invocationID(),
		Endpoint:     c.Endpoint().String(),
		RegisteredAt: opts.now().UTC().Format(time.RFC3339Nano),
	}

	if _, err := installRecord(dir, filepath.Base(path), func(w io.Writer) error {
		body, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("encode the registration record: %w", err)
		}

		body = append(body, '\n')

		if _, err := w.Write(body); err != nil {
			return fmt.Errorf("write the registration record: %w", err)
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// now is the loop's clock: the seam when a fixture sets one, the wall clock
// otherwise.
func (o LoopOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}

	return time.Now()
}
