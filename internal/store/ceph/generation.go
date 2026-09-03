package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GenerationLayout is how a published generation encodes when it was built.
//
// UTC BY CONSTRUCTION, because the alternative is a timezone bug waiting for a
// daylight-saving boundary: `rbd snap ls` prints a local-time string with no offset,
// so two nodes in different zones would disagree about the age of the same snapshot.
// billet names its own generations, so the name is the reliable clock.
const GenerationLayout = "20060102150405"

// GenerationPrefix marks a snapshot as one billet published.
const GenerationPrefix = "g"

// Generation is one published, immutable version of a golden image.
type Generation struct {
	Name string
	// Built is when billet made it, read from the name rather than from the
	// cluster's own timestamp.
	Built time.Time
}

// ParseGeneration reads the build time out of a generation's name.
//
// A NAME THAT DOES NOT PARSE IS NOT A GENERATION billet made — a snapshot somebody
// created by hand, or an older convention — and it is reported as such rather than
// guessed at. Deciding "a rebuild is not due" on a snapshot of unknown age is how a
// fleet quietly stops being rebuilt.
func ParseGeneration(name string) (Generation, bool) {
	trimmed := strings.TrimSpace(name)

	rest, found := strings.CutPrefix(trimmed, GenerationPrefix)
	if !found {
		return Generation{}, false
	}

	built, err := time.ParseInLocation(GenerationLayout, rest, time.UTC)
	if err != nil {
		return Generation{}, false
	}

	return Generation{Name: trimmed, Built: built}, true
}

// snapshot is the half of `rbd snap ls --format json` this needs.
type snapshot struct {
	Name string `json:"name"`
}

// NewestGeneration reports the most recently built generation of an image.
//
// BY BUILD TIME, NOT BY LIST ORDER. `rbd snap ls` returns snapshots in creation
// order today, and relying on that would make this answer wrong the first time
// somebody removes and re-adds one, or the ordering changes — for a question whose
// wrong answer is "no rebuild is due".
func (c *Client) NewestGeneration(ctx context.Context, image string) (Generation, bool, error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return Generation{}, false, fmt.Errorf("ceph: no image to list generations of")
	}

	out, err := c.rbdCmd(ctx, true, "-p", c.cfg.ImagePool, "snap", "ls", name)
	if err != nil {
		if isNoSuchFile(err) {
			// No image at all is not a failure to answer: nothing has been published,
			// which is exactly the state where a build IS due.
			return Generation{}, false, nil
		}

		return Generation{}, false, fmt.Errorf("ceph: list the generations of %s/%s: %w",
			c.cfg.ImagePool, name, err)
	}

	var snaps []snapshot
	if err := json.Unmarshal(trimSpace(out), &snaps); err != nil {
		return Generation{}, false, fmt.Errorf("ceph: %s did not answer with a json snapshot "+
			"list; is it the rbd command?", c.bin)
	}

	var newest Generation

	found := false

	for _, snap := range snaps {
		gen, ok := ParseGeneration(snap.Name)
		if !ok {
			continue
		}

		if !found || gen.Built.After(newest.Built) {
			newest, found = gen, true
		}
	}

	return newest, found, nil
}
