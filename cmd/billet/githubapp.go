package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
)

// onboard is github.Onboard behind a seam, so the refusals in front of it are
// reachable from a test.
//
// Without one they are not reachable at all: the only way past github.Onboard is
// an operator with a browser and the hour GitHub allows, so deleting the whole
// preflight left every assertion green — and what the preflight protects is a
// credential GitHub issues exactly once.
var onboard = github.Onboard

func cmdGitHubApp(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet github-app create --org <org>")
	}

	switch args[0] {
	case "create":
		return githubAppCreate(ctx, args[1:])
	case "store-key":
		return githubAppStoreKey(ctx, args[1:])
	case "-h", "--help":
		fmt.Println("usage: billet github-app create --org <org> [--name <name>] [--key-path <path>]")
		fmt.Println("       billet github-app store-key --from <path>   " +
			"(publish an existing key to a store-backed deployment)")
		return nil
	default:
		return fmt.Errorf("unknown github-app subcommand %q", args[0])
	}
}

// githubAppCreate mints the App and records it in the config the operator
// already has.
//
// THIS COMMAND EDITS A CONFIG IN PLACE, AND `billet init` DOES NOT. Two commands
// write the same path under different rules, which is fine, and for a while
// nothing said which rule was about to apply. init GENERATES a whole file, so it
// may replace one only when it can prove the existing bytes are its own output
// and otherwise writes the fresh generation to <path>.new; this one sets five
// scalars under `github:` and leaves every other value, the comments, the mode
// and the owner alone — because the App identity is exactly the thing that has
// to land in a config that already exists, and no generator can merge it for
// you. So it is stated, in configEditRule, before the browser flow.
//
// The ORDER below is the rest of that decision. Everything this run can refuse
// is refused before github.Onboard, because after it the App is registered and
// its private key is spent: planConfigEdit proves the file can take the block
// and reserveKeyFile proves the key has somewhere to go, and only then does
// anything reach GitHub.
func githubAppCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("billet github-app create")
	org := fs.String("org", "", "GitHub organization to create the App for (exactly one of --org and --repository)")
	repository := fs.String("repository", "", "GitHub repository, as owner/name, to create the App for")
	targetName := fs.String("target", config.DefaultTargetName,
		"the target this App serves: default writes the github block, any other name a targets entry")
	name := fs.String("name", "", "suggested App name (GitHub App names are globally unique; you can edit it there)")
	keyPath := fs.String("key-path", "", "where to write the App private key (default: alongside billet.yaml)")
	cfgPath := fs.String("config", "", "billet.yaml to write the github block into")
	noBrowser := fs.Bool("no-browser", false, "print URLs instead of opening a browser")
	port := fs.Int("port", 0, "fixed loopback callback port (needed for `ssh -L` when onboarding a remote host)")

	if err := parse(fs, args); err != nil {
		return err
	}

	// EXACTLY ONE SCOPE, held to the rule config validation applies, so a bad
	// flag is refused by its own name before anything reaches GitHub.
	switch {
	case *org == "" && *repository == "":
		return errors.New("one of --org or --repository is required")
	case *org != "" && *repository != "":
		return errors.New("pass either --org or --repository, not both: a target is an " +
			"organization or one repository")
	case *org != "":
		if err := config.CheckOrg(*org); err != nil {
			return fmt.Errorf("--org: %w", err)
		}
	default:
		if err := config.CheckRepository(*repository); err != nil {
			return fmt.Errorf("--repository: %w", err)
		}
	}

	if *targetName == "" {
		return errors.New("--target must name a target; the github block is named " + config.DefaultTargetName)
	}

	// BEFORE THE APP EXISTS, because a name the config refuses at load would
	// otherwise be found out after the key GitHub issues once has been spent.
	if err := config.CheckTargetName(*targetName); err != nil {
		return fmt.Errorf("--target: %w", err)
	}

	identity := githubBlock{Target: *targetName, Org: *org, Repository: *repository}

	// FIRST, so the config's own refusals are the ones an operator sees.
	//
	// defaultKeyPath READS the config, and it used to run before this — so a
	// --config that does not exist came back as a bare `read --config …: no such
	// file or directory` on the ordinary invocation, and the refusal that names
	// the seed was reachable only when --key-path happened to be given.
	plan, err := planConfigEdit(*cfgPath, identity)
	if err != nil {
		return err
	}

	// THE RESOLVED PATH, so a symlinked --config defaults its key beside the file
	// that actually holds the config rather than beside the link.
	if *keyPath == "" {
		resolved, err := defaultKeyPathFor(plan.path, *targetName)
		if err != nil {
			return err
		}
		*keyPath = resolved
	}

	// BEFORE ANYTHING THAT OUTLIVES THIS COMMAND, and its failure STOPS the run:
	// a notice that could silently not arrive is not a notice. Nothing has
	// happened yet that a person could find afterwards — the preflight stages a
	// probe to check the config's owner and removes it again, refusing if it
	// cannot — so stopping here is free, which is the opposite of the write
	// below, where the App already exists and a failure must not be fatal.
	if err := sayConfigEdit(os.Stdout, plan); err != nil {
		return err
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

	// WHERE THIS DEPLOYMENT KEEPS ITS APP KEY, read before the browser flow.
	//
	// PEEKED RATHER THAN LOADED, because the config is not valid yet: it has no
	// app_id and no installation_id, since registering the App is what produces
	// them. What has to be known here is one key, and knowing it late — after the
	// App exists — would mean discovering only then that the key has nowhere to go.
	storeBacked := config.PeekIdentityBackend(plan.body) == config.IdentitySSM

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

	target := identity.target()

	fmt.Printf("billet requests exactly these permissions for a %s:\n", target.Scope())

	perms := github.Permissions(target.Scope())

	names := make([]string, 0, len(perms))
	for name := range perms {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		fmt.Printf("  %-32s %s\n", name, perms[name])
	}

	fmt.Printf("\nNo repository Contents permission — billet cannot read your code.\n")

	if target.Scope() == github.ScopeRepository {
		// THE WIDER GRANT IS SAID OUT LOUD. It is the only permission GitHub
		// offers for registering a repository's runners, and it also covers the
		// repository's settings, collaborators and branch protection; billet uses
		// it for the registration endpoints and nothing else (ADR-011).
		fmt.Printf("Repository administration is the ONLY permission GitHub offers for registering\n" +
			"a repository's runners. billet uses it for that and nothing else: never the\n" +
			"repository's settings, collaborators or branch protection.\n")
	}

	fmt.Printf("GitHub allows one hour to finish; if it lapses, just run this again.\n\n")

	result, err := onboard(ctx, github.OnboardOptions{
		Target:      target,
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
				*keyPath, target)
		}

		return err
	}

	fmt.Printf("\nDone.\n\n")
	fmt.Printf("  private key      %s\n", *keyPath)

	block := githubBlock{
		Target:         *targetName,
		Org:            *org,
		Repository:     *repository,
		AppID:          result.App.ID,
		ClientID:       result.App.ClientID,
		InstallationID: result.Installation.ID,
		PrivateKeyPath: *keyPath,
	}

	// A STORE-BACKED DEPLOYMENT RECORDS NO PATH, because the key is not going to
	// stay at one. Writing both would be two spellings of where the App key lives
	// — the mistake internal/config has already made three times — and config
	// validation refuses the pair outright, so a block carrying it would produce a
	// file this binary then declines to load.
	if storeBacked {
		block.PrivateKeyPath = ""
	}

	// WRITTEN INTO THE FILE RATHER THAN PRINTED FOR PASTING. Printing was one
	// more step to get wrong, and getting it wrong is quiet: an app_id left at 0
	// is only reported later by `billet check`, and a mistyped one comes back as
	// an authentication failure that says nothing about which digit moved.
	// plan.path, NOT --config: a symlinked --config was resolved by the plan, and
	// writing through the link would replace the link itself with a regular file
	// while leaving the file it pointed at untouched.
	if plan.path != "" {
		if err := writeGitHubBlock(plan.path, block); err != nil {
			// NOT FATAL, because the App exists by now and the credential it
			// issued cannot be re-created. Printing the block is the fallback that
			// keeps this run recoverable.
			fmt.Fprintf(os.Stderr, "\ncould not update %s: %v\n", plan.path, err)

			// ON STDERR, WITH THE DIAGNOSTIC, AND ON STDOUT IF THAT FAILS. This
			// is the ONLY record of an App that now exists, and the run that
			// could not write the config may well be one whose stdout is the
			// reason — a redirect that filled the disk, or a reader that went
			// away. Sending the recovery block to the same stream that just
			// failed loses the identity while the command reports success.
			//
			// STILL NOT FATAL, WHATEVER HAPPENS, and that is about the EXIT
			// STATUS rather than about tidiness. The App exists; a non-zero exit
			// is read by a person and by every wrapper script as "that did not
			// work, run it again", and running it again mints a SECOND App. A
			// sentinel does not change what the process returns — main maps
			// every error to status 1 — so the only way to keep the contract is
			// not to return one. An earlier version of this returned a wrapped
			// ErrCredentialPreserved and read as non-fatal while being exactly
			// as fatal as any other error.
			// Stderr first, beside the diagnostic; then stdout, because a run
			// whose config write failed may well be one whose stderr is the
			// reason.
			reportIdentity(block, os.Stderr, os.Stdout)

			return nil
		}

		fmt.Printf("  config           %s (updated)\n", plan.path)

		// AND INTO THE IDENTITY STORE, AFTER THE CONFIG AND NOT BEFORE IT.
		//
		// THE ORDER IS FORCED BY WHAT A CONFIG IS AT EACH POINT. Until the block
		// above is written this file has no app_id and no installation_id — because
		// registering the App is what produces them — so it does not load, and the
		// publication needs a loaded config to know the region, the prefix and the
		// key. Doing it here is the first moment that is true.
		//
		// THE KEY IS ALREADY DURABLY ON DISK BY NOW, which is what makes the delay
		// safe and the publication recoverable: every failure below leaves a file
		// `billet github-app store-key` can publish, where a publication straight
		// from memory would have exactly one failure mode with no way back.
		if storeBacked {
			storeAppKeyDuringOnboarding(ctx, plan.path, *targetName, *keyPath, []byte(result.App.PEM))
		}

		fmt.Printf("\nThen run: billet check --config %s\n", plan.path)

		return nil
	}

	fmt.Printf("\nAdd this to your billet.yaml:\n\n")

	// Stdout here: this is the ordinary output of a run that was asked to print
	// the block rather than write it, and it is meant to be piped or copied. On
	// failure it falls to stderr and the run still exits 0, for the reason the
	// config path gives: the App exists, and a non-zero exit is an instruction
	// to re-run that mints a second one.
	reportIdentity(block, os.Stdout, os.Stderr)

	fmt.Printf("\nOr re-run with --config <path> and billet will write it for you.\n")

	return nil
}

