package main

import (
	"strings"
	"testing"
)

// `billet ami` DISPATCHES TO THE SUBCOMMAND THAT WAS TYPED.
//
// A one-subcommand command grew a second one, and the shape it grew from — an
// `args[0] != "build"` guard — accepts nothing else. What this asserts is that
// each name reaches its own command and an unknown one is refused, because the
// alternative failure is silent in the worst direction: `billet ami verify` that
// falls through to the builder would launch a paid machine and try to build an
// image named after nothing.
//
// EACH CASE STOPS AT ITS OWN FIRST REFUSAL, which is what proves where it landed
// without any of these touching AWS: build refuses without --base-image, and
// verify refuses without an image id.
func TestAMIDispatchesToTheSubcommandThatWasTyped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no subcommand",
			args: nil,
			want: "usage: billet ami <build|verify>",
		},
		{
			name: "an unknown subcommand",
			args: []string{"publish"},
			want: "usage: billet ami <build|verify>",
		},
		{
			name: "build, which needs a base image",
			args: []string{"build"},
			want: "--base-image is required",
		},
		{
			name: "verify, which needs an image id",
			args: []string{"verify"},
			want: "usage: billet ami verify <ami-id>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := cmdAMI(t.Context(), tc.args)
			if err == nil {
				t.Fatalf("billet ami %v returned success; it should have refused before "+
					"launching anything", tc.args)
			}

			// THE SPECIFIC REFUSAL, not merely an error. Every one of these cases
			// errors under a dispatch that ignores the subcommand entirely — build's
			// missing --base-image would answer for all four — so asserting "an error
			// came back" would agree with exactly the bug this is about.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("billet ami %v said %q, want it to contain %q", tc.args, err, tc.want)
			}
		})
	}
}
