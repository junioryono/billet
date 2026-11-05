package setup

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The kinds of thing the command says about a workflow it read. A rewrite is
// listed too, so the report is the whole account of every `runs-on`.
const (
	findRewritten  = "rewritten"
	findOnBillet   = "already on billet"
	findExpression = "an expression, left alone"
	findMatrix     = "a matrix value, left alone"
	findGroup      = "a runner group, left alone"
	findList       = "a list of several labels, left alone"
	findCaller     = "a label passed to a reusable workflow, left alone"
	findCall       = "a call to a reusable workflow, which has no runs-on of its own"
	findNoTier     = "no tier for the label, left alone"
	findUnknown    = "not a label this command knows, left alone"
	findCouldNot   = "could not be edited in place, left alone"
	findRightsize  = "a recommendation not applied"
	findUnreadable = "a file that could not be read, left alone"
)

// maxWorkflowSize bounds the read of one workflow; GitHub refuses a larger one.
const maxWorkflowSize = 4 << 20

// finding is one thing said about one place in one workflow. Text is what the
// workflow wrote, and is quoted wherever it is printed.
type finding struct {
	Kind, File string
	Line       int
	Job        string
	Text       string
	// Detail is the rest of the sentence, which never holds a string the workflow
	// chose without quoting it.
	Detail string
}

// edit replaces [start, end) of a file's bytes with text, which writes value into
// node: the label itself, quoted where a bare one would read as something else.
type edit struct {
	start, end int
	text       string
	node       *yaml.Node
	value      string
}

// workflowRewrite is what one file's read comes to: its findings and the bytes it
// would hold.
type workflowRewrite struct {
	findings []finding
	edits    []edit
	// edited is every value an edit replaces, so no second edit replaces it again:
	// two edits of one span would splice the second into the first's bytes.
	edited map[*yaml.Node]bool
	before []byte
	after  []byte
}

// jobRunsOn is a job already on one of this deployment's tiers, as a recommendation
// finds it: its key, the name GitHub shows for it (empty when only GitHub can work it
// out), whether it has a matrix, and the scalar that names the tier.
type jobRunsOn struct {
	key, display string
	matrix       bool
	node         *yaml.Node
}

// rewriteWorkflow reads one workflow and decides every `runs-on` in it. The bytes
// outside an edited value are the file's own: nothing is re-serialized, so comments,
// order and formatting survive by construction.
func rewriteWorkflow(file string, data []byte, chooser *tierChooser, recommend *rightsizeEntries) workflowRewrite {
	out := workflowRewrite{before: data, after: data, edited: map[*yaml.Node]bool{}}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		// NOT QUOTED: a parser's message quotes the file, and the file is not this
		// report's to repeat.
		out.findings = append(out.findings, finding{Kind: findUnreadable, File: file, Detail: "it is not valid YAML"})

		return out
	}
	jobs := mappingValue(rootOf(&doc), "jobs")
	switch {
	case jobs == nil:
		return out
	case jobs.Kind == yaml.AliasNode || jobs.Anchor != "":
		out.findings = append(out.findings, finding{Kind: findCouldNot, File: file, Line: jobs.Line,
			Detail: "its jobs are a YAML anchor or alias, which another mapping shares"})

		return out
	case jobs.Kind != yaml.MappingNode:
		return out
	}

	lines := newLineIndex(data)
	var known []jobRunsOn

	for i := 0; i+1 < len(jobs.Content); i += 2 {
		key, job := jobs.Content[i].Value, jobs.Content[i+1]
		// A JOB SHARED THROUGH AN ANCHOR MOVES WITH EVERY ALIAS OF IT, and one that
		// takes its keys from another mapping takes its runs-on from there, so none of
		// them can be edited as one job.
		switch {
		case job.Kind == yaml.AliasNode:
			out.findings = append(out.findings, finding{Kind: findCouldNot, File: file, Line: job.Line, Job: key,
				Detail: "the job is a YAML alias of another, and moves with it"})

			continue
		case job.Kind != yaml.MappingNode:
			continue
		case job.Anchor != "":
			out.findings = append(out.findings, finding{Kind: findCouldNot, File: file, Line: job.Line, Job: key,
				Detail: "the job is a YAML anchor, which other jobs may share"})

			continue
		case mappingValue(job, "<<") != nil:
			out.findings = append(out.findings, finding{Kind: findCouldNot, File: file, Line: job.Line, Job: key,
				Detail: "the job takes keys from another mapping with <<, which other jobs may share"})

			continue
		}

		if uses := mappingValue(job, "uses"); uses != nil {
			out.findings = append(out.findings, finding{Kind: findCall, File: file, Line: uses.Line, Job: key,
				Text: uses.Value})
			out.reportCallerLabels(file, key, job)

			continue
		}

		runsOn := mappingValue(job, "runs-on")
		if runsOn == nil {
			continue
		}

		scalar := out.decideRunsOn(file, key, job, runsOn, lines, chooser)
		if scalar != nil {
			known = append(known, jobRunsOn{key: key, display: displayName(key, job), matrix: hasMatrix(job),
				node: scalar})
		}
	}

	if recommend != nil {
		out.applyRightsize(file, known, lines, chooser, recommend)
	}
	out.after = applyEdits(data, out.edits)

	out.keepOnlyIfSame(&doc)

	return out
}

