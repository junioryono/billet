// Package reapi serves the cache half of the Remote Execution API (the
// Capabilities, ContentAddressableStorage, ActionCache and ByteStream services)
// over a node's content-addressed cache volume, for Bazel's and Buck2's gRPC
// remote caches. It executes nothing.
//
// It is the one importer of the API's generated code and of gRPC's server; the
// node hands it a Volume per call and it knows nothing of sessions, keys or
// publication.
package reapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazelbuild/remote-apis/build/bazel/semver"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Table is one of a volume's two namespaces.
type Table string

const (
	// TableAC holds action results by the action's digest.
	TableAC Table = "ac"
	// TableCAS holds blobs by the digest of their own content.
	TableCAS Table = "cas"
)

// What a Volume's refusals mean, so each becomes the gRPC code a client acts on.
var (
	ErrOff      = errors.New("this cache is off for this job")
	ErrBusy     = errors.New("too many concurrent cache transfers")
	ErrFull     = errors.New("the cache volume is full")
	ErrMismatch = errors.New("the blob's content does not hash to its name")
	ErrEnded    = errors.New("the cache session has ended")
)

// Volume is one admitted call's view of a session's cache volume.
type Volume interface {
	// Open returns a stored object, or an error satisfying fs.ErrNotExist.
	Open(table Table, hash string) (*os.File, error)
	// Put stores an object; a cas object is refused (ErrMismatch) unless its
	// bytes hash to its name.
	Put(ctx context.Context, table Table, hash string, body io.Reader) error
	Close()
}

// Opener admits one call and gives it its session's volume. write says the call
// may store something, which the node records before the first byte lands.
type Opener func(ctx context.Context, write bool) (Volume, error)

const (
	// maxBatch bounds a batch call's payload: gRPC's default message ceiling
	// is 4 MiB, and a client splits anything larger into ByteStream calls.
	maxBatch = 4<<20 - 64<<10
	// resultLimit bounds a stored ActionResult, the node's own limit on ac.
	resultLimit = 1 << 20
	chunk       = 256 << 10
)

// emptyHash is the SHA-256 of nothing, which every CAS holds without storing.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Server is the API's cache services over an Opener.
type Server struct {
	grpc *grpc.Server
}

// New returns a Server whose every call opens its volume through open.
func New(open Opener) *Server {
	g := grpc.NewServer(grpc.MaxRecvMsgSize(4 << 20))
	svc := &service{open: open}
	repb.RegisterCapabilitiesServer(g, svc)
	repb.RegisterContentAddressableStorageServer(g, svc)
	repb.RegisterActionCacheServer(g, svc)
	bspb.RegisterByteStreamServer(g, svc)

	return &Server{grpc: g}
}

// IsGRPC reports whether r is a gRPC call.
func IsGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// ServeHTTP answers one gRPC call arriving over HTTP/2.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.grpc.ServeHTTP(w, r) }

type service struct {
	repb.UnimplementedCapabilitiesServer
	repb.UnimplementedContentAddressableStorageServer
	repb.UnimplementedActionCacheServer
	bspb.UnimplementedByteStreamServer

	open Opener
}

// code is the gRPC status an error from the volume becomes.
func code(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, ErrOff):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, ErrBusy), errors.Is(err, ErrFull):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, ErrMismatch):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	default:
		return status.Error(codes.Unavailable, "the cache is unavailable")
	}
}

func itemStatus(err error) *spb.Status {
	if err == nil {
		return &spb.Status{}
	}

	return status.Convert(code(err)).Proto()
}

// checkDigest refuses a digest that is not a SHA-256 of a plausible size.
func checkDigest(d *repb.Digest) error {
	if d == nil {
		return status.Error(codes.InvalidArgument, "a digest is required")
	}
	if !validHash(d.GetHash()) || d.GetSizeBytes() < 0 {
		return status.Errorf(codes.InvalidArgument, "%q/%d is not a SHA-256 digest",
			d.GetHash(), d.GetSizeBytes())
	}

	return nil
}

func validHash(hash string) bool {
	if len(hash) != 64 || strings.ToLower(hash) != hash {
		return false
	}
	_, err := hex.DecodeString(hash)

	return err == nil
}

func checkFunction(function repb.DigestFunction_Value) error {
	if function != repb.DigestFunction_UNKNOWN && function != repb.DigestFunction_SHA256 {
		return status.Errorf(codes.InvalidArgument, "only SHA-256 digests are served, not %s", function)
	}

	return nil
}

// has reports whether the volume holds a blob of exactly that digest.
func has(v Volume, d *repb.Digest) (bool, error) {
	if d.GetHash() == emptyHash && d.GetSizeBytes() == 0 {
		return true, nil
	}
	file, err := v.Open(TableCAS, d.GetHash())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}

	return info.Size() == d.GetSizeBytes(), nil
}

