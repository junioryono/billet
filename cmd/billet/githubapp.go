package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/junioryono/billet/internal/github"
)

func cmdGitHubApp(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet github-app create --org <org>")
	}

	switch args[0] {
	case "create":
		return githubAppCreate(ctx, args[1:])
	case "-h", "--help":
		fmt.Println("usage: billet github-app create --org <org> [--name <name>] [--key-path <path>]")
		return nil
	default:
		return fmt.Errorf("unknown github-app subcommand %q", args[0])
	}
}

func githubAppCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("billet github-app create")
	org := fs.String("org", "", "GitHub organization to create the App for (required)")
	name := fs.String("name", "", "suggested App name (GitHub App names are globally unique; you can edit it there)")
	keyPath := fs.String("key-path", "", "where to write the App private key (default: alongside billet.yaml)")
	noBrowser := fs.Bool("no-browser", false, "print URLs instead of opening a browser")
	port := fs.Int("port", 0, "fixed loopback callback port (needed for `ssh -L` when onboarding a remote host)")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *org == "" {
		return errors.New("--org is required")
	}

	if *keyPath == "" {
		*keyPath = filepath.Join(filepath.Dir(defaultConfigPath()), "app-private-key.pem")
	}

	// RESERVE the real destination now, before the browser flow. A probe would be
	// TOCTOU-racy and, worse, would only tell us the directory was writable at
	// some earlier moment: if the create failed later, GitHub would already hold
	// a registered app whose one-time private key we had thrown away. Creating
	// the actual file with O_EXCL means the only remaining failure is a write
	// error on a descriptor we already own.
	keyFile, err := reserveKeyFile(*keyPath)
	if err != nil {
		return err
	}

	// Removed ONLY if GitHub never issued a credential.
	//
	// Keying this on "did the write succeed" was wrong in a way that shows up
	// only on unusual filesystems: a rename can commit and still report an error
	// — a FUSE or network mount that loses the reply — leaving the complete key
	// at the destination while the code below believed nothing was written and
	// unlinked it by pathname.
	//
	// Once OnAppCreated has been entered, GitHub has consumed the one-time code
	// and the credential exists. From that moment nothing here may delete the
	// destination. An empty reservation left behind after a genuine write failure
	// costs one `rm`; deleting a real key costs a credential GitHub will not
	// re-issue.
	credentialIssued := false
	keyWritten := false

	defer func() {
		if credentialIssued {
			keyFile.Close()

			return
		}

		// Verified to still be OUR reservation, AND still empty, before unlinking.
		//
		// The name is not proof of ownership: if this run's placeholder was
		// removed — which is exactly what the "delete it and re-run" diagnostic
		// tells an operator to do — a second run can reserve the same path, and
		// removing by pathname would delete that run's credential.
		//
		// Neither condition closes the window completely, because os.Remove takes
		// a pathname and the check cannot be atomic with it. What the pair does is
		// bound the damage: the only file that can be removed in the residual
		// window is one that was empty a moment earlier, so what is lost is
		// another run's placeholder rather than another run's key.
		if err := destinationIsStillReserved(keyFile, *keyPath); err == nil && isEmptyFile(*keyPath) {
			_ = os.Remove(*keyPath)
		}

		keyFile.Close()
	}()

	open := openBrowser
	if *noBrowser {
		open = nil
	}

	fmt.Printf("billet requests exactly these permissions:\n")

	perms := github.Permissions()

	names := make([]string, 0, len(perms))
	for name := range perms {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		fmt.Printf("  %-32s %s\n", name, perms[name])
	}

	fmt.Printf("\nNo repository Contents permission — billet cannot read your code.\n")
	fmt.Printf("GitHub allows one hour to finish; if it lapses, just run this again.\n\n")

	result, err := github.Onboard(ctx, github.OnboardOptions{
		Org:         *org,
		Name:        *name,
		Port:        *port,
		OpenBrowser: open,
		Log:         func(format string, a ...any) { fmt.Printf(format+"\n", a...) },
		// Called the instant the credentials exist, before installation. See the
		// OnAppCreated doc comment: this ordering is what stops a failed install
		// from orphaning a real app whose key GitHub will never re-issue.
		OnAppCreated: func(app *github.App) error {
			// Set FIRST. Reaching this callback means GitHub has registered the
			// App and consumed the one-time code, so the destination stops being
			// billet's to delete whatever happens next.
			credentialIssued = true

			// keyWritten is set from inside, the moment the key reaches its
			// final path — not after this returns. A durability error AFTER a
			// successful rename must not report the write as having failed.
			err := writeKeyAtomically(keyFile, *keyPath, []byte(app.PEM), func() {
				keyWritten = true

				fmt.Printf("Saved the private key to %s\n", *keyPath)
			})
			if err != nil {
				return err
			}

			return nil
		},
	})
	if err != nil {
		if keyWritten {
			// The app exists and its key is on disk, so this is recoverable rather
			// than a dead end — say so, and say how.
			fmt.Fprintf(os.Stderr,
				"\nThe App was created and its key saved to %s.\n"+
					"Fix the problem above, then finish by installing it on %s and running `billet check`.\n",
				*keyPath, *org)
		}

		return err
	}

	fmt.Printf("\nDone.\n\n")
	fmt.Printf("  private key      %s\n", *keyPath)
	fmt.Printf("\nAdd this to your billet.yaml:\n\n")
	fmt.Printf("github:\n")
	fmt.Printf("  org: %s\n", *org)
	fmt.Printf("  app_id: %d\n", result.App.ID)
	fmt.Printf("  installation_id: %d\n", result.Installation.ID)
	fmt.Printf("  private_key_path: %s\n", *keyPath)
	fmt.Printf("\nThen run: billet check\n")

	return nil
}

