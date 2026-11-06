package joblog

import (
	"bytes"
	"compress/flate"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func aChunk() Chunk {
	return Chunk{Seq: 3, Step: 2, StepName: "Run tests", Lines: []Line{
		{AtMillis: 1_760_000_000_123, Text: "go test ./..."},
		{Text: "\tcontinued with a tab"},
		{AtMillis: 1_760_000_000_500, Text: ""},
	}}
}

func TestAChunkRoundTrips(t *testing.T) {
	t.Parallel()

	data, err := Encode(aChunk())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, aChunk()) {
		t.Fatalf("read back %+v", got)
	}
}

// deflated is raw compressed as the codec compresses it, whatever raw holds.
func deflated(t *testing.T, raw string) []byte {
	t.Helper()

	var out bytes.Buffer

	w, err := flate.NewWriter(&out, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return out.Bytes()
}

// A CHUNK IS THE GUEST'S, so everything a reader relies on is refused, by the sentinel
// and without quoting a byte of what was sent.
func TestAChunkIsHeldToItsSchema(t *testing.T) {
	t.Parallel()

	const secret = "hunter2"

	good := deflated(t, `{"seq":1,"step":0,"lines":[{"text":"ok"}]}`)
	if _, err := Decode(good); err != nil {
		t.Fatalf("the shape every case below breaks is refused: %v", err)
	}

	for name, raw := range map[string]string{
		"not JSON":             secret,
		"an unknown field":     `{"seq":1,"step":0,"lines":[{"text":"ok"}],"` + secret + `":1}`,
		"no sequence":          `{"seq":0,"step":0,"lines":[{"text":"ok"}]}`,
		"a step out of range":  `{"seq":1,"step":-1,"lines":[{"text":"ok"}]}`,
		"no lines":             `{"seq":1,"step":0,"lines":[]}`,
		"a control character":  `{"seq":1,"step":0,"lines":[{"text":"a\u001b[31m` + secret + `"}]}`,
		"a line past its time": `{"seq":1,"step":0,"lines":[{"at_ms":-1,"text":"ok"}]}`,
		"a line past its bound": `{"seq":1,"step":0,"lines":[{"text":"` +
			strings.Repeat("x", MaxLineBytes+1) + `"}]}`,
		"a name past its bound": `{"seq":1,"step":0,"step_name":"` + strings.Repeat("x", MaxStepNameBytes+1) +
			`","lines":[{"text":"ok"}]}`,
		"a value after it": `{"seq":1,"step":0,"lines":[{"text":"ok"}]}{}`,
	} {
		_, err := Decode(deflated(t, raw))
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: refused with %v, want ErrRefused", name, err)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the refusal quotes what the guest wrote: %v", name, err)
		}
	}

	if _, err := Decode(append(good, 0)); !errors.Is(err, ErrRefused) {
		t.Errorf("bytes after the stream: %v", err)
	}
	if _, err := Decode(make([]byte, MaxChunkBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("past the compressed bound: %v", err)
	}
}

func TestEncodeRefusesWhatDecodeWould(t *testing.T) {
	t.Parallel()

	c := aChunk()
	c.Lines[0].Text = "a\nforged line"
	if _, err := Encode(c); !errors.Is(err, ErrRefused) {
		t.Errorf("a newline in a line: %v", err)
	}

	big := Chunk{Seq: 1, Lines: make([]Line, MaxChunkLines)}
	for i := range big.Lines {
		big.Lines[i].Text = strings.Repeat("x", 100)
	}
	if _, err := Encode(big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("past the inflated bound: %v", err)
	}
}

// WHAT THE RUNNER WRITES IS CLEANED, measured with its colours on 2026-11-04: a colour
// sequence is removed, not replaced, and whatever else a terminal would act on is
// replaced, so a chunk holding it validates.
func TestCleanRemovesWhatATerminalWouldActOn(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"\x1b[36;1msleep \"$S\"\x1b[0m": `sleep "$S"`,
		"tab\tkept":                     "tab\tkept",
		"bell\a":                        "bell?",
		"\x1b]0;title\a":                "?]0;title?",
		"\x1b[":                         "?[",
		"bad\xffbyte":                   "bad?byte",
		"csi\u009b31m":                  "csi?31m",
	} {
		got := Clean(in, MaxLineBytes)
		if got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
		if !isText(got) {
			t.Errorf("Clean(%q) = %q, which a chunk refuses", in, got)
		}
	}

	if got := Clean(strings.Repeat("é", 10), 5); got != "éé" {
		t.Errorf("a cut lands inside a character: %q", got)
	}
}

func TestALevelIsInferredFromTheLine(t *testing.T) {
	t.Parallel()

	for text, want := range map[string]Level{
		"##[error]Process completed with exit code 1.": LevelError,
		"##[warning]Node 16 is deprecated":             LevelWarning,
		"  Error: connection refused":                  LevelError,
		"panic: runtime error":                         LevelError,
		"FATAL ERROR: heap":                            LevelError,
		"warning: unused variable":                     LevelWarning,
		"WARN deprecated":                              LevelWarning,
		"no errors found":                              LevelOther,
		"errors.New(x)":                                LevelOther,
		"":                                             LevelOther,
	} {
		if got := LevelOf(text); got != want {
			t.Errorf("LevelOf(%q) = %s, want %s", text, got, want)
		}
	}

	for _, l := range []Level{LevelOther, LevelWarning, LevelError} {
		if got, ok := ParseLevel(l.String()); !ok || got != l {
			t.Errorf("%s does not read back: %s, %t", l, got, ok)
		}
	}
	if _, ok := ParseLevel("hunter2"); ok {
		t.Error("a word that is no level was read as one")
	}
}

func FuzzDecode(f *testing.F) {
	good, err := Encode(aChunk())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Decode(data)
		if err != nil {
			return
		}
		// A REFUSAL HERE IS NO BUG: something compressed better than this encoder does can
		// decode and then not fit its bound again.
		again, err := Encode(c)
		if err != nil {
			return
		}
		if _, err := Decode(again); err != nil {
			t.Fatalf("a chunk that encoded did not decode: %v", err)
		}
	})
}