// keepOnlyIfSame reads the result back before it is offered: it must be the same
// workflow with exactly the edited values changed, to exactly what was meant, or the
// file is left as it was and every rewrite in it is said not to have happened. Every
// check before this trusts a parser's position; this one trusts nothing but the parse.
func (r *workflowRewrite) keepOnlyIfSame(doc *yaml.Node) {
	reason := sameWorkflowBut(doc, r.after, r.edits)
	if reason == "" {
		return
	}
	r.after = r.before
	for i := range r.findings {
		if r.findings[i].Kind == findRewritten {
			r.findings[i].Kind = findCouldNot
			r.findings[i].Detail = "the edited file did not read back as the same workflow with only its runs-on " +
				"changed (" + reason + "), so nothing in it was changed"
		}
	}
}

// hasMatrix says whether a job has a matrix, true too for a strategy this cannot
// read, such as an alias: a job whose legs might be judged apart is never moved whole.
func hasMatrix(job *yaml.Node) bool {
	strategy := mappingValue(job, "strategy")
	switch {
	case strategy == nil:
		return false
	case strategy.Kind != yaml.MappingNode:
		return true
	}

	return mappingValue(strategy, "matrix") != nil
}

// sameWorkflowBut says why after is not before with exactly the edits' values changed,
// empty when it is.
func sameWorkflowBut(before *yaml.Node, after []byte, edits []edit) string {
	if len(edits) == 0 {
		return ""
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(after, &doc); err != nil {
		return "it no longer parses"
	}
	want := make(map[*yaml.Node]string, len(edits))
	for _, e := range edits {
		want[e.node] = e.value
	}

	return sameNodeBut(before, &doc, want)
}

// sameNodeBut walks two parses together.
func sameNodeBut(before, after *yaml.Node, want map[*yaml.Node]string) string {
	if before.Kind != after.Kind || len(before.Content) != len(after.Content) || before.Anchor != after.Anchor {
		return "its structure changed"
	}
	if value, edited := want[before]; edited {
		if after.Kind != yaml.ScalarNode || after.Tag != "!!str" || after.Value != value {
			return "a value did not become the label meant"
		}
	} else if before.Value != after.Value || before.Tag != after.Tag {
		return "a value that was not meant to change did"
	}
	for i := range before.Content {
		if reason := sameNodeBut(before.Content[i], after.Content[i], want); reason != "" {
			return reason
		}
	}

	return ""
}

// rootOf is the document's top mapping.
func rootOf(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}

	return doc
}

// mappingValue is a mapping's value under key, nil on anything else.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}

	return nil
}

// displayName is the name GitHub shows for a job, which is what the ledger records:
// its `name:` or, with none, its key. It is empty for a name only GitHub can evaluate.
func displayName(key string, job *yaml.Node) string {
	name := mappingValue(job, "name")
	if name == nil {
		return key
	}
	if name.Kind != yaml.ScalarNode || strings.Contains(name.Value, "${{") {
		return ""
	}

	return name.Value
}

