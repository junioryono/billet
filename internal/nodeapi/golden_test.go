package nodeapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/fnv"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// wireTypes is every struct the node wire encodes, by name. Each is pinned per
// protocol version as three files: <Type>.json, every field populated with a
// value derived from its path; <Type>.zero.json, every container reachable from
// it allocated and every scalar zero, which is what pins omitempty at every
// depth; and <Type>.shape.txt, the Go kind and width behind each JSON path,
// which pins what the samples cannot (an int64 narrowed to an int8 encodes 42
// the same way).
//
// THE ENCODING IS THE PROTOCOL, AND A ROUND TRIP CANNOT SEE IT CHANGE. Command
// carries alloc.Lease, a ledger struct without JSON tags, so renaming one of
// its Go fields renames a wire field; both sides of a round trip rename
// together and agree, while a node of the previous release, or a plane whose
// decoder refuses unknown fields, does not.
var wireTypes = map[string]any{
	"AdvanceRequest":            AdvanceRequest{},
	"BindRequest":               BindRequest{},
	"CAResponse":                CAResponse{},
	"CacheAuthority":            CacheAuthority{},
	"CacheAuthorityResponse":    CacheAuthorityResponse{},
	"CacheObservationRequest":   CacheObservationRequest{},
	"CachePolicyResponse":       CachePolicyResponse{},
	"Command":                   Command{},
	"CommandResult":             CommandResult{},
	"DescribeRequest":           DescribeRequest{},
	"DescribeResponse":          DescribeResponse{},
	"EnrollRequest":             EnrollRequest{},
	"EnrollResponse":            EnrollResponse{},
	"ErrorResponse":             ErrorResponse{},
	"GuestReportRequest":        GuestReportRequest{},
	"HeartbeatRequest":          HeartbeatRequest{},
	"JITRequest":                JITRequest{},
	"JITResponse":               JITResponse{},
	"Job":                       Job{},
	"LaunchedResponse":          LaunchedResponse{},
	"LeaseResponse":             LeaseResponse{},
	"MarkFailureRequest":        MarkFailureRequest{},
	"Range":                     Range{},
	"ReconcileRequest":          ReconcileRequest{},
	"ReconcileResponse":         ReconcileResponse{},
	"RecoverRunnerRequest":      RecoverRunnerRequest{},
	"RecoverRunnerResponse":     RecoverRunnerResponse{},
	"RegisterRequest":           RegisterRequest{},
	"RegisterResponse":          RegisterResponse{},
	"ReleaseRequest":            ReleaseRequest{},
	"RemoveRunnerRequest":       RemoveRunnerRequest{},
	"RenewRequest":              RenewRequest{},
	"RenewResponse":             RenewResponse{},
	"ResizeRequest":             ResizeRequest{},
	"TierSpec":                  TierSpec{},
	"TrustedRunnerGroupRequest": TrustedRunnerGroupRequest{},
	"UpgradeSpec":               UpgradeSpec{},
	"UsageRequest":              UsageRequest{},
	"WithdrawRequest":           WithdrawRequest{},
}

// firstPinnedVersion is the protocol version whose encoding was first recorded.
// Every version from here to Version keeps its directory while it is at or
// above MinVersion, so a deleted directory is a failure rather than a gap.
const firstPinnedVersion = 25

// direction is who decodes a body, and so how strictly an older peer's must
// still decode: the plane refuses unknown fields in a request, a node ignores
// them in a response.
type direction int

const (
	// insideOther travels only inside another wire type, whose fixtures carry it.
	insideOther direction = iota
	toPlane
	toNode
)

func directionOf(name string) direction {
	switch {
	case strings.HasSuffix(name, "Request"), name == "CommandResult":
		return toPlane
	case strings.HasSuffix(name, "Response"), name == "Command":
		return toNode
	default:
		return insideOther
	}
}

// fixtureKinds are the files each type is pinned as. .bools.json holds, for each
// boolean the type encodes, the populated encoding with only that boolean true:
// two booleans have only two values between them, so swapping which field feeds
// which key is invisible to any single sample. .empty.json is the Go zero value,
// every pointer, slice and map nil; .hollow.json allocates every pointer and
// leaves every slice and map empty but not nil; together with .zero.json they pin
// how nil, empty and zero values encode at every depth.
var fixtureKinds = []string{".json", ".zero.json", ".shape.txt", ".bools.json", ".empty.json", ".hollow.json"}

func wireDir(version int) string {
	return filepath.Join("testdata", "wire", "v"+strconv.Itoa(version))
}

