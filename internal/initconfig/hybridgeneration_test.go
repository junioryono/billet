package initconfig

import (
	"strings"
	"testing"
)

// A MINTED FINGERPRINT HAS THE ONE SHAPE THE CHECK ACCEPTS, and two mints
// differ: the runbook prints it, and a repeated value would bind two roots to
// each other's outputs.
func TestNewHybridGenerationIsAFingerprint(t *testing.T) {
	t.Parallel()

	a, err := NewHybridGeneration()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b, err := NewHybridGeneration()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	for _, g := range []string{a, b} {
		if err := CheckHybridGeneration(g); err != nil {
			t.Errorf("a minted fingerprint %q fails its own check: %v", g, err)
		}
	}
	if a == b {
		t.Errorf("two mints returned the same fingerprint %q", a)
	}
}

// THE CHECK REFUSES ANYTHING BUT THE PRINTED SHAPE, padding included, because
// the value is compared byte for byte with what the root declares.
func TestCheckHybridGenerationRefusesOtherShapes(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"", "0123456789abcdef", "0123-4567-89ab-cdeF", " 0123-4567-89ab-cdef",
		"0123-4567-89ab-cdef ", "0123-4567-89ab", "0123-4567-89ab-cdef-0000", "g123-4567-89ab-cdef",
	} {
		if err := CheckHybridGeneration(bad); err == nil {
			t.Errorf("%q was accepted as a fingerprint", bad)
		}
	}

	if err := CheckHybridGeneration(testGeneration); err != nil {
		t.Errorf("the printed shape was refused: %v", err)
	}
}

// THE ROOT A RENDER WRITES IS THE ROOT A LATER RENDER READS THE FINGERPRINT
// BACK FROM, so a re-run of the plan render and --force keep the generation.
func TestHybridGenerationOfReadsTheRenderedRoot(t *testing.T) {
	t.Parallel()

	files, _ := mustGenerateHybrid(t, hybridParams())

	got, found, err := HybridGenerationOf(files[HybridTerraformFile])
	if err != nil || !found || got != testGeneration {
		t.Fatalf("the rendered root reads back as (%q, %v, %v), want %q", got, found, err, testGeneration)
	}

	// A ROOT WRITTEN BEFORE THE FINGERPRINT declares none, which is not an
	// error: the plan render mints one over it.
	without := strings.Replace(files[HybridTerraformFile], `output "generation"`, `output "renamed"`, 1)
	if without == files[HybridTerraformFile] {
		t.Fatal("the rendered root no longer declares the generation output this case removes")
	}
	if _, found, err := HybridGenerationOf(without); found || err != nil {
		t.Errorf("a root with no generation output read as (found %v, %v)", found, err)
	}

	// A MALFORMED ONE IS AN ERROR, never a root without one: minting over it
	// would quietly start another generation under the same directory.
	malformed := strings.Replace(files[HybridTerraformFile], `"`+testGeneration+`"`, `"not-a-fingerprint"`, 1)
	if malformed == files[HybridTerraformFile] {
		t.Fatal("the rendered root no longer carries the fingerprint literal this case rewrites")
	}
	if _, found, err := HybridGenerationOf(malformed); found || err == nil {
		t.Errorf("a malformed fingerprint read as (found %v, %v), want an error", found, err)
	}

	// AND A VALUE THAT IS NOT A QUOTED STRING AT ALL, which the extraction
	// cannot even see: it is a declaration, so it refuses like a malformed one.
	for _, value := range []string{"null", "42", `"0123-4567`, "var.generation"} {
		odd := strings.Replace(files[HybridTerraformFile], `"`+testGeneration+`"`, value, 1)
		if _, found, err := HybridGenerationOf(odd); found || err == nil {
			t.Errorf("a generation output whose value is %s read as (found %v, %v), want an error",
				value, found, err)
		}
	}
}