// decideRunsOn says what happens to one job's `runs-on`, and returns the scalar when
// it already names one of this deployment's tiers, for a recommendation to find.
func (r *workflowRewrite) decideRunsOn(file, key string, job, runsOn *yaml.Node, lines lineIndex,
	chooser *tierChooser,
) *yaml.Node {
	// AN ANCHOR IS SHARED BY WHATEVER ALIASES IT, so an edit here would move jobs this
	// one says nothing about.
	if runsOn.Anchor != "" {
		r.findings = append(r.findings, finding{Kind: findCouldNot, File: file, Line: runsOn.Line, Job: key,
			Detail: "it is a YAML anchor, which other jobs may share"})

		return nil
	}

	switch runsOn.Kind {
	case yaml.ScalarNode:
	case yaml.SequenceNode:
		if len(runsOn.Content) != 1 || runsOn.Content[0].Kind != yaml.ScalarNode {
			r.findings = append(r.findings, finding{Kind: findList, File: file, Line: runsOn.Line, Job: key,
				Text: flowText(runsOn), Detail: "a job on several labels needs a runner carrying all of them"})

			return nil
		}
		runsOn = runsOn.Content[0]
	case yaml.MappingNode:
		r.findings = append(r.findings, finding{Kind: findGroup, File: file, Line: runsOn.Line, Job: key,
			Text: flowText(runsOn), Detail: "which runner group a job belongs in is the organization's to decide"})

		return nil
	default:
		r.findings = append(r.findings, finding{Kind: findCouldNot, File: file, Line: runsOn.Line, Job: key,
			Detail: "it is a YAML alias, which other jobs may share"})

		return nil
	}

	label := runsOn.Value
	if strings.Contains(label, "${{") {
		r.findings = append(r.findings, finding{Kind: findExpression, File: file, Line: runsOn.Line, Job: key, Text: label,
			Detail: "only GitHub can evaluate it"})
		r.reportMatrixLabels(file, key, job, chooser)

		return nil
	}
	if runsOn.Anchor != "" {
		r.findings = append(r.findings, finding{Kind: findCouldNot, File: file, Line: runsOn.Line, Job: key, Text: label,
			Detail: "it is a YAML anchor, which other jobs may share"})

		return nil
	}
	if chooser.onBillet(label) {
		r.findings = append(r.findings, finding{Kind: findOnBillet, File: file, Line: runsOn.Line, Job: key, Text: label})

		return runsOn
	}

	to, detail, kind := chooser.choose(label, file)
	if to == "" {
		r.findings = append(r.findings, finding{Kind: kind, File: file, Line: runsOn.Line, Job: key, Text: label,
			Detail: detail})

		return nil
	}

	placed, reason := lines.place(runsOn, to)
	if reason != "" {
		r.findings = append(r.findings, finding{Kind: findCouldNot, File: file, Line: runsOn.Line, Job: key, Text: label,
			Detail: reason})

		return nil
	}
	r.edits = append(r.edits, placed)
	r.edited[runsOn] = true
	r.findings = append(r.findings, finding{Kind: findRewritten, File: file, Line: runsOn.Line, Job: key, Text: label,
		Detail: "to " + to + ", " + detail})

	return nil
}

// reportCallerLabels says which of a reusable workflow call's inputs name a hosted
// label: the called workflow's own `runs-on` reads them, and rewriting the input is
// only right if the input means a runner, which a name cannot prove.
func (r *workflowRewrite) reportCallerLabels(file, key string, job *yaml.Node) {
	with := mappingValue(job, "with")
	if with == nil || with.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(with.Content); i += 2 {
		value := with.Content[i+1]
		if value.Kind == yaml.ScalarNode && mentionsHostedLabel(value.Value) {
			r.findings = append(r.findings, finding{Kind: findCaller, File: file, Line: value.Line, Job: key,
				Text: value.Value, Detail: "input " + strconv.Quote(with.Content[i].Value)})
		}
	}
}

// mentionsHostedLabel says whether a value is a label GitHub hosts.
func mentionsHostedLabel(value string) bool {
	_, ok := lookupHostedLabel(value, false)

	return ok
}

// reportMatrixLabels lists a matrix's hosted labels when `runs-on` reads a matrix
// value: a label there also picks what the steps expect, so which legs to move is the
// workflow's author's, and the report names what each would become.
func (r *workflowRewrite) reportMatrixLabels(file, key string, job *yaml.Node, chooser *tierChooser) {
	matrix := mappingValue(mappingValue(job, "strategy"), "matrix")
	if matrix == nil || matrix.Kind != yaml.MappingNode {
		return
	}

	report := func(value *yaml.Node) {
		if value.Kind != yaml.ScalarNode || !mentionsHostedLabel(value.Value) {
			return
		}
		detail := "no tier for it"
		if to, why, _ := chooser.choose(value.Value, file); to != "" {
			detail = "would be " + to + ", " + why
		}
		r.findings = append(r.findings, finding{Kind: findMatrix, File: file, Line: value.Line, Job: key,
			Text: value.Value, Detail: detail})
	}

	for i := 0; i+1 < len(matrix.Content); i += 2 {
		name, value := matrix.Content[i].Value, matrix.Content[i+1]
		if value.Kind == yaml.SequenceNode && (name == "include" || name == "exclude") {
			for _, entry := range value.Content {
				if entry.Kind == yaml.MappingNode {
					for j := 0; j+1 < len(entry.Content); j += 2 {
						report(entry.Content[j+1])
					}
				}
			}

			continue
		}
		if value.Kind == yaml.SequenceNode {
			for _, entry := range value.Content {
				report(entry)
			}
		}
	}
}

