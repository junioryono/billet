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
			if err := writeKeyAtomically(keyFile, *keyPath, []byte(app.PEM)); err != nil {
				return err
			}

			keyWritten = true

			fmt.Printf("Saved the private key to %s\n", *keyPath)

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

	// An EMPTY file here is billet's own reservation from a run that died before
	// writing anything — a SIGKILL or a power cut between reserving and
	// receiving the key. It cannot contain a credential, so adopting it loses
	// nothing; refusing it would leave the operator permanently unable to re-run
	// without knowing to delete a file they never created.
	//
	// Anything non-empty is refused: it may be a real key, and GitHub cannot
	// re-issue one that is lost.
	info, statErr := os.Stat(path)
	if statErr != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, statErr)
	}

	if info.Mode().IsRegular() && info.Size() == 0 {
		reused, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, fmt.Errorf("reuse the empty reservation at %s: %w", path, err)
		}

		return reused, nil
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
func writeKeyAtomically(reserved *os.File, path string, pem []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".billet-key-*")
	if err != nil {
		return fmt.Errorf("create a temporary file next to %s: %w", path, err)
	}

	tmpName := tmp.Name()

	defer func() {
		_ = tmp.Close()
		// A no-op once the rename has succeeded: the name no longer exists.
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
		return fmt.Errorf("install the key at %s: %w", path, err)
	}

	return syncDir(dir)
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
