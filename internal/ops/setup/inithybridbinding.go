package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssts"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/regularfile"
)

// hybridAccountWait bounds the one STS call a later render makes. A render is
// an operator at a terminal, and a credential chain that hangs is an answer of
// its own: the account was not verified. A variable so a test can shorten it.
var hybridAccountWait = 30 * time.Second

// hybridAccount answers which account the credentials this render runs under
// belong to. A seam, so the suite never reaches AWS; what it calls is
// accountOf, which the suite tests against a stand-in for STS.
var hybridAccount = func(ctx context.Context, region string) (string, error) {
	return accountOf(ctx, awssts.New(region, "", awscreds.Default(), nil))
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

// resolveHybridGeneration decides the fingerprint this render carries: the
// flag, then the GENERATION file a render already wrote into --out, then, on a
// plan render only, a fresh one.
//
// A FILE BILLET OWNS, NOT THE ROOT. The root declares the fingerprint as an
// output, because the outputs must carry it, but reading it back out of HCL
// means parsing a language billet has no parser for, and a lexical reader was
// tried and withdrawn after four reviews each found valid HCL it misread. The
// file is outside the marker rule, so a re-run of the plan render and --force
// both keep it. Every way of reading it wrongly fails safe: a render accepts
// outputs only when their generation equals its own, which no misread can
// produce for another root's outputs.
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
	case later:
		// WHERE A ROOT FROM BEFORE THE FINGERPRINT LANDS, since neither its
		// runbook nor its directory has one, so the way through is said here too.
		return "", fmt.Errorf("this render reads --terraform-output, and nothing says which generation "+
			"those outputs must belong to: pass --generation as the runbook prints it, or point "+
			"--out at the directory whose %s records it. A root rendered before billet minted "+
			"fingerprints has neither: run the plan render into a scratch --out with the same "+
			"flags, copy its %q and %q output blocks into your root and nothing else, check that "+
			"`terraform plan` adds only those two outputs, apply, save `terraform output -json` "+
			"again, and pass --generation with the value you copied. A root that still begins with "+
			"billet's marker is replaced whole by the next render; remove the marker from one you "+
			"want kept",
			HybridGenerationFile, initconfig.HybridOutputGeneration, initconfig.HybridOutputAccountID)
	}

	return initconfig.NewHybridGeneration()
}

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

// recordHybridGeneration writes --out's GENERATION when it has none.
//
// AFTER THE RENDER'S FILES, so a render that failed leaves no fingerprint
// claiming a directory it never wrote; a failure here, after the files landed,
// is said, because the next render would otherwise mint another. EXCLUSIVE, so
// two renders racing into one directory cannot both write one: the loser finds
// the winner's and is refused unless it is the same.
func recordHybridGeneration(out, g string) error {
	path := filepath.Join(out, HybridGenerationFile)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		recorded, err := readHybridGeneration(out)
		if err != nil {
			return err
		}
		if recorded != g {
			return fmt.Errorf("%s records %s while this render wrote generation %s: another render "+
				"into the same --out got there first; render again, which keeps the one recorded",
				path, recorded, g)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("record the generation in %s (the files were written; until it exists, "+
			"pass --generation %s on every render): %w", path, g, err)
	}

	if _, err := f.WriteString(g + "\n"); err != nil {
		_ = f.Close()

		return fmt.Errorf("record the generation in %s: %w", path, err)
	}

	return f.Close()
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
