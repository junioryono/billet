package ceph

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// ArchKey records which machine a generation's root filesystem was built for, as
// `uname -m` spells it (x86_64, aarch64). The full key is this plus "." plus the
// generation.
//
// PER GENERATION AND IN THE CLUSTER, because one site's nodes share the image pool
// and a node of one architecture must never boot another's generation (ADR-022).
// An import records the manifest's; a generation imported before this existed
// records none, and LearnArchitectures reads it from the generation itself.
const ArchKey = "billet.arch"

// archProbePath is the binary whose ELF header says what a root filesystem was
// built for: every image billet builds carries it, as a regular file under the
// merged /usr of Ubuntu 24.04.
const archProbePath = "/usr/bin/env"

// elfHeaderBytes is how much of the probe's head the ELF identification and
// e_machine take.
const elfHeaderBytes = 20

// The ELF machine numbers billet builds images for (the System V ABI's
// EM_X86_64 and EM_AARCH64).
const (
	elfMachineX8664   = 62
	elfMachineAArch64 = 183
)

// SetArch records which machine a generation was built for.
func (c *Client) SetArch(ctx context.Context, image, generation, machine string) error {
	return c.setGenerationMeta(ctx, ArchKey, image, generation, machine)
}

// GenerationArch reports which machine a generation records it was built for,
// and false for one that records none.
func (c *Client) GenerationArch(ctx context.Context, image, generation string) (string, bool, error) {
	if err := checkCloneName(image); err != nil {
		return "", false, err
	}

	if !isPublishedSnapshot(generation) {
		return "", false, fmt.Errorf("ceph: %q is not a generation billet published", generation)
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "get", image,
		ArchKey+"."+generation)
	if err != nil {
		if isNoSuchFile(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("ceph: could not read %s for %s@%s: %w",
			ArchKey, image, generation, err)
	}

	machine := strings.TrimSpace(string(out))

	return machine, machine != "", nil
}

// GenerationArches is the recorded machine of each generation of an image that
// records one.
func (c *Client) GenerationArches(ctx context.Context, image string) (map[string]string, error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return nil, errors.New("ceph: no image to read architectures of")
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "list", name)
	if err != nil {
		if isNoSuchFile(err) {
			return map[string]string{}, nil
		}

		return nil, fmt.Errorf("ceph: read the architectures of %s/%s: %w", c.cfg.ImagePool, name, err)
	}

	return parseGenerationArches(out), nil
}

// parseGenerationArches reads ArchKey's rows out of an `image-meta list` table.
func parseGenerationArches(out []byte) map[string]string {
	arches := map[string]string{}

	for line := range strings.SplitSeq(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}

		if generation, isArch := strings.CutPrefix(key, ArchKey+"."); isArch && isPublishedSnapshot(generation) {
			if machine := strings.TrimSpace(value); machine != "" {
				arches[generation] = machine
			}
		}
	}

	return arches
}

// otherMachine reports whether a generation records a machine other than this
// host's. One that records none is not another's: it resolves as it always did.
func (c *Client) otherMachine(recorded string) bool {
	return recorded != "" && recorded != c.machine
}

// refuseOtherMachine refuses a snapshot recorded as another machine than this
// host's, by name, for a reference that named it exactly.
func (c *Client) refuseOtherMachine(ctx context.Context, image, snapshot string) error {
	recorded, _, err := c.GenerationArch(ctx, image, snapshot)
	if err != nil {
		return err
	}

	if c.otherMachine(recorded) {
		return fmt.Errorf("ceph: %s@%s was built for %s and this host is %s, so it cannot boot "+
			"here; a tier that names an architecture is placed on hosts of it, so check the tier's "+
			"arch and its image", image, snapshot, recorded, c.machine)
	}

	return nil
}

// CheckImportArch is what an import of machine into image would answer about the
// generations already there, read without the publish lock: a pull asks before it
// downloads anything, and the import asks again under the lock, which is the one
// that decides.
func (c *Client) CheckImportArch(ctx context.Context, image, machine string) error {
	if err := checkCloneName(image); err != nil {
		return err
	}

	return c.checkImportArch(ctx, image, machine)
}