// updating reports whether this run writes the current version's fixtures. Only
// the value 1 does, so BILLET_UPDATE_FIXTURES=0 cannot rewrite them by accident.
func updating(t *testing.T) bool {
	t.Helper()

	switch v := os.Getenv("BILLET_UPDATE_FIXTURES"); v {
	case "":
		return false
	case "1":
		return true
	default:
		t.Fatalf("BILLET_UPDATE_FIXTURES=%q: set it to 1 to write fixtures, or unset it", v)

		return false
	}
}

// render is the named type's fixture of the given kind, as the files hold it.
func render(t *testing.T, name, kind string) []byte {
	t.Helper()

	typ := reflect.TypeOf(wireTypes[name])

	if kind == ".shape.txt" {
		var lines []string
		shape(t, typ, name, 0, &lines)

		return []byte(strings.Join(lines, "\n") + "\n")
	}

	if kind == ".bools.json" {
		return renderBools(t, name, typ)
	}

	v := reflect.New(typ).Elem()

	switch kind {
	case ".empty.json":
	case ".hollow.json":
		fill(t, v, name, 0, fillMode{onlyTrue: allBools, hollow: true})
	default:
		fill(t, v, name, 0, fillMode{populate: kind == ".json", onlyTrue: allBools})
	}

	return encodeIndented(t, v.Interface())
}

// renderBools is the populated encoding once per boolean, with only that boolean
// true, keyed by its JSON path.
func renderBools(t *testing.T, name string, typ reflect.Type) []byte {
	t.Helper()

	var paths []string
	fill(t, reflect.New(typ).Elem(), name, 0, fillMode{populate: true, onlyTrue: allBools, bools: &paths})

	variants := map[string]json.RawMessage{}

	for _, path := range paths {
		v := reflect.New(typ).Elem()
		fill(t, v, name, 0, fillMode{populate: true, onlyTrue: path})

		body, err := json.Marshal(v.Interface())
		if err != nil {
			t.Fatalf("encode %s with only %s true: %v", name, path, err)
		}

		variants[path] = body
	}

	return encodeIndented(t, variants)
}

// allBools sets every boolean true in a populated fill.
const allBools = "\x00all"

// fillMode is what a fill writes. Populated, every scalar gets a value derived
// from its JSON path; otherwise every scalar stays zero. onlyTrue names the one
// boolean set true, or allBools. bools, when set, collects every boolean's path.
// hollow leaves every slice and map empty rather than giving it one element.
type fillMode struct {
	populate bool
	onlyTrue string
	bools    *[]string
	hollow   bool
}

func encodeIndented(t *testing.T, v any) []byte {
	t.Helper()

	compact, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}

	var indented bytes.Buffer
	if err := json.Indent(&indented, compact, "", "  "); err != nil {
		t.Fatalf("indent %T: %v", v, err)
	}

	return append(indented.Bytes(), '\n')
}

// maxDepth bounds a type that contains itself; reaching it is a failure, because
// a field left unfilled is a field nothing pins.
const maxDepth = 12

// fill sets v and everything reachable from it. Populated, every scalar gets a
// non-zero value derived from its JSON path, sized to its width, so every field
// appears, the sample survives a refactor that leaves the encoding alone, and
// TestEveryPopulatedSampleIsDistinct holds that no two fields share one.
// Unpopulated, every scalar stays zero while every pointer, slice and map is
// allocated, so the encoding shows which zero fields omitempty drops at every
// depth. It fails on anything it cannot set.
func fill(t *testing.T, v reflect.Value, path string, depth int, mode fillMode) {
	t.Helper()

	if depth > maxDepth {
		t.Fatalf("%s is deeper than %d levels, so the fill would leave it unpinned", path, maxDepth)
	}

	switch v.Kind() {
	case reflect.String:
		if mode.populate {
			v.SetString(path)
		}
	case reflect.Bool:
		if mode.bools != nil {
			*mode.bools = append(*mode.bools, path)
		}

		v.SetBool(mode.populate && (mode.onlyTrue == allBools || mode.onlyTrue == path))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if mode.populate {
			v.SetInt(int64(1 + uint64(seed(path))%uint64(span(v.Kind()))))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if mode.populate {
			v.SetUint(1 + uint64(seed(path))%uint64(span(v.Kind())))
		}
	case reflect.Float32, reflect.Float64:
		if mode.populate {
			// Whole eighths below 2^20 are exact in a float32's mantissa.
			v.SetFloat(float64(1+seed(path)%(1<<20)) / 8)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(t, v.Elem(), path, depth+1, mode)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if mode.populate {
				v.SetBytes([]byte(path))
			} else {
				v.SetBytes([]byte{})
			}

			return
		}

		if mode.hollow {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))

			return
		}

		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(t, v.Index(0), path+"[0]", depth+1, mode)
	case reflect.Array:
		for i := range v.Len() {
			fill(t, v.Index(i), path+"["+strconv.Itoa(i)+"]", depth+1, mode)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))

		if mode.hollow {
			return
		}

		key := reflect.New(v.Type().Key()).Elem()
		fill(t, key, path+".key", depth+1, fillMode{populate: true, onlyTrue: allBools})
		elem := reflect.New(v.Type().Elem()).Elem()
		fill(t, elem, path+".value", depth+1, mode)
		v.SetMapIndex(key, elem)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			if mode.populate {
				v.Set(reflect.ValueOf(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC).
					Add(time.Duration(seed(path)%(1<<30)) * time.Second)))
			}

			return
		}

		for i := range v.NumField() {
			field := v.Type().Field(i)
			name, _, skip := jsonField(field)

			switch {
			case skip:
				continue
			case !field.IsExported() && field.Anonymous:
				t.Fatalf("%s embeds the unexported %s, whose exported fields JSON promotes and this "+
					"fill cannot reach", path, field.Type)
			case !field.IsExported():
				continue
			case field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct:
				// Promoted: its fields are encoded at this level, under this path.
				fill(t, v.Field(i), path, depth+1, mode)
			default:
				if name == "" {
					name = field.Name
				}

				fill(t, v.Field(i), path+"."+name, depth+1, mode)
			}
		}
	default:
		t.Fatalf("%s is a %s, which this fill cannot set and the wire should not carry", path, v.Kind())
	}
}

