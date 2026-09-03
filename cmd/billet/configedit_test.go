package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/initconfig"
)

// The App the stub mints. Distinctive values, so an assertion cannot be
// satisfied by a zero left behind from somewhere else.
const (
	stubAppID          = 4242
	stubInstallationID = 99
)

// stubOnboard replaces the onboarding seam for one test and reports how many
// times it was reached.
//
// A COUNTER RATHER THAN AN ERROR RETURN IS THE POINT for the refusals below.
// "githubAppCreate returned an error" is satisfied by the old behaviour too —
// which created the App, failed to record it, printed the block and returned
// NIL — so the assertion that actually distinguishes them is that GitHub was
// never reached at all.
//
// It judges nothing: when it is told to fail it fails, and otherwise it calls
// OnAppCreated with a real key and returns what it was given. A fake that
// re-implemented the production rule it is used to test would pass while the
// real one was deleted.
func stubOnboard(t *testing.T, key []byte, fail error) *int {
	t.Helper()

	calls := 0

	prev := onboard
	t.Cleanup(func() { onboard = prev })

	onboard = func(_ context.Context, opts github.OnboardOptions) (*github.Onboarding, error) {
		calls++

		if fail != nil {
			// BEFORE OnAppCreated, so this models the flow ending without an App:
			// the operator closed the browser, or the hour lapsed. Nothing is
			// written, which is what the config assertions rely on.
			return nil, fail
		}

		app := &github.App{ID: stubAppID, ClientID: "Iv1.stub", PEM: string(key)}
		if err := opts.OnAppCreated(app); err != nil {
			return nil, err
		}

		return &github.Onboarding{
			App:          app,
			Installation: &github.Installation{ID: stubInstallationID},
		}, nil
	}

	return &calls
}

// NOTHING REACHES GITHUB UNTIL THE CONFIG CAN TAKE THE BLOCK.
//
// The edit runs last, so every one of these used to surface as "could not update
// <path>" with the App already registered, its one-time private key already
// spent, and the command exiting 0 — after which re-running mints a SECOND App
// rather than recovering. --key-path is passed in every case so the preflight is
// what is being exercised: without it, defaultKeyPath's own read of the config
// refuses some of these first, and a fixed preflight would look covered by a
// guard that only fires on the other invocation.
func TestGitHubAppCreateRefusesAConfigItCannotUpdate(t *testing.T) {
	for name, tc := range map[string]struct {
		body       string
		absent     bool
		unwritable bool
		want       string
		// seed is which remedy the refusal may offer, and there are two because
		// they are different commands for different states. "create" writes the
		// seed to a file that is NOT THERE, under `set -C` so a file that appeared
		// in between is refused rather than truncated. "append" adds a document to
		// a file that has none, which is the only state where touching an existing
		// file is safe. "" means the refusal must offer neither: telling somebody
		// whose `github:` key holds a list to write a seed is telling them to
		// destroy what they wrote.
		seed string
	}{
		"a config that does not exist": {
			absent: true,
			want:   "does not exist",
			seed:   "create",
		},
		"a config somebody only touched": {
			body: "",
			want: "no YAML document",
			seed: "append",
		},
		"a config holding only a comment": {
			body: "# I will fill this in later\n",
			want: "no YAML document",
			seed: "append",
		},
		"a comment with no trailing newline": {
			body: "# I will fill this in later",
			want: "no YAML document",
			seed: "append",
		},
		"github holds a list": {
			body: "github:\n  - one\n  - two\n",
			want: "not a mapping",
		},
		"github holds a string": {
			body: "github: somewhere-else.yaml\n",
			want: "not a mapping",
		},
		"the document is not a mapping": {
			body: "- one\n- two\n",
			want: "not a mapping",
		},
		"a directory billet cannot write": {
			body:       "github: {}\n",
			unwritable: true,
			want:       "cannot write in",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "billet.yaml")
			keyPath := filepath.Join(dir, "app-private-key.pem")

			if !tc.absent {
				if err := os.WriteFile(cfgPath, []byte(tc.body), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			if tc.unwritable {
				prev := dirWritable
				t.Cleanup(func() { dirWritable = prev })

				dirWritable = func(string) bool { return false }
			}

			calls := stubOnboard(t, testKey(t), nil)

			var err error

			out := capture(t, func() {
				err = githubAppCreate(t.Context(), []string{
					"--org", "acme", "--config", cfgPath, "--key-path", keyPath, "--no-browser",
				})
			})

			if err == nil {
				t.Fatalf("the run did not refuse %s:\n%s", name, out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q: %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), cfgPath) {
				t.Errorf("the refusal does not name the config: %v", err)
			}

			// AND IT NEVER TELLS AN OPERATOR TO OVERWRITE THEIR OWN FILE. The
			// seed command is `> config`, which is safe advice only where the
			// file provably holds nothing — so a `github:` key holding a list
			// must not be handed it.
			offered := strings.Contains(err.Error(), "printf ")
			if offered != (tc.seed != "") {
				t.Errorf("the refusal %s a seed command, want %q:\n%v",
					map[bool]string{true: "offers", false: "withholds"}[offered], tc.seed, err)
			}

			switch tc.seed {
			case "create":
				// The file is not there. `set -C` is what keeps this from
				// truncating one that appears in between.
				if !strings.Contains(err.Error(), "set -C") {
					t.Errorf("the seed command can truncate a file that appears first:\n%v", err)
				}
			case "append":
				// APPENDS, AND STARTS WITH A NEWLINE. `>` would throw away a
				// comment the operator wrote, and an append with no separator
				// turns `# later` into `# latergithub: {}` — still a file with
				// no document in it, from a command billet handed over.
				if !strings.Contains(err.Error(), `printf '\n%s\n'`) ||
					!strings.Contains(err.Error(), ">> ") {
					t.Errorf("the seed command does not append cleanly to what is already "+
						"in the file:\n%v", err)
				}
			}

			// THE ASSERTION THAT DISTINGUISHES THIS FROM THE DEFECT.
			if *calls != 0 {
				t.Errorf("onboarding ran anyway: an App would exist and its one-time key "+
					"would be spent (%d calls)", *calls)
			}

			// And nothing was reserved on the way, so a re-run after the fix is a
			// clean re-run rather than one that trips over its own leftovers.
			for _, path := range []string{keyPath, stagingPath(keyPath)} {
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("%s was created before the refusal (%v)", path, statErr)
				}
			}
		})
	}
}