// reserveKeyFile creates the App key file 0600, refusing to clobber an existing
// one, and hands back the open descriptor.
//
// Creating the real file rather than probing is deliberate. A probe answers "was
// this directory writable a moment ago", which is both racy and useless at the
// point it matters: by the time the key exists, GitHub has already registered
// the app, and a create that fails then has thrown away a credential that cannot
// be re-issued. Holding the descriptor reduces the later failure surface to a
// write on a file we already own.
func reserveKeyFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return f, nil
	}

	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}

	// Deliberately NOT adopted, however empty it looks.
	//
	// An earlier version reused a zero-length file on the theory that it must be
	// billet's own reservation from a crashed run and so could not hold a
	// credential. Zero length does not prove that: a CONCURRENT
	// `billet github-app create` against the same path has a reservation that is
	// also empty, and adopting it puts two processes on one destination, where
	// each can rename its own App key over the other's and either one's cleanup
	// can unlink the installed key. Deciding from a Stat and then opening is
	// racy in its own right, and Stat follows symlinks.
	//
	// So: refuse, and say which case this is. Removing a file is a step the
	// operator can take safely; distinguishing two live processes is not.
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, statErr)
	}

	if info.Mode().IsRegular() && info.Size() == 0 {
		// A staging file beside an empty reservation is a real key: the previous
		// run was killed after the write and before the rename. It has to be
		// reported HERE, because the advice for a bare empty reservation is "rm
		// it and re-run" — followed literally, that abandons the key and the App
		// it belongs to.
		if staged := stagingPath(path); holdsUsableKey(staged) {
			return nil, fmt.Errorf(
				"%s is an empty placeholder, but %s holds an App private key from an interrupted run — "+
					"do NOT delete it. Move it into place and check it:\n    mv %s %s\n    billet check",
				path, staged, staged, path)
		}

		return nil, fmt.Errorf(
			"%s exists but is empty, which is what an interrupted `billet github-app create` leaves "+
				"behind. If no other billet run is in progress, delete it and re-run:\n    rm %s",
			path, path)
	}

	return nil, fmt.Errorf(
		"%s already exists; move it aside first — billet will not overwrite an App key, "+
			"and GitHub cannot re-issue one that is lost", path)
}

