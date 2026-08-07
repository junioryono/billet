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

	// RESERVE a file beside the destination now, before the browser flow. A probe
	// would be TOCTOU-racy and, worse, would only say the directory was writable
	// at some earlier moment: if the create failed later, GitHub would already
	// hold a registered app whose one-time private key had been thrown away.
	// Creating the file with O_EXCL reduces the remaining failure surface to a
	// write on a descriptor this process already owns.
	keyFile, err := reserveKeyFile(*keyPath)
	if err != nil {
		return err
	}

	keyWritten := false

	// The reservation is deliberately LEFT BEHIND on an aborted run.
	//
	// Two versions of an automatic cleanup were tried and neither is safe. Go
	// removes by pathname while this process owns a descriptor, and there is no
	// unlink-this-inode to reach for; os.SameFile narrows the window and cannot
	// close it. Nothing is removed here, so the cost of aborting is one `rm` —
	// and reserveKeyFile prints that exact command on the next run, after
	// checking whether the file is a leftover or a credential.
	defer keyFile.Close()

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

	// Printed when GitHub returned one, because the operator cannot recover it
	// from anywhere else without going back through the browser.
	if result.App.ClientID != "" {
		fmt.Printf("  client_id: %s\n", result.App.ClientID)
	}

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

	// Checked BEFORE reserving, not only when the destination is occupied.
	//
	// An interrupted run can leave a real key at the staging path with NO file
	// at the destination at all — the install clears the reservation before it
	// links, so a process killed in that window leaves the name free. Looking
	// for the staged key only after O_EXCL failed meant that run's key was never
	// mentioned: this call succeeded, onboarding went on to create a SECOND App,
	// and the first one's unrepeatable key sat there unreported.
	staging := stagingPath(path)

	switch inspectKey(staging) {
	case keyPresent:
		return nil, stagedKeyFoundError(path, staging)
	case keyUnverifiable:
		// NOT the "delete it and re-run" branch. A file billet cannot read may be
		// a perfectly good key whose mode or mount is temporarily wrong, and the
		// operator can unlink it regardless — unlink permission comes from the
		// directory, not the file. Saying "it holds no usable key" there is a
		// destructive claim billet has not earned.
		return nil, fmt.Errorf(
			"%s exists but billet cannot read it, so it cannot tell whether an interrupted run left an "+
				"App private key there. Do NOT delete it blind: check that file yourself. If it holds a "+
				"PEM private key, move it to %s and run `billet check`",
			staging, path)
	case keyAbsent:
	}

	// The destination is refused if anything occupies it, and then LEFT ALONE.
	//
	// This is a courtesy check for a clear diagnostic, not the safety property:
	// it is a snapshot, and the pathname can change immediately afterwards. What
	// actually protects the destination is that nothing here ever creates,
	// removes or renames it — the only thing that puts a file at that name is the
	// os.Link at install time, which refuses atomically when the name is taken.
	//
	// pathPresent, not "not absent": an unstattable destination must not block
	// onboarding, because the link is what guarantees safety and it does not need
	// this answer. Only a destination KNOWN to be occupied is worth stopping for.
	if lookupPath(path) == pathPresent {
		return nil, destinationOccupiedError(path)
	}

	// The reservation is a SEPARATE file, and that is the whole design.
	//
	// It used to be the destination itself, which forced the install to unlink
	// the destination before it could link the key into place — and no amount of
	// checking makes a pathname unlink safe, because the check cannot be atomic
	// with it. Three rounds of guards were tried and every one of them still had
	// an ordering where another run's key was deleted on the way to installing
	// this one.
	//
	// Reserving elsewhere removes the unlink entirely. The final name is created
	// exactly once, by a link that fails rather than replaces. It also collapses
	// two files into one: this descriptor is both the proof that the directory is
	// writable and the file the key is written to.
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return f, nil
	}

	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create %s: %w", staging, err)
	}

	// Inspected AGAIN, because the earlier answer is stale by now.
	//
	// Between that inspection and this O_EXCL, a concurrent run's empty
	// reservation can have become a complete key. Reusing the first answer to
	// print "it holds no usable key" alongside an exact `rm` handed the operator
	// a command that destroys a credential that arrived in between.
	switch inspectKey(staging) {
	case keyPresent:
		return nil, stagedKeyFoundError(path, staging)
	case keyUnverifiable:
		return nil, fmt.Errorf(
			"%s exists and billet cannot read it, so it cannot tell whether it holds an App private "+
				"key. Do NOT delete it blind: check that file yourself first", staging)
	case keyAbsent:
	}

	// Never adopted, however empty it looks. A zero-length file here is equally a
	// crashed run's leftover and a CONCURRENT run's live reservation, and
	// adopting it puts two processes on one file where either can destroy the
	// other's key. Deciding from a Stat and then opening is racy in its own
	// right, and Stat follows symlinks.
	return nil, fmt.Errorf(
		"%s already exists, which is what an interrupted `billet github-app create` leaves behind. "+
			"It held no usable key a moment ago — if no other billet run is in progress, delete it "+
			"and re-run:\n    rm %s",
		staging, staging)
}