// checkImportArch refuses an import of machine into an image unless every
// generation the image holds is known to be of it. Called under the publish lock,
// so no generation appears or loses its record between the check and the import.
//
// ONE ARCHITECTURE AN IMAGE (ADR-022). Two architectures in one image would let an
// older billet, which resolves by guest contract alone, boot the other's
// generation, and would let one site's x64 and arm64 nodes share an image by
// naming the same one. A generation that records none is could-not-tell, which is
// refused the same way rather than read as the import's.
func (c *Client) checkImportArch(ctx context.Context, image, machine string) error {
	generations, err := c.Generations(ctx, image)
	if err != nil {
		return err
	}

	if len(generations) == 0 {
		return nil
	}

	arches, err := c.GenerationArches(ctx, image)
	if err != nil {
		return err
	}

	var other, unknown []string

	for _, gen := range generations {
		switch recorded := arches[gen.Name]; recorded {
		case machine:
		case "":
			unknown = append(unknown, gen.Name)
		default:
			other = append(other, gen.Name+" ("+recorded+")")
		}
	}

	if len(other) > 0 {
		return fmt.Errorf("ceph: %s holds generations built for another machine than this %s "+
			"image (%s), and one image holds one architecture; a new architecture starts in an "+
			"image of its own, such as ubuntu-2404-arm64", image, machine, strings.Join(other, ", "))
	}

	if len(unknown) > 0 {
		return fmt.Errorf("ceph: %s holds generations whose architecture is not recorded and could "+
			"not be read from them (%s), so this %s image cannot be shown to be of theirs. A pull "+
			"says why it could not read each (a host without debugfs installs e2fsprogs, one without "+
			"node.lock_dir sets it); otherwise unpromote and reap them (`billet images unpromote`, "+
			"`billet images reap`; reap keeps a generation a tier pins exactly until the pin moves), "+
			"or import under a new image name", image, strings.Join(unknown, ", "), machine)
	}

	return nil
}

// errNoMapLock is why a client without the host's generation-map lock neither
// sweeps nor reads a golden image's generation: another process on the host could
// unmap its device between the map and the read, and a device the kernel reuses
// for the next map would then be read as this generation.
var errNoMapLock = errors.New("node.lock_dir is not set, so nothing keeps another process on " +
	"this host from unmapping a read-only generation map while it is read; set it")

// ArchLearned is what LearnArchitectures made of one generation that recorded no
// architecture.
type ArchLearned struct {
	Generation string
	// Machine is what was read from it, empty when Err says why nothing was.
	Machine string
	Err     error
	// Recorded is whether Machine was written; NotRecorded says why it was not,
	// for a generation whose machine was read.
	Recorded    bool
	NotRecorded error
}

// LearnArchitectures reads the architecture of each generation of image that
// records none from the generation itself, and records what it read.
//
// FROM THE ROOT FILESYSTEM, NOT FROM WHERE IT CAME FROM. billet published only
// x86_64 before generations recorded one, but an operator's own source could feed
// an arm64 node an arm64 image, so a generation of either may record none. Each is
// mapped read-only under the host's generation-map lock, which every read-only
// generation map on the host is taken under, and the ELF header of /usr/bin/env is
// read with debugfs without mounting anything. What was read is recorded under the
// publish lock, only for a generation that is still there and still records none.
//
// A GENERATION THAT CANNOT BE READ STAYS UNKNOWN, reported in its entry, and the
// next import into its image refuses with the remedies; so does every one on a
// client without the host's generation-map lock (errNoMapLock). One read and not
// recorded says why: another node may hold the publish lock, which is never waited
// for, and the import judges again under it. Only a failure to list the image is
// an error.
func (c *Client) LearnArchitectures(ctx context.Context, image string) ([]ArchLearned, error) {
	if err := checkCloneName(image); err != nil {
		return nil, err
	}

	generations, err := c.Generations(ctx, image)
	if err != nil {
		return nil, err
	}

	arches, err := c.GenerationArches(ctx, image)
	if err != nil {
		return nil, err
	}

	var learned []ArchLearned

	for _, gen := range generations {
		if arches[gen.Name] != "" {
			continue
		}

		if c.mapLock == "" {
			learned = append(learned, ArchLearned{Generation: gen.Name, Err: errNoMapLock})

			continue
		}

		var machine string

		readErr := c.withGenerationMap(ctx, c.cfg.ImagePool+"/"+image, gen.Name,
			func(device string) error {
				// BOUNDED, because the host's generation-map lock is held across it and
				// the node's own generation checks wait on that lock.
				probeCtx, cancel := context.WithTimeout(ctx, c.wait)
				defer cancel()

				var err error
				machine, err = c.readMachine(probeCtx, device)

				return err
			})
		if readErr != nil {
			machine = ""
		}

		learned = append(learned, ArchLearned{Generation: gen.Name, Machine: machine, Err: readErr})
	}

	if slices.ContainsFunc(learned, func(l ArchLearned) bool { return l.Err == nil }) {
		c.recordLearnedArches(ctx, image, learned)
	}

	return learned, nil
}

