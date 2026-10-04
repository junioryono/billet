package scripts_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// skillCorpusPath is every unit of text the skills held when they were frozen
// for the rewrite in #356, one per line: skillUnits over every SKILL.md at
// c0da9fb2. It is never regenerated, because regenerating it would bless
// whatever the skills had lost since; a unit removed on purpose belongs in the
// dropped list instead.
const skillCorpusPath = "testdata/skills-corpus.txt"

// skillDroppedPath lists corpus units removed on purpose: `<hash> <reason>`.
const skillDroppedPath = "testdata/skills-dropped.txt"

// maxSkillBytes bounds a SKILL.md, which loads whole whenever its skill does; the
// long form lives in references/ and loads only when a rule needs it.
const maxSkillBytes = 12 * 1024

// maxDescriptionRunes bounds a skill's description, which every session reads.
const maxDescriptionRunes = 600

// NOTHING A SKILL SAID IS LOST WITHOUT A REASON.
//
// The skills are being reorganised into a short SKILL.md and references/ files
// loaded on demand (#356), and a rewrite of 650 KB of measured rules is exactly
// where a sentence nobody meant to drop goes missing with every gate green.
// Every unit of the frozen corpus must still be a whole unit, verbatim after
// normalisation, of some Markdown file under .claude/skills, or be listed in the
// dropped file with a reason. A whole unit and not a substring, because "It is
// no longer true that SQLite must be on local storage." contains the rule it
// reverses.
func TestEverySkillSentenceIsKeptOrDroppedWithAReason(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)

	corpus := readCorpus(t)
	if len(corpus) == 0 {
		t.Fatalf("%s is empty, so this gate would pass over any loss", skillCorpusPath)
	}

	dropped := readDropped(t)

	texts := make([]string, 0, 64)

	for _, path := range skillMarkdown(t, root) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		texts = append(texts, string(raw))
	}

	for _, problem := range corpusProblems(corpus, dropped, texts) {
		t.Error(problem)
	}
}

// corpusProblems is the whole decision: each corpus unit must be a whole unit of
// one of the texts, or be dropped with a reason, and nothing may be dropped that
// is still said or that names no unit.
func corpusProblems(corpus []string, dropped map[string]string, texts []string) []string {
	said := map[string]bool{}

	for _, text := range texts {
		for _, unit := range skillUnits(text) {
			said[unit] = true
		}
	}

	var problems []string

	known := make(map[string]bool, len(corpus))

	for _, unit := range corpus {
		hash := unitHash(unit)
		known[hash] = true

		_, droppedOnPurpose := dropped[hash]

		switch kept := said[unit]; {
		case kept && droppedOnPurpose:
			problems = append(problems, fmt.Sprintf("%s lists %s as dropped, but the skills still say it: %q",
				skillDroppedPath, hash, excerpt(unit)))
		case !kept && !droppedOnPurpose:
			problems = append(problems, fmt.Sprintf("a skill no longer says this, and %s gives no reason "+
				"(add `%s <reason>`): %q", skillDroppedPath, hash, excerpt(unit)))
		}
	}

	for hash := range dropped {
		if !known[hash] {
			problems = append(problems, fmt.Sprintf("%s names %s, which is no unit of the corpus",
				skillDroppedPath, hash))
		}
	}

	slices.Sort(problems)

	return problems
}

// THE SKILLS FOLLOW THE RULES THE REPOSITORY STATES ABOUT THEM.
//
// CLAUDE.md states them and nothing checked them: frontmatter that Codex parses
// as strict YAML, a name that matches the directory, a symlink under
// .agents/skills for every skill, one paragraph per line, and every reference
// file reachable from its SKILL.md, because a reference nothing links to is one
// no session ever loads.
func TestEverySkillFollowsTheRepositoryRules(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	skills := filepath.Join(root, ".claude", "skills")

	entries, err := os.ReadDir(skills)
	if err != nil {
		t.Fatalf("read %s: %v", skills, err)
	}

	count := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		count++
		name := entry.Name()
		dir := filepath.Join(skills, name)

		raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			t.Errorf("skill %s: %v", name, err)

			continue
		}

		if len(raw) > maxSkillBytes {
			t.Errorf("skill %s: SKILL.md is %d bytes, over %d; move the long form into references/ and keep "+
				"one line per rule here", name, len(raw), maxSkillBytes)
		}

		checkFrontmatter(t, name, raw)
		checkAgentsLink(t, root, name)
		checkReferencesAreLinked(t, dir, name, string(raw))
	}

	if count == 0 {
		t.Fatalf("no skills under %s, so nothing here was checked", skills)
	}

	for _, path := range skillMarkdown(t, root) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		if line, wrapped := firstHardWrap(string(raw)); wrapped {
			t.Errorf("%s:%d continues a paragraph on a new line; write every paragraph on one line",
				strings.TrimPrefix(path, root+string(filepath.Separator)), line)
		}
	}
}

