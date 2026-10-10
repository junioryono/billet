package guestreport

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// EncodeBatch is b in Codec, refused if it would not decode or is over
// MaxBatchBytes.
func EncodeBatch(b Batch) ([]byte, error) {
	if err := validateBatch(&b); err != nil {
		return nil, err
	}

	return encode(&b, MaxBatchBytes, MaxBatchInflatedBytes)
}

// DecodeBatch reverses EncodeBatch, refusing anything EncodeBatch would not have
// written.
func DecodeBatch(data []byte) (Batch, error) {
	var b Batch

	raw, err := decode(data, &b, MaxBatchBytes, MaxBatchInflatedBytes, batchShape)
	if err != nil {
		return Batch{}, err
	}

	if err := validateBatch(&b); err != nil {
		return Batch{}, err
	}

	if err := canonical(raw, &b); err != nil {
		return Batch{}, err
	}

	return b, nil
}

// Encode is r in Codec, refused if it would not decode or is over MaxReportBytes.
// A merged report is encoded through Downsample, which halves it until it fits.
func Encode(r Report) ([]byte, error) {
	if err := validateReport(&r); err != nil {
		return nil, err
	}

	return encode(&r, MaxReportBytes, MaxReportInflatedBytes)
}

// Decode reverses Encode, refusing anything Encode would not have written.
func Decode(data []byte) (Report, error) {
	var r Report

	raw, err := decode(data, &r, MaxReportBytes, MaxReportInflatedBytes, reportShape)
	if err != nil {
		return Report{}, err
	}

	if err := validateReport(&r); err != nil {
		return Report{}, err
	}

	if err := canonical(raw, &r); err != nil {
		return Report{}, err
	}

	return r, nil
}

func encode(v any, limit, inflatedLimit int) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, refuse(ErrMalformed, "")
	}

	if len(raw) > inflatedLimit {
		return nil, refuse(ErrTooLarge, "")
	}

	var out bytes.Buffer

	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		return nil, err
	}

	if _, err := w.Write(raw); err != nil {
		return nil, err
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	if out.Len() > limit {
		return nil, refuse(ErrTooLarge, "")
	}

	return out.Bytes(), nil
}

// decode inflates data and decodes the one JSON value in it into v, with no field v
// does not have and no list longer than s admits, and returns the JSON.
func decode(data []byte, v any, limit, inflatedLimit int, s shape) ([]byte, error) {
	raw, err := inflate(data, limit, inflatedLimit)
	if err != nil {
		return nil, err
	}

	// encoding/json replaces an invalid byte with U+FFFD rather than refuse it.
	if !utf8.Valid(raw) {
		return nil, refuse(ErrInvalidUTF8, "")
	}

	if err := checkShape(raw, s); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		// THE ONE REFUSAL encoding/json gives no type, recognised by its text
		// (Go 1.26, TestEachRefusalIsTyped). Every message of its quotes the
		// guest's bytes, so none is passed on.
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return nil, refuse(ErrUnknownField, "")
		}

		return nil, refuse(ErrMalformed, "")
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, refuse(ErrMalformed, "")
	}

	return raw, nil
}

// inflate is data decompressed: one DEFLATE stream with nothing after it, refused
// past inflatedLimit before more than that is held.
func inflate(data []byte, limit, inflatedLimit int) ([]byte, error) {
	if len(data) > limit {
		return nil, refuse(ErrTooLarge, "")
	}

	// A bytes.Reader is an io.ByteReader, so flate reads no byte past its stream
	// and what is left in it is what followed.
	in := bytes.NewReader(data)

	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(in), int64(inflatedLimit)+1))
	if err != nil {
		return nil, refuse(ErrMalformed, "")
	}

	if len(raw) > inflatedLimit {
		return nil, refuse(ErrTooLarge, "")
	}

	if in.Len() != 0 {
		return nil, refuse(ErrMalformed, "")
	}

	return raw, nil
}

// canonical refuses raw unless it is exactly what the encoder writes for v: one
// spelling per value, so a decoded value and its bytes are one thing.
func canonical(raw []byte, v any) error {
	want, err := json.Marshal(v)
	if err != nil || !bytes.Equal(raw, want) {
		return refuse(ErrNotCanonical, "")
	}

	return nil
}