// recordLearnedArches writes each learned machine under the publish lock, having
// proved the generation is still there and still records none, and says in each
// entry whether it did and why not.
func (c *Client) recordLearnedArches(ctx context.Context, image string, learned []ArchLearned) {
	notRecorded := func(err error) {
		for i := range learned {
			if learned[i].Err == nil {
				learned[i].NotRecorded = err
			}
		}
	}

	lock, err := c.TakePublishLock(ctx, time.Now())
	if err != nil {
		notRecorded(fmt.Errorf("the publish lock is held, by a reap, a verification or a publish: %w", err))

		return
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		if err := lock.Release(releaseCtx); err != nil {
			notRecorded(fmt.Errorf("the publish lock was not released: %w", err))
		}
	}()

	generations, err := c.Generations(ctx, image)
	if err != nil {
		notRecorded(err)

		return
	}

	arches, err := c.GenerationArches(ctx, image)
	if err != nil {
		notRecorded(err)

		return
	}

	for i, l := range learned {
		if l.Err != nil {
			continue
		}

		switch present := slices.ContainsFunc(generations, func(g Generation) bool {
			return g.Name == l.Generation
		}); {
		case !present:
			learned[i].NotRecorded = errors.New("it was removed since it was read")
		case arches[l.Generation] != "":
			learned[i].NotRecorded = fmt.Errorf("it was recorded as %s since it was read",
				arches[l.Generation])
		default:
			if err := c.SetArch(ctx, image, l.Generation, l.Machine); err != nil {
				learned[i].NotRecorded = err
			} else {
				learned[i].Recorded = true
			}
		}
	}
}

// readMachine reads the ELF machine of archProbePath on a mapped root filesystem.
func (c *Client) readMachine(ctx context.Context, device string) (string, error) {
	head, err := c.readProbe(ctx, device)
	if err != nil {
		return "", err
	}

	return elfMachine(head)
}

// elfMachine is the `uname -m` spelling of the machine an ELF header names.
func elfMachine(head []byte) (string, error) {
	if len(head) < elfHeaderBytes || !bytes.HasPrefix(head, []byte("\x7fELF")) {
		return "", fmt.Errorf("%s is not an ELF binary on this filesystem", archProbePath)
	}

	var order binary.ByteOrder

	switch head[5] {
	case 1:
		order = binary.LittleEndian
	case 2:
		order = binary.BigEndian
	default:
		return "", fmt.Errorf("%s names no byte order", archProbePath)
	}

	switch machine := order.Uint16(head[18:20]); machine {
	case elfMachineX8664:
		return "x86_64", nil
	case elfMachineAArch64:
		return "aarch64", nil
	default:
		return "", fmt.Errorf("%s is built for ELF machine %d, which billet builds no image for",
			archProbePath, machine)
	}
}

// readProbeWithDebugfs is readProbe outside tests: the head of archProbePath on
// device, read by debugfs, which opens a filesystem read-only unless told
// otherwise and mounts nothing.
//
// AN EXIT OF 0 PROVES NOTHING. debugfs -R answers 0 for a path it could not find,
// printing the complaint to stderr, so only the bytes it wrote are evidence, and
// elfMachine refuses anything that is not an ELF header.
func readProbeWithDebugfs(ctx context.Context, device string) ([]byte, error) {
	debugfs, err := exec.LookPath("debugfs")
	if err != nil {
		return nil, fmt.Errorf("debugfs is required to read a generation's architecture: %w", err)
	}

	head := &headWriter{limit: elfHeaderBytes}
	cmd := exec.CommandContext(ctx, debugfs, "-R", "cat "+archProbePath, device)
	cmd.Stdout = head

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", archProbePath, device, err)
	}

	return head.buf, nil
}

// headWriter keeps the first limit bytes written to it and discards the rest, so
// the command writing them runs to its end without its output being held.
type headWriter struct {
	buf   []byte
	limit int
}

func (w *headWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}

	return len(p), nil
}
