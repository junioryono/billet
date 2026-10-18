package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/junioryono/billet/internal/provider"
)

// guestShape is the size billet launched a guest at, recorded where only billet can
// write: beside the jail's root, never inside it.
//
// A CAPTURE NEEDS IT FROM A SOURCE THE GUEST DOES NOT CONTROL. The VMM writes the
// memory file, and a VMM that has been taken over could make it any size; the shape
// billet asked for is the bound the copy is held to, and the shape a restore must ask
// for again, because admission charges a microVM by its spec.
type guestShape struct {
	VCPUs  int   `json:"vcpus"`
	Memory int64 `json:"memory_bytes"`
}

func (j jail) shapeFile() string { return filepath.Join(j.dir(), "billet-shape") }

// writeShape records the size a guest is launched at, staged and renamed like the
// resource record, so it is whole or absent.
func (p *Provider) writeShape(j jail, spec provider.Spec) error {
	encoded, err := json.Marshal(guestShape{VCPUs: spec.VCPU, Memory: int64(spec.Memory)})
	if err != nil {
		return fmt.Errorf("firecracker: encode the shape of %s: %w", j.id, err)
	}

	staged, err := os.CreateTemp(j.dir(), ".shape-")
	if err != nil {
		return fmt.Errorf("firecracker: stage the shape of %s: %w", j.id, err)
	}

	defer os.Remove(staged.Name())

	if _, err := staged.Write(append(encoded, '\n')); err != nil {
		return errors.Join(fmt.Errorf("firecracker: write the shape of %s: %w", j.id, err), staged.Close())
	}

	if err := p.syncFile(staged); err != nil {
		return errors.Join(fmt.Errorf("firecracker: sync the shape of %s: %w", j.id, err), staged.Close())
	}

	if err := staged.Close(); err != nil {
		return fmt.Errorf("firecracker: close the staged shape of %s: %w", j.id, err)
	}

	if err := os.Rename(staged.Name(), j.shapeFile()); err != nil {
		return fmt.Errorf("firecracker: record the shape of %s: %w", j.id, err)
	}

	if err := p.syncDir(j.dir()); err != nil {
		return fmt.Errorf("firecracker: sync the shape of %s: %w", j.id, err)
	}

	return nil
}

// shapeOf reads the size a guest was launched at.
func shapeOf(j jail) (guestShape, error) {
	raw, err := os.ReadFile(j.shapeFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return guestShape{}, fmt.Errorf("firecracker: %s was launched before billet recorded the size "+
				"of a guest, so nothing bounds what its capture may hold", j.id)
		}

		return guestShape{}, fmt.Errorf("firecracker: read the shape of %s: %w", j.id, err)
	}

	var shape guestShape
	if err := json.Unmarshal(raw, &shape); err != nil || shape.VCPUs <= 0 || shape.Memory <= 0 {
		return guestShape{}, fmt.Errorf("firecracker: %s does not hold a shape billet wrote", j.shapeFile())
	}

	return shape, nil
}
