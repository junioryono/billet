package lease

import "testing"

// A PLANE BELOW THE WORD REFUSES THE WHOLE OBSERVATION OVER IT, so every cache's is
// taken back, and nothing else moves.
func TestWithoutBypassedTakesEveryCachesWordBack(t *testing.T) {
	t.Parallel()

	got := CacheObservation{
		ImageCache:   ImageCacheBypassed,
		ActionsCache: ActionsCacheBypassed,
		BuildCaches: BuildCaches{
			Sticky: BuildCacheBypassed, Git: BuildCacheBypassed, Bazel: BuildCacheBypassed,
			Go: BuildCacheBypassed, Npm: BuildCacheBypassed,
		},
	}.WithoutBypassed()
	if got.ImageCache != ImageCacheUnavailable || got.ActionsCache != ActionsCacheUnavailable {
		t.Errorf("image %q, actions %q", got.ImageCache, got.ActionsCache)
	}
	for name, cache := range got.each() {
		if cache != BuildCacheUnavailable {
			t.Errorf("%s = %q", name, cache)
		}
	}

	warm := CacheObservation{ImageCache: ImageCacheWarm, CacheGeneration: "g1", BuildCaches: BuildCaches{Go: BuildCacheWarm}}
	if kept := warm.WithoutBypassed(); kept != warm {
		t.Errorf("another word moved: %+v", kept)
	}
}