// destinationOccupiedError explains a key path that is already taken.
func destinationOccupiedError(path string) error {
	return fmt.Errorf(
		"%s already exists; move it aside first — billet will not overwrite an App key, "+
			"and GitHub cannot re-issue one that is lost", path)
}

// stagedKeyFoundError reports a real key left at the staging path by a run that
// did not finish.
//
// It is a hard stop rather than a warning. The advice for the states this
// resembles — an empty placeholder, or nothing at all — is "delete it and
// re-run", and following that abandons both this key and the App on GitHub it
// belongs to.
func stagedKeyFoundError(path, staged string) error {
	// The `mv` is only offered when the destination is free.
	//
	// Unix mv REPLACES, so recommending it unconditionally handed the operator a
	// command that destroys a second App's key whenever one already sits at the
	// destination — the precise outcome every other rule here exists to prevent,
	// arrived at by following billet's own instructions.
	// The question is whether the destination is OCCUPIED, not whether it holds
	// something billet recognises. Several states that are not a usable billet key
	// still hold something worth keeping: a PEM with trailing junk, a key in a
	// format this build cannot parse, a file a live writer has not finished.
	// "Not currently a valid key" was never proof that clobbering it is safe —
	// and neither is "could not tell", which is why this is `!= pathAbsent`.
	if lookupPath(path) != pathAbsent {
		return fmt.Errorf(
			"two files are present and billet cannot tell which key you want:\n"+
				"    %s   (from an interrupted run)\n"+
				"    %s   (at the configured key path)\n"+
				"Neither can be re-issued by GitHub, so nothing here will be moved automatically. "+
				"Identify which App each belongs to, move the other one somewhere safe, and re-run "+
				"`billet check`",
			staged, path)
	}

	// ln rather than mv, and for the same reason as above: this text is composed
	// now and typed later, and mv replaces whatever arrived in between.
	return fmt.Errorf(
		"%s holds an App private key from an interrupted run — do NOT delete it, and do not create "+
			"another App. GitHub cannot re-issue this key. Put it in place with a command that "+
			"refuses to overwrite, then check it:\n    ln %s %s && rm %s\n    billet check",
		staged, staged, path, staged)
}

