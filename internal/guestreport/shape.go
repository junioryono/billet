package guestreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// shape is how many elements each list of the schema may hold, by its path of keys,
// and how many process rows a value may hold in all. checkShape holds the JSON's
// tokens to it before the typed decode allocates a slice for any list: a document
// of five million empty objects inflates within its bound and would otherwise cost
// hundreds of megabytes to refuse.
type shape struct {
	lists map[string]int
	rows  int
}

// maxDepth is the deepest the schema nests (a report, a list of process samples,
// one of them, its rows, one row) with room to spare; the scanner under
// json.Decoder.Token sets no depth of its own.
const maxDepth = 8

var (
	batchShape = shape{lists: map[string]int{
		"samples": MaxBatchSamples, "processes": MaxBatchProcessSamples, "processes.rows": MaxProcessRows,
		"steps": MaxBatchSteps, "tests": MaxBatchSuites, "tests.failed": MaxFailedNames,
	}, rows: MaxBatchProcessSamples * MaxProcessRows}
	reportShape = shape{lists: map[string]int{
		"samples": MaxReportSamples, "processes": MaxReportProcessSamples, "processes.rows": MaxProcessRows,
		"steps": MaxReportSteps, "tests": MaxReportSuites, "tests.failed": MaxFailedNames,
		"gaps": MaxReportGaps,
	}, rows: MaxReportRows}
)

// listKeys is every key that names a list. A key is matched as encoding/json
// matches it, without regard to case, so a key the typed decode would fill is a key
// counted here.
var listKeys = []string{"samples", "processes", "rows", "steps", "tests", "failed", "gaps"}

// frame is one open container: its path of keys, where it is (the path with each
// element's index, as a refusal names it), and for a list how many elements it has
// held, for an object the list a key last named and whether a key is due. Neither
// path holds a key the schema does not name.
type frame struct {
	path    string
	where   string
	list    bool
	n       int
	key     string
	wantKey bool
}

func checkShape(raw []byte, s shape) error {
	dec := json.NewDecoder(bytes.NewReader(raw))

	var (
		stack []*frame
		rows  int
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return refuse(ErrMalformed, "")
		}

		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}

		if top != nil && !top.list && top.wantKey {
			if tok == json.Delim('}') {
				stack = stack[:len(stack)-1]
				done(stack)

				continue
			}

			key, ok := tok.(string)
			if !ok {
				return refuse(ErrMalformed, "")
			}

			top.key, top.wantKey = listKey(key), false

			continue
		}

		if top != nil && top.list {
			if tok == json.Delim(']') {
				stack = stack[:len(stack)-1]
				done(stack)

				continue
			}

			top.n++
			if limit, ok := s.lists[top.path]; ok && top.n > limit {
				return refuse(ErrTooMany, top.where)
			}

			if top.path == "processes.rows" {
				rows++
				if rows > s.rows {
					return refuse(ErrTooMany, "processes")
				}
			}
		}

		switch tok {
		case json.Delim('['), json.Delim('{'):
			if len(stack) == maxDepth {
				return refuse(ErrMalformed, "")
			}

			path, where := child(top)
			stack = append(stack, &frame{path: path, where: where, list: tok == json.Delim('['),
				wantKey: tok == json.Delim('{')})
		default:
			done(stack)
		}
	}
}

// child is the path and the location of a container opened inside top: an element
// of a list takes the list's path, so its keys extend it, and its index; a value in
// an object takes the key that named it.
func child(top *frame) (string, string) {
	switch {
	case top == nil:
		return "", ""
	case top.list:
		return top.path, at(top.where, top.n-1)
	case top.path == "":
		return top.key, top.key
	default:
		return top.path + "." + top.key, top.where + "." + top.key
	}
}

// done marks a value finished: in an object, a key is due next.
func done(stack []*frame) {
	if len(stack) > 0 && !stack[len(stack)-1].list {
		stack[len(stack)-1].wantKey = true
	}
}

// listKey is the list a key names, or "-" for a key that names none, under which
// nothing is a list of the schema's.
func listKey(key string) string {
	for _, k := range listKeys {
		if strings.EqualFold(key, k) {
			return k
		}
	}

	return "-"
}
