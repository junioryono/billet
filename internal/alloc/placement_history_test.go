package alloc

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// pricedCloudAllocator is an allocator over one EC2 host whose two shapes carry
// prices, so a test can say what a lease was charged at. The eight-vCPU shape's
// price is the caller's; the sixteen's is fixed at 680000.
func pricedCloudAllocator(t *testing.T, eight config.USDPerHour) *Allocator {
	t.Helper()

	tier := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier})

	if _, err := a.RegisterNode(t.Context(), pricedCloudRegistration(eight, 680_000)); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return a
}

func pricedCloudRegistration(eight, sixteen config.USDPerHour) NodeRegistration {
	return NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, Site: "us-east",
		VCPU: 16, Memory: 64 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB, PriceUSDPerHour: eight},
			{Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB, PriceUSDPerHour: sixteen},
		},
	}
}

// launched takes a reserved lease through assignment, binding and launching on
// one host, the way the listener and the node do between them.
func launched(t *testing.T, a *Allocator, lease *Lease, node string) {
	t.Helper()

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, node); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance(launching): %v", err)
	}
}

// THE PRICE ON THE ROW IS THE ONE IN FORCE WHEN THE SHAPE WAS CHARGED.
//
// node.ec2.instance_types is config a restart can change, and the shape
// comparison that guards re-registration deliberately ignores price, so a node
// may re-register with a new catalogue while a lease is open. A history that
// read the price at archive would reprice last month's jobs at today's rates,
// wrongly and invisibly. The catalogue is changed BEFORE the lease terminalizes,
// so an archive that consulted it would be caught.
func TestTheArchivedPriceIsTheOneInForceWhenTheShapeWasCharged(t *testing.T) {
	const before, after = config.USDPerHour(340_000), config.USDPerHour(999_000)

	a := pricedCloudAllocator(t, before)

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if lease.PriceUSDPerHour != before || lease.Site != "us-east" {
		t.Fatalf("reserved lease carries price %d at site %q, want %d at us-east",
			lease.PriceUSDPerHour, lease.Site, before)
	}

	launched(t, a, lease, "cloud-1")

	if _, err := a.RegisterNode(t.Context(), pricedCloudRegistration(after, 680_000)); err != nil {
		t.Fatalf("re-registering with a new price while the lease is open: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement: %v", err)
	}

	want := JobPlacement{
		Provider: config.ProviderEC2, InstanceType: "eight", VCPU: 8, Memory: 16 * config.GiB,
		Site: "us-east", PriceUSDPerHour: before,
	}
	if got != want {
		t.Fatalf("HistoryPlacement = %+v, want %+v: the row must record what was bought, not "+
			"what the catalogue charges now", got, want)
	}
}

// A FALLBACK IS A DIFFERENT PURCHASE AT A DIFFERENT RATE, and the row records
// the shape that was actually bought.
func TestAFallbackResizeRecordsTheFallbackShapesPrice(t *testing.T) {
	a := pricedCloudAllocator(t, 200_000)

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	launched(t, a, lease, "cloud-1")

	if err := a.Resize(t.Context(), lease.ID, lease.Epoch, "sixteen", 16, 32*config.GiB); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	live, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if live.PriceUSDPerHour != 680_000 {
		t.Fatalf("resized lease carries price %d, want the sixteen shape's 680000", live.PriceUSDPerHour)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement: %v", err)
	}
	if got.InstanceType != "sixteen" || got.VCPU != 16 || got.Memory != 32*config.GiB ||
		got.PriceUSDPerHour != 680_000 {
		t.Fatalf("HistoryPlacement = %+v, want the sixteen shape at 680000", got)
	}
}

// A HOST-BACKED LEASE BUYS NOTHING: no shape, no price, and the tier request is
// what it was charged. Its site is still recorded.
func TestAHostBackedLeaseArchivesItsSiteAndNoPrice(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier("small", 2, 4*config.GiB)})

	reg := testRegistration("home-1", config.ProviderFirecracker)
	reg.Site = "garage"
	if _, err := a.RegisterNode(t.Context(), reg); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	launched(t, a, lease, "home-1")

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement: %v", err)
	}

	want := JobPlacement{
		Provider: config.ProviderFirecracker, VCPU: 2, Memory: 4 * config.GiB, Site: "garage",
	}
	if got != want {
		t.Fatalf("HistoryPlacement = %+v, want %+v", got, want)
	}
}