// writeKeyAtomically installs the key GitHub has just issued.
//
// One file, one atomic step. The reservation opened before the browser flow IS
// the staging file — it lives beside the destination, never at it — so the key
// is written into a descriptor this process already owns and then linked into
// place. os.Link creates the final name or fails; it never replaces.
//
// That shape is the answer to three rounds of failed patches. While the
// reservation occupied the destination, installing meant unlinking the
// destination first, and no check can be made atomic with a later unlink by
// pathname: every guard still had an ordering where another run's key was
// deleted on the way to installing this one. Reserving elsewhere removes the
// unlink entirely, and with it the fallback that existed only because the
// destination was already occupied.
//
// onInstalled fires the instant the key is at its final path, BEFORE durability
// is confirmed. Everything after that is best-effort reporting: the credential
// exists and must never be deleted, whatever else fails.
func writeKeyAtomically(reserved *os.File, path string, pem []byte, onInstalled func()) error {
	dir := filepath.Dir(path)
	staging := stagingPath(path)

	installed := false

	defer func() {
		if !installed {
			_ = reserved.Close()

			// The staging file is this run's only copy of the key, or the empty
			// reservation. Either way it is not deleted here: reserveKeyFile
			// inspects it on the next run and says which it is.
			return
		}

		// After the link both names refer to the same private key. Removing the
		// staging one is not optional — and a failure to remove it is not silent,
		// because an unreported second copy of an App key is exactly what nobody
		// finds until it matters.
		//
		// Checked against the descriptor first, and BEFORE closing it — the
		// descriptor is the only proof that the name still refers to the file this
		// run created, and a Stat on a closed one fails, which read as "not ours"
		// and left the second copy behind on every successful install.
		stillOurs := verifyInstalled(reserved, staging)

		_ = reserved.Close()

		switch stillOurs {
		case identityMatches:
		case identityDiffers:
			// Absence is the ORDINARY outcome when something moved staging to the
			// destination — the install succeeded through that very path — so it
			// is not worth alarming about. Anything else at that name is not
			// billet's to remove, and worth saying.
			if lookupPath(staging) == pathAbsent {
				return
			}

			fmt.Fprintf(os.Stderr,
				"\nWarning: %s is not this run's file, so it was left in place.\n", staging)

			return
		case identityUnknown:
			fmt.Fprintf(os.Stderr,
				"\nWarning: the key is installed at %s, but billet could not confirm what %s is, so it "+
					"was left alone. Check whether it is a second copy of the key.\n", path, staging)

			return
		}

		if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr,
				"\nWarning: the key is installed at %s, but %s is a second copy of it that could not be "+
					"removed (%v). Delete it once `billet check` passes.\n",
				path, staging, err)

			return
		}

		// The REMOVAL is made durable too. The directory was synced while both
		// names existed, so a power loss after this function returns could
		// otherwise resurrect the staging entry — leaving two durable names for
		// one private key, with the process that would have warned about it gone.
		if err := syncDir(dir); err != nil {
			fmt.Fprintf(os.Stderr,
				"\nWarning: the key is installed at %s and the extra copy at %s was removed, but that "+
					"removal could not be flushed (%v). Check after a reboot that %s is gone.\n",
				path, staging, err, staging)
		}
	}()

	// A FAILED write can still have left a usable key, and that possibility
	// decides what the operator is told.
	//
	// GitHub's PEM ends in a newline. A write that stops one byte short of it
	// produces something pem.Decode parses perfectly — so "the write returned an
	// error" and "there is no credential here" are different facts. What is on
	// disk is the authority, not the return value.
	//
	// (*os.File.Write reports an error for every short write, so the n check is
	// belt and braces against a future writer that does not.)
	if n, writeErr := reserved.Write(pem); writeErr != nil || n != len(pem) {
		// Flushed so the question below is asked of the filesystem rather than the
		// page cache. Neither result decides anything on its own.
		syncErr := reserved.Sync()

		failure := fmt.Errorf("write %s: wrote %d of %d bytes: %w",
			staging, n, len(pem), errors.Join(writeErr, syncErr))

		// Identity FIRST. inspectKey answers a question about a pathname, and
		// "there is a valid key at that name" is not "this run's key survived" —
		// another run's key at the staging name would have been reported as this
		// one's, and a staging name that was unlinked during the flow would have
		// been reported as holding a key it no longer has.
		//
		// `!= identityMatches` is NOT good enough here, and writing it that way
		// undid the three-valued type one line after introducing it: a failed stat
		// is not a proven mismatch, and treating it as one writes a second copy of
		// the key for no reason and reports only the copy.
		switch verifyInstalled(reserved, staging) {
		case identityMatches:
		case identityDiffers:
			return recoverKey(dir, path, pem, failure)
		case identityUnknown:
			return uncertainAt(staging, path, failure)
		}

		switch inspectKey(staging) {
		case keyPresent:
			return preservedAt(staging, failure)
		case keyUnverifiable:
			// NOT "preserved": that sentinel promises the key is readable, and here
			// billet does not know. Its own sentinel keeps the App alive and the
			// file untouched without asserting something it cannot see.
			return uncertainAt(staging, path, failure)
		case keyAbsent:
			// Nothing usable landed, but the bytes are still in memory.
			return recoverKey(dir, path, pem, failure)
		}
	}

	if err := reserved.Sync(); err != nil {
		failure := fmt.Errorf("sync %s: %w", staging, err)

		switch verifyInstalled(reserved, staging) {
		case identityMatches:
			return preservedAt(staging, failure)
		case identityDiffers:
			return recoverKey(dir, path, pem, failure)
		case identityUnknown:
			return uncertainAt(staging, path, failure)
		}
	}

	// The staging NAME is made durable before the link. A crash in the window
	// between them otherwise leaves the only copy of the key behind a directory
	// entry the filesystem has not committed — losing not just the location but
	// the file. Best-effort: linking anyway beats stopping here.
	_ = syncDir(dir) //nolint:errcheck // Durability in a window the link closes; not worth failing over.

	// os.Link takes a NAME, and what this run owns is a descriptor.
	//
	// If the staging name was removed during the browser flow — an operator
	// following a concurrent run's "rm" advice does exactly that — the key went
	// into an inode with no directory entry, and it dies when this process exits.
	// If the name was REPLACED, linking it installs somebody else's file at the
	// destination and reports success. Neither is detectable after the fact, so
	// both are checked for before, and the result is verified after.
	switch verifyInstalled(reserved, staging) {
	case identityMatches:
	case identityDiffers:
		// The key is still in memory, so this is RECOVERABLE — writing it
		// somewhere new costs nothing and the alternative is a credential GitHub
		// will not re-issue. Reporting it lost here was a plain logic error:
		// "the inode has no name and no portable call can give it one" is true
		// and irrelevant, because the bytes never depended on that inode.
		return recoverKey(dir, path, pem, fmt.Errorf(
			"the key was written, but %s no longer refers to the file it was written to", staging))
	case identityUnknown:
		// A stat that failed has not established that staging is gone, and
		// recovering here would write a second copy of the key while reporting
		// only one of them.
		return uncertainAt(staging, path, fmt.Errorf(
			"the key was written, but billet could not confirm %s still refers to it", staging))
	}

	// The one step that creates the destination, and it cannot replace.
	if err := os.Link(staging, path); err != nil {
		var cause error

		if errors.Is(err, os.ErrExist) {
			cause = fmt.Errorf(
				"%s was claimed by something else while this App was being created (%w)", path, err)
		} else {
			// Anything else is the filesystem declining to hard-link at all: FAT,
			// and some FUSE and SMB mounts. There is deliberately no rename
			// fallback — os.Rename has no no-clobber form in Go, so it can only be
			// made safe by checks that are not atomic with it.
			cause = fmt.Errorf(
				"%s could not be hard-linked to %s (%w), and billet will not fall back to a rename "+
					"because a rename cannot refuse to replace a file another run may have just installed",
				staging, path, err)
		}

		// The link can fail because staging was MOVED to the destination by
		// something else — in which case the key is already exactly where it
		// belongs, and writing a recovery copy would scatter a second one for no
		// reason. Checked before anything else is concluded.
		switch verifyInstalled(reserved, path) {
		case identityMatches:
			installed = true

			onInstalled()

			// Routed through the same durability step as the ordinary install: the
			// earlier sync made the STAGING name durable, and it is the rename to
			// the destination that now has to survive a crash.
			if err := syncDir(dir); err != nil {
				return preservedAt(path, fmt.Errorf(
					"the key is at %s but its directory entry could not be flushed: %w\n"+
						"It is present now; verify with `billet check` after a reboot", path, err))
			}

			return nil
		case identityUnknown:
			// Not knowing whether the key already landed is not grounds for
			// writing another copy of it.
			return uncertainAt(path, path, cause)
		case identityDiffers:
		}

		// Re-checked, because the pre-link check is stale by the time the link
		// has failed: the staging name can have been swapped in between, and
		// naming it as where the key is preserved would point the operator at
		// somebody else's file.
		switch verifyInstalled(reserved, staging) {
		case identityMatches:
			return preservedAt(staging, cause)
		case identityDiffers:
			return recoverKey(dir, path, pem, cause)
		case identityUnknown:
			return uncertainAt(staging, path, cause)
		}

		return cause
	}

	// The check above is check-then-act and cannot be made atomic with the link.
	// This is what catches the window: if the destination is not the inode this
	// run wrote, the link picked up a file that was swapped in, and the real key
	// is in an unlinked descriptor. Saying "Saved" there would be a lie about a
	// credential — and the file now at the destination is not billet's to remove.
	//
	// A failure to ANSWER is not a mismatch. Turning every stat error into "your
	// key is gone" told operators to delete an App over a transient Lstat
	// failure, or over a filesystem whose inode metadata SameFile cannot trust.
	switch verifyInstalled(reserved, path) {
	case identityMatches:
	case identityDiffers:
		// The link picked up a file swapped in behind the pre-check. What is at
		// the destination is not billet's to remove — but STAGING usually still
		// holds this run's key, and writing a third copy while naming only the
		// newest is worse than pointing at the one that is already there.
		switch verifyInstalled(reserved, staging) {
		case identityMatches:
			return preservedAt(staging, fmt.Errorf(
				"%s was linked, but it is not the file this run wrote — something replaced the staging "+
					"name mid-install. %s is not billet's to remove", path, path))
		case identityUnknown:
			// Staging may well still hold the key; writing another copy would
			// scatter one and then report only the new one.
			return uncertainAt(staging, path, fmt.Errorf(
				"%s was linked, but it is not the file this run wrote, and billet could not confirm "+
					"what %s is", path, staging))
		case identityDiffers:
		}

		// Staging is gone too, and the key is still in memory.
		return recoverKey(dir, path, pem, fmt.Errorf(
			"%s was linked, but it is not the file this run wrote — the staging name was replaced "+
				"mid-install", path))
	case identityUnknown:
		// If staging is still provably this run's file, SAY so — that is more than
		// "billet could not tell", and it is where the operator should look.
		if verifyInstalled(reserved, staging) == identityMatches {
			return uncertainAt(staging, path, fmt.Errorf(
				"%s was linked, but billet could not confirm it is the file this run wrote; the key is "+
					"also at %s", path, staging))
		}

		return uncertainAt(path, path, fmt.Errorf(
			"%s was linked, but billet could not confirm it is the file this run wrote", path))
	}

	installed = true

	// Announced BEFORE the directory fsync. The credential is at its final path
	// and must never be unlinked from here on: a directory sync can fail on a
	// filesystem that does not support it (some FUSE and SMB mounts return
	// ENOTSUP), and treating that as "the write failed" made the caller delete a
	// key that was successfully installed.
	onInstalled()

	if err := syncDir(dir); err != nil {
		return preservedAt(path, fmt.Errorf(
			"the key is installed at %s but its directory entry could not be flushed: %w\n"+
				"It is present now; a power loss before the filesystem flushes could still lose it, "+
				"so verify with `billet check` after a reboot", path, err))
	}

	return nil
}