// THE SCAN IS LEXICAL BUT NOT NAIVE: a declaration commented out, quoted
// inside a string or written in a heredoc is not one; a comment inside the
// live one does not hide it; and everything the scan cannot settle (two
// declarations, an expression, a template, an unterminated string, comment or
// heredoc) is an error, never "none", because "none" mints a fingerprint.
func TestHybridGenerationOfReadsOnlyALiveDeclaration(t *testing.T) {
	t.Parallel()

	const fp = "0123-4567-89ab-cdef"
	live := "output \"generation\" {\n  description = \"d\"\n  value       = \"" + fp + "\"\n}\n"

	for _, c := range []struct {
		name, root string
		found      bool
		fails      bool
	}{
		{"the rendered shape", live, true, false},
		{"a comment inside the declaration", "output /* why */ \"generation\" { # here\n  value = \"" + fp + "\" // and here\n}\n", true, false},
		{"an old block commented out above the live one",
			"/*\noutput \"generation\" {\n  value = \"fedc-ba98-7654-3210\"\n}\n*/\n" + live, true, false},
		{"a line comment", "# output \"generation\" { value = \"" + fp + "\" }\n", false, false},
		{"inside a description", "output \"name\" {\n  description = \"see output \\\"generation\\\" {\"\n  value = \"x\"\n}\n", false, false},
		{"inside a heredoc", "locals {\n  note = <<EOT\noutput \"generation\" {\n  value = \"" + fp + "\"\n}\nEOT\n}\n", false, false},
		{"a brace in the description", "output \"generation\" {\n  description = \"a } b\"\n  value = \"" + fp + "\"\n}\n", true, false},
		{"two declarations", live + live, false, true},
		{"an expression", "output \"generation\" {\n  value = \"" + fp + "\" + \"\"\n}\n", false, true},
		{"a template", "output \"generation\" {\n  value = \"${var.g}\"\n}\n", false, true},
		{"no value", "output \"generation\" {\n  description = \"d\"\n}\n", false, true},
		{"an unterminated string before it", "locals { a = \"oops\n}\n" + live, false, true},
		{"an unterminated comment before it", "/* oops\n" + live, false, true},
		{"an unterminated heredoc before it", "locals {\n  a = <<EOT\nnever closed\n" + live, false, true},
		{"a bare label", "output generation {\n  value = \"" + fp + "\"\n}\n", true, false},
		{"a bare label of another output", "output generation_x {\n  value = \"" + fp + "\"\n}\n", false, false},
		// A TEMPLATE'S OWN STRING, which holds a quote that does not close the
		// outer one: read as the end of the string, the /* after it swallowed
		// the declaration up to a */ further down.
		{"a quote inside a template", "locals { opener = \"${\"/*\"}\" }\n" + live + "locals { closer = \"*/#\" }\n", true, false},
		{"an escaped template", "locals { a = \"$${\\\"/*\\\"}\" }\n" + live, true, false},
		{"a template in the rendered root's bucket", "resource \"x\" \"y\" {\n  bucket = \"n-${data.aws_caller_identity.this.account_id}\"\n}\n" + live, true, false},
		// CRLF AND HYPHENATED DELIMITERS ARE HEREDOCS, so their bodies are not
		// code: a /* in one swallows nothing, and a declaration in one is dead.
		{"a CRLF heredoc holding a comment opener",
			strings.ReplaceAll("locals {\n  opener = <<EOT\n/*\nEOT\n}\n"+live+"locals { closer = \"*/#\" }\n", "\n", "\r\n"), true, false},
		{"a CRLF heredoc holding a declaration",
			strings.ReplaceAll("locals {\n  note = <<EOT\noutput \"generation\" {\n  value = \""+fp+"\"\n}\nEOT\n}\n", "\n", "\r\n"), false, false},
		{"a hyphenated delimiter", "locals {\n  note = <<-END-TEXT\n  output \"generation\" { value = \"x\" }\n  END-TEXT\n}\n" + live, true, false},
		{"a << that is not a heredoc", "locals { a = 1 << 2 }\n" + live, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got, found, err := HybridGenerationOf(c.root)
			switch {
			case c.fails && err == nil:
				t.Fatalf("read as (%q, %v) with no error, want an error", got, found)
			case !c.fails && err != nil:
				t.Fatalf("refused: %v", err)
			case found != c.found:
				t.Fatalf("found %v, want %v", found, c.found)
			case c.found && got != fp:
				t.Fatalf("read %q, want %q", got, fp)
			}
		})
	}
}