// span is how many distinct non-zero samples an integer kind holds, capped so a
// sample of int or int64 is still a sample an int32 could hold.
func span(kind reflect.Kind) int64 {
	switch kind {
	case reflect.Int8:
		return 1<<7 - 1
	case reflect.Uint8:
		return 1<<8 - 1
	case reflect.Int16:
		return 1<<15 - 1
	case reflect.Uint16:
		return 1<<16 - 1
	default:
		return 1<<31 - 1
	}
}

// NO TWO FIELDS SHARE A SAMPLE: a type whose populated encoding repeats a value
// cannot show two fields' keys swapped, which is the failure the fill exists to
// expose. A collision is two paths whose hashes meet within a narrow width, and
// it is reported rather than tolerated.
func TestEveryPopulatedSampleIsDistinct(t *testing.T) {
	t.Parallel()

	for name := range wireTypes {
		var decoded any
		if err := json.Unmarshal(render(t, name, ".json"), &decoded); err != nil {
			t.Fatal(err)
		}

		seen := map[string]string{}

		var walk func(v any, at string)
		walk = func(v any, at string) {
			switch x := v.(type) {
			case map[string]any:
				for k, child := range x {
					walk(child, at+"."+k)
				}
			case []any:
				for i, child := range x {
					walk(child, at+"["+strconv.Itoa(i)+"]")
				}
			case bool, nil:
			default:
				value := fmt.Sprint(x)
				if other, dup := seen[value]; dup {
					t.Errorf("%s: %s and %s share the sample %s, so their keys could swap unseen",
						name, other, at, value)
				}
				seen[value] = at
			}
		}
		walk(decoded, name)
	}
}

// shape appends one line per JSON path under typ: the path in JSON names and the
// Go kind and width behind it. Package and type names stay out of it, so moving a
// type without changing what it encodes leaves the shape alone.
func shape(t *testing.T, typ reflect.Type, path string, depth int, lines *[]string) {
	t.Helper()

	if depth > maxDepth {
		t.Fatalf("%s is deeper than %d levels", path, maxDepth)
	}

	switch typ.Kind() {
	case reflect.Pointer:
		*lines = append(*lines, path+" pointer")
		shape(t, typ.Elem(), path, depth+1, lines)
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			*lines = append(*lines, path+" bytes")

			return
		}

		*lines = append(*lines, path+" slice")
		shape(t, typ.Elem(), path+"[]", depth+1, lines)
	case reflect.Array:
		*lines = append(*lines, path+" array "+strconv.Itoa(typ.Len()))
		shape(t, typ.Elem(), path+"[]", depth+1, lines)
	case reflect.Map:
		*lines = append(*lines, path+" map "+typ.Key().Kind().String())
		shape(t, typ.Elem(), path+"{}", depth+1, lines)
	case reflect.Struct:
		if typ == reflect.TypeFor[time.Time]() {
			*lines = append(*lines, path+" time")

			return
		}

		for i := range typ.NumField() {
			field := typ.Field(i)
			name, opts, skip := jsonField(field)

			switch {
			case skip:
				continue
			case field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct:
				shape(t, field.Type, path, depth+1, lines)
			case !field.IsExported():
				continue
			default:
				if name == "" {
					name = field.Name
				}

				key := path + "." + name
				if opts != "" {
					*lines = append(*lines, key+" tag "+opts)
				}

				shape(t, field.Type, key, depth+1, lines)
			}
		}
	default:
		*lines = append(*lines, path+" "+typ.Kind().String())
	}
}

