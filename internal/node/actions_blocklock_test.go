package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// stalledBody is a block's body the guest is still sending: its first read
// says so and then waits until released.
type stalledBody struct {
	reading chan struct{}
	release chan struct{}
	sent    bool
}

func (b *stalledBody) Read(buffer []byte) (int, error) {
	if !b.sent {
		b.sent = true
		close(b.reading)
		<-b.release

		return copy(buffer, "one"), nil
	}

	return 0, io.EOF
}

// cutBlockBody is a block's body whose sender went away partway.
type cutBlockBody struct{ sent bool }

func (b *cutBlockBody) Read(buffer []byte) (int, error) {
	if !b.sent {
		b.sent = true

		return copy(buffer, "part"), nil
	}

	return 0, io.ErrUnexpectedEOF
}

func actionsBlockRequest(t *testing.T, uploadURL, comp, blockID string, body io.Reader) *http.Request {
	t.Helper()

	parsed, err := url.Parse(uploadURL)
	if err != nil {
		t.Fatalf("parse upload URL: %v", err)
	}
	query := parsed.Query()
	query.Set("comp", comp)
	if blockID != "" {
		query.Set("blockid", blockID)
	}
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, parsed.String(), body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	return request
}

// reserveActionsUpload reserves a save through the proxy, as the toolkit does,
// and returns its signed upload URL and the session's archive for it.
func reserveActionsUpload(
	t *testing.T,
	service *CacheService,
	session *cacheSession,
	key string,
) (string, *actionsArchive) {
	t.Helper()

	proxy := service.actionsProxy()
	if proxy == nil {
		t.Fatal("the intercepting session has no proxy")
	}
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"`+key+`","version":"v2"}`)
	response, handled := proxy.respond(create, session)
	if !handled || response.StatusCode != http.StatusOK {
		t.Fatalf("CreateCacheEntry handled=%t status=%v", handled, response)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	for _, archive := range session.actions {
		if archive.CacheKey == key {
			return uploadURL, archive
		}
	}
	t.Fatalf("no archive was reserved for %q", key)

	return "", nil
}

// awaitCondition waits for done, failing the test after ten seconds.
func awaitCondition(t *testing.T, what string, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(time.Millisecond)
	}
}

type blobResult struct {
	status int
	err    error
}

// serveInBackground answers request through the blob handler on a goroutine of
// its own, joined before the test ends.
func serveInBackground(t *testing.T, service *CacheService, session *cacheSession, request *http.Request) <-chan blobResult {
	t.Helper()

	done := make(chan blobResult, 1)
	go func() {
		response, err := service.serveActionsBlob(request, session)
		result := blobResult{err: err}
		if response != nil {
			result.status = response.StatusCode
			response.Body.Close()
		}
		done <- result
	}()

	return done
}

func observedActions(observer *recordingObserver) []alloc.ActionsCache {
	var told []alloc.ActionsCache
	calls := observer.recorded()
	for index := range calls {
		told = append(told, calls[index].obs.ActionsCache)
	}

	return told
}

// A BLOCK IN TRANSFER HOLDS NEITHER THE SESSION NOR THE OTHER BLOCKS (#694). The
// client uploads an archive's blocks side by side; each held the archive alone
// for its whole transfer, and the one waiting behind it held the session's lock
// meanwhile, which every new CONNECT of the job's waited on until the guest's
// relay gave up and sent the next block to GitHub.
func TestABlockInTransferHoldsNeitherTheSessionNorTheOtherBlocks(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	uploadURL, archive := reserveActionsUpload(t, service, session, "blocks")

	first := base64.StdEncoding.EncodeToString([]byte("block-1"))
	second := base64.StdEncoding.EncodeToString([]byte("block-2"))
	stalled := &stalledBody{reading: make(chan struct{}), release: make(chan struct{})}
	list := `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>` + first +
		`</Latest><Latest>` + second + `</Latest></BlockList>`
	commitRequest := actionsBlockRequest(t, uploadURL, "blocklist", "", bytes.NewBufferString(list))
	firstDone := serveInBackground(t, service, session, actionsBlockRequest(t, uploadURL, "block", first, stalled))
	released := false
	defer func() {
		if !released {
			close(stalled.release)
			<-firstDone
		}
	}()
	select {
	case <-stalled.reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the first block's body was never read")
	}

	// A SECOND BLOCK IS WRITTEN while the first is still arriving.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response, err := service.serveActionsBlob(
		actionsBlockRequest(t, uploadURL, "block", second, bytes.NewBufferString("two")).WithContext(ctx),
		session)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("a block behind a block in transfer: status=%v error=%v", response, err)
	}
	response.Body.Close()

	// THE COMMIT WAITS FOR THE ARCHIVE ALONE, and the session's lock, which a
	// CONNECT, an observation and a renewal wait on, is free while it does.
	commitDone := serveInBackground(t, service, session, commitRequest)
	committed := false
	defer func() {
		if !committed {
			if !released {
				close(stalled.release)
				released = true
				<-firstDone
			}
			<-commitDone
		}
	}()
	awaitCondition(t, "the commit waiting for the archive", func() bool {
		return archive.exclusiveWaiting.Load() == 1
	})
	lockCtx, lockCancel := context.WithTimeout(t.Context(), time.Second)
	err = lockCacheSession(lockCtx, session)
	lockCancel()
	if err != nil {
		t.Fatalf("the session's lock was held while the commit waited for a block: %v", err)
	}
	session.mu.Unlock()
	select {
	case result := <-commitDone:
		committed = true
		t.Fatalf("the block list was committed while a block was still arriving: %+v", result)
	default:
	}

	close(stalled.release)
	released = true
	if result := <-firstDone; result.err != nil || result.status != http.StatusCreated {
		t.Fatalf("the first block: %+v", result)
	}
	result := <-commitDone
	committed = true
	if result.err != nil || result.status != http.StatusCreated {
		t.Fatalf("the block list: %+v", result)
	}
	body, err := os.ReadFile(service.actionsArchivePath(session, archive))
	if err != nil || string(body) != "onetwo" {
		t.Fatalf("assembled archive = %q, error=%v", body, err)
	}
}

// A WAITING COMMIT ADMITS NO NEW BLOCK, so blocks arriving one after another
// cannot keep it out until its request expires: a lock taken by polling
// announces no waiting writer of its own.
func TestAWaitingCommitAdmitsNoNewBlock(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	uploadURL, archive := reserveActionsUpload(t, service, session, "starve")

	archive.mu.RLock()
	held := true
	defer func() {
		if held {
			archive.mu.RUnlock()
		}
	}()
	commitDone := serveInBackground(t, service, session, actionsBlockRequest(t, uploadURL, "blocklist", "",
		bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>YQ==</Latest></BlockList>`)))
	awaitCondition(t, "the commit waiting for the archive", func() bool {
		return archive.exclusiveWaiting.Load() == 1
	})

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := archive.lockShared(ctx); err == nil {
		archive.mu.RUnlock()
		t.Fatal("a block was let in beside a block in transfer while a commit waited")
	}

	archive.mu.RUnlock()
	held = false
	if result := <-commitDone; result.err == nil && result.status == 0 {
		t.Fatalf("the commit was never answered: %+v", result)
	}
}