// A REAPED LEASE KEEPS ITS PROVIDER. The reaper archives from its own
// projection, which did not select chosen_provider, so every lease that expired
// into quarantine and was then resolved was archived as having run on nothing.
func TestAReapedLeaseKeepsItsProviderInTheHistory(t *testing.T) {
	a := pricedCloudAllocator(t, 340_000)

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	launched(t, a, lease, "cloud-1")

	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("ExpireForTest: %v", err)
	}
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if err := a.ResolveQuarantine(t.Context(), lease.ID, PhaseDone); err != nil {
		t.Fatalf("ResolveQuarantine: %v", err)
	}

	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement: %v", err)
	}
	if got.Provider != config.ProviderEC2 || got.InstanceType != "eight" ||
		got.PriceUSDPerHour != 340_000 || got.Site != "us-east" {
		t.Fatalf("HistoryPlacement after a reap = %+v, want the ec2 host's eight shape at "+
			"340000 in us-east", got)
	}
}

// AN ESCROW THE REAPER FAILS OUTRIGHT ALSO KEEPS WHAT IT WAS CHARGED, from the
// reaper's own projection rather than a loaded lease.
func TestAnExpiredEscrowArchivesItsChargedShape(t *testing.T) {
	a := pricedCloudAllocator(t, 340_000)

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("ExpireForTest: %v", err)
	}
	if n, err := a.Reap(t.Context()); err != nil || n != 1 {
		t.Fatalf("Reap = %d, %v; want 1 reaped", n, err)
	}

	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement: %v", err)
	}
	if got.InstanceType != "eight" || got.PriceUSDPerHour != 340_000 || got.Site != "us-east" ||
		got.VCPU != 8 {
		t.Fatalf("HistoryPlacement after the reaper failed an escrow = %+v", got)
	}
}

func TestACacheObservationIsFencedAndValidated(t *testing.T) {
	a := pricedCloudAllocator(t, 340_000)

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	launched(t, a, lease, "cloud-1")

	warm := CacheObservation{ImageCache: ImageCacheWarm, CacheGeneration: "gen-7"}

	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch+1, warm); !errors.Is(err, ErrFenced) {
		t.Fatalf("a stale epoch was answered %v, want ErrFenced", err)
	}

	for name, bad := range map[string]CacheObservation{
		"nothing observed":        {},
		"unknown image token":     {ImageCache: "tepid"},
		"unknown actions token":   {ActionsCache: "maybe"},
		"generation without warm": {ImageCache: ImageCacheCold, CacheGeneration: "gen-7"},
		"warm without generation": {ImageCache: ImageCacheWarm},
	} {
		if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch, bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch, warm); err != nil {
		t.Fatalf("RecordCacheObservation: %v", err)
	}

	// ON THE HISTORY ROW BEFORE THE LEASE ENDS, like a disruption.
	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement while the lease is open: %v", err)
	}
	if got.ImageCache != ImageCacheWarm || got.CacheGeneration != "gen-7" {
		t.Fatalf("history before archive = %+v, want the warm observation already there", got)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch,
		CacheObservation{ActionsCache: ActionsCacheServed}); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("an observation on a finished lease was answered %v, want ErrLeaseNotFound", err)
	}
}

