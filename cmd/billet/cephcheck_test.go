package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// A COMPUTE HOST WITH NO rbd FAILS THE CHECK.
//
// Only a firecracker node may carry a `node.ceph` block, so a file that has one
// always describes a machine meant to run jobs — and one without the client
// package cannot map a single volume. Reporting it and exiting zero would make
// `billet check` say a host is fine when nothing on it can launch, which is the
// opposite of what the command is for.
//
// A control plane is unaffected and needs no case here: with no node section
// cmdCheck never reaches this function.
//
// This test exists because the package-level one exercises `ceph.New`, and a
// mutation restoring the old "print unverified, return nil" behaviour in the CLI
// leaves that one green.
func TestAHostWithNoRBDFailsTheCheck(t *testing.T) {
	// Not parallel: it edits PATH, which is process-global and which t.Setenv
	// refuses to touch in a parallel test for exactly that reason.
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	err := checkCephCluster(t.Context(), &config.CephConfig{
		User:      config.DefaultCephUser,
		ImagePool: "billet-images",
		CachePool: "billet-cache",
	})
	if err == nil {
		t.Fatal("checkCephCluster reported success on a host with no rbd command")
	}

	if !errors.Is(err, ceph.ErrNoRBD) {
		t.Errorf("the failure is not ErrNoRBD, so a caller cannot tell it from an unreachable "+
			"cluster: %v", err)
	}

	// The pools, because the operator's next question is which cluster this file
	// was talking about, and the remedy, because it is the whole point of failing
	// here rather than at the first job.
	for _, want := range []string{"billet-images", "billet-cache", "ceph-common"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}

	// SAID ONCE. Every wrapper renders the message beneath it, so naming the
	// package at both layers put the same remedy on the terminal twice in one
	// sentence.
	if n := strings.Count(err.Error(), "ceph-common"); n != 1 {
		t.Errorf("the remedy appears %d times in one sentence: %v", n, err)
	}
}

// A CONFIG THE CHECK CANNOT ACT ON AT ALL IS REFUSED BEFORE THE PATH LOOKUP.
//
// `billet check` is reachable with a config that never went through Load — a
// caller building one, or a future command doing so — and the constructor is what
// re-applies the rules there. An identity of `admin` must not reach a cluster
// merely because rbd happened to be installed.
func TestTheCheckRefusesAnAdministratorBeforeItLooksForRBD(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	err := checkCephCluster(t.Context(), &config.CephConfig{
		User:      "admin",
		ImagePool: "billet-images",
		CachePool: "billet-cache",
	})
	if err == nil {
		t.Fatal("checkCephCluster accepted an admin identity")
	}

	if errors.Is(err, ceph.ErrNoRBD) {
		t.Errorf("the refusal was about the missing binary rather than the identity: %v", err)
	}

	if !strings.Contains(err.Error(), "can delete the pools") {
		t.Errorf("the error does not explain the refusal: %v", err)
	}
}
