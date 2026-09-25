package reapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/junioryono/billet/internal/node/reapi"
)

// dirVolume is a volume in a directory that verifies cas content the way the
// node's does, and counts what it stored.
type dirVolume struct {
	root   string
	stored *atomic.Int64
}

func (v dirVolume) Open(table reapi.Table, hash string) (*os.File, error) {
	return os.Open(filepath.Join(v.root, string(table), hash))
}

func (v dirVolume) Put(_ context.Context, table reapi.Table, hash string, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); table == reapi.TableCAS && hex.EncodeToString(sum[:]) != hash {
		return reapi.ErrMismatch
	}
	if err := os.MkdirAll(filepath.Join(v.root, string(table)), 0o700); err != nil {
		return err
	}
	v.stored.Add(1)

	return os.WriteFile(filepath.Join(v.root, string(table), hash), data, 0o600)
}

func (dirVolume) Close() {}

// serve runs the API behind a real HTTP/2 listener in the clear, as the node's
// cache listener does, and returns a client connection to it.
func serve(t *testing.T, open reapi.Opener) *grpc.ClientConn {
	t.Helper()

	server := reapi.New(open)
	listener := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !reapi.IsGRPC(r) {
			http.Error(w, "not gRPC", http.StatusBadRequest)

			return
		}
		server.ServeHTTP(w, r)
	}))
	listener.Config.Protocols = new(http.Protocols)
	listener.Config.Protocols.SetHTTP1(true)
	listener.Config.Protocols.SetUnencryptedHTTP2(true)
	listener.Start()
	t.Cleanup(listener.Close)

	conn, err := grpc.NewClient("passthrough:///"+strings.TrimPrefix(listener.URL, "http://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func digestOf(body []byte) *repb.Digest {
	sum := sha256.Sum256(body)

	return &repb.Digest{Hash: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body))}
}

func volumeIn(t *testing.T) (dirVolume, reapi.Opener) {
	t.Helper()

	v := dirVolume{root: t.TempDir(), stored: new(atomic.Int64)}

	return v, func(context.Context) (reapi.Volume, error) { return v, nil }
}