// githubBlock is the App identity a config needs, for one target.
type githubBlock struct {
	// Target names where the block goes: config.DefaultTargetName (or empty)
	// is the github block, any other name a targets entry.
	Target string
	// Org or Repository, exactly one: the target's scope.
	Org            string
	Repository     string
	AppID          int64
	ClientID       string
	InstallationID int64
	PrivateKeyPath string
}

// reportIdentity prints the block to the first stream that will take it.
//
// IT RETURNS NOTHING, AND THAT IS THE POINT. Every caller is past the moment the
// App was created, where a failure must not become a non-zero exit: that reads,
// to a person and to every wrapper script, as "run it again", and running it
// again mints a SECOND App whose key GitHub also issues exactly once. When
// neither stream takes the block there is nothing left to try and nothing worth
// returning — so the streams are tried in order and each result is checked to
// decide whether to try the next.
func reportIdentity(b githubBlock, streams ...io.Writer) {
	for _, w := range streams {
		if err := printGitHubBlock(w, b); err == nil {
			return
		}
	}
}

// printGitHubBlock writes the identity in the shape a config takes.
//
// IT TAKES A WRITER, because the two callers need different streams. One is
// ordinary output; the other is the last record of an App that already exists,
// printed because writing the config failed — and stdout may be exactly what
// failed.
//
// RENDERED BY THE ENCODER, not by Fprintf. This block is meant to be pasted back
// into a config and read by config.Load, and printf does not quote: a key path
// containing ` #` became a YAML comment, so the value read back short and the
// server opened a file that is not the key. Sending it through the same render
// the writer uses means what is printed is exactly what would have been written,
// quoting included. Its errors are RETURNED, because this is the only record of
// an App that already exists and a stream that silently dropped it is the whole
// reason the caller is here.
func printGitHubBlock(w io.Writer, b githubBlock) error {
	rendered, err := renderIdentity([]byte(seedFor(b)), b)
	if err != nil {
		return fmt.Errorf("render the App identity: %w", err)
	}

	if _, err := w.Write(rendered); err != nil {
		return fmt.Errorf("write the App identity: %w", err)
	}

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
	// A courtesy check for a clear diagnostic, not the safety property: it is a
	// snapshot and the pathname can change immediately after. What protects the
	// destination is that nothing here creates, removes or renames it — the os.Link
	// at install time refuses atomically when the name is taken.
	//
	// pathPresent, not "not absent": an unstattable destination must not block
	// onboarding, because the link is what guarantees safety.
	if lookupPath(path) == pathPresent {
		return nil, destinationOccupiedError(path)
	}

	// The reservation is a SEPARATE file, and that is the whole design.
	//
	// Reserving the destination itself would force the install to unlink it before
	// linking the key into place, and no check can be made atomic with a later unlink
	// by pathname — every guard still has an ordering where another run's key is
	// deleted on the way to installing this one.
	//
	// Reserving elsewhere removes the unlink entirely: the final name is created
	// exactly once, by a link that fails rather than replaces. It also collapses two
	// files into one — this descriptor is both the proof that the directory is
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
	// The `mv` is only offered when the destination is free. Unix mv REPLACES, so
	// recommending it unconditionally would hand the operator a command that destroys
	// a second App's key — the outcome every other rule here exists to prevent,
	// reached by following billet's own instructions.
	//
	// The question is whether the destination is OCCUPIED, not whether it holds
	// something billet recognises: a PEM with trailing junk, a key in a format this
	// build cannot parse, or a file a live writer has not finished are all worth
	// keeping. "Could not tell" is not proof that clobbering is safe either, which is
	// why this is `!= pathAbsent`.
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
// One file, one atomic step. The reservation opened before the browser flow IS the
// staging file — it lives beside the destination, never at it — so the key is
// written into a descriptor this process already owns and then linked into place.
// os.Link creates the final name or fails; it never replaces. That is what removes
// the unlink, and with it every ordering in which another run's key is deleted.
//
// onInstalled fires the instant the key is at its final path, BEFORE durability is
// confirmed. Everything after is best-effort reporting: the credential exists and
// must never be deleted, whatever else fails.
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

	// A FAILED write can still have left a usable key, and that decides what the
	// operator is told.
	//
	// GitHub's PEM ends in a newline, so a write stopping one byte short produces
	// something pem.Decode parses perfectly: "the write returned an error" and "there
	// is no credential here" are different facts. What is on disk is the authority.
	if n, writeErr := reserved.Write(pem); writeErr != nil || n != len(pem) {
		// Flushed so the question below is asked of the filesystem rather than the
		// page cache. Neither result decides anything on its own.
		syncErr := reserved.Sync()

		failure := fmt.Errorf("write %s: wrote %d of %d bytes: %w",
			staging, n, len(pem), errors.Join(writeErr, syncErr))

		// Identity FIRST. "There is a valid key at that name" is not "this run's key
		// survived": another run's key at the staging name would be reported as this one's.
		//
		// `!= identityMatches` is not good enough — a failed stat is not a proven mismatch,
		// and treating it as one writes a second copy of the key and reports only the copy.
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

// recoverKey writes the key somewhere new when the ordinary install could not place
// it, and reports where it landed.
//
// A key written into an unlinked inode is not unrecoverable: writeKeyAtomically
// still holds the complete PEM in memory at every call site that reaches here.
// Declaring a credential lost while the bytes are in a live variable is the worst
// outcome in this file, because the advice that follows is "delete the App".
//
// The same applies one level down: a recovery write that reports an error may STILL
// have left a usable key, so the file is inspected rather than assumed empty. Loss
// is what remains after looking, never what is inferred from a return value.
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
	_, err := readPrivateKey(path)

	return err
}

// readPrivateKey validates the App key and returns its bytes.
//
// ONE implementation, used by both `billet check` and `billet server`. They had
// diverged: check rejected a non-regular file, bounded the read, worked from a
// single descriptor, opened with O_NONBLOCK so a FIFO could not hang it, and
// refused group- or world-readable modes — while the server did os.ReadFile and
// parsed the result. So `billet check` refused a mode-0644 organization
// credential that `billet server` would happily start with, which is the wrong
// way round for the command that runs unattended.
func readPrivateKey(path string) ([]byte, error) {
	// Opened ONCE and inspected through the descriptor. Stat-then-read is two
	// lookups of the same name: the file can be swapped in between, so the size,
	// type and mode may describe a different inode than the bytes that get
	// parsed — and os.ReadFile on a FIFO blocks forever rather than returning.
	f, err := openForInspection(path)
	if err != nil {
		return nil, fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("github.private_key_path %s is not a regular file", path)
	}

	if info.Size() == 0 {
		return nil, fmt.Errorf(
			"github.private_key_path %s is empty; an interrupted `billet github-app create` leaves "+
				"a placeholder there. Remove it and re-run that command", path)
	}

	if info.Size() > maxKeySize {
		return nil, fmt.Errorf("github.private_key_path %s is %d bytes; that is not an App key",
			path, info.Size())
	}

	// Group and other bits on a private key are a local exposure. Checked on
	// unix only: Windows permissions are ACL-based and these bits are meaningless
	// there, so testing them would produce a false alarm on every Windows host.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf(
				"github.private_key_path %s is mode %04o; it is readable beyond its owner. "+
					"Run: chmod 600 %s", path, perm, path)
		}
	}

	// Read from the descriptor already inspected, and bounded for real: the
	// size check above describes the inode at that moment, while this limit
	// holds regardless.
	pemBytes, err := io.ReadAll(io.LimitReader(f, maxKeySize+1))
	if err != nil {
		return nil, fmt.Errorf("read github.private_key_path %s: %w", path, err)
	}

	if len(pemBytes) > maxKeySize {
		return nil, fmt.Errorf("github.private_key_path %s is larger than %d bytes; that is not an App key",
			path, maxKeySize)
	}

	// Parsed, not merely read: a truncated PEM is exactly what an interrupted
	// write leaves, and it fails at the first API call rather than here.
	if err := github.ValidatePrivateKey(pemBytes); err != nil {
		return nil, fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	return pemBytes, nil
}

