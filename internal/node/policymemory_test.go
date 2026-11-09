package node

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// rememberingService is a cache service with one trusted-only session that names a
// repository, so every kill-switch question about it is asked of the plane.
func rememberingService(t *testing.T, plane *planeSwitch) (*CacheService, *fakeCacheStore, string) {
	t.Helper()

	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.SetCachePolicy(plane)
	credentials, err := service.PrepareScoped("billet-one", CacheSessionScope{
		Trust: provider.TrustTrusted, Owner: "acme", Repository: "api",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service, storage, credentials.Token
}

// A STICKY DISK ATTACHED BY A REMEMBERED ANSWER IS NOT PUBLISHED BY ONE: its
// commit asks the plane, keeps the clone while the plane cannot answer, and
// publishes once it can.
func TestAStickyCommitIsNotPublishedByARememberedAnswer(t *testing.T) {
	t.Parallel()

	plane := &planeSwitch{}
	service, storage, token := rememberingService(t, plane)
	service.servingPolicy(t.Context(), config.CacheSticky, "acme", "api")
	plane.mode.Store(switchUnreachable)

	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(1 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach with the plane down inside the age = %d: %s", attached.Code, attached.Body.String())
	}
	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK || !bytes.Contains(committed.Body.Bytes(), []byte(`"published":false`)) ||
		!bytes.Contains(committed.Body.Bytes(), []byte("kill switch")) {
		t.Fatalf("commit with the plane down = %d %s, want unpublished by the kill switch",
			committed.Code, committed.Body.String())
	}
	if storage.published != 0 {
		t.Fatalf("published %d time(s) by a remembered answer", storage.published)
	}

	plane.mode.Store(switchAllows)
	if again := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil); !bytes.Contains(again.Body.Bytes(),
		[]byte(`"published":true`)) {
		t.Fatalf("commit with the plane back = %d %s, want published", again.Code, again.Body.String())
	}
}

// settleDocker settles the session's Docker image store as the runner does at
// a job's completion, with the guest proving it quiesced, and returns what the
// settlement answered.
func settleDocker(t *testing.T, service *CacheService, token string) error {
	t.Helper()

	proof := map[string]any{"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true}}
	settled := make(chan error, 1)
	go func() { settled <- service.SettleDocker(t.Context(), "billet-one", true) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := cacheRequest(t, service, token, "/v1/docker-store/ready", proof)
		if ready.Code == http.StatusOK {
			break
		}
		if ready.Code != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("Docker store readiness status = %d: %s", ready.Code, ready.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	return <-settled
}

// A DOCKER IMAGE STORE ATTACHED BY A REMEMBERED ANSWER IS NOT PUBLISHED BY ONE,
// and one the plane allows at its settlement is.
func TestADockerStoreIsNotPublishedByARememberedAnswer(t *testing.T) {
	t.Parallel()

	for name, down := range map[string]bool{"plane down": true, "plane answering": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			plane := &planeSwitch{}
			service, storage, token := rememberingService(t, plane)
			service.servingPolicy(t.Context(), config.CacheDocker, "acme", "api")
			if down {
				plane.mode.Store(switchUnreachable)
			}
			if response := cacheRequest(t, service, token, "/v1/docker-store",
				map[string]any{"architecture": "amd64"}); response.Code != http.StatusCreated {
				t.Fatalf("Docker store = %d: %s", response.Code, response.Body.String())
			}
			asked := plane.asked.Load()
			err := settleDocker(t, service, token)
			if plane.asked.Load() == asked {
				t.Fatal("the settlement never asked the plane")
			}
			if down {
				if !errors.Is(err, errPublicationRefused) || storage.published != 0 {
					t.Fatalf("settlement with the plane down = %v, published %d; want refused by the kill switch",
						err, storage.published)
				}

				return
			}
			if err != nil || storage.published != 1 {
				t.Fatalf("settlement with the plane answering = %v, published %d; want published once",
					err, storage.published)
			}
		})
	}
}

// A BLOCK SET WHILE A STICKY COMMIT WAITS FOR ITS WRITER STOPS IT: the commit
// asks again immediately before the pointer moves, gives the writer back and
// publishes nothing.
func TestAStickyCommitAsksAgainBeforeThePointerMoves(t *testing.T) {
	t.Parallel()

	var asks atomic.Int32
	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	// THE ATTACH AND THE COMMIT'S FIRST ASK ARE ALLOWED; THE ASK AFTER THE WRITER IS REFUSED.
	service.SetCachePolicy(cachePolicyFunc(func(context.Context, config.CacheKind, string, string) (bool, error) {
		return asks.Add(1) <= 2, nil
	}))
	credentials, err := service.PrepareScoped("billet-one", CacheSessionScope{
		Trust: provider.TrustTrusted, Owner: "acme", Repository: "api",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	if attached := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(1 << 30),
	}); attached.Code != http.StatusCreated {
		t.Fatalf("attach = %d: %s", attached.Code, attached.Body.String())
	}
	committed := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/commit", nil)
	if !bytes.Contains(committed.Body.Bytes(), []byte(`"published":false`)) ||
		!bytes.Contains(committed.Body.Bytes(), []byte("kill switch")) {
		t.Fatalf("commit refused after its writer = %d %s, want unpublished by the kill switch",
			committed.Code, committed.Body.String())
	}
	if asks.Load() != 3 || storage.published != 0 || len(storage.released) != 1 {
		t.Fatalf("asks=%d published=%d released=%v; want the third ask to stop it and the writer given back",
			asks.Load(), storage.published, storage.released)
	}
}

// AN OBJECT-BACKED ACTIONS FINALIZE IS NOT PUBLISHED BY A REMEMBERED ANSWER,
// the first finalize or a retry, and is once the plane answers.
func TestAnObjectFinalizeIsNotPublishedByARememberedAnswer(t *testing.T) {
	t.Parallel()

	service, storage, _ := objectService(t)
	plane := &planeSwitch{}
	service.SetActionsPolicy(plane)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "remembered", []string{"body"}, []string{"a"})
	plane.mode.Store(switchUnreachable)

	finalize := func() (*http.Response, bool, error) {
		request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
			`{"key":"remembered","version":"v1","size_bytes":"4"}`)

		return service.actionsResponse(request, session)
	}
	for _, attempt := range []string{"first", "retried"} {
		response, _, err := finalize()
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, errActionsPublicationUnknown) {
			t.Fatalf("the %s finalize with the plane down answered %v, want the kill switch unread", attempt, err)
		}
		storage.mu.Lock()
		published := len(storage.archives)
		storage.mu.Unlock()
		if published != 0 {
			t.Fatalf("the %s finalize published %d archive(s) by a remembered answer", attempt, published)
		}
	}

	plane.mode.Store(switchAllows)
	response, handled, err := finalize()
	if err != nil || !handled {
		t.Fatalf("the finalize with the plane back handled=%t error=%v", handled, err)
	}
	if got := responseJSON(t, response); got["ok"] != true {
		t.Fatalf("the finalize with the plane back = %v", got)
	}
}