// THE PREFLIGHT IS THE WRITE, ASKED EARLY — never a second opinion about it.
//
// A separate reading of what `github:` may hold would agree with
// renderGitHubBlock today and drift from it on the next field added to the
// block, which is how the original defect arrived one layer up. This runs
// both over the same seeds and fails the moment they disagree in either
// direction: a preflight that refuses something the write accepts blocks a
// legitimate onboarding, and one that accepts something the write refuses puts
// the failure back after the App exists.
func TestThePreflightAgreesWithTheWrite(t *testing.T) {
	identity := githubBlock{
		Org: "acme", AppID: stubAppID, InstallationID: stubInstallationID,
		PrivateKeyPath: "/etc/billet/app-private-key.pem",
	}

	for name, seed := range map[string]string{
		"a null github key":      "github:\n",
		"an empty mapping":       "github: {}\n",
		"a bare document":        "{}\n",
		"no github key at all":   "server:\n  listen: 127.0.0.1:7717\n",
		"an existing identity":   "github:\n  org: old\n  app_id: 1\n  installation_id: 2\n",
		"a null key with a peer": "server:\n  listen: 127.0.0.1:7717\ngithub:\n",
		"github is a list":       "github:\n  - one\n  - two\n",
		"github is a string":     "github: somewhere-else.yaml\n",
		"github is a number":     "github: 42\n",
		"the root is a list":     "- one\n- two\n",
		"an empty file":          "",

		// RENDERS, AND DOES NOT READ BACK. setScalar assigns Value to the node
		// under the key without changing its KIND, so the encoder emits the
		// sequence or mapping that is already there and the identity is not in
		// the result. Only the read-back sees it — which is why both paths have
		// to run the same one.
		"org holds a sequence":      "github:\n  org: [old]\n",
		"app_id holds a mapping":    "github:\n  app_id: {a: b}\n",
		"two github keys":           "github: {}\ngithub: {}\n",
		"installation_id is a list": "github:\n  installation_id:\n    - 1\n",

		// THE ONE FIELD THE WRITE MAY OR MAY NOT SET. renderGitHubBlock touches
		// client_id only when GitHub returned one, so an unreadable value there
		// is repaired by the shape that carries a client id and left in place by
		// the shape that does not. billet cannot know which shape GitHub will
		// hand it, so the preflight has to refuse a config that either one
		// breaks on.
		"client_id cannot be read back": "github:\n  client_id: !!binary \"%%%\"\n",
		"client_id holds a mapping":     "github:\n  client_id: {a: b}\n",

		// AN ALIAS IS NOT A SCALAR, and setScalar treats it as one. Measured:
		// the Value it assigns becomes the alias NAME, so the encoder emits
		// `*probe` — an alias to an anchor that was never defined — and the
		// read-back fails to parse at all. Both the preflight and the write
		// refuse, which is the answer wanted; it is here because the shape is
		// one an operator can write and neither side had ever been asked about
		// it.
		"client_id is an alias": "anchors:\n  value: &shared not-a-client-id\n" +
			"github:\n  client_id: *shared\n",
		"private_key_path is an alias": "anchors:\n  value: &shared /not/the/key.pem\n" +
			"github:\n  private_key_path: *shared\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "billet.yaml")
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}

			_, planErr := planConfigEdit(path, identity.Org)

			// BOTH SHAPES THE WRITE CAN PRODUCE, because which one it is
			// depends on whether GitHub returned a client id — a thing the
			// preflight cannot know and therefore has to survive either way.
			// The preflight is right exactly when it refuses a config that
			// EITHER shape breaks on.
			withClientID := identity
			withClientID.ClientID = "Iv1.probe"

			var (
				writeErrs []error
				written   []string
			)

			for _, shape := range []githubBlock{withClientID, identity} {
				// A FRESH COPY PER SHAPE: the write mutates the file, so the
				// second shape would otherwise be answering about the first
				// one's output rather than about the seed.
				attempt := filepath.Join(t.TempDir(), "billet.yaml")
				if err := os.WriteFile(attempt, []byte(seed), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}

				writeErrs = append(writeErrs, writeGitHubBlock(attempt, shape))
				written = append(written, attempt)
			}

			writeRefused := writeErrs[0] != nil || writeErrs[1] != nil

			if (planErr != nil) != writeRefused {
				t.Fatalf("the preflight and the write disagree about %s:\n  preflight:      %v\n"+
					"  write (id):     %v\n  write (no id):  %v",
					name, planErr, writeErrs[0], writeErrs[1])
			}

			// AND A WRITE THAT RETURNED NIL PUT THE WHOLE IDENTITY THERE.
			// Comparing error-ness alone accepts a write that reported success
			// over a config carrying somebody else's client id — which is
			// exactly what it did, because the field the render may LEAVE ALONE
			// is the one nothing checked.
			for i, shape := range []githubBlock{withClientID, identity} {
				if writeErrs[i] != nil {
					continue
				}

				raw, readErr := os.ReadFile(written[i])
				if readErr != nil {
					t.Fatalf("read back: %v", readErr)
				}

				got, gotKeyPath, ok := existingGitHubBlock(raw)
				if !ok {
					t.Fatalf("no identity is readable after writing into %s:\n%s", name, raw)
				}
				if got.Org != shape.Org || got.AppID != shape.AppID ||
					got.InstallationID != shape.InstallationID ||
					got.ClientID != shape.ClientID || gotKeyPath != shape.PrivateKeyPath {
					t.Errorf("writing into %s left org %q app %d installation %d client %q "+
						"key %q, want %q/%d/%d/%q/%q\n%s",
						name, got.Org, got.AppID, got.InstallationID, got.ClientID, gotKeyPath,
						shape.Org, shape.AppID, shape.InstallationID, shape.ClientID,
						shape.PrivateKeyPath, raw)
				}
			}
		})
	}
}