// defaultKeyPath is where the default target's App key goes when --key-path is
// not given: what the config already names, then beside the config, then
// beside the per-user default. The first branch is what makes the
// local-service flow hold — its generated config names
// /etc/billet/app-private-key.pem, and defaulting to the per-user directory
// instead would move the key into a home directory the packaged unit's
// ProtectHome=true can never read, then rewrite the config to point there.
func defaultKeyPath(cfgPath string) (string, error) {
	return defaultKeyPathFor(cfgPath, config.DefaultTargetName)
}

// defaultKeyPathFor is defaultKeyPath for a named target: what the config
// already names for it, else app-private-key-<target>.pem beside the config,
// so two targets' keys never default to one file.
func defaultKeyPathFor(cfgPath, target string) (string, error) {
	file := "app-private-key.pem"
	if target != "" && target != config.DefaultTargetName {
		file = "app-private-key-" + target + ".pem"
	}

	if cfgPath != "" {
		named, err := configuredKeyPath(cfgPath, target)
		if err != nil {
			return "", err
		}
		if named != "" {
			return named, nil
		}

		return filepath.Join(filepath.Dir(cfgPath), file), nil
	}

	return filepath.Join(filepath.Dir(defaultConfigPath()), file), nil
}

// configuredKeyPath reads a target's private_key_path out of a config file
// that may not fully validate yet (an init-generated file has app_id 0), so it
// is a narrow YAML read rather than config.Load. A read or parse failure is an
// ERROR, not an absent field: collapsing them would silently move the key
// beside a config nobody can read, and the real problem would surface only
// after GitHub already holds a registered App — this flow is not repeatable.
func configuredKeyPath(cfgPath, target string) (string, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", fmt.Errorf("read --config %s: %w", cfgPath, err)
	}

	var doc struct {
		GitHub struct {
			PrivateKeyPath string `yaml:"private_key_path"`
		} `yaml:"github"`
		Targets []struct {
			Name           string `yaml:"name"`
			PrivateKeyPath string `yaml:"private_key_path"`
		} `yaml:"targets"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse --config %s: %w", cfgPath, err)
	}

	if target == "" || target == config.DefaultTargetName {
		return doc.GitHub.PrivateKeyPath, nil
	}

	for _, entry := range doc.Targets {
		if entry.Name == target {
			return entry.PrivateKeyPath, nil
		}
	}

	return "", nil
}

// target is the block's identity as the github package sees it.
func (b githubBlock) target() github.Target {
	if b.Repository != "" {
		owner, name, _ := config.SplitRepository(b.Repository)

		return github.RepositoryTarget(owner, name)
	}

	return github.OrganizationTarget(b.Org)
}

// isDefault reports whether the block goes under `github:`.
func (b githubBlock) isDefault() bool {
	return b.Target == "" || b.Target == config.DefaultTargetName
}

// seedFor is the smallest document the block can be rendered into, for the
// printed form.
func seedFor(b githubBlock) string {
	if b.isDefault() {
		return "github: {}\n"
	}

	return "targets: []\n"
}

// configEditRule is what BOTH commands say about which of them may edit a
// billet.yaml, written once so the two cannot drift apart again.
//
// It is declared once because the whole defect was that the two rules were true
// in different commands and stated in neither: an operator who has learned that
// `billet init` writes a .new rather than touching their config has no reason to
// expect the next command in the same sequence to edit it in place. This command
// prints it before it opens a browser; init prints it in the branch that writes
// a .new, which is where an operator learns the other half.
//
// configEditBrief is the same rule where there is room for a clause and not a
// paragraph — inside init's numbered next steps, which an operator reads as a
// list of commands. CHANGE THEM TOGETHER: two lengths of one rule is fine, two
// rules is the defect this exists to remove.
const (
	configEditRule = "`billet github-app create --config` EDITS the config in place: it sets " +
		"the App identity under `github:` and leaves everything else — every other value, the " +
		"comments, the file's mode and its owner — exactly as it is. `billet init` is the " +
		"command that will NOT overwrite a config, because it generates a whole file and " +
		"cannot merge what you wrote into it; adding one block to a file you already have is " +
		"a different operation."

	configEditBrief = "this EDITS the config in place, adding only the `github:` block"
)

// configEdit is what this run will do to --config, decided BEFORE the App exists.
type configEdit struct {
	// path is the file that will actually be written — --config with any symlink
	// resolved — or "" when no --config was given and the block is only printed.
	path string
	// given is what the operator typed, so the notice can say when the two differ.
	given string
	// existing is the App identity the file already records, when it records one.
	existing githubBlock
	hasApp   bool
	// body is the file's bytes, kept so the run can answer ONE question about a
	// config that is not valid yet: where this deployment keeps its App key. It is
	// empty when no --config was given.
	body []byte
}

// planConfigEdit proves the config can take the App identity, before anything
// reaches GitHub.
//
// EVERY WAY THE FILE CAN REFUSE THE BLOCK USED TO BE DISCOVERED LAST. The edit
// runs after onboarding, so a --config that does not exist, a `github:` key
// holding a list, and a directory billet cannot write all surfaced as "could not
// update <path>" — with the App already registered, its one-time private key
// already spent, and the command exiting 0 over a config that records none of
// it. Re-running is not a recovery there; it mints a second App. The one early
// read that did exist (defaultKeyPath) is skipped whenever --key-path is given,
// so the whole class was invisible on exactly the invocation somebody scripting
// an install writes.
//
// The structural check is the REAL renderer against a probe identity, not a
// second opinion about acceptable YAML. A separate reading of what `github:` may
// hold is a rule that agrees with renderGitHubBlock today and drifts from it on
// the next field — the same two-sources-of-truth mistake, one layer up.
func planConfigEdit(cfgPath string, identity githubBlock) (configEdit, error) {
	if cfgPath == "" {
		return configEdit{}, nil
	}

	given := cfgPath

	// A SYMLINK IS RESOLVED, BECAUSE THE WRITE IS A RENAME. os.Rename over a
	// symlink replaces the LINK with a regular file and leaves the file it
	// pointed at untouched — so an operator whose /etc/billet/billet.yaml points
	// somewhere else would lose the link, keep the old content at the target, and
	// be told their config was edited in place. Following it once, here, is what
	// makes that claim true; the notice says so when the two differ.
	if info, err := os.Lstat(cfgPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(cfgPath)
		if err != nil {
			return configEdit{}, fmt.Errorf("--config %s is a symbolic link and billet cannot "+
				"resolve what it points at: %w", cfgPath, err)
		}

		cfgPath = resolved
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configEdit{}, missingConfigError(cfgPath)
		}

		return configEdit{}, fmt.Errorf("read --config %s: %w", cfgPath, err)
	}

	// BOTH SHAPES THE WRITE CAN TAKE, through the SAME render-and-verify it uses.
	//
	// An earlier version rendered with zero ids and skipped the read-back
	// entirely, which made the preflight structurally weaker than the write in a
	// way no seed shape showed: `org: [old]` is a mapping key whose VALUE is a
	// sequence, and setScalar writes Value onto that node without changing its
	// kind — so the render succeeds, the encoder emits the sequence anyway, and
	// only the read-back notices.
	//
	// One probe was still not enough. renderGitHubBlock sets client_id ONLY when
	// GitHub returned one, so a config whose existing client_id cannot be read
	// back is repaired by a probe carrying one and left broken by a real identity
	// that omits it — and billet cannot know in advance which shape GitHub will
	// hand it. Both have to work, or the second is discovered after the App
	// exists.
	if err := probeConfig(raw, identity); err != nil {
		remedy := ""
		if errors.Is(err, errNoDocument) {
			// OFFERED HERE AND NOWHERE ELSE, because this is the one refusal
			// where nothing in the file is at risk. `touch billet.yaml` reaches
			// it, and so does a file holding only comments — which is why the
			// command APPENDS: `>` would be safe on the first and would throw
			// away what the operator wrote on the second. There is no document
			// to damage either way, so adding one destroys nothing, and that
			// also makes `set -C` unnecessary here.
			// The LEADING newline is not decoration: a file whose last byte is
			// not one — `printf '# later' > billet.yaml` — would otherwise gain
			// `# latergithub: {}`, which is still a file with no document in it,
			// from a command billet handed the operator.
			remedy = fmt.Sprintf("\n\nThere is no document in it yet, so either generate a "+
				"config with `billet init --config %s` or add the seed to it:\n  "+
				"printf '\\n%%s\\n' %s >> %s", shellArg(cfgPath), shellArg(bootstrapSeed),
				shellArg(cfgPath))
		}

		return configEdit{}, fmt.Errorf("%s cannot take the App identity: %w%s\n\n"+
			"Refused now rather than after the App exists: GitHub issues its private key exactly "+
			"once, so a config that cannot record it is not something to find out afterwards",
			cfgPath, err, remedy)
	}

	// THE DIRECTORY, because the write stages a sibling and renames over the
	// destination — a rename needs the directory, not the file. One-directional,
	// like the only other caller: a definite no refuses, and anything else goes
	// on to the write, which keeps its own non-fatal fallback.
	if dir := filepath.Dir(cfgPath); !dirWritable(dir) {
		return configEdit{}, fmt.Errorf(
			"billet cannot write in %s, so it could not record the App in %s after creating it. "+
				"GitHub issues an App private key exactly once, so this is refused now rather "+
				"than reported afterwards: fix the directory's permissions, or run this as the "+
				"account that owns it", dir, cfgPath)
	}

	// AND THAT THE REPLACEMENT CAN KEEP THE OWNER — proved by doing it, on this
	// filesystem, rather than by reasoning about uids.
	//
	// The write refuses rather than hand a root-owned config to the invoker, and
	// that refusal was reachable only from inside writeGitHubBlock, which runs
	// AFTER onboarding: a deterministic, knowable-in-advance failure landing on
	// the far side of an App that cannot be un-created. Reasoning about it
	// instead would be wrong in both directions — CAP_CHOWN grants it to a
	// non-root process, and a file's owner may change its group to one they
	// belong to — so the same operation is attempted here, against a file of our
	// own, while failing is still free.
	if err := ownershipPreservable(cfgPath, raw); err != nil {
		return configEdit{}, err
	}

	edit := configEdit{path: cfgPath, given: given, body: raw}
	if gb, _, ok := existingIdentity(raw, identity.Target); ok {
		edit.existing, edit.hasApp = gb, true
	}

	return edit, nil
}