// read returns a whole stored object of at most limit bytes.
func read(v Volume, table Table, hash string, limit int64) ([]byte, error) {
	if table == TableCAS && hash == emptyHash {
		return nil, nil
	}
	file, err := v.Open(table, hash)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s/%s is larger than %d bytes", table, hash, limit)
	}

	return body, nil
}

func (s *service) GetCapabilities(context.Context, *repb.GetCapabilitiesRequest) (*repb.ServerCapabilities, error) {
	return &repb.ServerCapabilities{
		CacheCapabilities: &repb.CacheCapabilities{
			DigestFunctions:               []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
			ActionCacheUpdateCapabilities: &repb.ActionCacheUpdateCapabilities{UpdateEnabled: true},
			MaxBatchTotalSizeBytes:        maxBatch,
			SymlinkAbsolutePathStrategy:   repb.SymlinkAbsolutePathStrategy_DISALLOWED,
		},
		LowApiVersion:  &semver.SemVer{Major: 2},
		HighApiVersion: &semver.SemVer{Major: 2, Minor: 3},
	}, nil
}

func (s *service) FindMissingBlobs(
	ctx context.Context, req *repb.FindMissingBlobsRequest,
) (*repb.FindMissingBlobsResponse, error) {
	if err := checkFunction(req.GetDigestFunction()); err != nil {
		return nil, err
	}
	for _, d := range req.GetBlobDigests() {
		if err := checkDigest(d); err != nil {
			return nil, err
		}
	}
	v, err := s.open(ctx, false)
	if err != nil {
		return nil, code(err)
	}
	defer v.Close()

	response := &repb.FindMissingBlobsResponse{}
	for _, d := range req.GetBlobDigests() {
		present, err := has(v, d)
		if err != nil {
			return nil, code(err)
		}
		if !present {
			response.MissingBlobDigests = append(response.MissingBlobDigests, d)
		}
	}

	return response, nil
}

func (s *service) BatchUpdateBlobs(
	ctx context.Context, req *repb.BatchUpdateBlobsRequest,
) (*repb.BatchUpdateBlobsResponse, error) {
	if err := checkFunction(req.GetDigestFunction()); err != nil {
		return nil, err
	}
	total := 0
	for _, item := range req.GetRequests() {
		if err := checkDigest(item.GetDigest()); err != nil {
			return nil, err
		}
		total += len(item.GetData())
	}
	if total > maxBatch {
		return nil, status.Errorf(codes.InvalidArgument, "a batch of %d bytes is larger than %d",
			total, maxBatch)
	}
	v, err := s.open(ctx, true)
	if err != nil {
		return nil, code(err)
	}
	defer v.Close()

	response := &repb.BatchUpdateBlobsResponse{}
	for _, item := range req.GetRequests() {
		d := item.GetDigest()
		var result error
		switch {
		case item.GetCompressor() != repb.Compressor_IDENTITY:
			result = status.Error(codes.InvalidArgument, "only uncompressed blobs are served")
		case int64(len(item.GetData())) != d.GetSizeBytes():
			result = ErrMismatch
		default:
			result = v.Put(ctx, TableCAS, d.GetHash(), bytes.NewReader(item.GetData()))
		}
		response.Responses = append(response.Responses, &repb.BatchUpdateBlobsResponse_Response{
			Digest: d, Status: itemStatus(result),
		})
	}

	return response, nil
}

func (s *service) BatchReadBlobs(
	ctx context.Context, req *repb.BatchReadBlobsRequest,
) (*repb.BatchReadBlobsResponse, error) {
	if err := checkFunction(req.GetDigestFunction()); err != nil {
		return nil, err
	}
	// CHECKED ONE DIGEST AT A TIME, so sizes a client chose cannot overflow the
	// total past the bound it exists to hold.
	var total int64
	for _, d := range req.GetDigests() {
		if err := checkDigest(d); err != nil {
			return nil, err
		}
		if d.GetSizeBytes() > maxBatch-total {
			return nil, status.Errorf(codes.InvalidArgument, "a batch larger than %d bytes", maxBatch)
		}
		total += d.GetSizeBytes()
	}
	v, err := s.open(ctx, false)
	if err != nil {
		return nil, code(err)
	}
	defer v.Close()

	response := &repb.BatchReadBlobsResponse{}
	for _, d := range req.GetDigests() {
		body, err := read(v, TableCAS, d.GetHash(), d.GetSizeBytes())
		if err == nil && int64(len(body)) != d.GetSizeBytes() {
			err = fs.ErrNotExist
		}
		response.Responses = append(response.Responses, &repb.BatchReadBlobsResponse_Response{
			Digest: d, Data: body, Status: itemStatus(err),
		})
	}

	return response, nil
}