func upload(t *testing.T, conn *grpc.ClientConn, resource string, parts ...[]byte) (*bspb.WriteResponse, error) {
	t.Helper()

	stream, err := bspb.NewByteStreamClient(conn).Write(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var offset int64
	for i, part := range parts {
		request := &bspb.WriteRequest{WriteOffset: offset, Data: part, FinishWrite: i == len(parts)-1}
		if i == 0 {
			request.ResourceName = resource
		}
		if err := stream.Send(request); err != nil {
			break
		}
		offset += int64(len(part))
	}

	return stream.CloseAndRecv()
}

// A BLOB UPLOADED IN PARTS IS STORED, FOUND AND READ BACK WHOLE, through every
// path a client reads by.
func TestABlobRoundTripsThroughByteStreamAndTheCAS(t *testing.T) {
	t.Parallel()

	volume, open := volumeIn(t)
	conn := serve(t, open)
	body := bytes.Repeat([]byte("object "), 100_000)
	d := digestOf(body)
	cas := repb.NewContentAddressableStorageClient(conn)

	missing, err := cas.FindMissingBlobs(t.Context(), &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
	if err != nil || len(missing.GetMissingBlobDigests()) != 1 {
		t.Fatalf("before the upload FindMissingBlobs = %v, %v; want the blob missing", missing, err)
	}

	resource := "main/uploads/7c0a/blobs/" + d.GetHash() + "/" + strconv.FormatInt(d.GetSizeBytes(), 10)
	written, err := upload(t, conn, resource, body[:300_000], body[300_000:])
	if err != nil || written.GetCommittedSize() != d.GetSizeBytes() {
		t.Fatalf("Write = %v, %v", written, err)
	}

	missing, err = cas.FindMissingBlobs(t.Context(), &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
	if err != nil || len(missing.GetMissingBlobDigests()) != 0 {
		t.Fatalf("after the upload FindMissingBlobs = %v, %v; want nothing missing", missing, err)
	}

	read, err := bspb.NewByteStreamClient(conn).Read(t.Context(), &bspb.ReadRequest{
		ResourceName: "main/blobs/" + d.GetHash() + "/" + strconv.FormatInt(d.GetSizeBytes(), 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		chunk, err := read.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got = append(got, chunk.GetData()...)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Read returned %d bytes, want the %d uploaded", len(got), len(body))
	}

	// A DIGEST IS ITS HASH AND ITS SIZE: the same hash at another size is missing.
	misstated := &repb.Digest{Hash: d.GetHash(), SizeBytes: d.GetSizeBytes() - 1}
	missing, err = cas.FindMissingBlobs(t.Context(), &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{misstated}})
	if err != nil || len(missing.GetMissingBlobDigests()) != 1 {
		t.Fatalf("a digest misstating the size = %v, %v; want it missing", missing, err)
	}

	// AN UPLOAD OF A BLOB ALREADY HELD IS COMPLETE AT ONCE, and stores nothing.
	stored := volume.stored.Load()
	written, err = upload(t, conn, resource, body)
	if err != nil || written.GetCommittedSize() != d.GetSizeBytes() || volume.stored.Load() != stored {
		t.Fatalf("a second upload = %v, %v, stored %d more", written, err, volume.stored.Load()-stored)
	}

	small := []byte("small")
	batch, err := cas.BatchUpdateBlobs(t.Context(), &repb.BatchUpdateBlobsRequest{
		Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: digestOf(small), Data: small}},
	})
	if err != nil || codes.Code(batch.GetResponses()[0].GetStatus().GetCode()) != codes.OK {
		t.Fatalf("BatchUpdateBlobs = %v, %v", batch, err)
	}
	reads, err := cas.BatchReadBlobs(t.Context(), &repb.BatchReadBlobsRequest{
		Digests: []*repb.Digest{digestOf(small), digestOf([]byte("absent"))},
	})
	if err != nil || !bytes.Equal(reads.GetResponses()[0].GetData(), small) ||
		codes.Code(reads.GetResponses()[1].GetStatus().GetCode()) != codes.NotFound {
		t.Fatalf("BatchReadBlobs = %v, %v", reads, err)
	}
}

// NOTHING IS STORED UNDER A DIGEST ITS BYTES DO NOT HAVE, by either upload path.
func TestABlobThatDoesNotMatchItsDigestIsRefused(t *testing.T) {
	t.Parallel()

	volume, open := volumeIn(t)
	conn := serve(t, open)
	d := digestOf([]byte("the real object"))
	resource := "uploads/1/blobs/" + d.GetHash() + "/" + strconv.FormatInt(d.GetSizeBytes(), 10)

	if _, err := upload(t, conn, resource, []byte("the fake object")); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a mismatched ByteStream upload answered %v, want InvalidArgument", err)
	}
	if _, err := upload(t, conn, resource, []byte("the real")); status.Code(err) == codes.OK {
		t.Error("a short ByteStream upload was accepted")
	}
	// AN UPLOAD WITH A GAP IS REFUSED, not stored with the gap closed up.
	stream, err := bspb.NewByteStreamClient(conn).Write(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&bspb.WriteRequest{ResourceName: resource, Data: []byte("the real ")})
	_ = stream.Send(&bspb.WriteRequest{WriteOffset: 10, Data: []byte("object"), FinishWrite: true})
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an upload with a gap answered %v, want InvalidArgument", err)
	}
	batch, err := repb.NewContentAddressableStorageClient(conn).BatchUpdateBlobs(t.Context(),
		&repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{
			{Digest: d, Data: []byte("the fake object")},
		}})
	if err != nil || codes.Code(batch.GetResponses()[0].GetStatus().GetCode()) != codes.InvalidArgument {
		t.Errorf("a mismatched batch item answered %v, %v", batch, err)
	}
	misstated := &repb.Digest{Hash: d.GetHash(), SizeBytes: d.GetSizeBytes() + 1}
	batch, err = repb.NewContentAddressableStorageClient(conn).BatchUpdateBlobs(t.Context(),
		&repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{
			{Digest: misstated, Data: []byte("the real object")},
		}})
	if err != nil || codes.Code(batch.GetResponses()[0].GetStatus().GetCode()) != codes.InvalidArgument {
		t.Errorf("a batch item misstating its size answered %v, %v", batch, err)
	}
	if volume.stored.Load() != 0 {
		t.Errorf("%d mismatched objects were stored", volume.stored.Load())
	}
}