// A BLOB REQUEST LET IN AFTER ITS SESSION CLOSED FINDS THE ARCHIVE SHUT: it no
// longer holds the session's lock while it waits for the archive, so the close
// comes in between, which is staged here.
func TestABlobRequestAfterItsSessionClosedIsRefused(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	uploadURL, archive := reserveActionsUpload(t, service, session, "closing")

	archive.mu.Lock()
	held := true
	defer func() {
		if held {
			archive.mu.Unlock()
		}
	}()
	done := serveInBackground(t, service, session,
		actionsBlockRequest(t, uploadURL, "block", "YQ==", bytes.NewBufferString("a")))
	awaitCondition(t, "the block waiting for the archive", func() bool {
		return archive.sharedWaiting.Load() == 1
	})
	session.closing.Store(true)
	archive.mu.Unlock()
	held = false

	if result := <-done; result.err != nil || result.status != http.StatusForbidden {
		t.Fatalf("a block let in after its session closed: %+v; want 403", result)
	}
}

// THE CONNECT IS ANSWERED WHILE THE SESSION IS BUSY (#694), and refused once it
// is closing. The guest's relay sends a tunnel it waited ten seconds for to
// GitHub instead, which knows nothing of the blob URLs billet signs.
func TestActionsProxyAuthenticationAnswersWhileTheSessionIsBusy(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodConnect,
		"http://"+actionsResultsHost+":443", http.NoBody)
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(actionsProxyUser+":"+session.token)))

	type answer struct {
		session *cacheSession
		ok      bool
	}
	session.mu.Lock()
	answered := make(chan answer, 1)
	go func() {
		got, ok := service.authenticateProxy(request)
		answered <- answer{got, ok}
	}()
	var got answer
	select {
	case got = <-answered:
		session.mu.Unlock()
	case <-time.After(5 * time.Second):
		session.mu.Unlock()
		<-answered
		t.Fatal("a CONNECT for a busy session waited on the session's lock")
	}
	if !got.ok || got.session != session {
		t.Fatalf("a CONNECT for a busy session was refused (ok=%t)", got.ok)
	}

	session.closing.Store(true)
	if _, ok := service.authenticateProxy(request); ok {
		t.Fatal("a CONNECT for a closing session was admitted")
	}
}