// GetActionResult answers a stored result only while every blob it names is
// present, so a client is never sent to fetch an output this cache lost.
func (s *service) GetActionResult(
	ctx context.Context, req *repb.GetActionResultRequest,
) (*repb.ActionResult, error) {
	if err := checkFunction(req.GetDigestFunction()); err != nil {
		return nil, err
	}
	if err := checkDigest(req.GetActionDigest()); err != nil {
		return nil, err
	}
	v, err := s.open(ctx, false)
	if err != nil {
		return nil, code(err)
	}
	defer v.Close()

	body, err := read(v, TableAC, req.GetActionDigest().GetHash(), resultLimit)
	if err != nil {
		return nil, code(err)
	}
	result := &repb.ActionResult{}
	if err := proto.Unmarshal(body, result); err != nil {
		return nil, status.Error(codes.NotFound, "not found")
	}
	referenced := []*repb.Digest{result.GetStdoutDigest(), result.GetStderrDigest()}
	for _, file := range result.GetOutputFiles() {
		referenced = append(referenced, file.GetDigest())
	}
	for _, dir := range result.GetOutputDirectories() {
		referenced = append(referenced, dir.GetTreeDigest())
		// AND EVERY FILE THE TREE NAMES: a tree that is present says nothing
		// about the outputs inside it.
		files, err := treeFiles(v, dir.GetTreeDigest())
		if err != nil {
			return nil, status.Error(codes.NotFound, "not found")
		}
		referenced = append(referenced, files...)
	}
	for _, d := range referenced {
		if d == nil {
			continue
		}
		if checkDigest(d) != nil {
			return nil, status.Error(codes.NotFound, "not found")
		}
		present, err := has(v, d)
		if err != nil {
			return nil, code(err)
		}
		if !present {
			return nil, status.Error(codes.NotFound, "not found")
		}
	}

	return result, nil
}

func (s *service) UpdateActionResult(
	ctx context.Context, req *repb.UpdateActionResultRequest,
) (*repb.ActionResult, error) {
	if err := checkFunction(req.GetDigestFunction()); err != nil {
		return nil, err
	}
	if err := checkDigest(req.GetActionDigest()); err != nil {
		return nil, err
	}
	body, err := proto.Marshal(req.GetActionResult())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "the action result does not encode")
	}
	if len(body) > resultLimit {
		return nil, status.Errorf(codes.InvalidArgument, "an action result of %d bytes is larger than %d",
			len(body), resultLimit)
	}
	v, err := s.open(ctx, true)
	if err != nil {
		return nil, code(err)
	}
	defer v.Close()
	if err := v.Put(ctx, TableAC, req.GetActionDigest().GetHash(), bytes.NewReader(body)); err != nil {
		return nil, code(err)
	}

	return req.GetActionResult(), nil
}

// treeLimit bounds one output directory's Tree, which names every file under
// it.
const treeLimit = 64 << 20

// treeFiles is every file digest a stored Tree names.
func treeFiles(v Volume, d *repb.Digest) ([]*repb.Digest, error) {
	if err := checkDigest(d); err != nil {
		return nil, err
	}
	if d.GetSizeBytes() > treeLimit {
		return nil, fmt.Errorf("a tree of %d bytes is larger than %d", d.GetSizeBytes(), treeLimit)
	}
	body, err := read(v, TableCAS, d.GetHash(), d.GetSizeBytes())
	if err != nil {
		return nil, err
	}
	tree := &repb.Tree{}
	if err := proto.Unmarshal(body, tree); err != nil {
		return nil, err
	}
	var files []*repb.Digest
	for _, dir := range append([]*repb.Directory{tree.GetRoot()}, tree.GetChildren()...) {
		for _, file := range dir.GetFiles() {
			files = append(files, file.GetDigest())
		}
	}

	return files, nil
}

// blobName reads `[{instance}/]blobs/{hash}/{size}` out of a ByteStream
// resource name, or `[{instance}/]uploads/{uuid}/blobs/{hash}/{size}[/...]`
// when upload is set. Compressed blobs are not served.
func blobName(resource string, upload bool) (*repb.Digest, error) {
	parts := strings.Split(resource, "/")
	start := 0
	if upload {
		for start < len(parts) && parts[start] != "uploads" {
			start++
		}
		start += 2
	}
	for i := start; i+2 < len(parts); i++ {
		if parts[i] != "blobs" {
			continue
		}
		size, err := strconv.ParseInt(parts[i+2], 10, 64)
		d := &repb.Digest{Hash: parts[i+1], SizeBytes: size}
		if err != nil || checkDigest(d) != nil {
			break
		}

		return d, nil
	}

	return nil, status.Errorf(codes.InvalidArgument, "%q names no uncompressed SHA-256 blob", resource)
}