// A BLOCK SET AFTER AN ACTIONS PUBLICATION'S FIRST ATTEMPT CONFLICTED STOPS ITS
// SECOND: the switch is asked again before each attempt moves the pointer.
func TestAnActionsPublicationAsksAgainBeforeItsSecondAttempt(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	uploadURL, _ := reserveActionsUpload(t, service, session, "second")
	upload := actionsBlockRequest(t, uploadURL, "", "", bytes.NewBufferString("body"))
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, handled := service.actionsProxy().respond(upload, session)
	if !handled || response.StatusCode != http.StatusCreated {
		t.Fatalf("upload handled=%t status=%v", handled, response)
	}
	closeActionsResponse(t, response)
	// ALLOWED UNTIL THE FIRST ATTEMPT HAS BEEN MADE, which conflicts over an
	// absent pointer, so the publication reacquires its writer and would try again.
	storage.publishConflicts = 1
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return storage.published == 0, nil
	}))

	finalize := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"second","version":"v2","size_bytes":"4"}`)
	response, _ = service.actionsProxy().respond(finalize, session)
	closeActionsResponse(t, response)
	if storage.published != 1 || storage.current != "" {
		t.Fatalf("attempts=%d current=%q; want the second attempt stopped by the block", storage.published,
			storage.current)
	}
}

// A BLOCK SET AFTER A LAST-WRITE-WINS PUBLICATION'S FIRST ATTEMPT STOPS ITS
// REBASE: the switch is asked again before the pointer moves a second time.
func TestALastWriteWinsRebaseAsksAgain(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{current: "baseline", advanceOnSnapshot: true}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.SetCachePolicy(cachePolicyFunc(func(context.Context, config.CacheKind, string, string) (bool, error) {
		return storage.published == 0, nil
	}))
	credentials, err := service.PrepareScoped("billet-one", CacheSessionScope{
		Trust: provider.TrustTrusted, Owner: "acme", Repository: "api",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	if attached := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
		"key": "acme/api/buildkit", "size_bytes": int64(10 << 30), "publication": publicationLWW,
	}); attached.Code != http.StatusCreated {
		t.Fatalf("attach = %d: %s", attached.Code, attached.Body.String())
	}
	committed := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/commit", nil)
	if !bytes.Contains(committed.Body.Bytes(), []byte("kill switch")) {
		t.Fatalf("commit = %d %s, want stopped by the kill switch before its rebase",
			committed.Code, committed.Body.String())
	}
	if got := storage.publishExpected; len(got) != 1 {
		t.Fatalf("publication attempts expected %v, want only the first", got)
	}
}

// AN ACTIONS PUBLICATION TELLS A REFUSAL FROM A SWITCH IT COULD NOT READ, even
// with an allowed answer remembered: the first is the operator's choice, the
// second could not be told, and neither publishes.
func TestAnActionsPublicationTellsARefusalFromAnUnreadSwitch(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mode int32
		want error
	}{
		"refused":    {switchRefuses, errActionsPublicationDisabled},
		"unreadable": {switchUnreachable, errActionsPublicationUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, _, session, _ := testActionsService(t)
			plane := &planeSwitch{}
			service.SetActionsPolicy(plane)
			if err := service.actionsPublicationErr(t.Context(), session); err != nil {
				t.Fatalf("actionsPublicationErr with the plane allowing = %v", err)
			}
			plane.mode.Store(tc.mode)
			if err := service.actionsPublicationErr(t.Context(), session); !errors.Is(err, tc.want) {
				t.Fatalf("actionsPublicationErr = %v, want %v", err, tc.want)
			}
		})
	}
}

// AN OBJECT-BACKED GO CACHE ATTACHED BY A REMEMBERED ANSWER COUNTS ITS THIRTY
// SECONDS FROM THAT ANSWER: attached a second before the age, it asks again at
// the age rather than thirty seconds after it.
func TestAnObjectCacheWindowEndsWithTheRememberedAnswer(t *testing.T) {
	t.Parallel()

	service, _, _ := objectService(t)
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Now()
	service.now = func() time.Time { return now }
	token := scopedObjectCAS(t, service)
	answered := now
	service.servingPolicy(t.Context(), config.CacheGo, "acme", "api")
	plane.mode.Store(switchUnreachable)

	now = answered.Add(policyRememberedFor - time.Second)
	if code := putObject(t, service, token, "a second before the age"); code != http.StatusOK {
		t.Fatalf("PUT attaching a second before the age = %d, want 200", code)
	}
	now = answered.Add(policyRememberedFor)
	if code := putObject(t, service, token, "at the age"); code == http.StatusOK {
		t.Fatalf("PUT at the age = %d, want refused", code)
	}
}

// AN UNPUBLISHED RECEIPT'S WRITER IS GIVEN BACK WHEN THE SESSION ENDS: a
// finalize whose publication failed keeps it for a retry, the retry is refused
// with the plane down, and the job ends; left alone, the writer would hold the
// key from every other job until its lease ran out.
func TestAnUnpublishedReceiptsWriterIsGivenBackAtCleanup(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	plane := &planeSwitch{}
	service.SetActionsPolicy(plane)
	uploadURL, _ := reserveActionsUpload(t, service, session, "kept")
	upload := actionsBlockRequest(t, uploadURL, "", "", bytes.NewBufferString("body"))
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, handled := service.actionsProxy().respond(upload, session)
	if !handled || response.StatusCode != http.StatusCreated {
		t.Fatalf("upload handled=%t status=%v", handled, response)
	}
	closeActionsResponse(t, response)

	storage.publishErr = errors.New("publication unavailable")
	finalizeBody := `{"key":"kept","version":"v2","size_bytes":"4"}`
	for _, down := range []bool{false, true} {
		if down {
			plane.mode.Store(switchUnreachable)
		}
		finalize := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
			finalizeBody)
		response, _ = service.actionsProxy().respond(finalize, session)
		closeActionsResponse(t, response)
	}
	// ONE ATTEMPT, THE FIRST, which the store failed; the refused retry made none.
	if storage.published != 1 || storage.current != "" || len(storage.released) != 0 {
		t.Fatalf("before cleanup attempts=%d current=%q released=%v; want one failed attempt and the receipt "+
			"holding its writer", storage.published, storage.current, storage.released)
	}

	if err := service.Cleanup(t.Context(), session.instance); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(storage.released) != 1 {
		t.Fatalf("released %v at cleanup, want the receipt's writer given back", storage.released)
	}
}

// A FINALIZE THE KILL SWITCH REFUSES IS NOT RECORDED AS BILLET'S FAILURE: it is
// the operator's choice, so `unavailable`, which outlasts every other
// observation, is never written (the reservation's `served`, being first, is
// kept).
func TestARefusedFinalizeIsNotRecordedAsAFailure(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	uploadURL, _ := reserveActionsUpload(t, service, session, "refused")
	upload := actionsBlockRequest(t, uploadURL, "", "", bytes.NewBufferString("body"))
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, handled := service.actionsProxy().respond(upload, session)
	if !handled || response.StatusCode != http.StatusCreated {
		t.Fatalf("upload handled=%t status=%v", handled, response)
	}
	closeActionsResponse(t, response)
	// THE FINALIZE'S SERVING ASK IS ALLOWED AND ITS PUBLICATION'S REFUSED, as when
	// a block lands between the two: a switch refusing both is answered by the
	// serving check before any publication is decided.
	var asked, refuseFrom atomic.Int32
	refuseFrom.Store(1 << 30)
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return asked.Add(1) < refuseFrom.Load(), nil
	}))
	refuseFrom.Store(asked.Load() + 2)

	finalize := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"refused","version":"v2","size_bytes":"4"}`)
	response, handled = service.actionsProxy().respond(finalize, session)
	if !handled || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("a refused finalize handled=%t status=%v; want a local failure", handled, response)
	}
	closeActionsResponse(t, response)
	if asked.Load() < refuseFrom.Load() {
		t.Fatalf("the switch was asked %d times; the publication's own ask (number %d) never came",
			asked.Load(), refuseFrom.Load())
	}
	told := observedActions(observer)
	if slices.Contains(told, alloc.ActionsCacheUnavailable) || session.actionsFailed.Load() {
		t.Fatalf("observer was told %v (failed %t), want the refusal never recorded as unavailable",
			told, session.actionsFailed.Load())
	}
}