// A LOCAL FAILURE REPLACES AN EARLIER OUTCOME, and nothing replaces it (#694): a
// job whose restore was served and whose save then failed on the node was
// recorded as served. The replacement is reported, once.
func TestAnActionsFailureReplacesAnEarlierServedOutcome(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)

	service.observeActions(t.Context(), session, alloc.ActionsCacheServed)
	service.observeActions(t.Context(), session, alloc.ActionsCacheUnavailable)
	service.observeActions(t.Context(), session, alloc.ActionsCacheUnavailable)
	service.observeActions(t.Context(), session, alloc.ActionsCacheServed)
	service.observeActions(t.Context(), session, alloc.ActionsCacheSpliced)

	record := sessionRecord(t, service, session.token)
	if record.Observed.ActionsCache != string(alloc.ActionsCacheUnavailable) || !record.Observed.Reported {
		t.Fatalf("session record = %+v, want the failure kept and reported", record.Observed)
	}
	if told := observedActions(observer); len(told) != 2 || told[0] != alloc.ActionsCacheServed ||
		told[1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want served and then unavailable, each once", told)
	}
}

// A FAILURE OBSERVED WHILE THE SESSION'S LOCK IS BUSY IS NOT LOST: a renewal
// holds it across storage for longer than an observation waits, and the job
// was then settled with its earlier served.
func TestAnActionsFailureObservedUnderABusyLockIsSettled(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	service.observeActions(t.Context(), session, alloc.ActionsCacheServed)

	session.mu.Lock()
	service.observeActions(t.Context(), session, alloc.ActionsCacheUnavailable)
	if session.observed.ActionsCache != string(alloc.ActionsCacheServed) {
		session.mu.Unlock()
		t.Fatal("the observation was written without the session's lock")
	}
	session.mu.Unlock()

	if err := service.Cleanup(t.Context(), session.instance); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	told := observedActions(observer)
	if len(told) == 0 || told[len(told)-1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want the failure last", told)
	}
}

// A SAVE WHOSE BLOCK FAILED ON THE NODE IS NOT RECORDED AS SERVED (#694). A blob
// leg is not a CacheService call, and only those were observed, so the job
// carried its reservation's `served`. Storage that refuses the block is
// billet's failure.
func TestABlobLegThatFailsLocallyRecordsTheJobAsUnavailable(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	uploadURL, archive := reserveActionsUpload(t, service, session, "broken")
	if err := os.RemoveAll(service.actionsStagingPath(session, archive)); err != nil {
		t.Fatalf("remove the staging directory: %v", err)
	}

	response, handled := service.actionsProxy().respond(
		actionsBlockRequest(t, uploadURL, "block", "YQ==", bytes.NewBufferString("a")), session)
	if !handled || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("a block storage refused: handled=%t status=%v; want a local 502", handled, response)
	}
	closeActionsResponse(t, response)

	if told := observedActions(observer); len(told) != 2 || told[0] != alloc.ActionsCacheServed ||
		told[1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want served for the reservation and then unavailable", told)
	}
}

