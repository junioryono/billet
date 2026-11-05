package node

import (
	"log/slog"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE BACKEND IS TOLD WHAT THE LEASE BOOTS, from the lease: the EC2 boot script is a
// different language on Windows, and the ledger is the authority on what was reserved.
func TestALaunchCarriesTheLeasesGuestOS(t *testing.T) {
	for _, os := range []config.GuestOS{config.GuestLinux, config.GuestWindows} {
		p := &fakeProvider{kind: config.ProviderDocker}
		a, host := newAllocatorWithHost(t)
		runner := New(a, host, &fakeJIT{setID: 7}, p, nil)
		lease := assignedLease(t, a)
		lease.GuestOS = os

		if err := runner.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11}); err != nil {
			t.Fatalf("Launch: %v", err)
		}

		if got := p.launched[0].GuestOS; got != os {
			t.Errorf("the backend was told %q for a %q lease", got, os)
		}
	}
}

// NO BEARER FOR A GUEST WITH NOTHING TO SPEND IT: every cache's client is in a Linux
// guest, so a Windows lease is prepared none, while a Linux one on the same node is.
func TestAWindowsLeaseIsPreparedNoCacheCredentials(t *testing.T) {
	for _, tc := range []struct {
		os   config.GuestOS
		want bool
	}{{config.GuestLinux, true}, {config.GuestWindows, false}} {
		p := &fakeProvider{kind: config.ProviderDocker}

		service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(), &fakeCacheStore{},
			&fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService: %v", err)
		}

		a, host := newAllocatorWithHost(t)
		runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
		lease := assignedLease(t, a)
		lease.GuestOS = tc.os

		if err := runner.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
			t.Fatalf("Launch: %v", err)
		}

		spec := p.launched[0]
		if got := spec.CacheToken != ""; got != tc.want {
			t.Errorf("a %s lease was given a cache bearer: %v, want %v", tc.os, got, tc.want)
		}
		if tc.os == config.GuestWindows && (spec.CacheEndpoint != "" || spec.GuestMonitor) {
			t.Errorf("a Windows lease was told of a cache: endpoint %q monitor %v", spec.CacheEndpoint, spec.GuestMonitor)
		}
	}
}