// AN ACTIONS CACHE ENTRY RESERVED AND UPLOADED BY A REMEMBERED ANSWER IS NOT
// PUBLISHED BY ONE: finalize asks the plane, fails locally while it cannot
// answer, and publishes on a retry once it can.
func TestAnActionsFinalizeIsNotPublishedByARememberedAnswer(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	plane := &planeSwitch{}
	service.SetActionsPolicy(plane)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"remembered","version":"v1"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}
	plane.mode.Store(switchUnreachable)
	upload := actionsRequestForTest(t, http.MethodPut, uploadURL, "archive-body")
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, _, err = service.actionsResponse(upload, session)
	if err != nil {
		t.Fatalf("upload with the plane down inside the age: %v", err)
	}
	response.Body.Close()

	finalizeBody := `{"key":"remembered","version":"v1","size_bytes":"12"}`
	finalize := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath, finalizeBody)
	response, handled := service.actions.respond(finalize, session)
	if !handled {
		t.Fatal("a reserved finalize was passed to GitHub")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || storage.published != 0 || storage.current != "" {
		t.Fatalf("finalize with the plane down: status=%d published=%d current=%q; want a local failure and no pointer",
			response.StatusCode, storage.published, storage.current)
	}

	plane.mode.Store(switchAllows)
	retry := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath, finalizeBody)
	response, _ = service.actions.respond(retry, session)
	defer response.Body.Close()
	if retried := responseJSON(t, response); retried["ok"] != true || storage.current != "next" {
		t.Fatalf("finalize with the plane back = %v current=%q, want published", retried, storage.current)
	}
}