// jsonField is a struct field's JSON name, its tag options, and whether JSON
// skips it.
func jsonField(field reflect.StructField) (name, opts string, skip bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", "", true
	}

	name, opts, _ = strings.Cut(tag, ",")

	return name, opts, false
}

func seed(path string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(path))

	return h.Sum32()
}

// manifest is the MANIFEST a version directory carries: every fixture file with
// its sha256, so a fixture deleted or rewritten later fails rather than quietly
// taking its coverage with it.
func manifest(t *testing.T, dir string) []byte {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var lines []string

	for _, e := range entries {
		// Only fixtures: a temporary file another updater has not yet renamed
		// into place begins with a dot.
		if e.Name() == "MANIFEST" || strings.HasPrefix(e.Name(), ".") {
			continue
		}

		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}

		sum := sha256.Sum256(body)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+e.Name())
	}

	slices.Sort(lines)

	return []byte(strings.Join(lines, "\n") + "\n")
}

// THE MANIFEST COUNTS FIXTURES, NOT AN UPDATER'S TEMPORARY FILES: one another
// process has not yet renamed into place would otherwise be recorded and then
// vanish.
func TestTheManifestCountsOnlyFixtures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Range.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	clean := manifest(t, dir)

	if err := os.WriteFile(filepath.Join(dir, ".Range.json.12345"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := manifest(t, dir); !bytes.Equal(got, clean) {
		t.Fatalf("a temporary file changed the manifest:\n%s\nwant\n%s", got, clean)
	}
}

// writeAtomic writes body to path through a temporary file of its own in the same
// directory, so no reader ever sees half a fixture and two updating processes
// never write through one temporary name.
func writeAtomic(t *testing.T, path string, body []byte) {
	t.Helper()

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		t.Fatal(err)
	}

	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
}

// THE CURRENT VERSION'S ENCODING IS PINNED, BYTE FOR BYTE. A change to any of
// these bytes, a renamed Go field in alloc.Lease included, is a wire change:
// bump Version, decide what an older peer does, and write the new version's
// fixtures with BILLET_UPDATE_FIXTURES=1 (billet-add-wire-change). Fixtures are
// written only for the current version, never for an earlier one.
func TestTheWireEncodingIsPinned(t *testing.T) {
	update := updating(t)
	if !update {
		t.Parallel()
	}

	dir := wireDir(Version)

	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for name := range wireTypes {
		for _, kind := range fixtureKinds {
			got := render(t, name, kind)
			path := filepath.Join(dir, name+kind)

			if update {
				writeAtomic(t, path, got)

				continue
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s has no %s fixture for protocol version %d (%v): a new type or a new "+
					"version needs its fixtures written with BILLET_UPDATE_FIXTURES=1", name, kind, Version, err)

				continue
			}

			if !bytes.Equal(got, want) {
				t.Errorf("%s%s no longer matches protocol version %d, which is a wire change: bump "+
					"Version, decide what an older peer does, and write the new version's fixtures "+
					"(billet-add-wire-change)\n--- version %d\n%s\n--- now\n%s", name, kind, Version, Version, want, got)
			}
		}
	}

	if update {
		writeAtomic(t, filepath.Join(dir, "MANIFEST"), manifest(t, dir))
	}
}

