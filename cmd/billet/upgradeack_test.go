package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/node"
)

func TestEarlyHostUpgradeRefusalsReachTheAcknowledgement(t *testing.T) {
	cfgPath := writeCAConfig(t, t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing-billet.yaml")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "instruction", args: []string{"--rollout", "decision"}, want: "Pass both, or neither"},
		{name: "configuration", args: []string{"--config", missing}, want: "missing-billet.yaml"},
		{name: "from-rollout", args: []string{"--config", cfgPath, "--from-rollout"},
			want: "--from-rollout takes its whole instruction from the ledger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ack, answer := ackReader(t)
			args := append([]string{"--ack-path", ack.path}, tc.args...)
			err := cmdHostUpgrade(t.Context(), args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the intended early refusal was not reached: %v", err)
			}

			want := node.AckRefused + strings.ReplaceAll(err.Error(), "\n", " ")
			if got := answer(); got != want {
				t.Errorf("the early refusal did not reach its caller: got %q, want %q", got, want)
			}
		})
	}
}