// probeConfig proves this config takes an App identity in either shape the real
// write can produce.
//
// COMPLETE IDENTITIES, because the verification half of renderIdentity reads the
// identity back out of the rendered bytes and a zero app_id reads as "no
// identity here" — so a probe with zero ids would skip the very check it exists
// to run. The values are never written anywhere: the rendered bytes are
// discarded, and the real identity is rendered from scratch after onboarding.
//
// TWO of them, because client_id is the one field whose treatment DEPENDS on the
// identity: renderGitHubBlock sets it when GitHub returned one and REMOVES it
// when it did not. billet cannot know in advance which it will get, so a config
// either shape breaks on has to be refused before the App exists.
//
// The two do diverge. `client_id: {a: b}` is a mapping, and the shape that
// carries a client id assigns a Value to that node without changing its kind —
// so the encoder emits the mapping, the read-back cannot decode it, and the
// write refuses; the shape that omits one deletes the key outright and succeeds.
//
// ONLY THE SHAPE CARRYING A CLIENT ID HAS A REACHABLE FAILING CASE TODAY,
// measured rather than reasoned: dropping the other changes no test. That was
// the other way round before removal was added, which is the point — the
// direction is a property of what renderGitHubBlock happens to do, and the next
// field given a conditional there would flip it again. Both are kept for the
// reason checkCarried is kept, and the surviving mutant is said out loud rather
// than left to look like a gap.
//
// The ORG is the run's own in both, because that is the field the verification
// compares against a caller-supplied value.
func probeConfig(raw []byte, identity githubBlock) error {
	base := githubBlock{
		Target:         identity.Target,
		Org:            identity.Org,
		Repository:     identity.Repository,
		AppID:          1,
		InstallationID: 1,
		PrivateKeyPath: "probe",
	}

	withClientID := base
	withClientID.ClientID = "probe"

	for _, probe := range []githubBlock{withClientID, base} {
		if _, err := renderIdentity(raw, probe); err != nil {
			return err
		}
	}

	return nil
}