// identity is the result of asking whether a pathname refers to a known file.
// The third value exists for the same reason keyState's does: a stat that fails
// has not established a mismatch, and treating it as one is destructive advice.
type identity int

const (
	identityUnknown identity = iota
	identityMatches
	identityDiffers
)

// verifyInstalled reports whether path names the same file as reserved.
func verifyInstalled(reserved *os.File, path string) identity {
	ours, err := reserved.Stat()
	if err != nil {
		return identityUnknown
	}

	current, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return identityDiffers
		}

		return identityUnknown
	}

	if os.SameFile(ours, current) {
		return identityMatches
	}

	return identityDiffers
}

// recoverKey writes the key somewhere new when the ordinary install could not
// place it, and reports where it landed.
//
// This function exists because the previous version did not: it concluded that a
// key written into an unlinked inode was unrecoverable — true of that inode, and
// beside the point, because writeKeyAtomically still holds the complete PEM in
// memory at every call site that reaches here. Declaring a credential lost while
// the bytes are in a live variable is the worst outcome in this file, since the
// advice that follows is "delete the App".
//
// The same reasoning applies one level down, which is what the first version got
// wrong: a recovery write that reports an error may STILL have left a usable key,
// so the file is inspected rather than assumed empty. Loss is what remains after
// looking, never what is inferred from a return value.
func recoverKey(dir, destination string, pem []byte, cause error) error {
	return recoverKeyAttempt(dir, destination, pem, cause, 1)
}

