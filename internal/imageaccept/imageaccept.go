// Package imageaccept is a node's record of which licensed image components its
// deployment accepts (#584): the control plane's images.accept, as the node was
// last told it at an accepted registration, kept under node.state_dir so the
// image commands that complete a generation read it on a host where the node has
// not registered since boot.
//
// A leaf: the standard library, durablefile and regularfile, nothing else of
// billet's. The node writes the record and the image commands read it, and
// neither may import the other.
package imageaccept

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
)

// FileName is the record's name under node.state_dir.
const FileName = "image-acceptance.json"

// schema is the record's schema number.
const schema = 1

// limit bounds a read. The record names at most a handful of components, so a
// longer file is not one the node wrote.
const limit = 64 << 10

// Record is the deployment's acceptance as a node last received it.
type Record struct {
	Schema int `json:"schema"`
	// Deployment is the identity of the control plane that said it, so a node
	// that moved to another deployment never installs what the old one accepted.
	Deployment string `json:"deployment"`
	// Components is what the control plane's images.accept names, sorted, and
	// empty when it accepts none. A name this build does not know is kept as
	// written: the reader refuses it rather than install less than was accepted.
	Components []string `json:"components"`
	// ReceivedAt is when the registration that carried it was accepted.
	ReceivedAt string `json:"received_at"`
}

// Path is the record's path under a node's state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// install is the one call the writer makes into the durable installer, a seam so
// a test can fail it.
var install = func(dir, name string, write func(io.Writer) error) (string, error) {
	return durablefile.Installer{}.Install(dir, name, 0o600, write)
}

// Write records what the control plane of deployment accepts, replacing any
// earlier record whole. An interruption leaves the previous record intact.
func Write(stateDir, deployment string, components []string, at time.Time) error {
	if deployment == "" {
		return errors.New("the deployment's image acceptance is recorded only with the deployment that said it")
	}

	sorted := slices.Clone(components)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	if sorted == nil {
		sorted = []string{}
	}

	body, err := json.Marshal(Record{
		Schema:     schema,
		Deployment: deployment,
		Components: sorted,
		ReceivedAt: at.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("encode the deployment's image acceptance: %w", err)
	}

	body = append(body, '\n')

	if _, err := install(stateDir, FileName, func(w io.Writer) error {
		_, err := w.Write(body)

		return err
	}); err != nil {
		return fmt.Errorf("record the deployment's image acceptance in %s: %w", stateDir, err)
	}

	return nil
}

// Read returns the record under stateDir. A record that is not there answers
// false with no error; one that is there and cannot be read or parsed is an
// error, never absence, because a reader that fell back on absence would complete
// a generation with less than the deployment accepted.
func Read(stateDir string) (Record, bool, error) {
	path := Path(stateDir)

	body, err := regularfile.ReadFile(path, limit, regularfile.Options{NoFollow: true})
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}

	if err != nil {
		return Record{}, false, fmt.Errorf("read the deployment's image acceptance: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, false, fmt.Errorf("the deployment's image acceptance at %s is not a record "+
			"this node wrote: %w", path, err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Record{}, false, fmt.Errorf("the deployment's image acceptance at %s has more after "+
			"its record", path)
	}

	switch {
	case rec.Schema != schema:
		return Record{}, false, fmt.Errorf("the deployment's image acceptance at %s is schema %d, and "+
			"this billet reads schema %d", path, rec.Schema, schema)
	case rec.Deployment == "":
		return Record{}, false, fmt.Errorf("the deployment's image acceptance at %s names no deployment", path)
	case rec.Components == nil:
		return Record{}, false, fmt.Errorf("the deployment's image acceptance at %s names no component "+
			"list", path)
	}

	return rec, true, nil
}