// EVERY SUPPORTED VERSION'S BODIES STILL DECODE, AND NOTHING IN THEM IS LOST. For
// each version from firstPinnedVersion (or MinVersion, if higher) to Version, the
// directory must exist and match its MANIFEST, and each body an older peer sends
// must decode as its receiver decodes it (strictly at the plane, leniently at a
// node) and re-encode carrying every field it had. A field added since is
// allowed; one lost or retyped is not. A version below MinVersion is no longer
// spoken, and its fixtures may be retired with it.
func TestEverySupportedVersionsBodiesStillDecode(t *testing.T) {
	if updating(t) {
		t.Skip("writing fixtures: this run checks nothing it is about to rewrite")
	}

	t.Parallel()

	for version := max(firstPinnedVersion, MinVersion); version <= Version; version++ {
		dir := wireDir(version)

		recorded, err := os.ReadFile(filepath.Join(dir, "MANIFEST"))
		if err != nil {
			t.Errorf("protocol version %d's fixtures are missing (%v): a supported version keeps its "+
				"directory until MinVersion passes it", version, err)

			continue
		}

		if got := manifest(t, dir); !bytes.Equal(got, recorded) {
			t.Errorf("%s no longer matches its MANIFEST: a fixture of a released version was deleted, "+
				"added or rewritten\n--- MANIFEST\n%s\n--- files\n%s", dir, recorded, got)
		}

		for _, problem := range incomplete(t, dir) {
			t.Error(problem)
		}

		shapes, err := filepath.Glob(filepath.Join(dir, "*.shape.txt"))
		if err != nil {
			t.Fatal(err)
		}

		for _, path := range shapes {
			name := strings.TrimSuffix(filepath.Base(path), ".shape.txt")
			if _, known := wireTypes[name]; known {
				for _, problem := range shapeLost(t, path, name) {
					t.Error(problem)
				}
			}
		}

		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}

		for _, path := range files {
			name, _, _ := strings.Cut(filepath.Base(path), ".")

			zero, known := wireTypes[name]
			if !known {
				t.Errorf("%s pins %s, which the wire no longer has, while version %d is still supported",
					path, name, version)

				continue
			}

			dir := directionOf(name)
			if dir == insideOther {
				continue
			}

			stillCarried(t, path, reflect.TypeOf(zero), dir == toPlane)
		}
	}
}

// incomplete names what a version directory lacks: any pinned type at all, or
// one of the three fixtures of a type it pins. A directory emptied consistently
// with its MANIFEST would otherwise check nothing.
func incomplete(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]map[string]bool{}

	for _, e := range entries {
		for _, kind := range fixtureKinds {
			if name, ok := strings.CutSuffix(e.Name(), kind); ok && !strings.Contains(name, ".") {
				if kinds[name] == nil {
					kinds[name] = map[string]bool{}
				}

				kinds[name][kind] = true
			}
		}
	}

	var problems []string

	if len(kinds) == 0 {
		problems = append(problems, dir+" pins no wire type, so it proves nothing about the version it names")
	}

	for name, have := range kinds {
		for _, kind := range fixtureKinds {
			if !have[kind] {
				problems = append(problems, fmt.Sprintf("%s pins %s without its %s fixture", dir, name, kind))
			}
		}
	}

	slices.Sort(problems)

	return problems
}

// shapeLost names each line of an older version's shape, a JSON path and the Go
// kind and width behind it, that the current shape no longer has: a field
// narrowed or retyped since then decodes the older version's sample and refuses
// a real body outside the new range. A path added since is allowed, and so is a
// change of tag, which the bodies themselves check.
func shapeLost(t *testing.T, path, name string) []string {
	t.Helper()

	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	before, after := kindsByPath(string(old)), kindsByPath(string(render(t, name, ".shape.txt")))

	var lost []string

	for at, was := range before {
		now, ok := after[at]

		switch {
		case !ok:
			lost = append(lost, fmt.Sprintf("%s: %s is no longer encoded, so a peer of that version sends "+
				"a field this binary does not know", path, at))
		case !compatibleKinds(was, now):
			lost = append(lost, fmt.Sprintf("%s: %s was %s and is now %s, a range that does not hold every "+
				"value a peer of that version can send", path, at, strings.Join(was, " "), strings.Join(now, " ")))
		}
	}

	slices.Sort(lost)

	return lost
}

// kindsByPath groups a shape's kind lines by JSON path, in order; tag lines are
// left to the bodies, which show what a tag does.
func kindsByPath(shape string) map[string][]string {
	out := map[string][]string{}

	for line := range strings.SplitSeq(shape, "\n") {
		at, kind, ok := strings.Cut(line, " ")
		if ok && !strings.HasPrefix(kind, "tag ") {
			out[at] = append(out[at], kind)
		}
	}

	return out
}

// compatibleKinds reports whether every value of the kinds was can still be held
// by now: the same kinds, or an integer or float widened to one whose range
// contains the old one (int is 64 bits on every platform billet supports).
func compatibleKinds(was, now []string) bool {
	if len(was) != len(now) {
		return false
	}

	for i := range was {
		if was[i] == now[i] {
			continue
		}

		lo, hi, ok := numericRange(was[i])
		nlo, nhi, nok := numericRange(now[i])

		// WITHIN ONE FAMILY ONLY: a float64 spans every int64 and holds none above
		// 2^53 exactly, so a range test alone would accept a lossy change.
		if !ok || !nok || isFloat(was[i]) != isFloat(now[i]) || nlo > lo || nhi < hi {
			return false
		}
	}

	return true
}