// The parts of the rule that can be wrong in a way the tree today cannot show.
func TestTheSkillRulesRefuseWhatTheyDescribe(t *testing.T) {
	t.Parallel()

	if line, wrapped := firstHardWrap("---\nname: x\n---\n\nOne paragraph\nwrapped here.\n"); !wrapped || line != 6 {
		t.Errorf("a paragraph continued on line 6 was reported as (%d, %t)", line, wrapped)
	}

	for _, ok := range []string{
		"---\nname: x\ndescription: y\n---\n\nOne paragraph.\n\nAnother.\n",
		"Text.\n\n- item one\n- item two\n",
		"Text.\n\n```\ncode line\nmore code\n```\n",
		"| a | b |\n|---|---|\n| c | d |\n",
		"1. first\n   continued inside the item\n",
	} {
		if line, wrapped := firstHardWrap(ok); wrapped {
			t.Errorf("reported a hard wrap at line %d in %q", line, ok)
		}
	}

	got := skillUnits("---\nname: x\ndescription: y\n---\n\n# Heading\n\n" +
		"- **A rule.** It holds `a.b` here. And it ends.\n\n```bash\nmake check\n```\n")
	want := []string{"A rule.", "It holds `a.b` here.", "And it ends.", "make check"}

	if !slices.Equal(got, want) {
		t.Errorf("units = %q, want %q", got, want)
	}

	// The corpus decision itself: a unit is kept only as a whole unit, so a
	// sentence that contains a rule while reversing it does not keep it.
	rule := []string{"SQLite must be on local storage."}
	for text, want := range map[string]int{
		"Some prose. SQLite must be on local storage.\n":               0,
		"It is no longer true that SQLite must be on local storage.\n": 1,
		"Nothing here.\n": 1,
	} {
		if got := corpusProblems(rule, nil, []string{text}); len(got) != want {
			t.Errorf("%q: %d corpus problems %q, want %d", text, len(got), got, want)
		}
	}

	droppedRule := map[string]string{unitHash(rule[0]): "superseded"}
	if got := corpusProblems(rule, droppedRule, []string{"Nothing here.\n"}); len(got) != 0 {
		t.Errorf("a unit dropped with a reason was still reported: %q", got)
	}

	if got := corpusProblems(rule, droppedRule, []string{rule[0] + "\n"}); len(got) != 1 {
		t.Errorf("a unit dropped while still said was not reported: %q", got)
	}

	// Code is meaning, not layout: neither a fenced line nor an inline span is
	// normalised, so two different commands never become one unit.
	stars := skillUnits("```\nprintf '%s\\n' '**'\n```\n")
	empty := skillUnits("```\nprintf '%s\\n' ''\n```\n")

	if slices.Equal(stars, empty) {
		t.Errorf("two different code lines became the same unit: %q", stars)
	}

	continued := skillUnits("```\nrun \\\n```\n")
	broken := skillUnits("```\nrun \\ \n```\n")

	if slices.Equal(continued, broken) {
		t.Errorf("a space after a continuation backslash was trimmed away: %q", broken)
	}

	if got, want := normaliseSkillText("- **A**  rule `x  **y**` here"), "A rule `x  **y**` here"; got != want {
		t.Errorf("normalised to %q, want %q", got, want)
	}

	// One strict YAML document, and nothing after it, whether the second parses
	// or not.
	for fm, says := range map[string]string{
		"---\nname: x\ndescription: y\n...\n--- # second\nother: [\n---\nbody\n": "continues past",
		"---\nname: x\ndescription: y\n--- # second\nname: z\n---\nbody\n":       "holds a second YAML document",
	} {
		if _, err := parseFrontmatter(fm); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: refused with %v, want an error saying %q", fm, err, says)
		}
	}

	if _, err := parseFrontmatter("---\nname: x\ndescription: y\n---\nbody\n"); err != nil {
		t.Errorf("one plain document was refused: %v", err)
	}

	// A link is judged by where it resolves, in every form Markdown spells one,
	// and neither a mention nor a link shown in code is a link.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "references", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"leases.md", filepath.Join("sub", "rules.md")} {
		if err := os.WriteFile(filepath.Join(dir, "references", name), []byte("x.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	both := " and [r](references/sub/rules.md)."
	for skill, want := range map[string]int{
		"See [leases](references/leases.md)" + both:                                                      0,
		"See [leases](<references/leases.md> \"title\")" + both:                                          0,
		"See [leases](references/leases.md \"title\")" + both:                                            0,
		"See references/leases.md. It is also [linked](references/leases.md)" + both:                     0,
		"See [leases][l]" + both + "\n\n[l]: references/leases.md\n":                                     0,
		"See [leases](references/leases.md#section) and [x](https://example.com/references/y.md)" + both: 0,
		"See [leases](../other/references/leases.md)" + both:                                             2, // missing; real file unlinked
		"See references/leases.md, which nothing links" + both:                                           1,
		"See [gone](references/gone.md \"title\") and [l](references/leases.md)" + both:                  1,
		"See [gone][g] and [l](references/leases.md)" + both + "\n\n[g]: references/gone.md\n":           1,
		"See [leases](references/leases.md) and [sub](references/sub).":                                  2, // a directory; its file unlinked
		"See `[leases](references/leases.md)`" + both:                                                    1, // shown in code, not linked
	} {
		if got := referenceProblems(dir, skill); len(got) != want {
			t.Errorf("%q: %d problems %q, want %d", skill, len(got), got, want)
		}
	}

	// The titled link is refused as a link, not only by the mention fallback.
	titled := referenceProblems(dir, "See [gone](references/gone.md \"title\") and [l](references/leases.md)"+both)
	if !slices.ContainsFunc(titled, func(p string) bool {
		return strings.HasPrefix(p, "SKILL.md links references/gone.md")
	}) {
		t.Errorf("a titled link to a missing file was not refused as a link: %q", titled)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}

	return root
}

// skillMarkdown is every Markdown file under .claude/skills, sorted.
func skillMarkdown(t *testing.T, root string) []string {
	t.Helper()

	var paths []string

	err := filepath.WalkDir(filepath.Join(root, ".claude", "skills"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			paths = append(paths, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk the skills: %v", err)
	}

	slices.Sort(paths)

	return paths
}

// skillUnits splits a skill into the units the corpus records: each sentence of
// prose and each line of code. Frontmatter and headings are left out, because
// the rewrite replaces them by design.
func skillUnits(text string) []string {
	var units []string

	inFence := false

	for line := range strings.SplitSeq(stripFrontmatter(text), "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence

			continue
		}

		// A line of code is kept as written, less its indentation: emphasis
		// markers, runs of spaces and trailing whitespace inside code are
		// meaning, not layout (a space after a continuation backslash ends the
		// command).
		if inFence {
			if trimmed != "" {
				units = append(units, strings.TrimLeft(line, " \t"))
			}

			continue
		}

		if trimmed == "" || strings.HasPrefix(trimmed, "#") || tableRule.MatchString(trimmed) {
			continue
		}

		for _, sentence := range splitSentences(normaliseSkillText(trimmed)) {
			if sentence != "" {
				units = append(units, sentence)
			}
		}
	}

	return units
}

var (
	listMarker = regexp.MustCompile(`^(?:[-*+]|\d+\.)\s+`)
	inlineLink = regexp.MustCompile(`\]\(\s*(<[^>]*>|[^)\s]+)(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?\s*\)`)
	linkDef    = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:\s*(<[^>]*>|\S+)`)
	mention    = regexp.MustCompile(`[A-Za-z0-9._/-]*references/[A-Za-z0-9._/-]*[A-Za-z0-9_]`)
	url        = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	codeSpan   = regexp.MustCompile("`[^`]*`")
	notProse   = regexp.MustCompile(`^(?:[-*+]\s|\d+\.\s|#|\||>)`)
	tableRule  = regexp.MustCompile(`^\|?\s*:?-{3,}`)
	spaces     = regexp.MustCompile(`\s+`)
)

// normaliseSkillText removes what a restructure changes without changing what a
// sentence says: a leading list marker, and outside inline code, emphasis
// markers and runs of spaces. Inside backticks everything is kept.
func normaliseSkillText(line string) string {
	line = listMarker.ReplaceAllString(strings.TrimSpace(line), "")

	parts := strings.Split(line, "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = spaces.ReplaceAllString(strings.ReplaceAll(parts[i], "**", ""), " ")
	}

	return strings.TrimSpace(strings.Join(parts, "`"))
}

// splitSentences breaks after ., ! or ? (and any closing quote, bracket or
// backtick) when a space and a capital, a digit, a backtick or an opening
// bracket follow. Splitting too often is harmless: every piece of a sentence
// that moved verbatim is still found.
func splitSentences(s string) []string {
	var out []string

	start := 0
	runes := []rune(s)

	for i := 0; i < len(runes); i++ {
		if !strings.ContainsRune(".!?", runes[i]) {
			continue
		}

		end := i + 1
		for end < len(runes) && strings.ContainsRune(`)"'”’`+"`", runes[end]) {
			end++
		}

		if end+1 >= len(runes) || runes[end] != ' ' {
			continue
		}

		if !opensSentence(runes[end+1]) {
			continue
		}

		out = append(out, strings.TrimSpace(string(runes[start:end])))
		start = end + 1
		i = end
	}

	return append(out, strings.TrimSpace(string(runes[start:])))
}

// opensSentence reports whether r can begin the sentence after a break.
func opensSentence(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("`(\"“", r)
}

func stripFrontmatter(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return text
	}

	_, body, closed := strings.Cut(text[len("---\n"):], "\n---\n")
	if !closed {
		return text
	}

	return body
}

// firstHardWrap reports the first line that continues the prose line above it.
// List items, tables, quotes, headings, code and indented continuations of a
// list item are not prose paragraphs, so none of them count.
func firstHardWrap(text string) (int, bool) {
	inFence := false
	inFrontmatter := strings.HasPrefix(text, "---\n")
	previousIsProse := false

	for i, line := range strings.Split(text, "\n") {
		if inFrontmatter {
			inFrontmatter = i == 0 || line != "---"

			continue
		}

		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			previousIsProse = false

			continue
		}

		prose := !inFence && isProseLine(line)
		if prose && previousIsProse {
			return i + 1, true
		}

		previousIsProse = prose
	}

	return 0, false
}

func isProseLine(line string) bool {
	if strings.TrimSpace(line) == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return false
	}

	if line == "---" {
		return false
	}

	return !notProse.MatchString(line)
}

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parseFrontmatter reads a SKILL.md's frontmatter as exactly one strict YAML
// document: known fields only, and nothing after it, because a second document
// is input the first decode never looked at.
func parseFrontmatter(text string) (skillFrontmatter, error) {
	var fm skillFrontmatter

	if !strings.HasPrefix(text, "---\n") {
		return fm, errors.New("SKILL.md does not open with frontmatter")
	}

	frontmatter, _, closed := strings.Cut(text[len("---\n"):], "\n---\n")
	if !closed {
		return fm, errors.New("the frontmatter is never closed")
	}

	decoder := yaml.NewDecoder(strings.NewReader(frontmatter))
	decoder.KnownFields(true)

	if err := decoder.Decode(&fm); err != nil {
		return fm, fmt.Errorf("the frontmatter is not strict YAML, which Codex refuses: %w", err)
	}

	var extra any

	switch err := decoder.Decode(&extra); {
	case err == nil:
		return fm, errors.New("the frontmatter holds a second YAML document")
	case !errors.Is(err, io.EOF):
		return fm, fmt.Errorf("the frontmatter continues past its first YAML document: %w", err)
	}

	return fm, nil
}