// IT SAYS SO BEFORE IT DOES IT, which is the whole of what the edit rule asks.
//
// The ordering is proved by making the flow FAIL: a notice printed after
// onboarding would not be printed at all on this path, so its presence beside a
// reached-and-failed flow is what places it in front. Reading a captured pipe
// for interleaving would prove nothing — both writes land in one buffer whatever
// their order.
func TestGitHubAppCreateSaysItWillEditTheConfigBeforeItDoes(t *testing.T) {
	t.Run("the flow never finishes", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "billet.yaml")
		seed := "# a note the operator wrote\ngithub: {}\n"

		if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}

		calls := stubOnboard(t, testKey(t), errors.New("the operator closed the browser"))

		var err error

		out := capture(t, func() {
			err = githubAppCreate(t.Context(), []string{
				"--org", "acme", "--config", cfgPath,
				"--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
			})
		})

		if err == nil {
			t.Fatal("a failed onboarding was reported as success")
		}
		if *calls != 1 {
			t.Fatalf("the flow was not reached, so this proves nothing about ordering (%d calls)",
				*calls)
		}
		if !strings.Contains(out, configEditRule) {
			t.Errorf("the rule was not stated before the flow ran:\n%s", out)
		}
		if !strings.Contains(out, "This run will edit "+cfgPath) {
			t.Errorf("the notice does not name the file it would edit:\n%s", out)
		}

		// AND SAYING IT CHANGED NOTHING. A notice that came with an edit would be
		// a different failure entirely.
		got, readErr := os.ReadFile(cfgPath)
		if readErr != nil {
			t.Fatalf("read back: %v", readErr)
		}
		if string(got) != seed {
			t.Errorf("the config was edited by a run that never created an App:\n%s", got)
		}
	})

	// BOTH SEEDS A PERSON WRITES BY HAND, driven through the whole command.
	//
	// A bare `github:` parses as a null SCALAR rather than a mapping, and the
	// renderer used to decline to fill it — writing no block, returning no error,
	// and printing "(updated)" over a file it had not changed while the App's
	// one-time key was already spent. That is fixed at the renderer, and a
	// review asked for it to be confirmed in the same pass; this confirms it
	// through the command an operator actually runs, which is where the claim
	// was made.
	for name, seed := range map[string]string{
		"a null github key": "# a note the operator wrote\ngithub:\n",
		"an empty mapping":  "# a note the operator wrote\ngithub: {}\n",
	} {
		t.Run("the flow finishes over "+name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "billet.yaml")
			keyPath := filepath.Join(dir, "app-private-key.pem")

			if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}

			calls := stubOnboard(t, testKey(t), nil)

			var err error

			out := capture(t, func() {
				err = githubAppCreate(t.Context(), []string{
					"--org", "acme", "--config", cfgPath, "--key-path", keyPath, "--no-browser",
				})
			})
			if err != nil {
				t.Fatalf("githubAppCreate: %v\n%s", err, out)
			}
			if *calls != 1 {
				t.Fatalf("onboarding ran %d times", *calls)
			}
			if !strings.Contains(out, configEditRule) {
				t.Errorf("the successful run never stated the rule:\n%s", out)
			}

			// Read back the way every later command reads it, rather than
			// trusting what the command printed about itself — which is exactly
			// the claim that was untrue.
			raw, readErr := os.ReadFile(cfgPath)
			if readErr != nil {
				t.Fatalf("read back: %v", readErr)
			}

			block, gotKeyPath, ok := existingGitHubBlock(raw)
			if !ok {
				t.Fatalf("no identity is readable after a successful run:\n%s", raw)
			}
			if block.AppID != stubAppID || block.InstallationID != stubInstallationID ||
				block.Org != "acme" {
				t.Errorf("read back app %d installation %d org %q, want %d/%d/acme",
					block.AppID, block.InstallationID, block.Org, stubAppID, stubInstallationID)
			}
			if gotKeyPath != keyPath {
				t.Errorf("the config names the key at %q, want %q", gotKeyPath, keyPath)
			}
			if !strings.Contains(string(raw), "# a note the operator wrote") {
				t.Errorf("the in-place edit lost the operator's comment:\n%s", raw)
			}

			// And the credential is where the config says it is, and usable — the
			// claim the whole command exists to make.
			if inspectKey(keyPath) != keyPresent {
				t.Errorf("no usable key at %s", keyPath)
			}

			// AND NOTHING BILLET STAGED IS STILL THERE. The preflight writes a
			// complete copy of the config to check the replacement can keep its
			// owner, and the write stages another; a directory that accumulates
			// either means a path forgot to clean up after itself.
			entries, dirErr := os.ReadDir(dir)
			if dirErr != nil {
				t.Fatalf("read the directory: %v", dirErr)
			}

			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".billet-config-") {
					t.Errorf("the run left %s behind", entry.Name())
				}
			}
		})
	}
}

