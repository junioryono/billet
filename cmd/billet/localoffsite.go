package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/archivestore"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/state"
)

// archiveStore is what the two commands need from the off-box copy. An
// interface so a test can drive both without a bucket, and so the command layer
// says out loud that it can PUT, GET and LIST — and cannot delete.
type archiveStore interface {
	deployarchive.ObjectStore

	Prefix(deployment string) string
	List(ctx context.Context, deployment string) ([]archivestore.Archive, error)
}

// openArchiveStore builds the store this config names, or reports that it names
// none.
//
// THE CREDENTIAL RESOLVER IS THE FLEET'S, adapted rather than reimplemented:
// environment variables or IMDSv2 and nothing else, which is the rule
// internal/provider/ec2 already states and the reason billet refuses to fall
// back to IMDSv1. On the recommended AWS control plane that means an instance
// role and no new stored secret on the one host that also holds the App key.
var openArchiveStore = func(cfg *config.Config) (archiveStore, bool, error) {
	if cfg.Backup == nil || cfg.Backup.S3 == nil {
		return nil, false, nil
	}

	store, err := archivestore.NewS3(*cfg.Backup.S3, awscreds.Default())
	if err != nil {
		return nil, true, err
	}

	return store, true, nil
}

// reportBackupAge says how old the newest archive in the bucket is.
//
// ADVISORY, ALWAYS, and it never returns an error: `billet check` is a
// diagnostic, and a bucket that cannot be reached must not stop an operator
// seeing the local facts they came for. It is here because the failure it makes
// visible is silent by construction — a timer that stopped firing looks exactly
// like one that is working, and nothing else in billet ever looks.
func reportBackupAge(ctx context.Context, cfg *config.Config, skipNetwork bool) {
	if cfg.Backup == nil || cfg.Backup.S3 == nil {
		return
	}

	if skipNetwork {
		fmt.Printf("backup   %s (age not checked during maintenance)\n", cfg.Backup.S3.Bucket)

		return
	}

	store, _, err := openArchiveStore(cfg)
	if err != nil {
		fmt.Printf("backup   UNCHECKED: %v\n", err)

		return
	}

	// AN ABSENT IDENTITY AND AN UNREADABLE ONE ARE DIFFERENT FACTS, and collapsing
	// them was the mistake this repository keeps writing down. Absent is the
	// ordinary uncommissioned host, where listing every deployment is the useful
	// answer. UNREADABLE is billet not knowing whose archives these are — and
	// answering it with "every deployment" would report a NEIGHBOUR's recent
	// backup as this host's newest, hiding a stale or missing one on the exact
	// host whose identity is already in trouble.
	deployment, _, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		fmt.Printf("backup   UNCHECKED: this host's deployment identity could not be read (%v),\n",
			err)
		fmt.Printf("         so billet cannot tell which archives are its own\n")

		return
	}

	archives, err := store.List(ctx, deployment)
	if err != nil {
		fmt.Printf("backup   UNCHECKED: %v\n", err)
		fmt.Printf("         (this says the age could not be established, not that the bucket " +
			"is empty)\n")

		return
	}

	if len(archives) == 0 {
		fmt.Printf("backup   %s holds NO archive for this deployment. An upload that was\n",
			cfg.Backup.S3.Bucket)
		fmt.Printf("         interrupted leaves no manifest, and billet will not offer such a\n")
		fmt.Printf("         prefix as a backup — run `billet local backup --out <dir>`\n")

		return
	}

	newest := archives[0]
	for _, a := range archives[1:] {
		if a.Modified.After(newest.Modified) {
			newest = a
		}
	}

	fmt.Printf("backup   %s, newest %s (%s old)\n", cfg.Backup.S3.Bucket, newest.Name,
		time.Since(newest.Modified).Round(time.Minute))
}