func checkFrontmatter(t *testing.T, name string, raw []byte) {
	t.Helper()

	fm, err := parseFrontmatter(string(raw))
	if err != nil {
		t.Errorf("skill %s: %v", name, err)

		return
	}

	if fm.Name != name {
		t.Errorf("skill %s: frontmatter names %q", name, fm.Name)
	}

	if n := len([]rune(fm.Description)); n > maxDescriptionRunes {
		t.Errorf("skill %s: the description is %d characters, over %d; it says when to load the skill, and "+
			"every session reads every description", name, n, maxDescriptionRunes)
	}

	if strings.TrimSpace(fm.Description) == "" {
		t.Errorf("skill %s: no description, so nothing tells a session when to load it", name)
	}
}

func checkAgentsLink(t *testing.T, root, name string) {
	t.Helper()

	link := filepath.Join(root, ".agents", "skills", name)

	target, err := os.Readlink(link)
	if err != nil {
		t.Errorf("skill %s: .agents/skills/%s is not a symlink, so Codex cannot see the skill: %v", name, name, err)

		return
	}

	if want := "../../.claude/skills/" + name; target != want {
		t.Errorf("skill %s: .agents/skills/%s points at %q, want %q", name, name, target, want)
	}
}

func checkReferencesAreLinked(t *testing.T, dir, name, skill string) {
	t.Helper()

	for _, problem := range referenceProblems(dir, skill) {
		t.Errorf("skill %s: %s", name, problem)
	}
}