// writeKeyAtomically installs the key GitHub has just issued, preferring a
// crash-safe staged write and falling back to the reserved descriptor.
//
// The fallback is the point. The staged path is better in every way EXCEPT that
// it needs to create a second file after issuance, and a directory that turned
// read-only during the browser flow, an exhausted inode table or a full disk all
// make that impossible — at which point the previous version returned an error
// and exited with the only copy of an unrepeatable private key in memory. The
// reservation was opened before the flow precisely so there is always somewhere
// to put it.
func writeKeyAtomically(reserved *os.File, path string, pem []byte, onInstalled func()) error {
	stageErr := installViaStagingFile(reserved, path, pem, onInstalled)
	if stageErr == nil {
		return nil
	}

	// The credential exists somewhere and the error says where, so there is
	// nothing left to attempt — retrying would risk writing a second copy.
	if errors.Is(stageErr, errCredentialPreserved) {
		return stageErr
	}

	// Staging never reached the destination, so the key GitHub issued is still
	// only in memory. Fall back to the descriptor reserved before the browser
	// flow — the whole reason it is held open.
	//
	// This gives up crash-atomicity: a power cut mid-write leaves a truncated
	// PEM at the final path, which `billet check` reports and an operator fixes
	// by deleting it and re-running. That is a strictly better outcome than the
	// alternative here, which is a registered App whose one and only private key
	// was discarded because a temporary file could not be created.
	if err := installIntoReservation(reserved, path, pem, onInstalled); err != nil {
		return fmt.Errorf(
			"the App key could not be stored (%w) and the fallback write also failed (%w). "+
				"GitHub cannot re-issue this key: delete the App on GitHub and run this command again",
			stageErr, err)
	}

	return nil
}