// uploadArchive copies a finished, verified archive off this disk.
//
// AFTER THE LOCAL WRITE AND NEVER INSTEAD OF IT. The directory is the contract:
// it is what an operator's own tooling picks up, what --dry-run reports on, and
// what stays behind when the network is down. Uploading first, or uploading
// instead, would make a backup somebody cannot see the normal case.
func uploadArchive(ctx context.Context, cfg *config.Config, dir, deployment string) error {
	store, configured, err := openArchiveStore(cfg)
	if err != nil {
		return err
	}

	if !configured {
		fmt.Println()
		fmt.Printf("note     this archive is on the same disk as the deployment it protects, and\n")
		fmt.Printf("         a disk that fails takes both. Copy it off this machine — the\n")
		fmt.Printf("         manifest's digests are there so your own tooling can verify what it\n")
		fmt.Printf("         carried — or set backup.s3 in the config, after which billet\n")
		fmt.Printf("         uploads what it writes and restores straight back from it.\n")

		return nil
	}

	a, err := deployarchive.Open(ctx, dir)
	if err != nil {
		return err
	}

	name := a.Name()
	prefix := store.Prefix(deployment) + name

	fmt.Println()
	fmt.Printf("upload   %s\n", prefix)

	keys, err := deployarchive.Upload(ctx, a, store, prefix)

	for _, key := range keys {
		fmt.Printf("         %s\n", key)
	}

	if err != nil {
		// THE LOCAL ARCHIVE IS STILL GOOD, and saying so is the difference between
		// an operator who has a backup and one who thinks they have none. What
		// landed in the bucket is not an archive — the manifest goes last — so
		// nothing there can be mistaken for one either.
		fmt.Println()
		fmt.Printf("warn     the upload did not finish: %v\n", err)
		fmt.Printf("         The archive in %s is COMPLETE and verified; what reached the\n", dir)
		fmt.Printf("         bucket has no manifest, so nothing will offer it as a backup.\n")
		fmt.Printf("         Copy it off this machine another way, or run this again.\n")

		return err
	}

	fmt.Println()
	fmt.Printf("Restore it on another machine with:\n")
	fmt.Printf("\n  billet local restore --from-backup %s --dry-run\n", name)

	return nil
}

// resolveBackup finds one archive in the bucket and reports what else is there.
//
// "latest" IS RESOLVED BY WHEN THE MANIFEST LANDED, which is when the upload
// FINISHED rather than when the backup started — the two differ by however long
// the upload took, and only the second is a fact about a complete archive.
//
// THE DEPLOYMENT IS OPTIONAL BECAUSE THE MACHINE IS NEW. A host restoring for
// the first time holds no identity, so a bucket holding two deployments is
// ambiguous and is REFUSED by name rather than resolved by guess: restoring the
// wrong deployment installs an authority no node in this fleet trusts.
func resolveBackup(
	ctx context.Context, store archiveStore, deployment, want string,
) (archivestore.Archive, error) {
	all, err := store.List(ctx, deployment)
	if err != nil {
		return archivestore.Archive{}, err
	}

	if len(all) == 0 {
		return archivestore.Archive{}, errors.New("this bucket holds no complete archive under " +
			"backup.s3.prefix. An upload that was interrupted leaves no manifest, and without " +
			"one billet will not offer a prefix as a backup — check `billet local backup` ran " +
			"somewhere it could reach the bucket")
	}

	// Newest first, and by NAME as the tiebreak so two archives whose manifests
	// landed in the same second still order the same way on every run.
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Modified.Equal(all[j].Modified) {
			return all[i].Modified.After(all[j].Modified)
		}

		return all[i].Name > all[j].Name
	})

	// AN EXPLICIT NAME IS AS AMBIGUOUS AS `latest`, and reading it as unambiguous
	// was a real defect rather than a theoretical one: an archive is named for the
	// SECOND it was taken, so two deployments sharing a bucket — the case the
	// deployment-scoped prefix exists to support — routinely produce the same
	// name, and taking the first match installs another deployment's ledger and
	// authority onto a host that has no identity of its own to contradict it.
	// Both forms therefore narrow to the same set and refuse the same way.
	candidates := all
	if want != "latest" {
		candidates = nil

		for _, a := range all {
			if a.Name == want {
				candidates = append(candidates, a)
			}
		}

		if len(candidates) == 0 {
			return archivestore.Archive{}, fmt.Errorf(
				"no archive named %q is in this bucket. What is:\n%s", want, renderBackups(all))
		}
	}

	deployments := map[string]bool{}
	for _, a := range candidates {
		deployments[a.Deployment] = true
	}

	if len(deployments) > 1 {
		return archivestore.Archive{}, fmt.Errorf(
			"%d deployments in this bucket answer to that, and billet will not choose between "+
				"them — restoring the wrong one installs a ledger and an authority no node in "+
				"your fleet trusts. Name one with --deployment:\n%s",
			len(deployments), renderBackups(candidates))
	}

	return candidates[0], nil
}