// ownershipPreservable is checkOwnershipPreservable behind a seam.
//
// The refusal it guards needs a config owned by an account this process is not,
// which a test cannot arrange without root — and what the refusal is really for
// is the ORDER: that a knowable-in-advance failure lands before an App exists
// rather than after. Without a seam that ordering is unobservable, and deleting
// the call left every assertion green.
var ownershipPreservable = checkOwnershipPreservable

// checkOwnershipPreservable proves the config's owner can be reproduced on a
// file billet creates in its directory.
//
// IT PERFORMS THE REAL WRITE'S SEQUENCE, on a file of the same size, rather than
// chowning an empty inode. A group block quota is charged for the blocks a file
// holds, so handing over an empty one can succeed where handing over the config
// fails with EDQUOT — and a preflight that proves the easy half is a preflight
// that lets the App be created and then fails.
//
// ITS FAILURE BRANCHES HAVE NO REACHABLE FAILING CASE and their mutants survive,
// which is said here rather than left to look like a gap: the body write and the
// sync need a filesystem that runs out of room or reports a device error, the
// removal needs a directory this process cannot unlink from, and the two
// non-matching identity states need something racing this exact temporary name.
// A test can arrange none of them, and on every filesystem it CAN arrange, an
// empty probe and a full one answer alike. What IS covered is that the check
// runs, that it runs before the App exists, and that a successful run leaves
// nothing of it behind.
//
// THE MODE IS WIDENED BEFORE THE OWNER IS FIXED, and that is deliberate. A 0640
// config whose group is not the caller's leaves this copy readable by the
// caller's group for the microseconds in between — a window worth naming, and
// not worth closing here: chowning first would leave a non-root caller holding a
// file it no longer owns, so the fchmod that follows would need CAP_FOWNER, and
// a legitimate path would start failing to remove an exposure of a file that
// holds no secret. The config names where the App key lives; it does not contain
// it. The real write has the same ordering for the same reason.
func checkOwnershipPreservable(cfgPath string, body []byte) error {
	info, err := os.Stat(cfgPath)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", cfgPath, err)
	}

	uid, gid, ok := fileOwner(info)
	if !ok {
		return nil
	}

	probe, err := os.CreateTemp(filepath.Dir(cfgPath), ".billet-config-probe-*")
	if err != nil {
		return fmt.Errorf("stage a probe beside %s: %w", cfgPath, err)
	}

	name := probe.Name()

	// NOT A defer, because its own failure has to be reported. This file is a
	// COMPLETE COPY of the operator's config, and everything here happens before
	// the App exists — so a cleanup that could not remove it is a reason to stop
	// rather than a leftover to shrug at. writeGitHubBlock's cleanup is deferred
	// and silent because by the time it runs the App may already exist, where
	// stopping is the one thing that must not happen.
	probeErr := probeReplacement(probe, body, info, uid, gid)

	// IDENTITY-BOUND, AND ALL THREE ANSWERS MEAN SOMETHING. Go unlinks by name,
	// and a name in a directory somebody else can write is not proof of a file —
	// but "not ours" and "could not tell" are not "removed", and folding them
	// into the same silent nothing is the collapse this codebase keeps having to
	// undo. Only a proven match may be unlinked; the other two say so, because a
	// complete copy of the operator's config may be sitting there and nothing
	// after this would look again.
	var cleanupErr error

	switch verifyInstalled(probe, name) {
	case identityMatches:
		cleanupErr = os.Remove(name)
	case identityDiffers:
		// ABSENCE IS FINE AND A STRANGER IS NOT. If the name is gone, nothing
		// survives — this run's bytes are in an inode with no directory entry
		// and die with the process. Anything else there is not billet's to
		// remove, and is worth stopping over while stopping is still free.
		if lookupPath(name) != pathAbsent {
			cleanupErr = fmt.Errorf("%s is no longer the file billet staged, so it was left "+
				"alone", name)
		}
	case identityUnknown:
		cleanupErr = fmt.Errorf("billet could not confirm what %s is, so it was left alone", name)
	}

	// EVERY FAILURE IS REPORTED, not the first one that happened to be checked.
	// Returning probeErr early hid a probe that could not be cleaned up, which
	// is the failure this whole branch exists to surface.
	if err := errors.Join(probeErr, cleanupErr, probe.Close()); err != nil {
		return fmt.Errorf("%s: billet staged %s to check the replacement, and %w",
			cfgPath, name, err)
	}

	return nil
}