// AN IDENTITY ALREADY IN THE FILE IS NAMED, because replacing one is a decision.
//
// It is not refused: minting a second App against a fresh --key-path is
// something an operator deliberately does, and init's own guidance describes it.
// What must not happen is finding out from a diff afterwards that the App a
// running deployment authenticates with has been replaced.
func TestTheNoticeNamesTheIdentityItWillReplace(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfgPath,
		[]byte("github:\n  org: old-org\n  app_id: 12345\n  installation_id: 7\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stubOnboard(t, testKey(t), errors.New("stop here"))

	out := capture(t, func() {
		_ = githubAppCreate(t.Context(), []string{ //nolint:errcheck // the stub's refusal is not what this asserts
			"--org", "acme", "--config", cfgPath,
			"--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
		})
	})

	for _, want := range []string{"already names App 12345", `"old-org"`, "REPLACES that identity"} {
		if !strings.Contains(out, want) {
			t.Errorf("the notice does not say %q:\n%s", want, out)
		}
	}
}

// AND THE OTHER HALF OF THE RULE IS SAID TOO: with no --config there is nothing
// on this machine to edit, and the block is printed instead. Stated up front for
// the same reason as its opposite — an operator should not have to run the
// command to find out what it does to their files.
func TestTheNoticeSaysWhenNothingWillBeEdited(t *testing.T) {
	dir := t.TempDir()

	stubOnboard(t, testKey(t), errors.New("stop here"))

	out := capture(t, func() {
		_ = githubAppCreate(t.Context(), []string{ //nolint:errcheck // the stub's refusal is not what this asserts
			"--org", "acme", "--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
		})
	})

	if !strings.Contains(out, "No --config was given") {
		t.Errorf("a run that edits nothing does not say so:\n%s", out)
	}
	if strings.Contains(out, "This run will edit") {
		t.Errorf("a run with no --config claimed it would edit a file:\n%s", out)
	}
}

// THE STEP THAT RECOMMENDS THE COMMAND SAYS WHAT IT WILL DO.
//
// The full rule is a paragraph and these are numbered steps an operator reads as
// a list of commands, so what goes here is the clause. It comes from the same
// declaration as the paragraph: two statements of one rule, kept together, is
// the fix — two rules is the defect.
func TestInitNextStepsSayTheAppCommandEditsTheConfig(t *testing.T) {
	out := capture(t, func() {
		printInitNext("/tmp/billet.yaml", initconfig.Params{
			Org: "acme", Provider: config.ProviderDocker,
			Profile: initconfig.ProfileLocal,
		}, true)
	})

	if !strings.Contains(out, configEditBrief) {
		t.Fatalf("step 1 recommends `github-app create` without saying it edits the config:\n%s",
			out)
	}

	// ON the step that recommends it, rather than further down where an operator
	// has already run the command.
	command := strings.Index(out, "billet github-app create")
	clause := strings.Index(out, configEditBrief)

	if command < 0 || clause > command {
		t.Errorf("the clause does not precede the command it is about (%d, %d):\n%s",
			clause, command, out)
	}
}

// THE CONFIG WRITE MUST NOT DESTROY THE APP KEY, AND ITS STAGING NAME DECIDED THAT.
//
// writeGitHubBlock staged at <config>.tmp with a truncating write, so an operator
// whose --key-path named that file had GitHub's one-and-only private key written
// there by onboarding and then overwritten by the config write, on its way to
// renaming the config over it. The App survives, its key does not, and the config
// it leaves behind points at a path that no longer exists.
//
// The invocation is odd; the outcome is a credential GitHub will not re-issue, and
// the same truncation destroys any unrelated file at that name.
func TestTheConfigWriteNeverClobbersTheKeyPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")
	keyPath := cfgPath + ".tmp"

	if err := os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	calls := stubOnboard(t, testKey(t), nil)

	var err error

	out := capture(t, func() {
		err = githubAppCreate(t.Context(), []string{
			"--org", "acme", "--config", cfgPath, "--key-path", keyPath, "--no-browser",
		})
	})
	if err != nil {
		t.Fatalf("githubAppCreate: %v\n%s", err, out)
	}
	if *calls != 1 {
		t.Fatalf("onboarding ran %d times", *calls)
	}

	// The credential is the assertion. It is at the path the operator named, and
	// it still parses.
	if state := inspectKey(keyPath); state != keyPresent {
		t.Fatalf("the App key at %s did not survive the config write (state %v)", keyPath, state)
	}

	// And the config was still written, so this is not passing because the write
	// was skipped.
	raw, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if block, _, ok := existingGitHubBlock(raw); !ok || block.AppID != stubAppID {
		t.Errorf("the config did not record the App:\n%s", raw)
	}
}

// AND NEITHER DOES THE OTHER WRITER, which is the half that was left behind.
//
// `billet init` stages its generation the same way `github-app create` stages
// its edit, and it staged at the same predictable <config>.tmp with a truncating
// write. init writes no App key itself, so it reaches that name only through one
// an earlier run put there — but the hazard is the NAME, and a rule enforced in
// one of two identical writers is a second entry point that does not enforce it.
func TestInitNeverClobbersAFileAtTheStagingName(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")
	keyPath := cfgPath + ".tmp"

	key := testKey(t)
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatalf("seed the key: %v", err)
	}

	var err error

	out := capture(t, func() {
		err = cmdInit(t.Context(), []string{
			"--config", cfgPath, "--org", "acme",
			"--runner-group", testTrialGroup,
			"--workflow", testTrialWorkflow,
		})
	})
	if err != nil {
		t.Fatalf("cmdInit: %v\n%s", err, out)
	}

	// The credential is the assertion.
	if state := inspectKey(keyPath); state != keyPresent {
		t.Fatalf("the App key at %s did not survive `billet init` (state %v)", keyPath, state)
	}

	// And the config was written, so this is not passing because init did
	// nothing at all.
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		t.Fatalf("no config was written: %v", statErr)
	}
}

