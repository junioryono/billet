package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/notices"
)

// THE BINARY CARRIES ITS NOTICES, which is how a copy of billet that left the
// archive it came in still satisfies the licenses of what it links.
func TestVersionNoticesPrintsTheEmbeddedNotices(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: io.Discard, Getenv: func(string) string { return "" }}

	if err := cmdVersion(t.Context(), env, []string{"--notices"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if got != notices.Text() {
		t.Fatalf("billet version --notices printed %d bytes, want the %d of the embedded notices",
			len(got), len(notices.Text()))
	}
	for _, want := range []string{"Third-party notices for billet", "The Go standard library", "github.com/jackc/pgx/v5"} {
		if !strings.Contains(got, want) {
			t.Errorf("the notices do not name %q", want)
		}
	}
}

func TestVersionWithoutNoticesPrintsNoNotices(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: io.Discard, Getenv: func(string) string { return "" }}

	if err := cmdVersion(t.Context(), env, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Third-party notices") || !strings.HasPrefix(out.String(), "billet ") {
		t.Fatalf("plain billet version printed %q", out.String())
	}
}