func isFloat(kind string) bool { return kind == "float32" || kind == "float64" }

// WIDENING WITHIN A FAMILY IS COMPATIBLE; CROSSING FAMILIES IS NOT. No wire field
// is a float today, so the cross-family rule is driven here: a float64's range
// spans every int64 and holds none above 2^53 exactly.
func TestCompatibleKindsWidenOnlyWithinAFamily(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		was, now string
		want     bool
	}{
		{"int32", "int64", true},
		{"int", "int64", true},
		{"uint8", "uint16", true},
		{"uint32", "int64", true},
		{"float32", "float64", true},
		{"int64", "int32", false},
		{"uint64", "int64", false},
		{"int64", "float64", false},
		{"int32", "float64", false},
		{"float64", "int64", false},
		{"string", "int64", false},
	} {
		if got := compatibleKinds([]string{tc.was}, []string{tc.now}); got != tc.want {
			t.Errorf("compatibleKinds(%s, %s) = %v, want %v", tc.was, tc.now, got, tc.want)
		}
	}
}

// numericRange is the range of a numeric kind, as float64 bounds that order the
// kinds correctly even where they round.
func numericRange(kind string) (float64, float64, bool) {
	switch kind {
	case "int8":
		return -1 << 7, 1<<7 - 1, true
	case "int16":
		return -1 << 15, 1<<15 - 1, true
	case "int32":
		return -1 << 31, 1<<31 - 1, true
	case "int", "int64":
		return -1 << 63, 1<<63 - 1, true
	case "uint8":
		return 0, 1<<8 - 1, true
	case "uint16":
		return 0, 1<<16 - 1, true
	case "uint32":
		return 0, 1<<32 - 1, true
	case "uint", "uint64":
		return 0, 1<<64 - 1, true
	case "float32":
		return -3.4e38, 3.4e38, true
	case "float64":
		return -1.7e308, 1.7e308, true
	default:
		return 0, 0, false
	}
}

// THE CHECKS AN OLDER VERSION'S DIRECTORY GETS CAN FAIL. Only the current version
// is recorded today, where they pass trivially, so they are driven here against a
// healthy copy and against the damages they exist for.
func TestAnOlderVersionsDirectoryIsCheckedForShapeAndCompleteness(t *testing.T) {
	t.Parallel()

	stage := func(t *testing.T, edit func(dir string)) string {
		t.Helper()

		dir := t.TempDir()
		for _, kind := range fixtureKinds {
			body, err := os.ReadFile(filepath.Join(wireDir(Version), "HeartbeatRequest"+kind))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "HeartbeatRequest"+kind), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		edit(dir)

		return dir
	}
	shapeOf := func(dir string) string { return filepath.Join(dir, "HeartbeatRequest.shape.txt") }

	healthy := stage(t, func(string) {})
	if got := incomplete(t, healthy); len(got) != 0 {
		t.Fatalf("a complete directory is reported incomplete: %v", got)
	}
	if got := shapeLost(t, shapeOf(healthy), "HeartbeatRequest"); len(got) != 0 {
		t.Fatalf("an unchanged shape is reported lost: %v", got)
	}

	noZero := stage(t, func(dir string) {
		if err := os.Remove(filepath.Join(dir, "HeartbeatRequest.zero.json")); err != nil {
			t.Fatal(err)
		}
	})
	if got := incomplete(t, noZero); len(got) != 1 || !strings.Contains(got[0], ".zero.json") {
		t.Fatalf("a missing zero fixture is reported as %v", got)
	}

	if got := incomplete(t, t.TempDir()); len(got) != 1 || !strings.Contains(got[0], "pins no wire type") {
		t.Fatalf("an empty directory is reported as %v", got)
	}

	withShape := func(body string) []string {
		t.Helper()

		dir := stage(t, func(dir string) {
			if err := os.WriteFile(shapeOf(dir), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		})

		return shapeLost(t, shapeOf(dir), "HeartbeatRequest")
	}

	// THE CURRENT FIELD IS int64. A uint64 range does not fit in it; an int32
	// range does, so widening to int64 since that version is compatible.
	if got := withShape("HeartbeatRequest.epoch uint64\n"); len(got) != 1 ||
		!strings.Contains(got[0], "HeartbeatRequest.epoch was uint64 and is now int64") {
		t.Fatalf("a narrowed range is reported as %v", got)
	}
	if got := withShape("HeartbeatRequest.epoch int32\n"); len(got) != 0 {
		t.Fatalf("a widened range is reported lost: %v", got)
	}
	// A float32's range fits inside nothing an int64 holds exactly.
	if got := withShape("HeartbeatRequest.epoch float32\n"); len(got) != 1 ||
		!strings.Contains(got[0], "was float32 and is now int64") {
		t.Fatalf("a change between number families is reported as %v", got)
	}
	if got := withShape("HeartbeatRequest.gone string\n"); len(got) != 1 ||
		!strings.Contains(got[0], "HeartbeatRequest.gone is no longer encoded") {
		t.Fatalf("a removed path is reported as %v", got)
	}
}

// stillCarried checks each body the fixture at path holds: the file itself, or
// for a .bools.json each variant inside it.
func stillCarried(t *testing.T, path string, typ reflect.Type, strict bool) {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(path, ".bools.json") {
		bodyCarried(t, path, body, typ, strict)

		return
	}

	var variants map[string]json.RawMessage
	if err := json.Unmarshal(body, &variants); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	for at, variant := range variants {
		bodyCarried(t, path+" ("+at+" true)", variant, typ, strict)
	}
}

// bodyCarried decodes body into typ as its receiver would, and checks the
// re-encoding carries every field of the original with its value.
func bodyCarried(t *testing.T, path string, body []byte, typ reflect.Type, strict bool) {
	t.Helper()

	v := reflect.New(typ)
	dec := json.NewDecoder(bytes.NewReader(body))
	if strict {
		dec.DisallowUnknownFields()
	}

	if err := dec.Decode(v.Interface()); err != nil {
		t.Errorf("%s no longer decodes as its receiver decodes it: %v", path, err)

		return
	}

	again, err := json.Marshal(v.Elem().Interface())
	if err != nil {
		t.Fatal(err)
	}

	// EXACT NUMBERS: decoded into float64, 9007199254740993 and 9007199254740992
	// compare equal, which is the loss this comparison is for.
	before, after := decodeExact(t, body), decodeExact(t, again)

	if lost := missing(before, after, "$"); lost != "" {
		t.Errorf("%s: %s, so a peer of that version loses it on the way", path, lost)
	}
}

// decodeExact decodes JSON with numbers kept as their exact text.
func decodeExact(t *testing.T, body []byte) any {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}

	return v
}

