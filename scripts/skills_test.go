package scripts_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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
// for the rewrite in #356, one per line.
const skillCorpusPath = "testdata/skills-corpus.txt"

// skillDroppedPath lists corpus units removed on purpose: `<hash> <reason>`.
const skillDroppedPath = "testdata/skills-dropped.txt"

// writeSkillCorpusEnv regenerates the corpus from the skills as they are now.
// It exists for the one freeze, and running it again would bless whatever the
// skills have lost since; a unit removed on purpose belongs in the dropped list.
const writeSkillCorpusEnv = "BILLET_WRITE_SKILL_CORPUS"

// NOTHING A SKILL SAID IS LOST WITHOUT A REASON.
//
// The skills are being reorganised into a short SKILL.md and references/ files
// loaded on demand (#356), and a rewrite of 650 KB of measured rules is exactly
// where a sentence nobody meant to drop goes missing with every gate green.
// Every unit of the frozen corpus must still appear, verbatim after
// normalisation, in some Markdown file under .claude/skills, or be listed in the
// dropped file with a reason.
func TestEverySkillSentenceIsKeptOrDroppedWithAReason(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)

	corpus := readCorpus(t)
	if len(corpus) == 0 {
		t.Fatalf("%s is empty, so this gate would pass over any loss", skillCorpusPath)
	}

	dropped := readDropped(t)

	var haystack strings.Builder

	for _, path := range skillMarkdown(t, root) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		for line := range strings.SplitSeq(string(raw), "\n") {
			haystack.WriteString(normaliseSkillText(line))
			haystack.WriteByte('\n')
		}
	}

	text := haystack.String()
	known := make(map[string]bool, len(corpus))

	for _, unit := range corpus {
		hash := unitHash(unit)
		known[hash] = true

		_, droppedOnPurpose := dropped[hash]
		kept := strings.Contains(text, unit)

		switch {
		case kept && droppedOnPurpose:
			t.Errorf("%s lists %s as dropped, but the skills still say it: %q", skillDroppedPath, hash,
				excerpt(unit))
		case !kept && !droppedOnPurpose:
			t.Errorf("a skill no longer says this, and %s gives no reason (add `%s <reason>`): %q",
				skillDroppedPath, hash, excerpt(unit))
		}
	}

	for hash := range dropped {
		if !known[hash] {
			t.Errorf("%s names %s, which is no unit of the corpus", skillDroppedPath, hash)
		}
	}
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
}

// TestWriteSkillCorpus freezes the corpus. It writes only when asked.
func TestWriteSkillCorpus(t *testing.T) {
	if os.Getenv(writeSkillCorpusEnv) != "1" {
		t.Skipf("set %s=1 to regenerate %s from the skills as they are now", writeSkillCorpusEnv, skillCorpusPath)
	}

	root := repositoryRoot(t)
	seen := map[string]bool{}

	var units []string

	for _, path := range skillMarkdown(t, root) {
		if filepath.Base(path) != "SKILL.md" {
			continue
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		for _, unit := range skillUnits(string(raw)) {
			if !seen[unit] {
				seen[unit] = true
				units = append(units, unit)
			}
		}
	}

	slices.Sort(units)

	if err := os.WriteFile(skillCorpusPath, []byte(strings.Join(units, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", skillCorpusPath, err)
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
// prose and each line of code, normalised. Frontmatter and headings are left
// out, because the rewrite replaces them by design.
func skillUnits(text string) []string {
	var units []string

	inFence := false

	for line := range strings.SplitSeq(stripFrontmatter(text), "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence

			continue
		}

		if inFence {
			if unit := normaliseSkillText(trimmed); unit != "" {
				units = append(units, unit)
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
	listMarker    = regexp.MustCompile(`^(?:[-*+]|\d+\.)\s+`)
	referenceLink = regexp.MustCompile(`references/([A-Za-z0-9._-]+\.md)`)
	notProse      = regexp.MustCompile(`^(?:[-*+]\s|\d+\.\s|#|\||>)`)
	tableRule     = regexp.MustCompile(`^\|?\s*:?-{3,}`)
	spaces        = regexp.MustCompile(`\s+`)
)

// normaliseSkillText removes what a restructure changes without changing what a
// sentence says: emphasis markers, a leading list marker, and runs of spaces.
func normaliseSkillText(line string) string {
	line = strings.ReplaceAll(line, "**", "")
	line = strings.TrimSpace(line)
	line = listMarker.ReplaceAllString(line, "")

	return strings.TrimSpace(spaces.ReplaceAllString(line, " "))
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

func checkFrontmatter(t *testing.T, name string, raw []byte) {
	t.Helper()

	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		t.Errorf("skill %s: SKILL.md does not open with frontmatter", name)

		return
	}

	frontmatter, _, closed := strings.Cut(text[len("---\n"):], "\n---\n")
	if !closed {
		t.Errorf("skill %s: the frontmatter is never closed", name)

		return
	}

	decoder := yaml.NewDecoder(strings.NewReader(frontmatter))
	decoder.KnownFields(true)

	var fm skillFrontmatter
	if err := decoder.Decode(&fm); err != nil {
		t.Errorf("skill %s: the frontmatter is not strict YAML, which Codex refuses: %v", name, err)

		return
	}

	if fm.Name != name {
		t.Errorf("skill %s: frontmatter names %q", name, fm.Name)
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

	for _, m := range referenceLink.FindAllStringSubmatch(skill, -1) {
		if _, err := os.Stat(filepath.Join(dir, "references", m[1])); err != nil {
			t.Errorf("skill %s: SKILL.md links references/%s, which cannot be read: %v", name, m[1], err)
		}
	}

	refs, err := os.ReadDir(filepath.Join(dir, "references"))
	if os.IsNotExist(err) {
		return
	}

	if err != nil {
		t.Errorf("skill %s: read references: %v", name, err)

		return
	}

	for _, ref := range refs {
		if !strings.Contains(skill, "references/"+ref.Name()) {
			t.Errorf("skill %s: references/%s is linked from nowhere in SKILL.md, so no session loads it", name,
				ref.Name())
		}
	}
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
