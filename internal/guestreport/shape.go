package guestreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// checkShape walks the inflated JSON token by token against the schema before the
// typed decode allocates anything for it: every key is one the schema names, once
// in its object and spelled as the encoder spells it; every value is the kind of
// value the schema puts there; every list is within its bound, and the process
// rows within theirs in all. A document of five million empty objects inflates
// within its bound and would otherwise cost hundreds of megabytes to refuse, under
// any key, or under one key repeated.
func checkShape(raw []byte, s *shape) error {
	dec := json.NewDecoder(bytes.NewReader(raw))

	var (
		stack []frame
		rows  int
		begun bool
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return refuse(ErrMalformed, "")
		}

		var want field

		switch {
		case len(stack) == 0:
			if begun {
				return refuse(ErrMalformed, "")
			}

			begun, want = true, field{kind: kindObject, node: s.root}
		case stack[len(stack)-1].isObject() && stack[len(stack)-1].wantKey:
			top := &stack[len(stack)-1]
			if tok == json.Delim('}') {
				stack = done(stack[:len(stack)-1])

				continue
			}

			if err := top.takeKey(tok); err != nil {
				return err
			}

			continue
		case stack[len(stack)-1].isObject():
			want = stack[len(stack)-1].pending
		default:
			top := &stack[len(stack)-1]
			if tok == json.Delim(']') {
				stack = done(stack[:len(stack)-1])

				continue
			}

			top.n++
			if top.n > top.list.limit {
				return refuse(ErrTooMany, location(stack))
			}

			if top.list.rows {
				rows++
				if rows > s.rows {
					return refuse(ErrTooMany, "processes")
				}
			}

			want = *top.list.elem
		}

		next, opened, err := open(tok, want)
		if err != nil {
			return err
		}

		if !opened {
			stack = done(stack)

			continue
		}

		stack = append(stack, next)
	}
}

// open checks that tok begins the kind of value want describes, and returns the
// frame it opens, if it opens one: a scalar opens none.
func open(tok json.Token, want field) (frame, bool, error) {
	delim, isDelim := tok.(json.Delim)

	switch want.kind {
	case kindObject, kindList:
		opener := json.Delim('{')
		if want.kind == kindList {
			opener = '['
		}

		switch {
		case tok == nil:
			// The encoder writes no null: a list it leaves out, an object it
			// always writes.
			return frame{}, false, refuse(ErrNotCanonical, "")
		case !isDelim || delim != opener:
			return frame{}, false, refuse(ErrMalformed, "")
		case want.kind == kindObject:
			return frame{object: want.node, wantKey: true}, true, nil
		default:
			return frame{list: &want}, true, nil
		}
	default:
		if isDelim {
			return frame{}, false, refuse(ErrMalformed, "")
		}

		return frame{}, false, nil
	}
}

// done marks the value just closed finished: in an object, a key is due next.
func done(stack []frame) []frame {
	if len(stack) > 0 && stack[len(stack)-1].isObject() {
		stack[len(stack)-1].wantKey = true
	}

	return stack
}

// location is where the list on top of the stack is, as a refusal names it: the
// list keys that lead to it and the index of each element on the way, all the
// schema's own names.
func location(stack []frame) string {
	var b strings.Builder

	for i := range stack {
		l := stack[i].list
		if l == nil {
			continue
		}

		if b.Len() > 0 {
			b.WriteByte('.')
		}

		b.WriteString(l.name)

		if i < len(stack)-1 {
			b.WriteString("[" + strconv.Itoa(stack[i].n-1) + "]")
		}
	}

	return b.String()
}

// frame is one open container: an object, with the keys it has held and the field
// its next value fills, or a list, with how many elements it has held.
type frame struct {
	object  *node
	seen    uint64
	pending field
	wantKey bool

	list *field
	n    int
}

func (f *frame) isObject() bool { return f.object != nil }

// takeKey checks tok as the object's next key: one the schema names, spelled as the
// encoder spells it, and not one the object has held already.
func (f *frame) takeKey(tok json.Token) error {
	key, ok := tok.(string)
	if !ok {
		return refuse(ErrMalformed, "")
	}

	i, ok := f.object.index[key]
	if !ok {
		for name := range f.object.index {
			// encoding/json would fill this field from a key in another case;
			// the encoder never writes one.
			if strings.EqualFold(key, name) {
				return refuse(ErrNotCanonical, "")
			}
		}

		return refuse(ErrUnknownField, "")
	}

	if f.seen&(1<<i) != 0 {
		return refuse(ErrNotCanonical, "")
	}

	f.seen |= 1 << i
	f.pending, f.wantKey = f.object.fields[i], false

	return nil
}

// The kinds of value the schema holds.
const (
	kindScalar = iota
	kindObject
	kindList
)

// field is what one key of the schema holds: a scalar, an object, or a list of
// elem, within limit elements, whose name is the key; rows marks the list of
// process rows, which is also bounded in all.
type field struct {
	name  string
	kind  int
	node  *node
	elem  *field
	limit int
	rows  bool
}

// node is an object of the schema: its fields, by key.
type node struct {
	index  map[string]int
	fields []field
}

// shape is the schema a value is held to, and its bound on process rows in all.
type shape struct {
	root *node
	rows int
}

var (
	batchShape = &shape{root: schemaOf(reflect.TypeFor[Batch](), "", map[string]int{
		"samples": MaxBatchSamples, "processes": MaxBatchProcessSamples, "processes.rows": MaxProcessRows,
		"steps": MaxBatchSteps, "tests": MaxBatchSuites, "tests.failed": MaxFailedNames,
	}), rows: MaxBatchProcessSamples * MaxProcessRows}
	reportShape = &shape{root: schemaOf(reflect.TypeFor[Report](), "", map[string]int{
		"samples": MaxReportSamples, "processes": MaxReportProcessSamples, "processes.rows": MaxProcessRows,
		"steps": MaxReportSteps, "tests": MaxReportSuites, "tests.failed": MaxFailedNames,
		"gaps": MaxReportGaps,
	}), rows: MaxReportRows}
)

// schemaOf is the schema of a struct type as encoding/json reads it: each field by
// its JSON name, an embedded struct's fields as the struct's own. A list the
// limits do not name holds nothing (TestEveryListOfTheSchemaHasABound).
func schemaOf(t reflect.Type, path string, limits map[string]int) *node {
	n := &node{index: map[string]int{}}

	var add func(reflect.Type)

	add = func(t reflect.Type) {
		for i := range t.NumField() {
			sf := t.Field(i)
			if sf.Anonymous {
				add(sf.Type)

				continue
			}

			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			n.index[name] = len(n.fields)
			n.fields = append(n.fields, fieldOf(sf.Type, name, path+name, limits))
		}
	}

	add(t)

	return n
}

func fieldOf(t reflect.Type, name, path string, limits map[string]int) field {
	switch t.Kind() {
	case reflect.Struct:
		return field{name: name, kind: kindObject, node: schemaOf(t, path+".", limits)}
	case reflect.Slice:
		elem := field{kind: kindScalar}
		if t.Elem().Kind() == reflect.Struct {
			elem = field{kind: kindObject, node: schemaOf(t.Elem(), path+".", limits)}
		}

		return field{name: name, kind: kindList, elem: &elem, limit: limits[path], rows: path == "processes.rows"}
	default:
		return field{name: name, kind: kindScalar}
	}
}