// NO WIRE TYPE ENCODES OR DECODES ITSELF. A MarshalJSON or MarshalText anywhere in
// the graph decides its own bytes, which a fill that sets fields cannot know for a
// nil, an empty or a zero value; an UnmarshalJSON or UnmarshalText decides what it
// accepts, and can refuse an older peer's ordinary value that no fixture sample
// reaches. time.Time is the one exception, and the fixtures carry it.
func TestNoWireTypeEncodesItself(t *testing.T) {
	t.Parallel()

	selfCoding := []reflect.Type{
		reflect.TypeFor[json.Marshaler](),
		reflect.TypeFor[json.Unmarshaler](),
		reflect.TypeFor[interface{ MarshalText() ([]byte, error) }](),
		reflect.TypeFor[interface{ UnmarshalText(text []byte) error }](),
	}
	seen := map[reflect.Type]bool{}

	var walk func(typ reflect.Type, at string)
	walk = func(typ reflect.Type, at string) {
		if seen[typ] {
			return
		}
		seen[typ] = true

		if typ == reflect.TypeFor[time.Time]() {
			return
		}

		for _, candidate := range []reflect.Type{typ, reflect.PointerTo(typ)} {
			for _, iface := range selfCoding {
				if candidate.Implements(iface) {
					t.Errorf("%s (%s) implements %s, so its fixtures cannot show what it sends or accepts",
						at, typ, iface)
				}
			}
		}

		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem(), at)
		case reflect.Map:
			walk(typ.Key(), at+".key")
			walk(typ.Elem(), at+".value")
		case reflect.Struct:
			for i := range typ.NumField() {
				if field := typ.Field(i); field.IsExported() || field.Anonymous {
					walk(field.Type, at+"."+field.Name)
				}
			}
		default:
		}
	}

	for name, zero := range wireTypes {
		walk(reflect.TypeOf(zero), name)
	}
}

// missing names the first field of before that after does not carry with the
// same value, or is empty when after carries all of before.
func missing(before, after any, path string) string {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			return path + " is no longer an object"
		}

		keys := make([]string, 0, len(b))
		for k := range b {
			keys = append(keys, k)
		}

		slices.Sort(keys)

		for _, k := range keys {
			av, ok := a[k]
			if !ok {
				return path + "." + k + " is no longer encoded"
			}

			if lost := missing(b[k], av, path+"."+k); lost != "" {
				return lost
			}
		}

		return ""
	case []any:
		a, ok := after.([]any)
		if !ok || len(a) != len(b) {
			return path + " is no longer the same list"
		}

		for i := range b {
			if lost := missing(b[i], a[i], path+"["+strconv.Itoa(i)+"]"); lost != "" {
				return lost
			}
		}

		return ""
	default:
		if !reflect.DeepEqual(before, after) {
			return fmt.Sprintf("%s was %v and is now %v", path, before, after)
		}

		return ""
	}
}