// renderBackups lists what an operator can choose from.
func renderBackups(all []archivestore.Archive) string {
	var b strings.Builder

	for i, a := range all {
		if i == backupsListed {
			fmt.Fprintf(&b, "\n  … and %d more", len(all)-backupsListed)

			break
		}

		fmt.Fprintf(&b, "\n  %s  deployment %s  (uploaded %s)",
			a.Name, a.Deployment, a.Modified.UTC().Format("2006-01-02 15:04:05Z"))
	}

	return b.String()
}

// backupsListed bounds that list. A bucket with a year of daily archives would
// otherwise answer a typo with several hundred lines.
const backupsListed = 20

// fetchFromBackup resolves what the operator asked for and brings it here.
//
// IT RUNS BEFORE ANYTHING TOUCHES THE TARGET, so a bucket that cannot be
// reached, an archive that is not there, or a name two deployments both answer
// to all fail with the host exactly as it was — no lock, no fence, no journal.
func fetchFromBackup(ctx context.Context, cfg *config.Config, o restoreOptions) (string, error) {
	store, configured, err := openArchiveStore(cfg)
	if err != nil {
		return "", err
	}

	if !configured {
		return "", fmt.Errorf("--from-backup needs a backup.s3 block in %s naming the bucket, "+
			"its region and (for Ceph RGW or MinIO) its endpoint. An s3 url alone cannot say "+
			"which region to sign for", o.configPath)
	}

	chosen, err := resolveBackup(ctx, store, o.deployment, o.fromBackup)
	if err != nil {
		return "", err
	}

	dir, err := fetchBackup(ctx, store, chosen, o.into, cfg.Server.IdentityDir)
	if err != nil {
		return "", err
	}

	fmt.Printf("fetched  deployment %s, uploaded %s\n", chosen.Deployment,
		chosen.Modified.UTC().Format("2006-01-02 15:04:05Z"))
	fmt.Printf("         this copy is KEPT at %s\n\n", dir)

	return dir, nil
}

// fetchBackup brings an archive down to this machine and leaves it there.
//
// KEPT RATHER THAN CLEANED UP. On the day this runs the operator has a new
// machine and no local copy of anything; deleting the archive after restoring
// from it would take away the only one they have, and the restore is the moment
// they are least able to fetch it again.
func fetchBackup(
	ctx context.Context, store archiveStore, chosen archivestore.Archive, dest, stateDir string,
) (string, error) {
	if dest == "" {
		// A SIBLING OF THE STATE DIRECTORY, not a temporary: this holds two
		// private keys, and /tmp is somewhere an operator does not expect to find
		// a copy of their App key weeks later.
		dest = filepath.Join(filepath.Dir(stateDir), "billet-backup-"+chosen.Name)
	}

	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dest, err)
	}

	// THE PLACE RULES BEFORE ANYTHING ELSE. Whichever branch below runs, this
	// directory must not be a symlink, a file, or inside the state directory —
	// only the emptiness rule is negotiable, and only for a proven duplicate.
	if err := deployarchive.CheckDestinationPlace(abs, stateDir); err != nil {
		return "", err
	}

	prefix := store.Prefix(chosen.Deployment) + chosen.Name

	// A RETRY MUST NOT BE REFUSED BY ITS OWN FIRST ATTEMPT. The fetched copy is
	// KEPT deliberately, and the default destination is named after the archive,
	// so an operator whose restore or recovery was interrupted runs the same
	// command again and lands on a directory that already holds exactly this
	// archive. Refusing it as "not empty" is a disaster-recovery command with
	// nowhere to go, on the day it is least convenient.
	//
	// AND IT IS THE MANIFEST THAT DECIDES, not the pathname: the digest the
	// bucket holds is compared against the digest on disk, so a directory holding
	// a DIFFERENT archive still gets the ordinary refusal.
	same, err := holdsThisArchive(ctx, store, prefix, abs)
	if err != nil {
		return "", err
	}

	if same {
		// 0700 EXPLICITLY, the same as the fetch path. Skipping PrepareDestination
		// is what makes the retry possible and it is also what skips the one line
		// that tightens this directory, which holds the node-wire CA key and the
		// App key. An archive whose bytes verify says nothing about who can read
		// it.
		//
		// THROUGH A DESCRIPTOR, NOT A PATHNAME. os.Chmod FOLLOWS a symlink, and
		// this command runs as root: the checks above and the change below would
		// otherwise be free to address two different objects, and the one that
		// gets the new mode is whatever is at the name by then.
		if err := tighten(abs); err != nil {
			return "", err
		}

		fmt.Printf("have     %s is already this archive; it is not fetched again\n\n", abs)

		return abs, nil
	}

	// THE SAME RULES A BACKUP DESTINATION GETS: absolute, empty, 0700, and never
	// inside the state directory — a fetched archive is the same two private keys
	// under the same directory a restore scans.
	if err := deployarchive.PrepareDestination(abs, stateDir); err != nil {
		return "", err
	}

	fmt.Printf("fetch    %s\n", prefix)
	fmt.Printf("         into %s\n\n", abs)

	if _, err := deployarchive.Fetch(ctx, store, prefix, abs); err != nil {
		return "", err
	}

	return abs, nil
}