// installViaStagingFile is the crash-safe path: write a sibling file, make it
// durable, then rename it over the reservation.
//
// Writing straight into the reserved file was not crash-safe: a SIGKILL or
// power loss mid-write leaves a truncated PEM at the final path, which the next
// run then refuses because the file exists — wedging the deployment on a
// credential that looks present and is not. Syncing only the file was not
// durable either: without syncing the DIRECTORY, the entry can be lost after
// billet has printed "Saved".
//
// onInstalled is called the instant the key is at its final path, BEFORE
// durability is confirmed. Everything after that point is best-effort
// reporting: once the credential is installed it must never be deleted,
// whatever else fails, because GitHub consumed the one-time code to produce it.
func installViaStagingFile(reserved *os.File, path string, pem []byte, onInstalled func()) error {
	dir := filepath.Dir(path)
	staging := stagingPath(path)

	// O_EXCL, and never adopting what is already there: a file at this name is
	// either a concurrent run's staging file or a previous run's preserved key,
	// and truncating either one destroys a credential. reserveKeyFile reports it
	// before the browser flow starts, so reaching this branch means it appeared
	// during the flow.
	tmp, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", staging, err)
	}

	// staged flips the moment the staging file holds the COMPLETE key, which is
	// well before it reaches the destination. Every failure from that point on
	// is a preserved credential, not a lost one — and the caller must not write
	// a second copy anywhere, because two files holding one private key is an
	// exposure nothing cleans up and the operator never hears about.
	staged := false
	reachedDestination := false

	defer func() {
		_ = tmp.Close()

		// Removed once the key is at its destination (where the name is already
		// gone, so this is a no-op), and removed when it never held a complete
		// key. A FRAGMENT is not a credential, and leaving one behind makes the
		// next run report it as a preserved key and tell the operator to install
		// it. The only case that keeps the file is the one where it holds the
		// whole key and the destination does not.
		if reachedDestination || !staged {
			_ = os.Remove(staging)
		}
	}()

	// The short-write case is checked explicitly: io.Writer may return n <
	// len(p) with a nil error, and treating that as success would mark a
	// truncated PEM as a preserved credential.
	if n, err := tmp.Write(pem); err != nil {
		return fmt.Errorf("write %s: %w", staging, err)
	} else if n != len(pem) {
		return fmt.Errorf("write %s: wrote %d of %d bytes", staging, n, len(pem))
	}

	staged = true

	if err := tmp.Sync(); err != nil {
		return preservedAt(staging, fmt.Errorf("sync %s: %w", staging, err))
	}

	if err := tmp.Close(); err != nil {
		return preservedAt(staging, fmt.Errorf("close %s: %w", staging, err))
	}

	// The staging NAME is made durable before the rename. A crash in the window
	// between the two otherwise leaves the only copy of the key behind an entry
	// the filesystem has not committed — losing not just the location but the
	// file. Best-effort: if the directory cannot be synced, renaming anyway is
	// strictly better than stopping here.
	//
	//nolint:errcheck // A failure here costs durability in a window the rename closes; it is not worth failing over.
	_ = syncDir(dir)

	// The destination is verified to still be THIS run's reservation.
	//
	// Both the rename below and the caller's cleanup act on a pathname, while
	// what this run owns is a descriptor. If the empty reservation is removed
	// and a second run installs its key at the same path, renaming over it
	// destroys a credential that is not ours to replace.
	if err := destinationIsStillReserved(reserved, path); err != nil {
		return preservedAt(staging, err)
	}

	// The reservation descriptor is released first: on Windows an open handle
	// blocks the install, and it buys nothing once the content is durable.
	_ = reserved.Close()

	if err := installByLink(staging, path); err != nil {
		// The staging file survives and holds the only copy. Name it, or the
		// operator is left with a registered App and no way to find its key.
		return preservedAt(staging, fmt.Errorf("install the key at %s: %w", path, err))
	}

	reachedDestination = true

	// Announced BEFORE the directory fsync. The credential is at its final path
	// and must never be unlinked from here on: a directory sync can fail on a
	// filesystem that does not support it (some FUSE and SMB mounts return
	// ENOTSUP), and treating that as "the write failed" made the caller delete
	// a key that was successfully installed.
	onInstalled()

	if err := syncDir(dir); err != nil {
		return fmt.Errorf(
			"%w: the key is installed at %s but its directory entry could not be flushed: %w\n"+
				"It is present now; a power loss before the filesystem flushes could still lose it, "+
				"so verify with `billet check` after a reboot",
			errCredentialPreserved, path, err)
	}

	return nil
}

// installByLink puts the staging file at path without ever REPLACING what is
// there.
//
// os.Rename replaces unconditionally, so the only thing standing between a
// racing second run and a destroyed key was a SameFile check taken moments
// earlier — check-then-act, with the mutation not atomic against it. os.Link
// refuses when the destination exists, and refusing is the outcome that cannot
// destroy a credential.
//
// The reservation is cleared first because it occupies the name. That opens a
// window in which the path does not exist; a second run creating its own
// reservation there makes the link fail with EEXIST, which is exactly the
// answer wanted — this run's key stays at the staging path and is reported.
func installByLink(staging, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear the reservation at %s: %w", path, err)
	}

	err := os.Link(staging, path)
	if err == nil {
		return nil
	}

	// EEXIST means another run got there first. Never fall back — a rename here
	// would replace the key that run just installed.
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s was claimed by another run: %w", path, err)
	}

	// Anything else is the filesystem declining to hard-link at all: FAT, and
	// some FUSE and SMB mounts. Rename is the fallback, and it is safe here
	// precisely because the destination is known not to exist — the remove
	// above succeeded and the link failed for an unrelated reason.
	if renameErr := os.Rename(staging, path); renameErr != nil {
		return fmt.Errorf("link failed (%w) and so did rename: %w", err, renameErr)
	}

	return nil
}