// referenceProblems compares a SKILL.md's links with its references/ directory.
// Code is taken out first, since a link shown in code is not a link. Every
// relative link destination (inline, image or reference definition) and every
// path that names references/ must resolve to a regular file, and every file
// under references/, at any depth, must be the destination of a link, not
// merely a name the text mentions.
func referenceProblems(dir, skill string) []string {
	var problems []string

	text := codeSpan.ReplaceAllString(withoutFences(skill), "")
	linked := map[string]bool{}

	var dests []string
	for _, m := range inlineLink.FindAllStringSubmatch(text, -1) {
		dests = append(dests, m[1])
	}

	for _, m := range linkDef.FindAllStringSubmatch(text, -1) {
		dests = append(dests, m[1])
	}

	for _, dest := range dests {
		dest = strings.TrimSuffix(strings.TrimPrefix(dest, "<"), ">")
		dest, _, _ = strings.Cut(dest, "#")

		if dest == "" || strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:") {
			continue
		}

		resolved := filepath.Clean(filepath.Join(dir, dest))
		linked[resolved] = true

		if problem := notARegularFile(resolved); problem != "" {
			problems = append(problems, fmt.Sprintf("SKILL.md links %s, which %s", dest, problem))
		}
	}

	// A path the prose names outside any link, and outside any URL.
	prose := url.ReplaceAllString(linkDef.ReplaceAllString(inlineLink.ReplaceAllString(text, "]"), ""), "")

	for _, path := range mention.FindAllString(prose, -1) {
		if problem := notARegularFile(filepath.Join(dir, path)); problem != "" {
			problems = append(problems, fmt.Sprintf("SKILL.md names %s, which %s", path, problem))
		}
	}

	err := filepath.WalkDir(filepath.Join(dir, "references"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.Type().IsRegular() && !linked[path] {
			rel, _ := filepath.Rel(dir, path) //nolint:errcheck // path is under dir; the message only names it.
			problems = append(problems, fmt.Sprintf("%s is the destination of no link in SKILL.md, so no "+
				"session loads it", rel))
		}

		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		problems = append(problems, fmt.Sprintf("walk references: %v", err))
	}

	return problems
}

