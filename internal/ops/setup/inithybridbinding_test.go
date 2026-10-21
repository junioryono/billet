package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssts"
	"github.com/junioryono/billet/internal/initconfig"
)

// THE ADAPTER BEHIND THE SEAM, against a stand-in for STS: it answers the
// ACCOUNT (not the ARN or the user id), an answer without one is ErrNoAccount
// rather than an empty match, and a service that never answers is cut off by
// the wait rather than holding the operator's terminal.
//
// Mutations: returning id.ARN fails the first case; dropping the wait leaves
// the third case's request held for ever, which the bubble reports as a
// deadlock.
func TestAccountOfAnswersTheAccountUnderItsWait(t *testing.T) {
	creds := awscreds.Static{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}

	t.Run("an account", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>` +
				`<Arn>arn:aws:sts::123456789012:assumed-role/x/y</Arn><UserId>AROAEXAMPLE:y</UserId>` +
				`<Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)); err != nil {
				t.Errorf("the stand-in for STS could not answer: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		got, err := accountOf(t.Context(), awssts.New("us-west-2", srv.URL, creds, srv.Client()))
		if err != nil || got != "123456789012" {
			t.Fatalf("accountOf = (%q, %v), want the account 123456789012", got, err)
		}
	})

	t.Run("no account in the answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>` +
				`</GetCallerIdentityResult></GetCallerIdentityResponse>`)); err != nil {
				t.Errorf("the stand-in for STS could not answer: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		_, err := accountOf(t.Context(), awssts.New("us-west-2", srv.URL, creds, srv.Client()))
		if !errors.Is(err, awssts.ErrNoAccount) {
			t.Fatalf("an answer without an account gave %v, want ErrNoAccount", err)
		}
	})

	// IN A BUBBLE, through an in-memory transport that holds every request until
	// its context ends: the wait is the only thing that can end it, so the
	// deadline is the error, it lands exactly at the wait on the bubble's clock,
	// and no real socket or wall clock is involved.
	t.Run("a service that never answers", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reached := 0
			hold := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				reached++
				<-r.Context().Done()

				return nil, r.Context().Err()
			})

			start := time.Now()
			_, err := accountOf(t.Context(), awssts.New("us-west-2", "https://sts.invalid/", creds, &http.Client{Transport: hold}))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a service that never answered gave %v, want the wait's deadline", err)
			}
			if reached != 1 {
				t.Fatalf("the request reached the transport %d times, want once", reached)
			}
			if elapsed := time.Since(start); elapsed != hybridAccountWait {
				t.Fatalf("accountOf gave up after %s, want the wait of %s", elapsed, hybridAccountWait)
			}
		})
	})
}

// roundTripFunc is an http.RoundTripper made of one function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// hybridArgsMinting is hybridArgs without --generation, so the plan render
// resolves one itself: from --out's GENERATION, or a fresh one.
func hybridArgsMinting(out, cfg string) []string {
	args := hybridArgs(out, cfg)
	i := slices.Index(args, "--generation")

	return slices.Delete(args, i, i+2)
}

// rootGeneration is the fingerprint out's GENERATION records, held to the
// root's own declaration of it: the two are written by one render.
func rootGeneration(t *testing.T, out string) string {
	t.Helper()

	g := strings.TrimSuffix(readHybrid(t, out, HybridGenerationFile), "\n")
	if err := initconfig.CheckHybridGeneration(g); err != nil {
		t.Fatalf("%s/%s: %v", out, HybridGenerationFile, err)
	}
	if root := readHybrid(t, out, initconfig.HybridTerraformFile); !strings.Contains(root, `value       = "`+g+`"`) {
		t.Fatalf("the root in %s does not declare the generation %s its %s records", out, g, HybridGenerationFile)
	}

	return g
}

// assertEveryRenderCarries holds each `billet init hybrid` command in text to
// carrying --generation g, and requires at least min of them, so a text that
// stopped printing the commands cannot pass by having none.
func assertEveryRenderCarries(t *testing.T, where, text, g string, minimum int) {
	t.Helper()

	var commands []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "billet init hybrid --out") {
			commands = append(commands, line)
		}
	}
	if len(commands) < minimum {
		t.Fatalf("%s prints %d render commands, want at least %d:\n%s", where, len(commands), minimum, text)
	}
	for _, c := range commands {
		if !strings.Contains(c, "--generation "+g) {
			t.Errorf("a render command in %s does not carry --generation %s:\n  %s", where, g, c)
		}
	}
}