// installIntoReservation writes the key through the descriptor reserved before
// the browser flow, for when no second file can be created at all.
func installIntoReservation(reserved *os.File, path string, pem []byte, onInstalled func()) error {
	if err := destinationIsStillReserved(reserved, path); err != nil {
		return err
	}

	if n, err := reserved.Write(pem); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	} else if n != len(pem) {
		return fmt.Errorf("write %s: wrote %d of %d bytes", path, n, len(pem))
	}

	syncErr := reserved.Sync()

	// Re-verified AFTER the write, not only before it. The check before is
	// check-then-act: the reservation can be unlinked in between, and a write
	// through the descriptor then lands in a file with no name — gone the moment
	// this process exits, while the CLI reports it saved. There is no way to
	// recover the key at that point, so this must NOT be reported as preserved.
	if err := destinationIsStillReserved(reserved, path); err != nil {
		return fmt.Errorf(
			"the key was written but %s no longer names this run's file, so it is not retrievable there: %w",
			path, err)
	}

	// The key is at its destination. Everything from here is best-effort, for
	// the same reason as in the staged path — and it is announced BEFORE the
	// sync result is considered, because the bytes are readable at the final
	// path either way.
	onInstalled()

	if syncErr != nil {
		// Only durability is unconfirmed. Preserved, with the caveat stated.
		return preservedAt(path, fmt.Errorf("sync %s: %w", path, syncErr))
	}

	// Not reported: Sync above already forced the bytes out, so a close failure
	// cannot un-write a key that is now on disk.
	_ = reserved.Close()

	return nil
}

// preservedAt marks an error as one that left the credential readable on disk,
// and says where.
//
// The distinction is the whole reason ErrCredentialPreserved exists: onboarding
// tells the operator to delete the App and retry when the key is gone, and that
// instruction destroys an App whose key is merely somewhere unexpected.
func preservedAt(where string, err error) error {
	return fmt.Errorf(
		"%w: %w\nThe key IS saved at %s — GitHub cannot re-issue it, so do not delete it",
		errCredentialPreserved, err, where)
}

// destinationIsStillReserved reports whether path still names the file this run
// reserved, rather than one another process created in its place.
func destinationIsStillReserved(reserved *os.File, path string) error {
	ours, err := reserved.Stat()
	if err != nil {
		return fmt.Errorf("inspect the reserved key file: %w", err)
	}

	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}

	if !os.SameFile(ours, current) {
		return fmt.Errorf(
			"%s is no longer the file this run reserved — another `billet github-app create` "+
				"has claimed it, and overwriting it would destroy that App's key", path)
	}

	return nil
}

// errCredentialPreserved is the sentinel onboarding checks to decide whether
// its "delete the App and try again" advice applies. It lives in the github
// package because that is the layer that renders the advice.
var errCredentialPreserved = github.ErrCredentialPreserved

// stagingPath names the staging file deterministically, derived from the
// destination.
//
// os.CreateTemp's random suffix meant a crash between the synced staging file
// and the rename left the only copy of the key under a hidden name nothing
// reported and nothing could predict. A derived name is one the next run can
// look for and the operator can be told about, and deriving it from the
// destination keeps two runs onto different key paths out of each other's way.
func stagingPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".billet-partial")
}

// isEmptyFile reports whether path is a zero-length regular file. Lstat, so a
// symlink is not followed into something else.
func isEmptyFile(path string) bool {
	info, err := os.Lstat(path)

	return err == nil && info.Mode().IsRegular() && info.Size() == 0
}