// A BODY THE GUEST CUT SHORT IS NOT BILLET'S FAILURE, and does not replace the
// reservation's outcome: a client that retries and succeeds would otherwise be
// recorded as failed.
func TestABlockTheGuestCutShortIsNotRecordedAsAFailure(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	uploadURL, _ := reserveActionsUpload(t, service, session, "cut")

	response, handled := service.actionsProxy().respond(
		actionsBlockRequest(t, uploadURL, "block", "YQ==", &cutBlockBody{}), session)
	if !handled || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("a cut block handled=%t status=%v; want a local 502", handled, response)
	}
	closeActionsResponse(t, response)

	if told := observedActions(observer); len(told) != 1 || told[0] != alloc.ActionsCacheServed {
		t.Fatalf("observer was told %v, want only the reservation's served", told)
	}
}

// AN ARCHIVE THIS PROCESS COULD NOT MOUNT AGAIN IS BILLET'S FAILURE, answered as
// one and recorded, not refused as if the URL were unknown.
func TestALostArchiveIsRecordedAsUnavailable(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	uploadURL, archive := reserveActionsUpload(t, service, session, "lost")
	session.mu.Lock()
	archive.lost = true
	session.mu.Unlock()

	response, handled := service.actionsProxy().respond(
		actionsBlockRequest(t, uploadURL, "block", "YQ==", bytes.NewBufferString("a")), session)
	if !handled || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("a block for a lost archive handled=%t status=%v; want a local 502", handled, response)
	}
	closeActionsResponse(t, response)

	if told := observedActions(observer); len(told) != 2 || told[1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want served and then unavailable", told)
	}
}

// AN ARCHIVE THAT FAILS TO READ WHILE IT IS SERVED IS RECORDED, when the session
// settles: the read happens as the answer is written, after the request's
// disposition was recorded.
func TestADownloadThatFailsToReadIsRecordedAsUnavailable(t *testing.T) {
	t.Parallel()

	service, storage, session, volumes := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	storage.current = "g1"
	volumes.published = []byte("archive-body")
	proxy := service.actionsProxy()
	lookup := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsDownloadPath,
		`{"key":"reads","restore_keys":[],"version":"v1"}`)
	response, handled := proxy.respond(lookup, session)
	if !handled || response.StatusCode != http.StatusOK {
		t.Fatalf("GetCacheEntryDownloadURL handled=%t status=%v", handled, response)
	}
	found := responseJSON(t, response)
	downloadURL, ok := found["signed_download_url"].(string)
	if !ok || downloadURL == "" {
		t.Fatalf("GetCacheEntryDownloadURL response = %v", found)
	}

	response, handled = proxy.respond(actionsRequestForTest(t, http.MethodGet, downloadURL, ""), session)
	if !handled || response.StatusCode != http.StatusOK {
		t.Fatalf("download handled=%t status=%v", handled, response)
	}
	defer response.Body.Close()
	file, ok := response.Body.(*limitedFile)
	if !ok {
		t.Fatalf("the download's body is a %T, not the archive", response.Body)
	}
	if err := file.file.Close(); err != nil {
		t.Fatalf("close the archive under the answer: %v", err)
	}
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("reading a closed archive succeeded")
	}
	if err := service.Cleanup(t.Context(), session.instance); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if told := observedActions(observer); len(told) != 2 || told[0] != alloc.ActionsCacheServed ||
		told[1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want served and then unavailable", told)
	}
}