// planeSwitch answers the kill switch as a plane does: allowing, refusing, or
// not answering at all, as a restarting controller does.
type planeSwitch struct {
	mode  atomic.Int32
	asked atomic.Int32
}

const (
	switchAllows int32 = iota
	switchRefuses
	switchUnreachable
)

var errPlaneUnreachable = errors.New("dial tcp 10.3.1.117:7717: connect: no route to host")

func (p *planeSwitch) CacheAllowed(context.Context, config.CacheKind, string, string) (bool, error) {
	p.asked.Add(1)
	switch p.mode.Load() {
	case switchAllows:
		return true, nil
	case switchRefuses:
		return false, nil
	default:
		return false, errPlaneUnreachable
	}
}

func (p *planeSwitch) ActionsCacheAllowed(ctx context.Context, owner, repository string) (bool, error) {
	return p.CacheAllowed(ctx, config.CacheActions, owner, repository)
}

// noteAt records an answer about the npm cache to an ask begun at at, as
// askKindPolicy does.
func noteAt(memory *policyMemory, owner, repository string, allowed bool, at time.Time) {
	memory.note(config.CacheNpm, owner, repository, allowed, memory.begin(at))
}

func TestPolicyMemoryKeepsAnAllowedAnswerForItsAgeOnly(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	noteAt(&memory, "Acme", "API", true, start)

	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(policyRememberedFor-time.Second)); !ok {
		t.Fatal("an allowed answer younger than its age does not stand (names folded)")
	}
	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(policyRememberedFor)); ok {
		t.Fatal("an allowed answer as old as its age still stands")
	}
	if _, ok := memory.standing(config.CacheGo, "acme", "api", start); ok {
		t.Fatal("an answer about npm stands for the Go cache")
	}
	if _, ok := memory.standing(config.CacheNpm, "acme", "web", start); ok {
		t.Fatal("an answer about one repository stands for another")
	}
	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(-time.Second)); ok {
		t.Fatal("an answer from a clock now behind it stands")
	}
}