// A SYMLINKED --config IS FOLLOWED, because a rename over one REPLACES the link.
//
// The write stages a sibling and renames it over the destination. Renaming over a
// symlink leaves the file it pointed at untouched and puts a regular file where
// the link was — so the operator keeps their old content, loses the link, and is
// told their config was edited in place. A symlinked config is an ordinary
// arrangement, so it is resolved rather than refused, and the notice says which
// file actually changes.
func TestASymlinkedConfigIsFollowedRatherThanReplaced(t *testing.T) {
	// THE FIXTURE LIVES IN AN ALREADY-RESOLVED DIRECTORY, because resolving a
	// symlinked config resolves the WHOLE path — and on macOS a t.TempDir sits
	// under /var, which is itself a link to /private/var. Without this the
	// assertions below would be comparing two spellings of one directory and
	// would have to canonicalize the production output to pass, which is a test
	// agreeing with whatever came out.
	dir, dirErr := filepath.EvalSymlinks(t.TempDir())
	if dirErr != nil {
		t.Fatalf("resolve the temp directory: %v", dirErr)
	}

	// IN A DIRECTORY OF ITS OWN, so the key default has somewhere to be wrong.
	// With the link and its target side by side, "beside the config" is the same
	// answer for both and the test cannot see which one was used.
	target := filepath.Join(dir, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	actual := filepath.Join(target, "real.yaml")
	link := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(actual, []byte("# the operator's own note\ngithub: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(actual, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	stubOnboard(t, testKey(t), nil)

	var err error

	// NO --key-path, so the default is exercised too: the key belongs beside the
	// file that actually holds the config, not beside the link.
	out := capture(t, func() {
		err = githubAppCreate(t.Context(), []string{
			"--org", "acme", "--config", link, "--no-browser",
		})
	})
	if err != nil {
		t.Fatalf("githubAppCreate: %v\n%s", err, out)
	}

	// The link is still a link.
	info, lstatErr := os.Lstat(link)
	if lstatErr != nil {
		t.Fatalf("lstat the link: %v", lstatErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symbolic link; the write replaced it", link)
	}

	// And the identity landed in the file it points at.
	raw, readErr := os.ReadFile(actual)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}

	block, _, ok := existingGitHubBlock(raw)
	if !ok || block.AppID != stubAppID {
		t.Fatalf("the target of the link did not record the App:\n%s", raw)
	}
	if !strings.Contains(string(raw), "# the operator's own note") {
		t.Errorf("the edit lost the operator's comment:\n%s", raw)
	}

	// AND THE OPERATOR WAS TOLD WHICH FILE CHANGES. They typed one path and a
	// different one was written; without this the diff they go and look at is
	// the wrong file.
	if !strings.Contains(out, "This run will edit "+actual) {
		t.Errorf("the notice does not name the file that is actually edited:\n%s", out)
	}
	if !strings.Contains(out, link+" is a symbolic link to it") {
		t.Errorf("the notice does not say the given path is a link:\n%s", out)
	}

	// AND THE KEY WENT BESIDE THE REAL FILE. The config it wrote names that path,
	// so a key beside the link would be a config pointing at nothing.
	key := filepath.Join(target, "app-private-key.pem")
	if state := inspectKey(key); state != keyPresent {
		t.Errorf("no usable key beside the file the link points at (%s, state %v)", key, state)
	}
	if _, named, ok := existingGitHubBlock(raw); !ok || named != key {
		t.Errorf("the config names the key at %q, want %q", named, key)
	}
}

// THE GOOD REFUSAL MUST REACH THE ORDINARY INVOCATION, not only the one that
// happens to pass --key-path.
//
// defaultKeyPath READS the config to find where the key belongs, and it used to
// run first — so a --config that does not exist came back as a bare `read
// --config …: no such file or directory`, and the refusal that explains what this
// command does and offers the seed was reachable only with --key-path given. The
// refusal table passes --key-path in every case, which is exactly what hid this.
func TestTheMissingConfigRefusalDoesNotDependOnKeyPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "billet.yaml")

	calls := stubOnboard(t, testKey(t), nil)

	var err error

	out := capture(t, func() {
		err = githubAppCreate(t.Context(), []string{
			"--org", "acme", "--config", cfgPath, "--no-browser",
		})
	})

	if err == nil {
		t.Fatalf("a missing config was not refused:\n%s", out)
	}
	if *calls != 0 {
		t.Errorf("onboarding ran anyway (%d calls)", *calls)
	}
	for _, want := range []string{"does not exist", "never creates one", "set -C"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// A NOTICE THAT COULD NOT BE DELIVERED STOPS THE RUN.
//
// The notice is the commitment this command makes before it acts, so a stdout
// that cannot take it — a full disk on a redirected run — must not leave
// onboarding to proceed in silence. Nothing has happened at that point, so
// refusing costs nothing; the opposite rule holds after the App exists, where a
// failure to record it must never be fatal.
func TestAnUndeliverableNoticeStopsTheRun(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	calls := stubOnboard(t, testKey(t), nil)

	// A pipe with no reader. Writing to it returns EPIPE rather than raising a
	// signal, because the descriptor is not the process's own fd 1.
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("pipe: %v", pipeErr)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}

	saved := os.Stdout
	os.Stdout = w

	t.Cleanup(func() {
		os.Stdout = saved
		_ = w.Close()
	})

	err := githubAppCreate(t.Context(), []string{
		"--org", "acme", "--config", cfgPath,
		"--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
	})

	os.Stdout = saved

	if err == nil {
		t.Fatal("a notice that could not be printed did not stop the run")
	}
	if !strings.Contains(err.Error(), "has not been done") {
		t.Errorf("the refusal does not say the run was stopped: %v", err)
	}
	if *calls != 0 {
		t.Errorf("onboarding ran without the operator being told anything (%d calls)", *calls)
	}
}

// AND THE NOTICE COMES BEFORE ANYTHING THAT OUTLIVES THE COMMAND.
//
// The failed-onboarding case proves the notice precedes `onboard`. It does not
// prove it precedes the key reservation, which is the first thing this command
// creates and LEAVES on disk — moving sayConfigEdit between the two would leave
// that case green. Here the reservation fails, so a notice printed after it
// never runs.
//
// "Before the first side effect" would now be too strong a claim, and saying it
// exactly is the point: the preflight stages a probe file to prove the config
// owner can be reproduced, and removes it again within the same call. Nothing
// survives it, nothing reaches GitHub, and the operator is told before the first
// thing that does.
func TestTheNoticeComesBeforeTheFirstSideEffect(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")
	keyPath := filepath.Join(dir, "app-private-key.pem")

	if err := os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// An occupied destination is what reserveKeyFile refuses, and it refuses
	// before anything else happens.
	if err := os.WriteFile(keyPath, []byte("an App key that is not ours\n"), 0o600); err != nil {
		t.Fatalf("seed the key: %v", err)
	}

	calls := stubOnboard(t, testKey(t), nil)

	var err error

	out := capture(t, func() {
		err = githubAppCreate(t.Context(), []string{
			"--org", "acme", "--config", cfgPath, "--key-path", keyPath, "--no-browser",
		})
	})

	if err == nil {
		t.Fatalf("an occupied key path was not refused:\n%s", out)
	}
	if *calls != 0 {
		t.Errorf("onboarding ran anyway (%d calls)", *calls)
	}
	if !strings.Contains(out, configEditRule) {
		t.Errorf("the notice had not been printed by the time the first side effect was "+
			"attempted:\n%s", out)
	}
}

// THE APP'S ONLY RECORD MUST NOT GO TO THE STREAM THAT JUST FAILED.
//
// Once onboarding has returned, the App exists and its one-time key is spent, so
// a config that cannot be written is reported and the identity is printed for
// pasting instead — deliberately non-fatal, because an error here sends the
// operator to re-run, which mints a SECOND App. That block is then the only
// local record of the App, and it used to go to stdout while the diagnostic went
// to stderr: the redirect that filled the disk, or the reader that went away, is
// exactly what could have caused the failure being reported.
func TestTheRecoveryBlockGoesToStderr(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The config disappears the instant the App exists, which is the state this
	// fallback is for and the one place a test can create it: everything the
	// preflight checks was true when it ran.
	prev := onboard
	t.Cleanup(func() { onboard = prev })

	onboard = func(_ context.Context, opts github.OnboardOptions) (*github.Onboarding, error) {
		app := &github.App{ID: stubAppID, ClientID: "Iv1.stub", PEM: string(testKey(t))}
		if err := opts.OnAppCreated(app); err != nil {
			return nil, err
		}

		if err := os.Remove(cfgPath); err != nil {
			return nil, err
		}

		return &github.Onboarding{
			App:          app,
			Installation: &github.Installation{ID: stubInstallationID},
		}, nil
	}

	var (
		err    error
		stdout string
	)

	stderr := captureStderr(t, func() {
		stdout = capture(t, func() {
			err = githubAppCreate(t.Context(), []string{
				"--org", "acme", "--config", cfgPath,
				"--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
			})
		})
	})

	// NON-FATAL, still. The App exists; an error here is an instruction to
	// re-run, and re-running mints another one.
	if err != nil {
		t.Fatalf("a config that could not be written became fatal: %v", err)
	}

	identity := fmt.Sprintf("app_id: %d", stubAppID)

	if !strings.Contains(stderr, "could not update") {
		t.Errorf("the failure was not reported:\n%s", stderr)
	}
	if !strings.Contains(stderr, identity) {
		t.Errorf("the only record of the App did not reach stderr:\n%s", stderr)
	}
	if strings.Contains(stdout, identity) {
		t.Errorf("the recovery block went to stdout, which is what may have failed:\n%s", stdout)
	}
}

// OWNERSHIP IS PRESERVED OR THE WRITE REFUSES, and "the caller is not root" is
// not a reason to skip it.
//
// The rename swaps the inode, so a config owned by root in a directory the
// invoker can write comes back owned by the invoker — after which the packaged
// server unit, which runs as billet, cannot read its own config, and this
// command has just promised the owner was left alone. A failed chown used to be
// ignored for every non-root caller on the reasoning that such a caller already
// owns the file, which is the ordinary case and not the only one.
func TestOwnershipIsNotSilentlyHandedToTheInvoker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the chown succeeds, so there is nothing to refuse")
	}

	staged, err := os.CreateTemp(t.TempDir(), "staged-*")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	t.Cleanup(func() { _ = staged.Close() })

	info, statErr := staged.Stat()
	if statErr != nil {
		t.Fatalf("stat: %v", statErr)
	}

	uid, gid, ok := fileOwner(info)
	if !ok {
		t.Skip("this platform does not report file ownership")
	}

	// THE ACCOMMODATION THE OLD RULE WAS REALLY FOR: a platform that refuses even
	// a same-id chown must not break an operator rewriting their own file.
	if err := preserveOwner(staged, uid, gid); err != nil {
		t.Errorf("preserving the ownership a file already has was refused: %v", err)
	}

	// AND THE CASE IT WAS HIDING. A non-root process cannot give this file away,
	// so the config would silently change hands.
	err = preserveOwner(staged, uid+1, gid)
	if err == nil {
		t.Fatal("a write that would change the config's owner was allowed")
	}
	if !strings.Contains(err.Error(), "owns the config") {
		t.Errorf("the refusal does not say what to do about it: %v", err)
	}
}