// flowText is a list or mapping of `runs-on` as one line, for the report.
func flowText(node *yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		parts := make([]string, 0, len(node.Content))
		for _, entry := range node.Content {
			parts = append(parts, flowText(entry))
		}

		return "[" + strings.Join(parts, ", ") + "]"
	case yaml.MappingNode:
		parts := make([]string, 0, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			parts = append(parts, node.Content[i].Value+": "+flowText(node.Content[i+1]))
		}

		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return node.Value
	}
}

// lineIndex finds a parser's line and column in a file's bytes.
type lineIndex struct {
	data   []byte
	starts []int
}

// newLineIndex counts lines as yaml.v3 does: a CR LF pair is one break, and a lone
// CR, LF, NEL, LS or PS each another. A line counted differently would put every
// later position on another line.
func newLineIndex(data []byte) lineIndex {
	starts := []int{0}
	for i := 0; i < len(data); {
		size := breakAt(data, i)
		if size == 0 {
			i++

			continue
		}
		i += size
		starts = append(starts, i)
	}

	return lineIndex{data: data, starts: starts}
}

// breakAt is the length of the line break at data[i], zero for none.
func breakAt(data []byte, i int) int {
	switch {
	case data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n':
		return 2
	case data[i] == '\r' || data[i] == '\n':
		return 1
	case bytes.HasPrefix(data[i:], []byte("\u0085")):
		return 2
	case bytes.HasPrefix(data[i:], []byte("\u2028")), bytes.HasPrefix(data[i:], []byte("\u2029")):
		return 3
	}

	return 0
}

// place finds a scalar's value in the file's bytes, and refuses anything but the
// value as written: a parser's position is a claim, and an edit made on a wrong one
// corrupts somebody's workflow, so the bytes are compared before anything trusts it.
//
// yaml.v3 COUNTS A COLUMN IN CHARACTERS, measured by the multibyte test beside this:
// a label after a non-ASCII entry on its line is further along in bytes.
func (l lineIndex) place(node *yaml.Node, value string) (edit, string) {
	placed, reason := l.span(node)
	if reason != "" {
		return edit{}, reason
	}
	text, reason := replacementText(node, value)
	if reason != "" {
		return edit{}, reason
	}
	placed.text, placed.node, placed.value = text, node, value

	return placed, ""
}

// replacementText is how value is written in node's place: as it is inside the
// quotes it already has, bare where a bare value reads back as that string, and
// double-quoted where it would not (a tier named null, true or 1 would read as a
// null, a boolean or a number, which GitHub does not take as the label).
func replacementText(node *yaml.Node, value string) (string, string) {
	switch node.Style {
	case yaml.DoubleQuotedStyle:
		if strings.ContainsAny(value, "\"\\") || !printableASCII(value) {
			return "", "the label meant has a character the double quotes would need to escape"
		}

		return value, ""
	case yaml.SingleQuotedStyle:
		if strings.Contains(value, "'") || !printableASCII(value) {
			return "", "the label meant has a character the single quotes would need to escape"
		}

		return value, ""
	}
	if !printableASCII(value) {
		return "", "the label meant is not printable ASCII"
	}
	if readsBackAs(value) {
		return value, ""
	}

	return strconv.Quote(value), ""
}

// readsBackAs says whether value written bare, alone or in a flow list, reads back as
// the string value.
func readsBackAs(value string) bool {
	for _, form := range []string{"k: " + value + "\n", "k: [" + value + "]\n"} {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(form), &doc); err != nil {
			return false
		}
		v := mappingValue(rootOf(&doc), "k")
		if v != nil && v.Kind == yaml.SequenceNode && len(v.Content) == 1 {
			v = v.Content[0]
		}
		if v == nil || v.Kind != yaml.ScalarNode || v.Tag != "!!str" || v.Value != value || v.Style != 0 {
			return false
		}
	}

	return true
}