func TestPolicyMemoryForgetsAnAllowedAnswerWhenTheSwitchRefuses(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	noteAt(&memory, "acme", "api", true, start)
	noteAt(&memory, "ACME", "api", false, start.Add(time.Second))

	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(2*time.Second)); ok {
		t.Fatal("an allowed answer outlived a later refusal")
	}
}

// AN ANSWER THAT LEFT THE PLANE BEFORE A BLOCK REACHED IT, and arrives after a
// refusal was noted, is not kept: two asks race, the one begun first answers
// last.
func TestPolicyMemoryKeepsNoAllowedAnswerThatRacedARefusal(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	early := memory.begin(start)
	noteAt(&memory, "acme", "api", false, start.Add(time.Second))
	memory.note(config.CacheNpm, "acme", "api", true, early)

	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(2*time.Second)); ok {
		t.Fatal("an allowed answer begun before a refusal was noted outlived it")
	}

	noteAt(&memory, "acme", "api", true, start.Add(3*time.Second))
	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(4*time.Second)); !ok {
		t.Fatal("an allowed answer begun after the refusal is not kept")
	}
}

// AN ANSWER IS DATED WHEN IT WAS ASKED, so a slow reply does not stand longer.
func TestPolicyMemoryDatesAnAnswerWhenItWasAsked(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	noteAt(&memory, "acme", "api", true, start)

	answered, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(time.Minute))
	if !ok || !answered.Equal(start) {
		t.Fatalf("the remembered answer is dated %s (standing %t), want %s", answered, ok, start)
	}
}