// probeReplacement performs on the probe what the real write performs on its
// staged file, in the same order.
func probeReplacement(probe *os.File, body []byte, info os.FileInfo, uid, gid int) error {
	if _, err := probe.Write(body); err != nil {
		return fmt.Errorf("a replacement of this size cannot be staged: %w", err)
	}

	if err := probe.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("the replacement cannot be given the mode this file has: %w", err)
	}

	if err := preserveOwner(probe, uid, gid); err != nil {
		return err
	}

	// AND THE SYNC, which is the step the real write ends on. Delayed allocation
	// means a quota or a device error is reported HERE rather than at the write,
	// so a probe that stopped at the chown proved the size and not the space.
	if err := probe.Sync(); err != nil {
		return fmt.Errorf("a replacement of this size cannot be flushed to disk: %w", err)
	}

	return nil
}

// missingConfigError explains a --config that is not there.
//
// This command writes INTO a config; it never creates one, and a file it
// invented would name no tiers, no state directories and no ceiling. The seed it
// offers instead is the one init's own guidance already prints, down to the
// `set -C`: after a successful run that file is the only local record of the App
// id, the installation id and the key path, and a plain `>` truncates it.
func missingConfigError(cfgPath string) error {
	return fmt.Errorf("--config %s does not exist. `billet github-app create` writes the App "+
		"identity into a config you already have; it never creates one. Generate one with "+
		"`billet init --config %s`, or — if all you want is the identity — seed a file that "+
		"holds nothing else:\n  (set -C; printf '%%s\\n' %s > %s)\n\n`set -C` is why that "+
		"refuses an existing file rather than truncating it",
		cfgPath, shellArg(cfgPath), shellArg(bootstrapSeed), shellArg(cfgPath))
}

// sayConfigEdit states what this run will do to the operator's own file, before
// the browser flow rather than after it.
//
// There is no prompt. Authorising the App in a browser is already the consent
// point, and a second one here would break --no-browser and every scripted
// install; what was missing is that an operator could not read what was about to
// happen to their config before they got there.
func sayConfigEdit(w io.Writer, edit configEdit) error {
	if edit.path == "" {
		_, err := fmt.Fprint(w, "No --config was given, so nothing on this machine will be "+
			"edited: the github: block is printed at the end for you to paste.\n\n")

		return sayFailed(err)
	}

	if _, err := fmt.Fprintf(w, "%s\n\nThis run will edit %s. Run without --config to print "+
		"the block for pasting instead.\n\n", configEditRule, edit.path); err != nil {
		return sayFailed(err)
	}

	if edit.given != edit.path {
		// THE FILE THAT CHANGES IS NOT THE ONE THEY TYPED, and they have to be
		// told which, or the diff they go looking at afterwards is the wrong one.
		if _, err := fmt.Fprintf(w, "(%s is a symbolic link to it, and the link itself is "+
			"left alone.)\n\n", edit.given); err != nil {
			return sayFailed(err)
		}
	}

	if edit.hasApp {
		// SAID, NOT REFUSED. Minting a second App against a fresh --key-path is a
		// deliberate thing an operator does and init's own guidance describes it —
		// but replacing the identity a running deployment authenticates with is
		// not something to discover from a diff afterwards.
		if _, err := fmt.Fprintf(w, "NOTE: %s already names App %d (%s) for target %s. Finishing this "+
			"REPLACES that identity with the new App's; the old App stays on GitHub, "+
			"unreferenced by this config.\n\n",
			edit.path, edit.existing.AppID, edit.existing.describe(), edit.existing.targetName()); err != nil {
			return sayFailed(err)
		}
	}

	return nil
}

// sayFailed turns a failure to deliver the notice into a refusal.
//
// THE NOTICE IS THE COMMITMENT, so a run that could not print it must not go on
// to create an App. A full disk on a redirected stdout is the realistic case,
// and the alternative is the failure shape this repository keeps naming: the
// thing that would have objected was itself missing.
func sayFailed(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("could not say what this run would do to your config, so it has not "+
		"been done: %w", err)
}

// writeGitHubBlock sets the App identity in a config, leaving everything else —
// including the comments — as it was.
//
// A YAML NODE TREE RATHER THAN MARSHALLING THE STRUCT BACK. config.Config drops
// every comment and reorders nothing predictably, so a round trip through it
// would hand back a file that is technically equivalent and unrecognisable: the
// operator's own notes gone, and the diff impossible to review. Editing the
// tree touches five scalars.
//
// ATOMIC, because this file is the only record of where the state directory and
// the App key live. A partial write during a crash would leave a config that
// does not parse and a deployment that cannot start.
func writeGitHubBlock(path string, b githubBlock) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	rendered, err := renderIdentity(raw, b)
	if err != nil {
		return fmt.Errorf("%s: %w; it was NOT updated, and the App that was just created is "+
			"recorded nowhere here", path, err)
	}

	// THE REPLACEMENT PRESERVES WHAT THE ORIGINAL HAD. A rename swaps the
	// inode, so without this a package-installed root:billet 0640 config — or
	// the one `init --profile local-service` just arranged — silently becomes
	// 0600 owned by the invoker, and the billet-server unit (User=billet) can
	// no longer read its own config. Mode preservation is mandatory; ownership
	// is enforced only for root, because a non-root invoker rewriting its own
	// file already owns it and some platforms refuse even a same-id chown.
	prev, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	// A FILE THIS CALL CREATED, NEVER <path>.tmp.
	//
	// That name is predictable and os.WriteFile TRUNCATES whatever holds it, so
	// `--key-path <config>.tmp` put the App private key at exactly the name this
	// write then destroyed — on its way to renaming the config over it, leaving
	// the App registered, its one-and-only key gone, and the config pointing at a
	// path that no longer exists. It also silently clobbered any unrelated file
	// there. CreateTemp opens O_EXCL with a name nothing else can be holding, so
	// this can only ever write over its own work.
	staged, err := os.CreateTemp(filepath.Dir(path), ".billet-config-*")
	if err != nil {
		return fmt.Errorf("stage a replacement for %s: %w", path, err)
	}

	name := staged.Name()

	committed := false

	// THE DESCRIPTOR IS HELD TO THE END, because everything below would otherwise
	// act on a NAME. A name in a directory somebody else can write is not proof
	// of a file: the inode can be renamed away and something else created under
	// it, and this directory is exactly where another run keeps its App key. So
	// the mode and the owner are set through the descriptor, which cannot be
	// substituted, and the two operations that can only take a name — the removal
	// here and the rename below — ask os.SameFile first. That narrows the window
	// the way writeKeyAtomically narrows its own; it does not close it, and the
	// residual is the same one recorded there.
	//
	// THE REMOVAL'S OWN BRANCH HAS NO REACHABLE TEST, and that is worth saying
	// rather than leaving to be rediscovered: every failure a test can arrange
	// here — an unreadable config, an identity that will not read back, an
	// unwritable directory — happens BEFORE this file is created, so there is
	// nothing staged to leave behind. Its mutant survives. What is covered is
	// that a successful write leaves nothing, which is what catches a forgotten
	// rename.
	defer func() {
		if !committed && verifyInstalled(staged, name) == identityMatches {
			_ = os.Remove(name)
		}

		_ = staged.Close()
	}()

	if _, err := staged.Write(rendered); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	if err := staged.Chmod(prev.Mode().Perm()); err != nil {
		return fmt.Errorf("preserve the mode of %s: %w", path, err)
	}

	if uid, gid, ok := fileOwner(prev); ok {
		if err := preserveOwner(staged, uid, gid); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	// SYNCED AFTER THE MODE AND THE OWNER, not just after the bytes.
	//
	// It ran before them at first, which flushes the contents and leaves the
	// metadata unflushed: a crash could then leave the config surviving as
	// CreateTemp made it — 0600, owned by whoever ran this — instead of the
	// root:billet 0640 the packaged server unit has to read. The rename that
	// follows is what publishes all of it, so everything it publishes has to be
	// on disk first. commitConfig gives the same reason for syncing at all.
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}

	// CHECK-THEN-ACT, and it cannot be made atomic with the rename. It is here
	// for the same reason the key install checks before its os.Link: without it,
	// a name substituted in the meantime is what gets installed as the config.
	if verifyInstalled(staged, name) != identityMatches {
		return fmt.Errorf("%s is no longer the file this run staged, so it was NOT installed "+
			"as %s", name, path)
	}

	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	committed = true

	// THE RENAME IS MADE DURABLE, and its failure is NOT this call's failure. The
	// config is in place; a directory entry that has not reached the disk is a
	// power-loss risk to say out loud, not a reason to tell the caller the write
	// failed — which would print the block as though nothing had been recorded.
	if err := syncDir(filepath.Dir(path)); err != nil {
		fmt.Fprintf(os.Stderr, "\nWarning: %s was updated, but that could not be flushed to "+
			"disk (%v). Confirm it still names the App after a reboot.\n", path, err)
	}

	return nil
}