func (s *service) Read(req *bspb.ReadRequest, stream bspb.ByteStream_ReadServer) error {
	d, err := blobName(req.GetResourceName(), false)
	if err != nil {
		return err
	}
	if req.GetReadOffset() < 0 || req.GetReadLimit() < 0 || req.GetReadOffset() > d.GetSizeBytes() {
		return status.Error(codes.OutOfRange, "the read is outside the blob")
	}
	if d.GetSizeBytes() == 0 {
		if d.GetHash() != emptyHash {
			return status.Error(codes.NotFound, "not found")
		}

		return nil
	}
	v, err := s.open(stream.Context(), false)
	if err != nil {
		return code(err)
	}
	defer v.Close()
	file, err := v.Open(TableCAS, d.GetHash())
	if err != nil {
		return code(err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.Size() != d.GetSizeBytes() {
		return status.Error(codes.NotFound, "not found")
	}

	remaining := d.GetSizeBytes() - req.GetReadOffset()
	if limit := req.GetReadLimit(); limit > 0 && limit < remaining {
		remaining = limit
	}
	section := io.NewSectionReader(file, req.GetReadOffset(), remaining)
	buffer := make([]byte, chunk)
	for {
		n, err := section.Read(buffer)
		if n > 0 {
			if sendErr := stream.Send(&bspb.ReadResponse{Data: buffer[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return code(err)
		}
	}
}

// Write stores one uploaded blob. The upload is streamed straight into the
// volume's own verification, so nothing is kept in memory and nothing that
// fails to hash to its name is stored.
func (s *service) Write(stream bspb.ByteStream_WriteServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	d, err := blobName(first.GetResourceName(), true)
	if err != nil {
		return err
	}
	v, err := s.open(stream.Context(), true)
	if err != nil {
		return code(err)
	}
	defer v.Close()

	// ALREADY PRESENT IS A COMPLETE UPLOAD, which the API lets a server say at
	// once rather than take the bytes again.
	if present, err := has(v, d); err == nil && present {
		return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: d.GetSizeBytes()})
	}

	reader, writer := io.Pipe()
	stored := make(chan error, 1)
	go func() {
		stored <- v.Put(stream.Context(), TableCAS, d.GetHash(), reader)
		_ = reader.Close()
	}()

	var offset int64
	request := first
	for {
		if request.GetWriteOffset() != offset {
			_ = writer.CloseWithError(errors.New("out of order"))
			<-stored

			return status.Errorf(codes.InvalidArgument, "a write at offset %d after %d bytes",
				request.GetWriteOffset(), offset)
		}
		if _, err := writer.Write(request.GetData()); err != nil {
			return code(errors.Join(err, <-stored))
		}
		offset += int64(len(request.GetData()))
		if offset > d.GetSizeBytes() {
			_ = writer.CloseWithError(ErrMismatch)
			<-stored

			return status.Error(codes.InvalidArgument, "more bytes than the blob's size")
		}
		if request.GetFinishWrite() {
			break
		}
		request, err = stream.Recv()
		if err != nil {
			_ = writer.CloseWithError(err)
			<-stored

			return err
		}
	}
	// A DIGEST IS ITS HASH AND ITS SIZE: an upload of the right bytes under a
	// wrong size is refused, not stored and reported committed. Checked before
	// the stream is closed, which is what lets the store finish.
	if offset != d.GetSizeBytes() {
		_ = writer.CloseWithError(ErrMismatch)
		<-stored

		return status.Error(codes.InvalidArgument, "the upload is not the blob's size")
	}
	_ = writer.Close()
	if err := <-stored; err != nil {
		return code(err)
	}

	return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: offset})
}

// QueryWriteStatus reports an upload complete only when its blob is stored;
// an upload in progress is not resumable here, so the client starts again.
func (s *service) QueryWriteStatus(
	ctx context.Context, req *bspb.QueryWriteStatusRequest,
) (*bspb.QueryWriteStatusResponse, error) {
	d, err := blobName(req.GetResourceName(), true)
	if err != nil {
		return nil, err
	}
	v, err := s.open(ctx, false)
	if err != nil {
		return nil, code(err)
	}
	defer v.Close()
	present, err := has(v, d)
	if err != nil {
		return nil, code(err)
	}
	if !present {
		return nil, status.Error(codes.NotFound, "not found")
	}

	return &bspb.QueryWriteStatusResponse{CommittedSize: d.GetSizeBytes(), Complete: true}, nil
}