// AN ADDED FIELD IS ALLOWED; A LOST OR CHANGED ONE IS NAMED. Every fixture today
// matches the current encoding, so this is the only place the comparison an
// older version's body gets is made to fail.
func TestMissingNamesWhatALaterEncodingLost(t *testing.T) {
	t.Parallel()

	decode := func(s string) any {
		t.Helper()

		return decodeExact(t, []byte(s))
	}

	for _, tc := range []struct {
		before, after, want string
	}{
		{`{"a":1,"b":{"c":"x"}}`, `{"a":1,"b":{"c":"x","d":2},"e":true}`, ""},
		{`{"a":1,"b":2}`, `{"a":1}`, "$.b is no longer encoded"},
		{`{"b":{"c":"x"}}`, `{"b":{"c":"y"}}`, "$.b.c was x and is now y"},
		{`{"l":[1,2]}`, `{"l":[1]}`, "$.l is no longer the same list"},
		{`{"o":{"c":1}}`, `{"o":"flat"}`, "$.o is no longer an object"},
		// Equal as float64, and a different value on the wire.
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`, "$.n was 9007199254740993 and is now 9007199254740992"},
	} {
		if got := missing(decode(tc.before), decode(tc.after), "$"); got != tc.want {
			t.Errorf("missing(%s, %s) = %q, want %q", tc.before, tc.after, got, tc.want)
		}
	}
}

// notWire is every exported type declaration in nodeapi that is not a struct the
// wire encodes, with why. A declaration that is neither here nor in wireTypes
// fails, so a new body cannot travel unpinned by being declared some other way.
var notWire = map[string]string{
	"CommandKind": "a string the Command struct carries",
}

// EVERY EXPORTED TYPE IS CLASSIFIED, AND EVERY PINNED ENTRY IS THE TYPE ITS NAME
// SAYS: a struct declared as `type X Y` or as an alias, or an entry naming one
// type and holding another, would otherwise pin the wrong bytes or none.
func TestEveryWireTypeIsPinned(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	declared := map[string]ast.Expr{}

	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}

			for _, spec := range gen.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() {
					declared[ts.Name.Name] = ts.Type
				}
			}
		}
	}

	if len(declared) == 0 {
		t.Fatal("no exported types found, so this check proves nothing")
	}

	for name, expr := range declared {
		_, pinned := wireTypes[name]
		_, excused := notWire[name]

		switch {
		case pinned && excused:
			t.Errorf("%s is both pinned and excused", name)
		case pinned:
			if _, isStruct := expr.(*ast.StructType); !isStruct {
				t.Errorf("%s is pinned but is not declared as a struct literal here", name)
			}
		case excused:
		default:
			t.Errorf("%s is an exported type in nodeapi that is neither pinned in wireTypes nor "+
				"excused in notWire", name)
		}
	}

	self := reflect.TypeFor[Command]().PkgPath()

	for name, zero := range wireTypes {
		typ := reflect.TypeOf(zero)
		if typ.Name() != name || typ.PkgPath() != self {
			t.Errorf("wireTypes[%q] holds %s.%s", name, typ.PkgPath(), typ.Name())
		}

		if _, ok := declared[name]; !ok {
			t.Errorf("wireTypes names %s, which nodeapi no longer declares", name)
		}
	}

	for name := range notWire {
		if _, ok := declared[name]; !ok {
			t.Errorf("notWire excuses %s, which nodeapi no longer declares", name)
		}
	}
}

// ONE WRITER FOR THE DIRECTIONS: a type the rule above cannot place is one whose
// history is never checked.
func TestEveryTopLevelBodyHasADirection(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Range", "TierSpec", "UpgradeSpec", "CacheAuthority", "Job"} {
		if directionOf(name) != insideOther {
			t.Errorf("%s travels only inside another type and should not have a direction", name)
		}
	}

	for name := range wireTypes {
		if directionOf(name) == insideOther &&
			!slices.Contains([]string{"Range", "TierSpec", "UpgradeSpec", "CacheAuthority", "Job"}, name) {
			t.Errorf("%s has no direction, so no older peer's body of it is ever checked", name)
		}
	}
}