// maxRecoveryAttempts bounds retrying against a directory something else keeps
// disturbing. Each attempt writes one file, so this is also a bound on how many
// copies of the key a hostile directory can be made to scatter.
const maxRecoveryAttempts = 3

func recoverKeyAttempt(dir, destination string, pem []byte, cause error, attempt int) error {
	f, err := os.CreateTemp(dir, ".billet-key-recovered-*")
	if err != nil {
		// This directory failed; that is not proof no directory would work. Say
		// which claim is being made.
		return fmt.Errorf(
			"%w\nThe key could not be written to a recovery file in %s (%w). It is not recoverable "+
				"from this run: delete the App on GitHub and run this command again",
			cause, dir, err)
	}

	// CreateTemp already creates 0600 atomically — there is no window where the
	// key is world-readable, and an extra Chmod would only add a failure path.
	name := f.Name()

	writeErr := writeKey(f, pem)

	// Verified against the DESCRIPTOR before any promise is made about the name.
	// A random name is unlikely to be disturbed, but "unlikely" is not the
	// standard this file holds anywhere else, and preservedAt is a promise.
	held := verifyInstalled(f, name)

	_ = f.Close()

	// IDENTITY decides whether the pathname may be spoken about at all.
	//
	// Inspecting `name` answers "is there a usable key at that name", which is a
	// different question from "is this run's key there" — and conflating them let
	// a replaced recovery file be reported as this App's key while the real one
	// sat at a moved path nobody was told about. The pathname is only attributed
	// to this descriptor when they are known to be the same file.
	switch held {
	case identityMatches:
	case identityUnknown:
		// Not knowing is not grounds for writing yet another copy, and not grounds
		// for a promise either.
		return uncertainAt(name, destination, fmt.Errorf(
			"%w\nThe key was written to a recovery file, but %s could not be confirmed to be it "+
				"(write: %w). Look for a recently written PEM private key in %s before doing anything else",
			cause, name, writeErr, dir))
	case identityDiffers:
		// PROVEN mismatch: this name is not the file that was written, so closing
		// the descriptor is about to destroy an unlinked inode. The PEM is still
		// in memory — the same fact that started this function — so try again
		// rather than returning uncertainty about a key that is about to vanish.
		if attempt >= maxRecoveryAttempts {
			// NOT uncertainAt(name, ...): `name` has just been PROVEN not to be
			// this descriptor's file, and uncertainAt would attribute it anyway —
			// naming it as somewhere to look, and potentially offering to link it
			// into place. The directory is all billet can honestly point at.
			return fmt.Errorf(
				"%w: %w\nEvery recovery file billet wrote was moved or replaced before it could be "+
					"confirmed, so it cannot say where this App's key is. Do NOT delete the App: look "+
					"for a recently written PEM private key in %s, and check anything already at %s "+
					"before replacing it",
				github.ErrCredentialUncertain, cause, dir, destination)
		}

		return recoverKeyAttempt(dir, destination, pem, cause, attempt+1)
	}

	if writeErr == nil {
		// The NAME is made durable too. f.Sync persists contents, not the new
		// directory entry, so a crash could otherwise lose a path billet has
		// already told the operator to go to.
		if err := syncDir(dir); err != nil {
			return uncertainAt(name, destination, fmt.Errorf(
				"%w\nThe key was written to %s, but that name could not be flushed (%w) — confirm it "+
					"exists after a reboot", cause, name, err))
		}

		return preservedAt(name, fmt.Errorf(
			"%w\nThe key was re-written to %s. Check whether %s already holds a different App's key "+
				"before moving it there", cause, name, destination))
	}

	// The write reported a failure and the name IS this descriptor's file, so
	// what is in it decides — not the return value.
	switch inspectKey(name) {
	case keyPresent:
		return preservedAt(name, fmt.Errorf(
			"%w\nThe recovery write reported an error (%w) but %s holds a usable key. Check whether "+
				"%s already holds a different App's key before moving it there",
			cause, writeErr, name, destination))
	case keyUnverifiable:
		return uncertainAt(name, destination, fmt.Errorf(
			"%w\nThe recovery write reported an error (%w) and %s cannot be read", cause, writeErr, name))
	case keyAbsent:
		return fmt.Errorf(
			"%w\nThe key could not be written to a recovery file (%w), and %s holds nothing usable. "+
				"The key is gone: delete the App on GitHub and run this command again",
			cause, writeErr, name)
	}

	return cause
}