// THE FIRST OBSERVATION IS KEPT, on both rows, and nothing later erases it: not
// a different observation, not an archive from a caller that did not load the
// columns, not the reaper.
func TestTheFirstCacheObservationSurvivesEverythingAfterIt(t *testing.T) {
	a := pricedCloudAllocator(t, 340_000)

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	launched(t, a, lease, "cloud-1")

	first := CacheObservation{ImageCache: ImageCacheCold, ActionsCache: ActionsCacheDisabled}
	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch, first); err != nil {
		t.Fatalf("RecordCacheObservation: %v", err)
	}

	// A LATER, DIFFERENT OBSERVATION CHANGES NOTHING: the statement keeps the
	// first, and the call is not an error, because a node retrying a lost
	// response must not be told its report was refused.
	later := CacheObservation{
		ImageCache: ImageCacheWarm, CacheGeneration: "gen-9", ActionsCache: ActionsCacheServed,
	}
	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch, later); err != nil {
		t.Fatalf("a second observation was refused: %v", err)
	}

	live, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if live.ImageCache != ImageCacheCold || live.CacheGeneration != "" ||
		live.ActionsCache != ActionsCacheDisabled {
		t.Fatalf("the lease row moved to the later observation: %+v", live)
	}

	// AND THE HISTORY ROW, READ NOW rather than after the archive: the archive
	// copies the lease row's first observation back over the history, so a
	// history statement that overwrote would be masked by reading afterwards.
	early, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement while open: %v", err)
	}
	if early.ImageCache != ImageCacheCold || early.CacheGeneration != "" ||
		early.ActionsCache != ActionsCacheDisabled {
		t.Fatalf("the history row moved to the later observation: %+v", early)
	}

	// THE REAPER ARCHIVES FROM ITS OWN PROJECTION, then the archive from
	// quarantine loads the row again; neither may blank the observation.
	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("ExpireForTest: %v", err)
	}
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if err := a.ResolveQuarantine(t.Context(), lease.ID, PhaseDone); err != nil {
		t.Fatalf("ResolveQuarantine: %v", err)
	}

	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement: %v", err)
	}
	if got.ImageCache != ImageCacheCold || got.CacheGeneration != "" ||
		got.ActionsCache != ActionsCacheDisabled {
		t.Fatalf("history after the reap = %+v, want the first observation kept", got)
	}
}

// EACH HALF IS OBSERVED ON ITS OWN: the image store at boot, the Actions cache
// at the first request. A second call filling the other half is not a repeat.
func TestTheTwoHalvesOfACacheObservationArriveSeparately(t *testing.T) {
	a := pricedCloudAllocator(t, 340_000)

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	launched(t, a, lease, "cloud-1")

	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch,
		CacheObservation{ImageCache: ImageCacheWarm, CacheGeneration: "gen-2"}); err != nil {
		t.Fatalf("image observation: %v", err)
	}
	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch,
		CacheObservation{ActionsCache: ActionsCacheSpliced}); err != nil {
		t.Fatalf("actions observation: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement: %v", err)
	}
	if got.ImageCache != ImageCacheWarm || got.CacheGeneration != "gen-2" ||
		got.ActionsCache != ActionsCacheSpliced {
		t.Fatalf("history = %+v, want both halves", got)
	}
}

// THE FIRST-WINS GUARD IS IN THE STATEMENTS, and this is what pins it there.
//
// Both statements write three columns, each only while it is empty, and the
// generation is keyed on the image-cache column rather than its own so it can
// never be recorded beside a token it does not belong to. A guard that drifts on
// one statement is a lease and its history disagreeing about what the guest
// saw, which no test above can tell from a bug in the other statement.
func TestBothCacheObservationStatementsKeepTheFirst(t *testing.T) {
	t.Parallel()

	for _, query := range []string{"RecordLeaseCacheObservation", "RecordHistoryCacheObservation"} {
		body := namedQuery(t, "cacheobservation.sql", query)

		for _, guard := range []string{
			"image_cache      = CASE WHEN image_cache = '' THEN CAST(@image_cache AS TEXT)",
			"cache_generation = CASE WHEN image_cache = '' THEN CAST(@cache_generation AS TEXT)",
			"actions_cache    = CASE WHEN actions_cache = '' THEN CAST(@actions_cache AS TEXT)",
		} {
			if !strings.Contains(body, guard) {
				t.Errorf("%s does not carry the guard %q; the first observation would not be kept",
					query, guard)
			}
		}
	}

	if !strings.Contains(namedQuery(t, "cacheobservation.sql", "RecordLeaseCacheObservation"),
		"WHERE id = @id AND epoch = @epoch") {
		t.Error("RecordLeaseCacheObservation is not fenced on the epoch")
	}
}