// writeOutputs writes the outputs fixture with its generation replaced.
func writeOutputs(t *testing.T, dir, generation string) string {
	t.Helper()

	path := filepath.Join(dir, "outputs.json")
	body := strings.Replace(hybridOutputsJSON, `"value": "`+testGeneration+`"`, `"value": "`+generation+`"`, 1)
	if generation != testGeneration && body == hybridOutputsJSON {
		t.Fatal("the outputs fixture no longer carries the generation this case rewrites")
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// THE GENERATION SURVIVES A RE-RUN OF THE PLAN RENDER AND --force (#62). The
// first plan render mints it and records it in GENERATION; every later one into
// the same --out reads it back, so the outputs of the one apply keep matching.
// A fresh mint on the second run would refuse the apply's outputs at the
// prepare render.
//
// Mutation: dropping the read of GENERATION makes the second render mint
// another fingerprint, and this fails.
func TestInitHybridKeepsItsGenerationAcrossReRuns(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("first plan render: %v", err)
	}
	minted := rootGeneration(t, out)
	if minted == testGeneration {
		t.Fatalf("the plan render took the fixture's generation %s without being given it", minted)
	}

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("second plan render: %v", err)
	}
	if got := rootGeneration(t, out); got != minted {
		t.Errorf("a re-run of the plan render moved the generation from %s to %s", minted, got)
	}

	printed, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--force")...)
	if err != nil {
		t.Fatalf("forced plan render: %v", err)
	}
	if got := rootGeneration(t, out); got != minted {
		t.Errorf("--force moved the generation from %s to %s", minted, got)
	}

	// EVERY RENDER COMMAND REPEATS IT, the runbook's and the one on stdout,
	// because the prepare render on another machine has only the command it
	// was given; one command that lost its flags would mint another generation.
	assertEveryRenderCarries(t, "the runbook", readHybrid(t, out, HybridRunbookFile), minted, 3)
	assertEveryRenderCarries(t, "stdout", printed, minted, 1)

	// And a prepare render with no flag at all reads it from GENERATION and
	// accepts the outputs of that root.
	outputs := writeOutputs(t, dir, minted)
	if _, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--terraform-output", outputs)...); err != nil {
		t.Errorf("a prepare render over the root that minted %s refused its outputs: %v", minted, err)
	}
}

// AN OPERATOR'S OWN ROOT KEEPS THE GENERATION: a root that is not billet's own
// is left alone and the fresh render goes beside it, carrying the fingerprint
// GENERATION records, which is the one the operator's root declares.
func TestInitHybridKeepsTheGenerationBesideAForeignRoot(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("plan render: %v", err)
	}
	minted := rootGeneration(t, out)

	root := filepath.Join(out, initconfig.HybridTerraformFile)
	raw, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	edited := "# my own root now\n" + string(raw)
	if err := os.WriteFile(root, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("plan render over an edited root: %v", err)
	}

	beside, err := os.ReadFile(root + ".new")
	if err != nil {
		t.Fatalf("the fresh root did not land beside the operator's: %v", err)
	}
	if !strings.Contains(string(beside), `value       = "`+minted+`"`) {
		t.Errorf("the render beside the operator's root does not carry the generation %s", minted)
	}
}

