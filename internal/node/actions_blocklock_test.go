package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// cutBlockBody is a block's body whose sender went away partway.
type cutBlockBody struct{ sent bool }

func (b *cutBlockBody) Read(buffer []byte) (int, error) {
	if !b.sent {
		b.sent = true

		return copy(buffer, "part"), nil
	}

	return 0, io.ErrUnexpectedEOF
}

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

// A BLOCK IN TRANSFER HOLDS NEITHER THE SESSION NOR THE OTHER BLOCKS (#694). The
// client uploads an archive's blocks side by side; each held the archive alone
// for its whole transfer, and the one waiting behind it held the session's lock
// meanwhile, which every new CONNECT of the job's waited on until the guest's
// relay gave up and sent the next block to GitHub.
func TestABlockInTransferHoldsNeitherTheSessionNorTheOtherBlocks(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"blocks","version":"v2"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}

	first := base64.StdEncoding.EncodeToString([]byte("block-1"))
	second := base64.StdEncoding.EncodeToString([]byte("block-2"))
	stalled := &stalledBody{reading: make(chan struct{}), release: make(chan struct{})}
	firstRequest := actionsBlockRequest(t, uploadURL, "block", first, stalled)
	list := `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>` + first +
		`</Latest><Latest>` + second + `</Latest></BlockList>`
	commitRequest := actionsBlockRequest(t, uploadURL, "blocklist", "", bytes.NewBufferString(list))
	firstDone := make(chan error, 1)
	go func() {
		response, _, err := service.actionsResponse(firstRequest, session)
		if err == nil && response.StatusCode != http.StatusCreated {
			err = io.ErrUnexpectedEOF
		}
		if response != nil {
			response.Body.Close()
		}
		firstDone <- err
	}()
	released := false
	defer func() {
		if !released {
			close(stalled.release)
		}
	}()
	select {
	case <-stalled.reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the first block's body was never read")
	}

	// THE COMMIT WAITS FOR THE ARCHIVE ALONE, and must not hold the session's
	// lock while it does.
	commitDone := make(chan error, 1)
	go func() {
		response, _, err := service.actionsResponse(commitRequest, session)
		if err == nil && response.StatusCode != http.StatusCreated {
			err = io.ErrUnexpectedEOF
		}
		if response != nil {
			response.Body.Close()
		}
		commitDone <- err
	}()

	// A SECOND BLOCK IS WRITTEN while the first is still arriving.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response, _, err = service.actionsResponse(
		actionsBlockRequest(t, uploadURL, "block", second, bytes.NewBufferString("two")).WithContext(ctx),
		session)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("a block behind a block in transfer: status=%v error=%v", response, err)
	}
	response.Body.Close()

	// AND THE SESSION'S LOCK IS FREE: what a CONNECT, an observation or a renewal
	// waits on. Asked repeatedly, so a commit that takes it between polls of the
	// archive's lock and holds it there is caught.
	for range 20 {
		lockCtx, lockCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		err := lockCacheSession(lockCtx, session)
		lockCancel()
		if err != nil {
			t.Fatalf("the session's lock was held while blocks were in transfer: %v", err)
		}
		session.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-commitDone:
		t.Fatalf("the block list was committed while a block was still arriving: %v", err)
	default:
	}

	close(stalled.release)
	released = true
	if err := <-firstDone; err != nil {
		t.Fatalf("the first block: %v", err)
	}
	if err := <-commitDone; err != nil {
		t.Fatalf("the block list: %v", err)
	}
	session.mu.Lock()
	var archive *actionsArchive
	for _, candidate := range session.actions {
		archive = candidate
	}
	session.mu.Unlock()
	body, err := os.ReadFile(service.actionsArchivePath(session, archive))
	if err != nil || string(body) != "onetwo" {
		t.Fatalf("assembled archive = %q, error=%v", body, err)
	}
}

// A BLOB REQUEST LET IN AFTER ITS SESSION CLOSED FINDS THE ARCHIVE SHUT: it no
// longer holds the session's lock while it waits for the archive, so the close
// can come in between.
func TestABlobRequestAfterItsSessionClosedIsRefused(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"closing","version":"v2"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}

	session.closing.Store(true)
	response, err = service.serveActionsBlob(
		actionsBlockRequest(t, uploadURL, "block", "YQ==", bytes.NewBufferString("a")), session)
	if err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("a block for a closing session: status=%v error=%v; want 403", response, err)
	}
	response.Body.Close()
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

	session.mu.Lock()
	got, ok := service.authenticateProxy(request)
	session.mu.Unlock()
	if !ok || got != session {
		t.Fatalf("a CONNECT for a busy session was refused (ok=%t)", ok)
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
	var told []alloc.ActionsCache
	for _, call := range observer.recorded() {
		told = append(told, call.obs.ActionsCache)
	}
	if len(told) != 2 || told[0] != alloc.ActionsCacheServed || told[1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want served and then unavailable, each once", told)
	}
}

// A SAVE WHOSE BLOCK FAILED ON THE NODE IS NOT RECORDED AS SERVED (#694). A blob
// leg is not a CacheService call, and only those were observed, so the job
// carried its reservation's `served`.
func TestABlobLegThatFailsLocallyRecordsTheJobAsUnavailable(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	proxy := service.actionsProxy()
	if proxy == nil {
		t.Fatal("the intercepting session has no proxy")
	}

	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"cut","version":"v2"}`)
	response, handled := proxy.respond(create, session)
	if !handled || response.StatusCode != http.StatusOK {
		t.Fatalf("CreateCacheEntry handled=%t status=%v", handled, response)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}

	response, handled = proxy.respond(
		actionsBlockRequest(t, uploadURL, "block", "YQ==", &cutBlockBody{}), session)
	if !handled || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("a cut block handled=%t status=%v; want a local 502", handled, response)
	}
	closeActionsResponse(t, response)

	var told []alloc.ActionsCache
	for _, call := range observer.recorded() {
		told = append(told, call.obs.ActionsCache)
	}
	if len(told) != 2 || told[0] != alloc.ActionsCacheServed || told[1] != alloc.ActionsCacheUnavailable {
		t.Fatalf("observer was told %v, want served for the reservation and then unavailable", told)
	}
}