// A FAILURE MARKED AFTER CLEANUP SETTLED IS STILL REPORTED: cleanup settles the
// session before it unmounts and discards, and a download read or an
// observation that waited out its hold lands in between. The record that took
// it up was about to be deleted unreported.
func TestAFailureMarkedDuringCleanupIsReportedBeforeTheRecordGoes(t *testing.T) {
	t.Parallel()

	service, _, session, volumes := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	reserveActionsUpload(t, service, session, "late")
	volumes.unmountHook = func() { session.actionsFailed.Store(true) }

	if err := service.Cleanup(t.Context(), session.instance); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	told := observedActions(observer)
	if len(told) == 0 || told[len(told)-1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want the failure marked during cleanup last", told)
	}
}

// A FINALIZATION OF AN ARCHIVE THIS PROCESS COULD NOT MOUNT AGAIN IS BILLET'S
// FAILURE: it answered {"ok": false} and kept the reservation's served.
func TestFinalizingALostArchiveIsRecordedAsUnavailable(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	_, archive := reserveActionsUpload(t, service, session, "lost-final")
	session.mu.Lock()
	archive.lost = true
	session.mu.Unlock()

	finalize := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"lost-final","version":"v2","size_bytes":"4"}`)
	response, handled := service.actionsProxy().respond(finalize, session)
	if !handled || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("finalizing a lost archive handled=%t status=%v; want a local 502", handled, response)
	}
	closeActionsResponse(t, response)
	if told := observedActions(observer); len(told) != 2 || told[1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want served and then unavailable", told)
	}
}

// A RESERVED BLOB WHOSE POLICY CANNOT BE READ IS BILLET'S FAILURE: GitHub knows
// nothing of the URL, so it is not a splice, and the kill switch is a refusal.
func TestAReservedBlobWhosePolicyCannotBeReadIsRecordedAsUnavailable(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		allowed bool
		err     error
		want    []alloc.ActionsCache
	}{
		"unreadable": {err: errors.New("the policy service did not answer"),
			want: []alloc.ActionsCache{alloc.ActionsCacheServed, alloc.ActionsCacheUnavailable}},
		"disabled": {allowed: false, want: []alloc.ActionsCache{alloc.ActionsCacheServed}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, _, session, _ := testActionsService(t)
			observer := &recordingObserver{}
			service.SetCacheObserver(observer)
			uploadURL, _ := reserveActionsUpload(t, service, session, "policy")
			service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
				return tc.allowed, tc.err
			}))

			response, handled := service.actionsProxy().respond(
				actionsBlockRequest(t, uploadURL, "block", "YQ==", bytes.NewBufferString("a")), session)
			if !handled || response.StatusCode != http.StatusBadGateway {
				t.Fatalf("a reserved block handled=%t status=%v; want a local 502", handled, response)
			}
			closeActionsResponse(t, response)
			if told := observedActions(observer); !slices.Equal(told, tc.want) {
				t.Fatalf("observer was told %v, want %v", told, tc.want)
			}
		})
	}
}

// NEITHER A METADATA BODY THE GUEST CUT SHORT NOR A REQUEST THAT RAN OUT OF TIME
// IS BILLET'S FAILURE, on any branch: `unavailable` outlasts a later success.
func TestACutOrCancelledCallIsNotRecordedAsAFailure(t *testing.T) {
	t.Parallel()

	for name, request := range map[string]func(t *testing.T) *http.Request{
		"cut metadata": func(t *testing.T) *http.Request {
			t.Helper()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				"https://"+actionsResultsHost+actionsFinalizePath, &cutBlockBody{})
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", actionsUserAgent+"test")

			return request
		},
		"cancelled finalization": func(t *testing.T) *http.Request {
			t.Helper()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			return actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
				`{"key":"cancelled","version":"v2","size_bytes":"4"}`).WithContext(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, _, session, _ := testActionsService(t)
			observer := &recordingObserver{}
			service.SetCacheObserver(observer)
			reserveActionsUpload(t, service, session, "cancelled")

			response, _ := service.actionsProxy().respond(request(t), session)
			closeActionsResponse(t, response)
			if told := observedActions(observer); len(told) != 1 || told[0] != alloc.ActionsCacheServed {
				t.Fatalf("observer was told %v, want only the reservation's served", told)
			}
		})
	}
}