// holdsUsableKey reports whether path contains a private key that actually
// parses.
//
// "Non-empty" was not enough. An interrupted write can leave a FRAGMENT at the
// staging path, and telling the operator that a truncated PEM is their App key
// — instructing them to move it into place and keep the App — is worse advice
// than saying nothing, because they act on it and only find out later. Parsing
// is the same standard `billet check` applies.
func holdsUsableKey(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxKeySize {
		return false
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	return github.ValidatePrivateKey(contents) == nil
}

// syncDir forces a directory entry to durable storage. Syncing a file does not
// guarantee its NAME survives a power cut.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s to sync it: %w", dir, err)
	}

	defer d.Close()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}

	return nil
}

// openBrowser is best-effort. A machine being onboarded over SSH has no browser,
// which is the normal case for a CI host rather than an edge case — the caller
// prints the URL either way.
func openBrowser(ctx context.Context, target string) error {
	var (
		cmd  string
		args = make([]string, 0, 2)
	)

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", append(args, "url.dll,FileProtocolHandler")
	default:
		cmd = "xdg-open"
	}

	args = append(args, target)

	proc := exec.CommandContext(ctx, cmd, args...)

	if err := proc.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}

	// Reaped in the background. Without a Wait the child stays a zombie for the
	// life of the CLI — which for this command can be the full hour GitHub
	// allows — and onboarding starts two of them. Not waiting inline because
	// `open`/`xdg-open` may not exit until the browser does.
	//nolint:errcheck // The browser's exit status is not billet's business; this Wait exists only to reap the child.
	go func() { proc.Wait() }()

	return nil
}

// maxKeySize bounds what is read from the key path. A real App key is a couple
// of kilobytes; anything larger is a misconfiguration, and reading it whole
// would be the misconfiguration's problem to solve rather than billet's.
const maxKeySize = 64 << 10

// checkPrivateKey proves the App key is usable, not merely present.
//
// os.Stat alone accepted a directory, an empty file left behind by an
// interrupted onboarding, a truncated PEM, and a world-readable one. Each of
// those is a deployment that looks configured and is not — and mode 0644 on an
// App private key is a local credential exposure that `billet check` existed to
// catch and did not.
func checkPrivateKey(path string) error {
	// Opened ONCE and inspected through the descriptor. Stat-then-read is two
	// lookups of the same name: the file can be swapped in between, so the size,
	// type and mode may describe a different inode than the bytes that get
	// parsed — and os.ReadFile on a FIFO blocks forever rather than returning.
	f, err := openForInspection(path)
	if err != nil {
		return fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("github.private_key_path %s is not a regular file", path)
	}

	if info.Size() == 0 {
		return fmt.Errorf(
			"github.private_key_path %s is empty; an interrupted `billet github-app create` leaves "+
				"a placeholder there. Remove it and re-run that command", path)
	}

	if info.Size() > maxKeySize {
		return fmt.Errorf("github.private_key_path %s is %d bytes; that is not an App key",
			path, info.Size())
	}

	// Group and other bits on a private key are a local exposure. Checked on
	// unix only: Windows permissions are ACL-based and these bits are meaningless
	// there, so testing them would produce a false alarm on every Windows host.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return fmt.Errorf(
				"github.private_key_path %s is mode %04o; it is readable beyond its owner. "+
					"Run: chmod 600 %s", path, perm, path)
		}
	}

	// Read from the descriptor already inspected, and bounded for real: the
	// size check above describes the inode at that moment, while this limit
	// holds regardless.
	pemBytes, err := io.ReadAll(io.LimitReader(f, maxKeySize+1))
	if err != nil {
		return fmt.Errorf("read github.private_key_path %s: %w", path, err)
	}

	if len(pemBytes) > maxKeySize {
		return fmt.Errorf("github.private_key_path %s is larger than %d bytes; that is not an App key",
			path, maxKeySize)
	}

	// Parsed, not merely read: a truncated PEM is exactly what an interrupted
	// write leaves, and it fails at the first API call rather than here.
	if err := github.ValidatePrivateKey(pemBytes); err != nil {
		return fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	return nil
}