// THE ROOT DECLARES THE FINGERPRINT IT WAS RENDERED WITH AND THE ACCOUNT FROM
// THE APPLY'S OWN CALLER IDENTITY, so the outputs carry both facts a later
// render compares.
func TestGenerateHybridRootDeclaresItsGenerationAndAccount(t *testing.T) {
	t.Parallel()

	files, _ := mustGenerateHybrid(t, hybridParams())
	root := files[HybridTerraformFile]

	for _, want := range []string{
		"output \"" + HybridOutputGeneration + "\" {",
		"value       = \"" + testGeneration + "\"",
		"output \"" + HybridOutputAccountID + "\" {",
		"value       = data.aws_caller_identity.this.account_id",
	} {
		if !strings.Contains(root, want) {
			t.Errorf("terraform/main.tf does not carry %q", want)
		}
	}
}

// A GENERATION IS REQUIRED, and only in the printed shape: a render without
// one would write a root whose outputs bind to nothing.
func TestGenerateHybridRefusesAMissingGeneration(t *testing.T) {
	t.Parallel()

	for _, g := range []string{"", "0123"} {
		p := hybridParams()
		p.Generation = g

		if _, _, err := GenerateHybrid(p); err == nil || !strings.Contains(err.Error(), "--generation") {
			t.Errorf("generation %q was not refused by its flag: %v", g, err)
		}
	}
}

// OUTPUTS FROM A ROOT THAT PREDATES THE FINGERPRINT ARE REFUSED WITH THE WAY
// TO A ROOT THAT HAS ONE, and an account id that is not one is refused rather
// than compared.
func TestParseTerraformOutputDemandsTheBinding(t *testing.T) {
	t.Parallel()

	full := `{
  "control_plane_private_ip": {"value": "10.60.0.10"},
  "ledger_volume_id": {"value": "vol-0abc"},
  "subnet_id": {"value": "subnet-0abc"},
  "runner_security_group_id": {"value": "sg-trusted"},
  "ami_payload_bucket": {"value": "acme-ci-ami-payloads-1"},
  "name": {"value": "acme-ci"},
  "region": {"value": "us-west-2"},
  "generation": {"value": "0123-4567-89ab-cdef"},
  "account_id": {"value": "123456789012"}
}`
	if _, err := ParseTerraformOutput([]byte(full), HybridNeeds{}); err != nil {
		t.Fatalf("a complete output: %v", err)
	}

	for _, name := range []string{HybridOutputGeneration, HybridOutputAccountID} {
		old := strings.Replace(full, `"`+name+`"`, `"renamed"`, 1)
		_, err := ParseTerraformOutput([]byte(old), HybridNeeds{})
		if err == nil {
			t.Errorf("outputs with no %q were accepted", name)

			continue
		}
		for _, want := range []string{name, "scratch --out", "terraform plan", "only those two outputs", "--generation"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of a missing %q does not say %q: %v", name, want, err)
			}
		}
	}

	notAnAccount := strings.Replace(full, `"123456789012"`, `"12345"`, 1)
	if _, err := ParseTerraformOutput([]byte(notAnAccount), HybridNeeds{}); err == nil ||
		!strings.Contains(err.Error(), HybridOutputAccountID) {
		t.Errorf("an account id that is not twelve digits was not refused by name: %v", err)
	}
}
