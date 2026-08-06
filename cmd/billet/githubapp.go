package main

import (
	"context"
	"errors"
	"fmt"
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

	// Removed only on a path where nothing was ever written into it.
	keyWritten := false

	defer func() {
		keyFile.Close()

		if !keyWritten {
			_ = os.Remove(*keyPath)
		}
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
			// keyWritten is set from inside, the moment the key reaches its
			// final path — not after this returns. A durability error AFTER a
			// successful rename must not make the deferred cleanup delete an
			// installed credential.
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
		return nil, fmt.Errorf(
			"%s exists but is empty, which is what an interrupted `billet github-app create` leaves "+
				"behind. If no other billet run is in progress, delete it and re-run:\n    rm %s",
			path, path)
	}

	return nil, fmt.Errorf(
		"%s already exists; move it aside first — billet will not overwrite an App key, "+
			"and GitHub cannot re-issue one that is lost", path)
}

// writeKeyAtomically installs the key so no crash can leave a partial one at the
// destination.
//
// Writing straight into the reserved file was not crash-safe: a SIGKILL or power
// loss mid-write leaves a truncated PEM at the final path, which the next run
// then refuses because the file exists — wedging the deployment on a credential
// that looks present and is not. Syncing only the file was not durable either:
// without syncing the DIRECTORY, the entry can be lost after billet has printed
// "Saved".
//
// So: write a sibling temp file, fsync it, rename it over the reservation —
// rename is atomic on POSIX — then fsync the directory. The destination only
// ever holds the empty reservation or the complete key.
// onInstalled is called the instant the key is at its final path, BEFORE
// durability is confirmed. Everything after that point is best-effort reporting:
// once the credential is installed it must never be deleted, whatever else
// fails, because GitHub consumed the one-time code to produce it.
func writeKeyAtomically(reserved *os.File, path string, pem []byte, onInstalled func()) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".billet-key-*")
	if err != nil {
		return fmt.Errorf("create a temporary file next to %s: %w", path, err)
	}

	tmpName := tmp.Name()
	installed := false

	defer func() {
		_ = tmp.Close()

		// The temp file is removed ONLY while it is still scratch. Once the
		// rename has succeeded this name no longer exists, and if the rename
		// FAILED the temp file is the only copy of the key — deleting it there
		// would throw away a credential that cannot be re-issued.
		if !installed {
			return
		}

		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict %s: %w", tmpName, err)
	}

	if _, err := tmp.Write(pem); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}

	// The reservation descriptor is released first: on Windows an open handle
	// blocks the rename, and it buys nothing once the content is durable.
	_ = reserved.Close()

	if err := os.Rename(tmpName, path); err != nil {
		// The temp file survives and holds the only copy. Name it, or the
		// operator is left with a registered App and no way to find its key.
		return fmt.Errorf(
			"install the key at %s: %w\nThe key IS saved at %s — move it to %s by hand; "+
				"GitHub cannot re-issue it",
			path, err, tmpName, path)
	}

	installed = true

	// Announced BEFORE the directory fsync. The credential is at its final path
	// and must never be unlinked from here on: a directory sync can fail on a
	// filesystem that does not support it (some FUSE and SMB mounts return
	// ENOTSUP), and treating that as "the write failed" made the caller delete
	// a key that was successfully installed.
	onInstalled()

	if err := syncDir(dir); err != nil {
		return fmt.Errorf(
			"the key is installed at %s but its directory entry could not be flushed: %w\n"+
				"It is present now; a power loss before the filesystem flushes could still lose it, "+
				"so verify with `billet check` after a reboot",
			path, err)
	}

	return nil
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