// AN ACTION RESULT IS SERVED ONLY WHILE EVERY BLOB IT NAMES IS PRESENT, so a
// client is never sent to fetch an output the cache does not hold.
func TestAnActionResultIsServedOnlyWithItsOutputs(t *testing.T) {
	t.Parallel()

	_, open := volumeIn(t)
	conn := serve(t, open)
	ac := repb.NewActionCacheClient(conn)
	action := digestOf([]byte("an action"))
	output := []byte("an output")
	result := &repb.ActionResult{
		OutputFiles: []*repb.OutputFile{{Path: "out", Digest: digestOf(output)}},
		ExitCode:    0,
	}

	if _, err := ac.GetActionResult(t.Context(), &repb.GetActionResultRequest{ActionDigest: action}); status.Code(err) != codes.NotFound {
		t.Fatalf("an unknown action answered %v, want NotFound", err)
	}
	if _, err := ac.UpdateActionResult(t.Context(), &repb.UpdateActionResultRequest{
		ActionDigest: action, ActionResult: result,
	}); err != nil {
		t.Fatalf("UpdateActionResult: %v", err)
	}
	if _, err := ac.GetActionResult(t.Context(), &repb.GetActionResultRequest{ActionDigest: action}); status.Code(err) != codes.NotFound {
		t.Fatalf("a result whose output is absent answered %v, want NotFound", err)
	}
	if _, err := repb.NewContentAddressableStorageClient(conn).BatchUpdateBlobs(t.Context(),
		&repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{
			{Digest: digestOf(output), Data: output},
		}}); err != nil {
		t.Fatal(err)
	}
	got, err := ac.GetActionResult(t.Context(), &repb.GetActionResultRequest{ActionDigest: action})
	if err != nil || got.GetOutputFiles()[0].GetPath() != "out" {
		t.Fatalf("GetActionResult = %v, %v", got, err)
	}
}

// A REFUSAL FROM THE NODE BECOMES THE CODE A CLIENT ACTS ON.
func TestTheNodesRefusalsBecomeGRPCCodes(t *testing.T) {
	t.Parallel()

	for refusal, want := range map[error]codes.Code{
		reapi.ErrOff:            codes.PermissionDenied,
		reapi.ErrBusy:           codes.ResourceExhausted,
		reapi.ErrEnded:          codes.Unavailable,
		errors.New("unmounted"): codes.Unavailable,
		fs.ErrNotExist:          codes.NotFound,
	} {
		conn := serve(t, func(context.Context) (reapi.Volume, error) { return nil, refusal })
		_, err := repb.NewContentAddressableStorageClient(conn).FindMissingBlobs(t.Context(),
			&repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{digestOf([]byte("x"))}})
		if status.Code(err) != want {
			t.Errorf("%v became %v, want %v", refusal, status.Code(err), want)
		}
	}
}

// THE SERVER SAYS WHAT IT IS: a SHA-256 cache that takes updates and executes
// nothing, which is what Bazel and Buck2 check before using it.
func TestTheCapabilitiesAreACacheOnly(t *testing.T) {
	t.Parallel()

	_, open := volumeIn(t)
	capabilities, err := repb.NewCapabilitiesClient(serve(t, open)).GetCapabilities(t.Context(),
		&repb.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cache := capabilities.GetCacheCapabilities()
	if len(cache.GetDigestFunctions()) != 1 || cache.GetDigestFunctions()[0] != repb.DigestFunction_SHA256 ||
		!cache.GetActionCacheUpdateCapabilities().GetUpdateEnabled() || capabilities.GetExecutionCapabilities() != nil {
		t.Fatalf("capabilities = %v", capabilities)
	}
}