// preserveOwner gives the staged file the ownership the config already had, and
// refuses when it cannot.
//
// A FAILED CHOWN USED TO BE IGNORED FOR ANY NON-ROOT CALLER, on the reasoning
// that such a caller is rewriting a file it already owns. That is the ordinary
// case and not the only one: a config owned by root in a directory the invoker
// can write is replaced by the rename, and its owner silently becomes the
// invoker — after which the packaged server unit, which runs as billet, cannot
// read its own config, and this command has just promised the owner was left
// alone.
//
// What the accommodation was really for is the filesystem that refuses even a
// same-id chown — root-squashed NFS, and mounts with synthesized ownership — so
// that is what is checked, and the EFFECTIVE UID decides nothing. It was in the
// condition at first, and it was wrong in both directions: root gets no
// accommodation on those filesystems, and a non-root process holding CAP_CHOWN
// succeeds outright. What the file ALREADY IS answers both.
func preserveOwner(staged *os.File, uid, gid int) error {
	chownErr := staged.Chown(uid, gid)
	if chownErr == nil {
		return nil
	}

	info, err := staged.Stat()
	if err != nil {
		return fmt.Errorf("preserve the ownership (%w): %w", chownErr, err)
	}

	got, gotGID, ok := fileOwner(info)
	if !ok {
		return fmt.Errorf("billet cannot read the ownership of the file it staged (%w), so it "+
			"cannot promise the config keeps the owner it has", chownErr)
	}

	// ALREADY WHAT WAS ASKED FOR. Nothing has to happen, so a filesystem that
	// declined to do nothing is not a reason to stop.
	if got == uid && gotGID == gid {
		return nil
	}

	return fmt.Errorf("this would replace a config owned by %d:%d with one owned by %d:%d, and "+
		"handing it back was refused (%w). Run this as the account that owns the config, or "+
		"as root", uid, gid, got, gotGID, chownErr)
}

// renderIdentity renders the App identity into raw and PROVES it survived.
//
// ONE FUNCTION FOR BOTH CALLERS, because two were not the same rule. The write
// rendered and then read the identity back; the preflight only rendered — and
// `org: [old]` is a key whose VALUE is a sequence, which setScalar assigns a
// Value to without changing its kind, so the render succeeds, the encoder emits
// the sequence anyway, and only the read-back notices. That is a config the
// preflight accepted and the write refused, after the App existed: exactly the
// failure the preflight was added to remove, reintroduced by stating its rule
// twice.
//
// The check itself is older, and its reason is unchanged: a render that silently
// wrote no block returned nil, and by then the App exists and its one-time
// private key is spent, so "run it again" is not a recovery — it mints a second
// App. It is kept for whatever field is added to the block next.
func renderIdentity(raw []byte, b githubBlock) ([]byte, error) {
	rendered, err := renderGitHubBlock(raw, b)
	if err != nil {
		return nil, err
	}

	// EVERY FIELD THE BLOCK CARRIES, not the three that were easy.
	//
	// NONE OF THE PER-FIELD COMPARISONS HAS A REACHABLE FAILING CASE, measured
	// rather than assumed, and their mutants survive. Every shape that can go
	// wrong today is caught one line earlier by the DECODE: an aliased value
	// re-emits as an alias to an anchor that does not exist, a sequence or
	// mapping under one of these keys emits as itself, and a duplicate key is a
	// decode error — all of which land on `!ok`. What the field checks are for is
	// the case none of that catches, a value that decodes cleanly and is not the
	// one asked for, which is the shape the NEXT field added here would arrive
	// in. That is the same reason checkCarried exists, and it is why client_id
	// and the key path are checked rather than left to the three that happened to
	// be here.
	got, gotKeyPath, ok := existingIdentity(rendered, b.Target)
	if !ok || got.AppID != b.AppID || got.InstallationID != b.InstallationID ||
		got.Org != b.Org || got.Repository != b.Repository || got.ClientID != b.ClientID ||
		gotKeyPath != b.PrivateKeyPath {
		return nil, errIdentityLost
	}

	return rendered, nil
}

// errIdentityLost is a config the identity can be written into and not read back
// out of.
var errIdentityLost = errors.New("the App identity does not survive being written into this file")