// notARegularFile says why path is not a readable regular file, or "".
func notARegularFile(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("cannot be read: %v", err)
	}

	if !info.Mode().IsRegular() {
		return "is not a regular file"
	}

	return ""
}

// withoutFences drops fenced code blocks.
func withoutFences(text string) string {
	var out []string

	inFence := false

	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence

			continue
		}

		if !inFence {
			out = append(out, line)
		}
	}

	return strings.Join(out, "\n")
}

func readCorpus(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(skillCorpusPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillCorpusPath, err)
	}

	var units []string

	for line := range strings.SplitSeq(string(raw), "\n") {
		if line != "" {
			units = append(units, line)
		}
	}

	return units
}

func readDropped(t *testing.T) map[string]string {
	t.Helper()

	f, err := os.Open(skillDroppedPath)
	if err != nil {
		t.Fatalf("open %s: %v", skillDroppedPath, err)
	}

	defer f.Close()

	dropped := map[string]string{}
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		hash, reason, _ := strings.Cut(line, " ")
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: %s is dropped with no reason", skillDroppedPath, hash)
		}

		dropped[hash] = reason
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", skillDroppedPath, err)
	}

	return dropped
}

func unitHash(unit string) string {
	sum := sha256.Sum256([]byte(unit))

	return hex.EncodeToString(sum[:6])
}

func excerpt(s string) string {
	const width = 160

	runes := []rune(s)
	if len(runes) <= width {
		return s
	}

	return string(runes[:width]) + "…"
}