// writeKey writes the whole of b and forces it to disk. The descriptor is left
// OPEN so the caller can still ask what it refers to.
func writeKey(f *os.File, b []byte) error {
	if n, err := f.Write(b); err != nil {
		return fmt.Errorf("write %s: %w", f.Name(), err)
	} else if n != len(b) {
		return fmt.Errorf("write %s: wrote %d of %d bytes", f.Name(), n, len(b))
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", f.Name(), err)
	}

	return nil
}

// pathState is what a pathname lookup concluded, with the same third value as
// everywhere else here: a stat that FAILED has not established absence.
type pathState int

const (
	pathUnknown pathState = iota
	pathAbsent
	pathPresent
)

// lookupPath reports whether anything occupies path.
//
// fileExists collapsed every error into "nothing there", which is fine for a
// diagnostic and wrong for a decision: on EACCES or ESTALE it made billet
// recommend `mv onto-the-destination`, and Unix mv replaces. Only a confirmed
// ErrNotExist may unlock that advice.
func lookupPath(path string) pathState {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pathAbsent
		}

		return pathUnknown
	}

	return pathPresent
}

// uncertainAt marks an error where billet could not determine whether the
// credential survived.
//
// Deliberately NOT ErrCredentialPreserved. That sentinel is a promise — "the key
// is at this path, go and get it" — and asserting it off a FAILED inspection
// sends an operator to a file that may be empty while telling them to keep an
// App whose key may be gone. Treating "could not tell" as "confirmed present" is
// the same mistake as treating it as "confirmed absent", pointed the other way.
//
// What it shares with preserved is the part that matters: nothing is deleted,
// and onboarding does not tell the operator to delete the App.
func uncertainAt(where, destination string, err error) error {
	// The `mv` is offered only when the destination is free — and it is checked
	// HERE rather than assumed, because another run can claim it during the
	// browser flow. Unix mv replaces, so the unconditional version handed the
	// operator a command that destroys a second App's key.
	move := fmt.Sprintf(
		"%s cannot be confirmed empty, so do not move anything on top of it — work out which App "+
			"each file belongs to first", destination)

	if where != destination && lookupPath(destination) == pathAbsent {
		// ln, not mv. The destination is checked when this text is COMPOSED and
		// executed by a human some time later, so a plain mv can replace a key
		// another run installed in between. ln refuses when the destination
		// exists — the same no-clobber primitive billet installs with.
		move = fmt.Sprintf(
			"if it holds a PEM private key, put it in place with a command that refuses to "+
				"overwrite:\n    ln %s %s && rm %s\n    billet check", where, destination, where)
	}

	return fmt.Errorf(
		"%w: %w\nbillet could not read %s to find out whether the key reached it. Do NOT delete the App "+
			"yet: inspect that file, and %s",
		github.ErrCredentialUncertain, err, where, move)
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

// keyState is what inspecting a path concluded. The third value is the point:
// "I could not tell" is not the same answer as "there is nothing here", and
// collapsing them meant a transient open or read failure was read as proof that
// no credential existed — after which the file holding it was deleted.
type keyState int

const (
	// keyAbsent means the path was inspected and holds no usable key.
	keyAbsent keyState = iota
	// keyPresent means it holds a private key that parses.
	keyPresent
	// keyUnverifiable means inspection itself failed. Every caller must treat it
	// as if a key were present: refuse to delete, and refuse to tell the operator
	// their credential is gone.
	keyUnverifiable
)

// inspectKey reports whether path holds a usable private key, or says that it
// could not find out.
//
// One descriptor, inspected and read through — the same discipline
// checkPrivateKey uses, and for the same reasons. Lstat-then-ReadFile is two
// lookups of one name: the file can be swapped in between, so ReadFile would
// follow a symlink planted after the check, block forever on a FIFO, or read
// past maxKeySize — and could validate an entirely different file than the one
// inspected, which here decides what the operator is told about their credential.
func inspectKey(path string) keyState {
	f, err := openForInspection(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return keyAbsent
		}

		return keyUnverifiable
	}

	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return keyUnverifiable
	}

	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxKeySize {
		return keyAbsent
	}

	contents, err := io.ReadAll(io.LimitReader(f, maxKeySize+1))
	if err != nil {
		return keyUnverifiable
	}

	if len(contents) > maxKeySize || github.ValidatePrivateKey(contents) != nil {
		return keyAbsent
	}

	return keyPresent
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
