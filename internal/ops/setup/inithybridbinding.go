package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssts"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/regularfile"

	"golang.org/x/sys/unix"
)

// hybridAccountWait bounds the one STS call a later render makes. A render is
// an operator at a terminal, and a credential chain that hangs is an answer of
// its own: the account was not verified. A variable so a test can shorten it.
var hybridAccountWait = 30 * time.Second

// hybridAccount answers which account the credentials this render runs under
// belong to. A seam, so the suite never reaches AWS; what it calls is
// accountOf, which the suite tests against a stand-in for STS.
var hybridAccount = func(ctx context.Context, region string) (string, error) {
	return accountOf(ctx, hybridSTS(region))
}

// hybridSTS is the client hybridAccount asks, for the region the render signs
// into. A seam, so a test runs hybridAccount itself against a stand-in.
var hybridSTS = func(region string) *awssts.Client {
	return awssts.New(region, "", awscreds.Default(), nil)
}

// accountOf is the account a caller identity answer names, under the wait.
// Never the ARN or the user id: the account is the only field compared.
func accountOf(ctx context.Context, sts *awssts.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hybridAccountWait)
	defer cancel()

	id, err := sts.Whoami(ctx)
	if err != nil {
		return "", err
	}

	return id.Account, nil
}

// HybridGenerationFile is where a generation keeps its fingerprint, in --out
// beside the files it renders: one line, written once and never replaced.
const HybridGenerationFile = "GENERATION"

// hybridLockFile is the advisory lock a render holds on --out while it
// resolves its generation, writes its files and records the generation.
const hybridLockFile = ".billet-init-hybrid.lock"

// lockHybridOut holds --out against another render for the whole render:
// between resolving the generation and recording it, a second render into the
// same directory would otherwise write its files over the first's and then be
// refused by the record the first left, leaving a root and a record of two
// generations. Refused at once rather than waited for, since the other is an
// operator's command that may be waiting on nothing.
func lockHybridOut(out string) (func(), error) {
	// DURABLY, because the record this render publishes into it is only as
	// durable as the entries that name the directory.
	if err := (durablefile.Installer{}).MkdirAll(out, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", out, err)
	}

	path := filepath.Join(out, hybridLockFile)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the render lock %s: %w", path, err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another billet init hybrid is rendering into %s; wait for it to "+
				"finish and run this again", out)
		}

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return func() { _ = f.Close() }, nil
}

// The render's seams, for a test that stages two renders interleaving: one
// runs after the generation is resolved and before the lock is taken, the
// other with the lock held, before the first write. Both do nothing here.
var (
	hybridBeforeLock = func() {}
	hybridLocked     = func() {}
)

// recheckHybridGeneration holds the generation this render resolved to what
// --out records now, under the lock: a render that held the lock first and
// recorded another refuses this one, and so does a root that appeared without
// a record (a render that died before recording it) unless this render's
// generation was supplied, which is an adoption. Every render records before it
// lets the lock go, so a live one is never seen half done.
func recheckHybridGeneration(out, g string, supplied bool) error {
	recorded, err := readHybridGeneration(out)
	switch {
	case err != nil:
		return err
	case recorded == g:
		return nil
	case recorded != "":
		return fmt.Errorf("another render recorded generation %s in %s while this one rendered %s; run "+
			"this again, which takes the one recorded", recorded, out, g)
	case supplied:
		return nil
	}

	root := filepath.Join(out, initconfig.HybridTerraformFile)
	_, statErr := os.Lstat(root)
	switch {
	case statErr == nil:
		return fmt.Errorf("%s appeared without a record while this render ran (another render wrote it "+
			"and died before recording its generation), and billet will not mint a fresh one over it. "+
			"If the root declares a %q output, pass its value as --generation", root,
			initconfig.HybridOutputGeneration)
	case !errors.Is(statErr, os.ErrNotExist):
		return fmt.Errorf("check %s: %w", root, statErr)
	}

	return nil
}