// A SESSION'S OWN WINDOW ENDS WHERE A REMEMBERED ANSWER STOPS STANDING.
func TestASessionWindowNeverOutlivesTheRememberedAnswer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	if got := allowedAt(now, now); !got.Equal(now) {
		t.Fatalf("a live answer's window starts at %s, want now", got)
	}
	answered := now.Add(-policyRememberedFor + time.Second)
	if got := allowedAt(now, answered); !got.Add(casPolicyAge).Equal(answered.Add(policyRememberedFor)) {
		t.Fatalf("a window from a remembered answer one second from its age ends at %s, want %s",
			got.Add(casPolicyAge), answered.Add(policyRememberedFor))
	}
}

func TestPolicyMemoryDropsAnswersPastTheirAge(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	noteAt(&memory, "acme", "old", true, start)
	noteAt(&memory, "acme", "new", true, start.Add(policyRememberedFor))

	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, kept := memory.allowed[newPolicyKey(config.CacheNpm, "acme", "old")]; kept {
		t.Fatal("an answer past its age is still held: the memory grows with every repository ever asked")
	}
}

// SERVING A JOB OUTLIVES A CONTROLLER RESTART; a publication does not. The
// plane allows, then stops answering: serving goes on by the last answer until
// it is policyRememberedFor old, and a publication's ask is could-not-tell
// throughout.
func TestServingStandsOnTheLastAllowedAnswerWhileThePlaneIsDown(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := t.Context()

	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyAllowed {
		t.Fatalf("serving while the plane allows = %v", got)
	}
	plane.mode.Store(switchUnreachable)
	now = now.Add(policyRememberedFor - time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyAllowed {
		t.Fatalf("serving with the plane down inside the age = %v, want allowed", got)
	}
	if got := service.kindPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyUnknown {
		t.Fatalf("a publication's ask with the plane down = %v, want could-not-tell", got)
	}
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "web"); got != policyUnknown {
		t.Fatalf("serving another repository the plane never allowed = %v, want could-not-tell", got)
	}
	now = now.Add(time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyUnknown {
		t.Fatalf("serving with the plane down past the age = %v, want could-not-tell", got)
	}
	if plane.asked.Load() != 5 {
		t.Fatalf("the plane was asked %d times, want every call to ask it first", plane.asked.Load())
	}
}

// A BLOCK THAT HAS REACHED THE NODE IS NEVER OUTLIVED by an older "allowed".
func TestServingDoesNotOutliveARefusalThePlaneGave(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := t.Context()

	service.servingPolicy(ctx, config.CacheNpm, "acme", "api")
	plane.mode.Store(switchRefuses)
	now = now.Add(time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyRefused {
		t.Fatalf("serving while the plane refuses = %v", got)
	}
	plane.mode.Store(switchUnreachable)
	now = now.Add(time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyUnknown {
		t.Fatalf("serving with the plane down after it refused = %v, want could-not-tell", got)
	}
}

// A PUBLICATION'S OWN ALLOWED ANSWER IS REMEMBERED TOO: the memory is of what
// the plane said, whoever asked it.
func TestAPublicationsAllowedAnswerStandsForServing(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := t.Context()

	if got := service.kindPolicy(ctx, config.CacheGo, "acme", "api"); got != policyAllowed {
		t.Fatalf("a publication's ask while the plane allows = %v", got)
	}
	plane.mode.Store(switchUnreachable)
	if got := service.servingPolicy(ctx, config.CacheGo, "acme", "api"); got != policyAllowed {
		t.Fatalf("serving after a publication's allowed answer, plane down = %v", got)
	}
}

// THROUGH A REAL REQUEST: a running Go cache's next write after casPolicyAge
// asks the switch again, and with the plane down it is served by the last
// "allowed" until that is too old, then refused.
func TestACacheInUseIsServedThroughAControllerRestart(t *testing.T) {
	t.Parallel()

	spec := goBazelCache()
	spec.Owner, spec.Repository = "acme", "api"
	service, _, token, _ := casService(t, provider.TrustTrusted, spec, &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Now()
	service.now = func() time.Time { return now }

	if code := putObject(t, service, token, "before the restart"); code != http.StatusOK {
		t.Fatalf("PUT with the plane answering = %d", code)
	}
	answered := now
	plane.mode.Store(switchUnreachable)
	now = now.Add(casPolicyAge)
	asked := plane.asked.Load()
	if code := putObject(t, service, token, "during the restart"); code != http.StatusOK {
		t.Fatalf("PUT with the plane down inside the age = %d, want 200", code)
	}
	if plane.asked.Load() == asked {
		t.Fatal("the write was not asked of the switch again, so the memory decided nothing")
	}
	now = answered.Add(policyRememberedFor - time.Second)
	if code := putObject(t, service, token, "a second before the age"); code != http.StatusOK {
		t.Fatalf("PUT with the plane down a second inside the age = %d, want 200", code)
	}
	// THE SESSION'S OWN THIRTY SECONDS DO NOT CARRY IT PAST THE AGE.
	now = answered.Add(policyRememberedFor)
	if code := putObject(t, service, token, "at the age"); code == http.StatusOK {
		t.Fatalf("PUT with the plane down at the age = %d, want refused", code)
	}
}

// THE ACTIONS CACHE OF A TRUSTED-ONLY SESSION reads the switch through its own
// reader, and keeps the same memory.
func TestTheActionsCacheKeepsItsLastAllowedAnswer(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetActionsPolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	session := &cacheSession{trust: provider.TrustTrusted, owner: "acme", repository: "api",
		workflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"}

	if _, disposition := service.actionsKeysFor(t.Context(), session); disposition != "" {
		t.Fatalf("with the plane allowing, disposition = %q, want served", disposition)
	}
	plane.mode.Store(switchUnreachable)
	now = now.Add(policyRememberedFor - time.Second)
	if _, disposition := service.actionsKeysFor(t.Context(), session); disposition != "" {
		t.Fatalf("with the plane down inside the age, disposition = %q, want served", disposition)
	}
	now = now.Add(time.Second)
	keys, disposition := service.actionsKeysFor(t.Context(), session)
	if disposition != "spliced" || keys.write != nil {
		t.Fatalf("with the plane down past the age, disposition = %q, want spliced", disposition)
	}
	plane.mode.Store(switchRefuses)
	if _, disposition := service.actionsKeysFor(t.Context(), session); disposition != "disabled" {
		t.Fatalf("with the plane refusing, disposition = %q, want disabled", disposition)
	}
}