// AND THE WRITE LEAVES NOTHING BEHIND IT, which is what catches a rename that
// stopped happening: the staged file would then sit in the operator's config
// directory, a full copy of a config naming where the App key lives, while the
// command reported success over a file it never replaced.
//
// IT DOES NOT COVER THE REMOVAL ON A FAILED WRITE, and the deferred removal's
// mutant therefore survives. Every failure this test can arrange — an unreadable
// config, an identity that will not read back, an unwritable directory —
// happens before os.CreateTemp runs, so nothing is staged to be left. Said here
// rather than left as an unexplained survivor.
func TestTheConfigWriteLeavesNoStagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(path, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: stubAppID, InstallationID: stubInstallationID,
		PrivateKeyPath: "/etc/billet/app-private-key.pem",
	}); err != nil {
		t.Fatalf("writeGitHubBlock: %v", err)
	}

	// The identity is there, so this is not passing because the write did
	// nothing at all.
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if block, _, ok := existingGitHubBlock(raw); !ok || block.AppID != stubAppID {
		t.Fatalf("the write did not record the App:\n%s", raw)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".billet-config-") {
			t.Errorf("the write left %s behind", entry.Name())
		}
	}
}

// AN IDENTITY THAT REPLACES ANOTHER TAKES THE WHOLE OF IT, client_id included.
//
// renderGitHubBlock sets client_id only when GitHub returned one, and it used to
// LEAVE an existing value alone otherwise — so replacing an App with one that
// has no client id kept the previous App's. That is not cosmetic: newScaleSetClient
// PREFERS client_id over app_id when it mints the App JWT, so the config would
// authenticate as an App whose key it no longer holds, and the failure surfaces at
// GitHub rather than anywhere near this file.
func TestReplacingAnIdentityLeavesNoneOfTheOldOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	seed := "github:\n  org: old-org\n  app_id: 1\n  client_id: Iv1.old\n" +
		"  installation_id: 2\n  private_key_path: /old/key.pem\n"

	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The new App has no client id, which is the case the old code could not
	// express: every other field is written and that one is left behind.
	want := githubBlock{
		Org: "acme", AppID: stubAppID, InstallationID: stubInstallationID,
		PrivateKeyPath: "/etc/billet/app-private-key.pem",
	}

	if err := writeGitHubBlock(path, want); err != nil {
		t.Fatalf("writeGitHubBlock: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if strings.Contains(string(raw), "Iv1.old") {
		t.Errorf("the previous App's client id survived the replacement:\n%s", raw)
	}

	got, gotKeyPath, ok := existingGitHubBlock(raw)
	if !ok {
		t.Fatalf("no identity is readable:\n%s", raw)
	}
	if got.ClientID != "" || got.Org != want.Org || got.AppID != want.AppID ||
		got.InstallationID != want.InstallationID || gotKeyPath != want.PrivateKeyPath {
		t.Errorf("read back %+v key %q, want %+v", got, gotKeyPath, want)
	}
}