// resolveHybridGeneration decides the fingerprint this render carries: the
// flag, then the GENERATION file a render already wrote into --out, then, on a
// plan render into a directory that holds no root yet, a fresh one.
//
// A FILE BILLET OWNS, NOT THE ROOT. The root declares the fingerprint as an
// output, because the outputs must carry it, but reading it back out of HCL
// means parsing a language billet has no parser for, and a lexical reader was
// tried and withdrawn after four reviews each found valid HCL it misread. The
// file is outside the marker rule, so a re-run of the plan render and --force
// both keep it. Every way of reading it wrongly fails safe: a render accepts
// outputs only when their generation equals its own, which no misread can
// produce for another root's outputs.
//
// NEVER MINTED OVER A ROOT. A root with no record (one rendered before the
// fingerprint, or one whose render died before recording it) belongs to a
// generation billet cannot see, and a fresh one recorded beside it would refuse
// that root's outputs from then on, --generation included; so it is adopted by
// --generation or migrated through a scratch render, never guessed.
func resolveHybridGeneration(out, flag string, flagSet, later bool) (string, error) {
	if flagSet {
		if err := initconfig.CheckHybridGeneration(flag); err != nil {
			return "", err
		}
	}

	recorded, err := readHybridGeneration(out)
	if err != nil {
		return "", err
	}

	switch {
	case flagSet && recorded != "" && recorded != flag:
		return "", fmt.Errorf("--generation %s and %s, which records %s, are two generations: one "+
			"directory holds one. Render into another --out, or drop --generation to keep the one "+
			"recorded", flag, filepath.Join(out, HybridGenerationFile), recorded)
	case flagSet:
		return flag, nil
	case recorded != "":
		return recorded, nil
	}

	root := filepath.Join(out, initconfig.HybridTerraformFile)

	_, statErr := os.Lstat(root)
	switch {
	case statErr == nil:
		return "", fmt.Errorf("%s exists and %s does not, so which generation that root belongs to "+
			"is not recorded, and billet will not mint a fresh one over it. If the root declares a "+
			"%q output, pass its value as --generation. %s",
			root, filepath.Join(out, HybridGenerationFile), initconfig.HybridOutputGeneration, oldRootRemedy)
	case !errors.Is(statErr, os.ErrNotExist):
		return "", fmt.Errorf("check %s: %w", root, statErr)
	case later:
		// WHERE A ROOT FROM BEFORE THE FINGERPRINT LANDS when --out is not its
		// directory, so the way through is said here too.
		return "", fmt.Errorf("this render reads --terraform-output, and nothing says which generation "+
			"those outputs must belong to: pass --generation as the runbook prints it, or point "+
			"--out at the directory whose %s records it. %s", HybridGenerationFile, oldRootRemedy)
	}

	return initconfig.NewHybridGeneration()
}

// oldRootRemedy is the way through for a root rendered before billet minted
// fingerprints, said wherever one is refused.
var oldRootRemedy = fmt.Sprintf("A root rendered before billet minted fingerprints has neither: run "+
	"the plan render into a scratch --out with the same flags, copy its %q and %q output blocks "+
	"into your root and nothing else, check that `terraform plan` adds only those two outputs, "+
	"apply, save `terraform output -json` again, and pass --generation with the value you copied. "+
	"A root that still begins with billet's marker is replaced whole by the next render; remove "+
	"the marker from one you want kept",
	initconfig.HybridOutputGeneration, initconfig.HybridOutputAccountID)

// readHybridGeneration is the fingerprint --out's GENERATION records, or ""
// when there is none. A file that is there and cannot be read, or does not hold
// a fingerprint, is an error, never none.
func readHybridGeneration(out string) (string, error) {
	path := filepath.Join(out, HybridGenerationFile)

	raw, err := regularfile.ReadFile(path, 256, regularfile.Options{NoFollow: true})
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	g := strings.TrimSuffix(string(raw), "\n")
	if err := initconfig.CheckHybridGeneration(g); err != nil {
		return "", fmt.Errorf("%s does not hold one generation fingerprint: %w", path, err)
	}

	return g, nil
}