// A GENERATION FILE THAT DOES NOT HOLD A FINGERPRINT IS AN ERROR, never none,
// and a record already there is never replaced by another.
func TestInitHybridRefusesAnUnreadableOrOtherRecord(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(out, HybridGenerationFile)

	if err := os.WriteFile(record, []byte("not a fingerprint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err == nil ||
		!strings.Contains(err.Error(), HybridGenerationFile) {
		t.Fatalf("a GENERATION holding no fingerprint was not refused by name: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, initconfig.HybridTerraformFile)); !os.IsNotExist(statErr) {
		t.Errorf("the refused render wrote a root (stat: %v)", statErr)
	}

	// The record stands: a render that resolved another generation (a race
	// into one directory) cannot replace it.
	if err := os.WriteFile(record, []byte(testGeneration+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordHybridGeneration(out, "fedc-ba98-7654-3210"); err == nil {
		t.Error("a second generation replaced the one recorded")
	}
	if err := recordHybridGeneration(out, testGeneration); err != nil {
		t.Errorf("recording the generation already recorded was refused: %v", err)
	}
	if got, err := os.ReadFile(record); err != nil || string(got) != testGeneration+"\n" {
		t.Errorf("the record now reads %q (%v), want %s", got, err, testGeneration)
	}
}

// TWO GENERATIONS IN ONE DIRECTORY ARE REFUSED, and a later render that cannot
// tell which generation its outputs must belong to is refused too, rather than
// minting one that no apply ever wrote.
func TestInitHybridRefusesAnUnboundGeneration(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	outputs := writeOutputs(t, dir, testGeneration)

	_, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--terraform-output", outputs)...)
	if err == nil || !strings.Contains(err.Error(), "--generation") {
		t.Fatalf("a prepare render with no generation and no root was not refused by the flag: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, initconfig.HybridTerraformFile)); !os.IsNotExist(statErr) {
		t.Errorf("the refused render wrote a root (stat: %v)", statErr)
	}

	if _, err := runInitHybrid(t, hybridArgs(out, cfg)...); err != nil {
		t.Fatalf("plan render: %v", err)
	}

	other := "fedc-ba98-7654-3210"
	args := append(hybridArgsMinting(out, cfg), "--generation", other)
	_, err = runInitHybrid(t, args...)
	if err == nil {
		t.Fatal("a second generation was rendered into a directory whose root declares another")
	}
	for _, want := range []string{other, testGeneration} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if got := rootGeneration(t, out); got != testGeneration {
		t.Errorf("the refused render moved the root's generation to %s", got)
	}

	for _, bad := range []string{"", "0123"} {
		if _, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--generation", bad)...); err == nil {
			t.Errorf("--generation %q was accepted", bad)
		}
	}
}

// A ROOT FROM BEFORE THE FINGERPRINT, ITS OUTPUTS AND ITS RUNBOOK'S COMMAND
// (no --generation) is the path an existing deployment takes, and it reaches
// the merge remedy rather than only being told to pass a flag it never had.
func TestInitHybridGivesAnOldRootTheWayThrough(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("plan render: %v", err)
	}

	// THE ROOT AS IT WAS: no generation output at all.
	root := filepath.Join(out, initconfig.HybridTerraformFile)
	raw, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(string(raw), `output "generation"`, `output "removed_generation"`, 1)
	if old == string(raw) {
		t.Fatal("the rendered root no longer declares the generation output this case removes")
	}
	if err := os.WriteFile(root, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	// And no record: a directory a release before the fingerprint wrote.
	if err := os.Remove(filepath.Join(out, HybridGenerationFile)); err != nil {
		t.Fatal(err)
	}

	outputs := filepath.Join(dir, "outputs.json")
	legacy := strings.Replace(hybridOutputsJSON,
		",\n  \"generation\": {\"sensitive\": false, \"type\": \"string\", \"value\": \"0123-4567-89ab-cdef\"},\n"+
			"  \"account_id\": {\"sensitive\": false, \"type\": \"string\", \"value\": \"123456789012\"}", "", 1)
	if legacy == hybridOutputsJSON {
		t.Fatal("the outputs fixture no longer carries the two outputs this case removes")
	}
	if err := os.WriteFile(outputs, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--terraform-output", outputs)...)
	if err == nil {
		t.Fatal("an old root's outputs were accepted with no generation")
	}
	for _, want := range []string{"scratch --out", "terraform plan", "--generation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not give the way through (%q): %v", want, err)
		}
	}
}

// OUTPUTS FROM ANOTHER GENERATION ARE REFUSED BEFORE A PLACEHOLDER IS FILLED,
// even when their name and region match: that is the case #62 says a name and
// a region cannot see.
//
// Mutation: dropping the generation comparison lets this render succeed.
func TestInitHybridRefusesAnotherGenerationsOutputs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	other := "fedc-ba98-7654-3210"
	outputs := writeOutputs(t, dir, other)

	_, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
	if err == nil {
		t.Fatal("outputs written by another generation were accepted")
	}
	for _, want := range []string{other, testGeneration, "--terraform-output"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(out, initconfig.HybridInventoryFile)); !os.IsNotExist(statErr) {
		t.Errorf("the refused render wrote an inventory (stat: %v)", statErr)
	}
}

// THE ACCOUNT IS COMPARED WHEN STS ANSWERS AND REPORTED WHEN IT CANNOT: a
// different account is refused, the same one passes in silence, and an answer
// nobody could get is said out loud and does not stop a render the generation
// already binds.
func TestInitHybridComparesTheAccountWhenItCan(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "none.yaml")
	outputs := writeOutputs(t, dir, testGeneration)

	t.Run("another account", func(t *testing.T) {
		answerAccount(t, "210987654321", nil)
		out := filepath.Join(t.TempDir(), "acme-ci")

		_, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
		if err == nil {
			t.Fatal("outputs minted in another account were accepted")
		}
		for _, want := range []string{"210987654321", hybridTestAccount} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name account %s: %v", want, err)
			}
		}
		if _, statErr := os.Stat(filepath.Join(out, initconfig.HybridInventoryFile)); !os.IsNotExist(statErr) {
			t.Errorf("the refused render wrote an inventory (stat: %v)", statErr)
		}
	})

	t.Run("the same account", func(t *testing.T) {
		answerAccount(t, hybridTestAccount, nil)
		out := filepath.Join(t.TempDir(), "acme-ci")

		printed, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
		if err != nil {
			t.Fatalf("outputs from this account were refused: %v", err)
		}
		if strings.Contains(printed, "NOT verified") {
			t.Errorf("a verified account was reported as unverified:\n%s", printed)
		}
	})

	t.Run("could not tell", func(t *testing.T) {
		// The suite's own default: no AWS.
		out := filepath.Join(t.TempDir(), "acme-ci")

		printed, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
		if err != nil {
			t.Fatalf("an account nobody could ask about stopped the render: %v", err)
		}
		for _, want := range []string{"NOT verified", hybridTestAccount, errNoAWSInTheSuite.Error()} {
			if !strings.Contains(printed, want) {
				t.Errorf("the report of an unverified account does not say %q:\n%s", want, printed)
			}
		}
	})
}