// renderGitHubBlock is writeGitHubBlock's pure half: it sets the App identity
// in a config's BYTES, leaving everything else — comments included — as it
// was. Split out so `init`'s convergence can build the complete final file in
// memory and commit it in one atomic replace, with no window where the file on
// disk lacks the identity.
func renderGitHubBlock(raw []byte, b githubBlock) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errNoDocument
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("the config is not a mapping")
	}

	// THE DEFAULT TARGET IS THE github BLOCK; ANY OTHER IS A targets ENTRY, found
	// by name or appended. One renderer for both, because two would be two
	// rules about what a target block may hold.
	var (
		gh  *yaml.Node
		err error
	)

	if b.isDefault() {
		gh, err = mappingFor(root, "github")
	} else {
		gh, err = targetEntryFor(root, b.Target)
	}

	if err != nil {
		return nil, err
	}

	// EXACTLY ONE SCOPE KEY SURVIVES. A block that carried org and gains a
	// repository would be a target config refuses at load, so the other
	// spelling goes.
	if b.Repository != "" {
		setScalar(gh, "repository", b.Repository)
		removeScalar(gh, "org")
	} else {
		setScalar(gh, "org", b.Org)
		removeScalar(gh, "repository")
	}

	setScalar(gh, "app_id", strconv.FormatInt(b.AppID, 10))
	setScalar(gh, "installation_id", strconv.FormatInt(b.InstallationID, 10))
	setScalar(gh, "private_key_path", b.PrivateKeyPath)

	// SET WHEN GITHUB RETURNED ONE, AND REMOVED WHEN IT DID NOT.
	//
	// Leaving a client_id in place was the dangerous half. It belongs to the App
	// being REPLACED, and newScaleSetClient PREFERS it over app_id when minting
	// the App JWT — so a config carrying the old App's client id beside the new
	// App's private key authenticates as neither, and the failure surfaces at
	// GitHub as a rejected credential rather than anywhere near this file.
	// Writing an empty one instead would be a key the operator has to wonder
	// about, so the key goes.
	if b.ClientID != "" {
		setScalar(gh, "client_id", b.ClientID)
	} else {
		removeScalar(gh, "client_id")
	}

	var out bytes.Buffer

	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)

	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	return out.Bytes(), nil
}

// errNoDocument is a config with no YAML document in it at all — empty, or
// nothing but comments.
//
// A SENTINEL RATHER THAN PROSE, because it is the one structural refusal with a
// remedy billet can name: there is nothing in the file to destroy, so "put
// `github: {}` in it" is safe advice. Telling a config whose `github:` key holds
// a list the same thing would be telling an operator to overwrite what they
// wrote. Recognising it with errors.Is is what keeps planConfigEdit from reading
// the YAML a second time to work out which case it is in.
var errNoDocument = errors.New("the config has no YAML document in it")

// targetEntryFor returns the mapping of the named targets entry, appending a
// new entry carrying only its name when there is none.
//
// The list is created when absent, and refused when the key holds anything
// but a sequence: filling something an operator wrote would destroy it, and
// the caller is about to write a credential identity.
func targetEntryFor(root *yaml.Node, name string) (*yaml.Node, error) {
	var list *yaml.Node

	for i := 0; i+1 < len(root.Content); i += 2 {
		if !isKey(root.Content[i], "targets") {
			continue
		}

		found := root.Content[i+1]

		switch {
		case found.Kind == yaml.SequenceNode:
			list = found
		case found.Kind == yaml.ScalarNode && (found.Tag == "!!null" || found.Value == ""):
			found.Kind = yaml.SequenceNode
			found.Tag = ""
			found.Value = ""
			found.Style = 0
			list = found
		default:
			return nil, fmt.Errorf("the %q key is a %s, not a list; billet will not replace it",
				"targets", nodeKind(found))
		}

		break
	}

	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "targets"}, list)
	}

	for _, entry := range list.Content {
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("a targets entry is a %s, not a mapping; billet will not "+
				"replace it", nodeKind(entry))
		}

		for i := 0; i+1 < len(entry.Content); i += 2 {
			if isKey(entry.Content[i], "name") {
				var decoded string
				if err := entry.Content[i+1].Decode(&decoded); err == nil && decoded == name {
					return entry, nil
				}
			}
		}
	}

	entry := &yaml.Node{Kind: yaml.MappingNode}
	setScalar(entry, "name", name)
	list.Content = append(list.Content, entry)

	return entry, nil
}

// mappingFor returns the mapping at a top-level key, creating it if absent.
func mappingFor(root *yaml.Node, key string) (*yaml.Node, error) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if !isKey(root.Content[i], key) {
			continue
		}

		found := root.Content[i+1]

		switch {
		case found.Kind == yaml.MappingNode:
			return found, nil

		// A PRESENT KEY WITH NOTHING UNDER IT IS THE SHAPE A PERSON WRITES.
		//
		// `github:` on its own parses as a null scalar, not a mapping. This
		// used to return that scalar, and setScalar then appended to a scalar's
		// Content — which the encoder ignores. The result was the exact failure
		// this codebase keeps naming: no block written, no error returned, and
		// `github-app create` printing "(updated)" over a file it had not
		// changed, having already minted an App whose one-time key is spent.
		//
		// Filling it is what the caller means, and it destroys nothing.
		case found.Kind == yaml.ScalarNode && (found.Tag == "!!null" || found.Value == ""):
			found.Kind = yaml.MappingNode
			found.Tag = ""
			found.Value = ""
			found.Style = 0

			return found, nil

		// Anything else holds content that filling it would destroy. Refusing
		// is the only safe answer: the caller is about to write a credential
		// identity, and silently discarding whatever an operator put here is
		// worse than making them look at it.
		default:
			return nil, fmt.Errorf("the %q key is a %s, not a mapping; billet will not replace "+
				"it", key, nodeKind(found))
		}
	}

	k := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	v := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content, k, v)

	return v, nil
}

// nodeKind names a YAML node for a diagnostic an operator has to act on.
func nodeKind(n *yaml.Node) string {
	switch n.Kind {
	case yaml.SequenceNode:
		return "list"
	case yaml.ScalarNode:
		return "value"
	case yaml.MappingNode:
		return "mapping"
	case yaml.DocumentNode:
		return "document"
	case yaml.AliasNode:
		return "alias"
	default:
		return "node"
	}
}

// isKey reports whether a mapping's key node IS the named key.
//
// IT ASKS THE DECODER, because a node's Value is not always its key and matching
// on Value alone is wrong in BOTH directions. An ALIAS used as a key carries the
// ANCHOR NAME in Value, so `? *client_id : keep-me` matched a search for
// `client_id` — and removeScalar, which deletes, would have taken the operator's
// unrelated entry with it while the identity still read back correctly, so
// nothing downstream would have noticed. Refusing every alias instead was the
// other direction: an anchor named something else whose VALUE is `github` is a
// key yaml resolves to `github`, and skipping it makes mappingFor append a
// second one, leaving a document with two.
//
// Decoding answers both, and answers with what every later reader of this file
// will see — including the struct decoder that reads the identity back, which
// converts the same way. A key that does not decode to this spelling is not this
// key; a mapping or a sequence used as one does not decode at all.
//
// One definition for all three callers, because a rule about what a key IS that
// three functions each spell for themselves is the two-sources-of-truth mistake
// this file has already made twice.
func isKey(n *yaml.Node, key string) bool {
	var decoded string
	if err := n.Decode(&decoded); err != nil {
		return false
	}

	return decoded == key
}

// removeScalar drops a key and its value from a mapping, if it is there.
func removeScalar(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if isKey(m.Content[i], key) {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)

			return
		}
	}
}

// setScalar sets a key in a mapping, replacing the value and keeping the key's
// comments.
func setScalar(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if isKey(m.Content[i], key) {
			m.Content[i+1].Value = value
			m.Content[i+1].Tag = ""
			m.Content[i+1].Style = 0

			return
		}
	}

	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value})
}