// publishHybridGeneration puts the staged record at its name, failing rather
// than replacing. A seam, so a test can observe the record before it is
// published.
var publishHybridGeneration = os.Link

// recordHybridGeneration writes --out's GENERATION when it has none.
//
// AFTER THE RENDER'S FILES, so a render that failed leaves no fingerprint
// claiming a directory it never wrote; a failure here, after the files landed,
// is said, because the root it wrote is then one a later render will not mint
// over. PUBLISHED WHOLE AND NEVER REPLACED, by durablefile's one ordering with
// a link for the rename (InstallNew), so a crash leaves either no record or a
// complete one, and a record already there is kept and compared.
func recordHybridGeneration(out, g string) error {
	path := filepath.Join(out, HybridGenerationFile)

	installer := durablefile.Installer{Link: func(from, to string) error { return publishHybridGeneration(from, to) }}

	_, err := installer.InstallNew(out, HybridGenerationFile, 0o644, func(w io.Writer) error {
		_, err := io.WriteString(w, g+"\n")

		return err
	})
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("record the generation in %s (the files were written; until it exists, "+
				"pass --generation %s on every render): %w", path, g, err)
		}

		recorded, rerr := readHybridGeneration(out)
		if rerr != nil {
			return rerr
		}
		if recorded != g {
			return fmt.Errorf("%s records %s while this render wrote generation %s; render again, "+
				"which keeps the one recorded", path, recorded, g)
		}

		return nil
	}

	return nil
}

// bindHybridOutputs refuses outputs another root wrote, before a single
// placeholder is filled.
//
// THE GENERATION IS THE BINDING. Every rendering takes the name and region from
// the flags while every id in the outputs comes from an apply, so another
// root's outputs render a config that signs against one deployment and names
// another's subnet, security groups, buckets and controller, and nothing
// downstream refuses it. The fingerprint the plan render minted is in this
// root's outputs and nowhere else.
//
// The name and region are still compared, because they say WHICH mistake it
// was. The account is compared with the credentials this render runs under
// whenever STS answers; when it cannot, that is said, and the render goes on
// bound by the generation, because could-not-tell is not a mismatch and the
// render needs no AWS access of its own.
func bindHybridOutputs(
	ctx context.Context, env cli.Env, f initconfig.HybridFacts, generation, name, region string,
) error {
	if f.Generation != generation {
		return fmt.Errorf("--terraform-output was written by the root of generation %s, and this "+
			"render is generation %s: every id in that file names another root's resources. Point "+
			"--terraform-output at the outputs of the root this generation rendered, or, if that "+
			"root was rendered again since its apply, apply it and save its outputs again",
			f.Generation, generation)
	}

	if f.Name != name || f.Region != region {
		return fmt.Errorf("--terraform-output was written by the %q root in %s and this "+
			"generation is %q in %s: every id in that file names another deployment's "+
			"resources, so either re-render with --name %s --region %s or point "+
			"--terraform-output at this generation's own outputs",
			f.Name, f.Region, name, region, cli.ShellArg(f.Name), cli.ShellArg(f.Region))
	}

	account, err := hybridAccount(ctx, region)
	switch {
	case err != nil:
		fmt.Fprintf(env.Stdout, "NOTE: the account these outputs were minted in (%s) was NOT verified: %v. "+
			"The generation binds them to this root; run this where AWS credentials resolve to "+
			"compare the account too.\n\n", f.AccountID, err)
	case account != f.AccountID:
		return fmt.Errorf("--terraform-output was minted in account %s, and the credentials this render "+
			"runs under are account %s: the commands this generation prints would act in an account "+
			"that does not hold these resources. Run it with credentials for %s",
			f.AccountID, account, f.AccountID)
	}

	return nil
}