// THE REFUSAL THAT PROTECTS OWNERSHIP HAS TO LAND BEFORE THE APP EXISTS.
//
// It was reachable only from inside writeGitHubBlock, which runs after
// onboarding — a deterministic, knowable-in-advance failure on the far side of an
// App that cannot be un-created, which is the whole class this preflight removes.
// Driven through the seam because the real refusal needs a config owned by an
// account this process is not, and what matters here is the ORDER rather than the
// mechanism.
func TestOwnershipIsProvedBeforeTheAppIsCreated(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prev := ownershipPreservable
	t.Cleanup(func() { ownershipPreservable = prev })

	ownershipPreservable = func(string, []byte) error {
		return errors.New("this would replace a config owned by 0:0")
	}

	calls := stubOnboard(t, testKey(t), nil)

	var err error

	out := capture(t, func() {
		err = githubAppCreate(t.Context(), []string{
			"--org", "acme", "--config", cfgPath,
			"--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
		})
	})

	if err == nil {
		t.Fatalf("a config whose owner cannot be kept was not refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "owned by") {
		t.Errorf("the refusal does not say what it is about: %v", err)
	}
	if *calls != 0 {
		t.Errorf("onboarding ran anyway, so the App exists and its key is spent (%d calls)",
			*calls)
	}
}

// THE PRINTED BLOCK IS YAML, not a string that looks like it.
//
// It is the only record of an App when the config could not be written, and it is
// meant to be pasted back and read by config.Load. Printf does not quote: a key
// path containing " #" became a comment, so the value read back short and the
// server would open a file that is not the key.
func TestThePrintedBlockSurvivesBeingReadBack(t *testing.T) {
	awkward := githubBlock{
		Org: "acme", AppID: stubAppID, InstallationID: stubInstallationID,
		ClientID:       "Iv1.stub",
		PrivateKeyPath: "/tmp/keys/app # not a comment.pem",
	}

	var out strings.Builder

	if err := printGitHubBlock(&out, awkward); err != nil {
		t.Fatalf("printGitHubBlock: %v", err)
	}

	got, gotKeyPath, ok := existingGitHubBlock([]byte(out.String()))
	if !ok {
		t.Fatalf("the printed block does not parse:\n%s", out.String())
	}
	if gotKeyPath != awkward.PrivateKeyPath {
		t.Errorf("the key path read back as %q, want %q\n%s",
			gotKeyPath, awkward.PrivateKeyPath, out.String())
	}
	if got.Org != awkward.Org || got.AppID != awkward.AppID ||
		got.InstallationID != awkward.InstallationID || got.ClientID != awkward.ClientID {
		t.Errorf("the printed block read back as %+v, want %+v\n%s", got, awkward, out.String())
	}

	// AND A STREAM THAT WILL NOT TAKE IT SAYS SO. Silence here is an App whose
	// identity was recorded nowhere at all.
	if err := printGitHubBlock(refusingWriter{}, awkward); err == nil {
		t.Error("a block that could not be written was reported as printed")
	}
}

// refusingWriter fails every write, which is what a full disk or a closed
// redirect looks like to the one output that carries an App's only record.
type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, errors.New("no room") }

