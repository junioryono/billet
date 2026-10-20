package ceph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ComponentsKey records, per generation, which components of GitHub's image the
// published image left out and which of them this deployment installed (#584).
//
// ABSENT MEANS THE IMAGE LEFT NOTHING OUT. Every generation imported before images
// left anything out, or by a hand-run build, carries no such key, and those are
// images that carry everything GitHub's does.
const ComponentsKey = "billet.components"

// GenerationComponents is what ComponentsKey holds: the components the image left
// out, and the version installed of each this deployment accepted.
type GenerationComponents struct {
	LeftOut   []string          `json:"left_out"`
	Installed map[string]string `json:"installed"`
}

var (
	componentNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	componentVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+:~_-]{0,63}$`)
)

// validate refuses a record that would read back as something else: a name or a
// version that is not one, a component installed that was not left out, or a
// component left out twice.
func (g *GenerationComponents) validate() error {
	if len(g.LeftOut) == 0 {
		return fmt.Errorf("ceph: a components record names nothing left out; an image that " +
			"left nothing out records none")
	}

	for i, name := range g.LeftOut {
		if !componentNamePattern.MatchString(name) {
			return fmt.Errorf("ceph: %q is not a component name", name)
		}
		if slices.Contains(g.LeftOut[:i], name) {
			return fmt.Errorf("ceph: component %s is left out twice", name)
		}
	}

	for name, version := range g.Installed {
		if !slices.Contains(g.LeftOut, name) {
			return fmt.Errorf("ceph: component %s is recorded as installed but was not left out",
				name)
		}
		if !componentVersionPattern.MatchString(version) {
			return fmt.Errorf("ceph: %q is not a version of %s", version, name)
		}
	}

	return nil
}

// SetComponents records what a generation left out and what was installed.
func (c *Client) SetComponents(ctx context.Context, image, generation string,
	components GenerationComponents,
) error {
	if err := components.validate(); err != nil {
		return err
	}

	// SORTED, so one record has one spelling and a comparison of two is a
	// comparison of their bytes.
	record := GenerationComponents{
		LeftOut:   slices.Sorted(slices.Values(components.LeftOut)),
		Installed: components.Installed,
	}
	if record.Installed == nil {
		record.Installed = map[string]string{}
	}

	value, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("ceph: encode the components of %s@%s: %w", image, generation, err)
	}

	return c.setGenerationMeta(ctx, ComponentsKey, image, generation, string(value))
}

// Components reports what a generation left out and what was installed. Found is
// false for a generation that carries no record, which is one that left nothing
// out; a record that does not parse is an error, never an absence.
func (c *Client) Components(ctx context.Context, image, generation string,
) (GenerationComponents, bool, error) {
	if err := checkCloneName(image); err != nil {
		return GenerationComponents{}, false, err
	}

	if _, ok := ParseGeneration(generation); !ok {
		return GenerationComponents{}, false, fmt.Errorf("ceph: %q is not a generation billet "+
			"published", generation)
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "get", image,
		ComponentsKey+"."+generation)
	if err != nil {
		if isNoSuchFile(err) {
			return GenerationComponents{}, false, nil
		}

		return GenerationComponents{}, false, fmt.Errorf("ceph: could not read %s for %s@%s: %w",
			ComponentsKey, image, generation, err)
	}

	// UNKNOWN FIELDS REFUSED. A newer billet's record that this one half-read
	// would be judged on the half it understood.
	var components GenerationComponents
	dec := json.NewDecoder(bytes.NewReader([]byte(strings.TrimSpace(string(out)))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&components); err != nil {
		return GenerationComponents{}, false, fmt.Errorf("ceph: the %s record of %s@%s does not "+
			"parse: %w", ComponentsKey, image, generation, err)
	}
	if err := components.validate(); err != nil {
		return GenerationComponents{}, false, fmt.Errorf("ceph: the %s record of %s@%s: %w",
			ComponentsKey, image, generation, err)
	}

	return components, true, nil
}