func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}

	return s != ""
}

// span is where a scalar's value lies in the file's bytes.
func (l lineIndex) span(node *yaml.Node) (edit, string) {
	switch node.Style {
	case 0, yaml.DoubleQuotedStyle, yaml.SingleQuotedStyle:
	default:
		return edit{}, "it is a block scalar, whose bytes are not the label's"
	}
	if node.Line < 1 || node.Line > len(l.starts) || node.Column < 1 {
		return edit{}, "the parser's position for it is not in the file"
	}

	start := l.starts[node.Line-1]
	for range node.Column - 1 {
		if start >= len(l.data) || breakAt(l.data, start) > 0 {
			return edit{}, "the parser's position for it is not in the file"
		}
		_, size := utf8.DecodeRune(l.data[start:])
		start += size
	}

	if node.Style != 0 {
		quote := byte('"')
		if node.Style == yaml.SingleQuotedStyle {
			quote = '\''
		}
		if start >= len(l.data) || l.data[start] != quote {
			return edit{}, "the file does not hold the label where the parser says it is"
		}
		start++
		end := start + len(node.Value)
		if end >= len(l.data) || string(l.data[start:end]) != node.Value || l.data[end] != quote {
			return edit{}, "the label is written with an escape, whose bytes are not the label's"
		}

		return edit{start: start, end: end}, ""
	}

	end := start + len(node.Value)
	if end > len(l.data) || string(l.data[start:end]) != node.Value {
		return edit{}, "the file does not hold the label where the parser says it is"
	}
	// A PLAIN SCALAR ENDS WHERE YAML SAYS IT DOES, so what follows must be something
	// that ends one: otherwise the bytes matched a prefix of something else.
	if end < len(l.data) && !strings.ContainsRune(" \t\r\n,]}#", rune(l.data[end])) {
		return edit{}, "the file does not hold the label where the parser says it is"
	}

	return edit{start: start, end: end}, ""
}

// applyEdits splices the edits into a copy of data, which is returned as it is when
// there are none.
func applyEdits(data []byte, edits []edit) []byte {
	if len(edits) == 0 {
		return data
	}

	sorted := slices.Clone(edits)
	slices.SortFunc(sorted, func(x, y edit) int { return cmp.Compare(y.start, x.start) })

	out := slices.Clone(data)
	for _, e := range sorted {
		out = slices.Concat(out[:e.start], []byte(e.text), out[e.end:])
	}

	return out
}

// unifiedDiff writes the difference between before and after, whose lines correspond
// one to one because an edit never crosses a line. name is printed as the paths of a
// patch `git apply` takes.
func unifiedDiff(name string, before, after []byte) string {
	if bytes.Equal(before, after) {
		return ""
	}

	const context = 3
	old, updated := bytes.Split(before, []byte("\n")), bytes.Split(after, []byte("\n"))

	var changed []int
	for i := range old {
		if !bytes.Equal(old[i], updated[i]) {
			changed = append(changed, i)
		}
	}

	// THE LAST SPLIT ELEMENT OF A FILE ENDING IN A NEWLINE IS NOT A LINE, and a file
	// that ends without one says so after its last line on each side it appears.
	lastLine, noNewline := len(old)-1, len(old[len(old)-1]) != 0
	if !noNewline {
		lastLine--
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", diffPath("a/"+name), diffPath("b/"+name))

	line := func(prefix byte, text []byte, i int) {
		fmt.Fprintf(&b, "%c%s\n", prefix, text)
		if noNewline && i == lastLine {
			b.WriteString("\\ No newline at end of file\n")
		}
	}

	for at := 0; at < len(changed); {
		first := max(changed[at]-context, 0)
		last := changed[at]
		for at+1 < len(changed) && changed[at+1]-last <= 2*context {
			at++
			last = changed[at]
		}
		at++

		end := min(last+context, lastLine)

		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", first+1, end-first+1, first+1, end-first+1)
		for i := first; i <= end; i++ {
			if bytes.Equal(old[i], updated[i]) {
				line(' ', old[i], i)

				continue
			}
			line('-', old[i], i)
			line('+', updated[i], i)
		}
	}

	return b.String()
}

// diffPath is a path as a diff's header can hold it: quoted the way git quotes one
// where its bytes would otherwise end the header or forge a line of it.
func diffPath(path string) string {
	if strconv.Quote(path) != `"`+path+`"` || strings.ContainsAny(path, " \t") {
		return strconv.Quote(path)
	}

	return path
}