// A KEY IS A SCALAR, AND AN ALIAS USED AS ONE IS NOT THE KEY IT NAMES.
//
// An alias node carries the ANCHOR NAME in Value, so `? *client_id : keep-me`
// answered to a search for `client_id` — and removing an identity's client id
// would then delete the operator's unrelated entry instead. Nothing downstream
// would have noticed: the identity still reads back exactly as asked for, so the
// write reports success over a file it quietly took something out of.
func TestAnAliasedKeyIsNotTheKeyItNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	seed := "key_name: &client_id retained_setting\n" +
		"github:\n" +
		"  ? *client_id\n" +
		"  : keep-me\n" +
		"  org: old\n"

	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// An identity with NO client id, which is what makes the removal run.
	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: stubAppID, InstallationID: stubInstallationID,
		PrivateKeyPath: "/etc/billet/app-private-key.pem",
	}); err != nil {
		t.Fatalf("writeGitHubBlock: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// The operator's entry is still there. Asserting on the identity alone
	// cannot see this: it was correct either way, which is what made the
	// deletion silent.
	if !strings.Contains(string(raw), "keep-me") {
		t.Errorf("writing the identity deleted an entry that only shares an alias name:\n%s", raw)
	}

	got, _, ok := existingGitHubBlock(raw)
	if !ok || got.AppID != stubAppID || got.Org != "acme" {
		t.Errorf("the identity was not written:\n%s", raw)
	}
}

// AND THE OTHER DIRECTION: AN ALIAS WHOSE VALUE *IS* THE KEY.
//
// Refusing every alias node was the first fix and it was wrong the other way
// round. An anchor named something else whose VALUE is `github` decodes as the
// key `github`, so skipping it made mappingFor append a SECOND one — leaving a
// document with two `github` keys, which the read-back then refuses. The config
// is perfectly readable; billet simply could not see its own block. Asking the
// decoder answers both directions with what every later reader sees.
func TestAnAliasWhoseValueIsTheKeyIsTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	seed := "key_name: &actual github\n" +
		"? *actual\n" +
		": {org: old}\n"

	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: stubAppID, InstallationID: stubInstallationID,
		PrivateKeyPath: "/etc/billet/app-private-key.pem",
	}); err != nil {
		t.Fatalf("writeGitHubBlock: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	got, _, ok := existingGitHubBlock(raw)
	if !ok {
		t.Fatalf("no identity is readable, so the block was written somewhere else:\n%s", raw)
	}
	if got.AppID != stubAppID || got.Org != "acme" {
		t.Errorf("read back app %d org %q, want %d/acme\n%s",
			got.AppID, got.Org, stubAppID, raw)
	}
}

// A RUN THAT CANNOT REPORT ITS RESULT STILL EXITS 0, once the App exists.
//
// The App cannot be un-created and its key is spent, so a non-zero exit is an
// instruction — to a person and to every wrapper script — to run the command
// again, which mints a SECOND App. Wrapping the error in a sentinel does not
// change that: main maps every error to status 1, so the only way to keep the
// contract is not to return one.
func TestAPostAppFailureNeverAsksToBeRerun(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prev := onboard
	t.Cleanup(func() { onboard = prev })

	onboard = func(_ context.Context, opts github.OnboardOptions) (*github.Onboarding, error) {
		app := &github.App{ID: stubAppID, ClientID: "Iv1.stub", PEM: string(testKey(t))}
		if err := opts.OnAppCreated(app); err != nil {
			return nil, err
		}

		// The config goes away after the preflight passed, so the write fails
		// with the App already registered — the state the whole fallback is for.
		if err := os.Remove(cfgPath); err != nil {
			return nil, err
		}

		return &github.Onboarding{
			App:          app,
			Installation: &github.Installation{ID: stubInstallationID},
		}, nil
	}

	// AND BOTH STREAMS BREAK ONCE THE APP EXISTS, which is the case that used to
	// return an error. They are broken from INSIDE the stub, because breaking
	// them earlier would refuse at the notice instead — correctly, and before an
	// App exists, which is a different rule and a different test.
	savedOut, savedErr := os.Stdout, os.Stderr

	t.Cleanup(func() { os.Stdout, os.Stderr = savedOut, savedErr })

	inner := onboard
	onboard = func(ctx context.Context, opts github.OnboardOptions) (*github.Onboarding, error) {
		result, err := inner(ctx, opts)

		os.Stdout = brokenPipe(t)
		os.Stderr = brokenPipe(t)

		return result, err
	}

	err := githubAppCreate(t.Context(), []string{
		"--org", "acme", "--config", cfgPath,
		"--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
	})

	os.Stdout, os.Stderr = savedOut, savedErr

	if err != nil {
		t.Errorf("a run that created an App and could not report it returned an error, which "+
			"reads as `run me again`: %v", err)
	}
}

// brokenPipe is a writer that fails every write, the way a redirect onto a full
// disk or a reader that went away does. It is a real *os.File because the code
// under test writes to os.Stdout, which is one.
func brokenPipe(t *testing.T) *os.File {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}

	t.Cleanup(func() { _ = w.Close() })

	return w
}

// AND WHEN THE FIRST STREAM WILL NOT TAKE IT, THE SECOND IS TRIED.
//
// The recovery block is the only local record of an App that already exists, and
// the run that could not write the config is quite possibly one whose stderr is
// the reason — a redirect onto the filesystem that just filled up takes both. A
// fallback nobody exercises is a fallback that quietly is not there.
func TestTheRecoveryBlockFallsBackToTheOtherStream(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prev := onboard
	t.Cleanup(func() { onboard = prev })

	savedErr := os.Stderr
	t.Cleanup(func() { os.Stderr = savedErr })

	onboard = func(_ context.Context, opts github.OnboardOptions) (*github.Onboarding, error) {
		app := &github.App{ID: stubAppID, ClientID: "Iv1.stub", PEM: string(testKey(t))}
		if err := opts.OnAppCreated(app); err != nil {
			return nil, err
		}

		// The config goes away and stderr stops working, both after the
		// preflight passed: the write fails and the stream it would report on
		// cannot take the block.
		if err := os.Remove(cfgPath); err != nil {
			return nil, err
		}

		os.Stderr = brokenPipe(t)

		return &github.Onboarding{
			App:          app,
			Installation: &github.Installation{ID: stubInstallationID},
		}, nil
	}

	var err error

	stdout := capture(t, func() {
		err = githubAppCreate(t.Context(), []string{
			"--org", "acme", "--config", cfgPath,
			"--key-path", filepath.Join(dir, "app-private-key.pem"), "--no-browser",
		})
	})

	os.Stderr = savedErr

	if err != nil {
		t.Fatalf("the fallback must stay non-fatal: %v", err)
	}

	if !strings.Contains(stdout, fmt.Sprintf("app_id: %d", stubAppID)) {
		t.Errorf("the App's only record was not written to the stream that still worked:\n%s",
			stdout)
	}
}