// holdsThisArchive reports whether a directory already holds the archive under
// this prefix, whole.
//
// TWO PROOFS, AND BOTH ARE NEEDED. The remote manifest's bytes must equal the
// local ones — that is what says it is the same backup rather than one with a
// similar name — and deployarchive.Open must accept the directory, which is what
// says every file listed in that manifest is present and matches its digest. A
// manifest that matched over a truncated fetch would otherwise be read as "no
// need to fetch again" for a directory missing the App key.
//
// AN ABSENT OR UNRECOGNISED DIRECTORY IS NOT AN ERROR HERE, because the caller's
// next step gives the better answer: PrepareDestination creates one that is
// missing and refuses one holding something else, in the words an operator needs.
// The caller has already applied the rules about WHERE the directory may be.
func holdsThisArchive(
	ctx context.Context, store archiveStore, prefix, dir string,
) (bool, error) {
	local, err := os.ReadFile(filepath.Join(dir, deployarchive.EntryManifest))
	if err != nil {
		return false, nil //nolint:nilerr // an unreadable local copy is not this archive
	}

	remote, err := store.Get(ctx, path.Join(prefix, deployarchive.EntryManifest))
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path.Join(prefix, deployarchive.EntryManifest),
			err)
	}

	if !bytes.Equal(local, remote) {
		return false, nil
	}

	if _, err := deployarchive.Open(ctx, dir); err != nil {
		return false, fmt.Errorf("%s already holds this archive's manifest, and the archive "+
			"itself does not verify (%w). Remove that directory, or point --into somewhere "+
			"else", dir, err)
	}

	return true, nil
}

// tighten sets 0700 on a directory it opens without following a link.
//
// O_NOFOLLOW IS WHAT IT CLOSES AND IT IS WORTH SAYING WHAT IT DOES NOT. It stops
// a link at the final component being followed, so the mode lands on a directory
// rather than on whatever an attacker pointed at. It does NOT prove this is the
// same directory holdsThisArchive verified — that was established through a
// pathname, and a pathname is not a handle, so a swap between the two calls is
// still a swap.
//
// CLOSING THAT COMPLETELY MEANS CARRYING A DESCRIPTOR through Open, the planner
// and the executor, since each of them re-opens the archive by name; a pathname
// return value cannot preserve the proof, and re-verifying after the swap only
// moves the window. That is a refactor of deployarchive's whole entry surface
// for a race whose precondition is write access to the parent of --into. Where
// that lands is the operator's choice: the DEFAULT is beside the state directory
// (root's own /var/lib on a packaged host, at which point the state directory
// itself is writable and this is not the way in), and an --into somewhere a
// second principal can write does not get that for free.
//
// WHAT BOUNDS THE RESIDUAL IS DIFFERENT FOR THE TWO COMMANDS, and the honest
// version is worth more than a reassuring one. A substituted directory must
// still be a whole archive: Open verifies every entry against its manifest. For
// `billet local recover` that is where it stops, because the planner refuses an
// archive whose deployment is not the one already on this host, and the App key
// and authority must be byte-identical to what is installed. For `billet local
// restore` onto a FRESH host there is no identity, no key and no authority to
// compare against — so a substitution there installs whatever deployment the
// substituted archive describes, bounded by nothing but the filesystem
// permissions on the directory the operator named. Recorded rather than closed,
// so the next person weighing the refactor has the real shape of it.
func tighten(dir string) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open %s to tighten it: %w", dir, err)
	}

	defer func() { _ = f.Close() }()

	if err := f.Chmod(0o700); err != nil {
		return fmt.Errorf("tighten %s: %w", dir, err)
	}

	return nil
}
