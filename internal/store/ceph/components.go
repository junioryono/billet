package ceph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// CompletionRootOwnerKey records, per completion root, the node whose pull made it
// and may remove it.
const CompletionRootOwnerKey = "billet.completion_root_owner"

// ResolveCompletionRoot is ResolveGeneration for the completion microVM: ref must
// name a completion root, exactly, recorded as speaking guestContract.
func (c *Client) ResolveCompletionRoot(ctx context.Context, ref, guestContract string) (string, error) {
	image, snapshot, found := strings.Cut(strings.TrimSpace(ref), "@")
	if !found || !IsCompletionRoot(snapshot) {
		return "", fmt.Errorf("ceph: %s is not a completion root", bounded(ref))
	}

	contract, ok, err := c.GuestContract(ctx, image, snapshot)
	if err != nil {
		return "", err
	}
	if !ok || contract != guestContract {
		return "", fmt.Errorf("ceph: the completion root %s does not speak guest contract %s", bounded(ref),
			guestContract)
	}

	return image + "@" + snapshot, nil
}

// RemoveCompletionRoots removes the completion roots of image that owner made.
// Another node's are left to it: one may be booting that node's completion.
//
// ONLY A ROOT THE LISTING SAW. An owner key whose snapshot is not listed may be an
// import of this owner's between its owner write and its snapshot, which a pull
// that lost the probe lock is still making; removing its key would leave that
// snapshot with no owner, so the key is left.
//
// SAFE UNDER A CLONE, because billet requires clone v2, under which removing a
// snapshot that still has a clone moves it to the trash until the clone is gone.
func (c *Client) RemoveCompletionRoots(ctx context.Context, image, owner string) error {
	if err := checkCloneName(image); err != nil {
		return err
	}
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("ceph: no owner to remove the completion roots of %s for", image)
	}

	names, err := c.snapshotNames(ctx, image)
	if err != nil {
		return err
	}

	owners, err := c.completionRootOwners(ctx, image)
	if err != nil {
		return err
	}

	var errs []error
	for snap, recorded := range owners {
		// AN UNKNOWN OR OTHER OWNER IS NOT THIS NODE.
		if recorded != owner {
			continue
		}

		if !slices.Contains(names, snap) {
			continue
		}

		if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "snap", "rm",
			image+"@"+snap); err != nil && !isNoSuchFile(err) {
			errs = append(errs, fmt.Errorf("ceph: remove the completion root %s@%s: %w", image, snap, err))

			continue
		}

		c.removeGenerationMetadata(ctx, image, snap,
			KernelKey, RunnerVersionKey, GuestContractKey, CompletionRootOwnerKey)
	}

	return errors.Join(errs...)
}

// completionRoots lists image's completion roots that exist.
func (c *Client) completionRoots(ctx context.Context, image string) ([]string, error) {
	names, err := c.snapshotNames(ctx, image)
	if err != nil {
		return nil, err
	}

	var roots []string
	for _, name := range names {
		if IsCompletionRoot(name) {
			roots = append(roots, name)
		}
	}

	return roots, nil
}

// completionRootOwners reads every completion-root owner key of image, whether or
// not its snapshot exists.
func (c *Client) completionRootOwners(ctx context.Context, image string) (map[string]string, error) {
	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "list", image)
	if err != nil {
		if isNoSuchFile(err) {
			return map[string]string{}, nil
		}

		return nil, fmt.Errorf("ceph: read the metadata of %s/%s: %w", c.cfg.ImagePool, image, err)
	}

	owners := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if snap, isOwner := strings.CutPrefix(key, CompletionRootOwnerKey+"."); isOwner && IsCompletionRoot(snap) {
			owners[snap] = strings.TrimSpace(value)
		}
	}

	return owners, nil
}

// snapshotNames lists every snapshot of image, whatever billet calls it. No image
// is no snapshots.
func (c *Client) snapshotNames(ctx context.Context, image string) ([]string, error) {
	out, err := c.rbdCmd(ctx, true, "-p", c.cfg.ImagePool, "snap", "ls", image)
	if err != nil {
		if isNoSuchFile(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("ceph: list the snapshots of %s/%s: %w", c.cfg.ImagePool, image, err)
	}

	var snaps []snapshot
	if err := json.Unmarshal(trimSpace(out), &snaps); err != nil {
		return nil, fmt.Errorf("ceph: %s did not answer with a json snapshot list; is it the rbd command?", c.bin)
	}

	names := make([]string, 0, len(snaps))
	for _, snap := range snaps {
		names = append(names, snap.Name)
	}

	return names, nil
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
