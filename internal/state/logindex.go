package state

import (
	"context"
	"crypto/rand"
	"database/sql"
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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/joblog"
	"github.com/junioryono/billet/internal/regularfile"
)

// LogIndex is a trigram index of what jobs wrote to their logs, so a search can find a
// line across every job. IT IS NOT THE LEDGER AND SHARES NOTHING WITH IT: indexing a
// busy job must not take the ledger's one writer slot, and losing all of this loses no
// decision, since it is a cache of what the nodes hold. The text itself is not here:
// a contentless table holds only which rowids match a query, so a hit is read back from
// the node that ran the job.
//
// ONE FILE A DAY, in a directory of its own. A retention and a size bound then remove
// whole files, which costs no delete, no tombstones and no vacuum, and a search opens
// only the days in its window. Measured on the shared Linux host on 2026-11-04 with
// modernc.org/sqlite v1.56.0 (SQLite 3.53.3): 35,000 lines a second, the file 3.06 times
// the text, and a phrase across 200,000 lines in about a millisecond.
//
// The index's id says which index this is. A node re-ships what it holds to an id it has
// not shipped to, which is how a promoted controller, a restored one or a deleted
// directory comes to hold it all again.
type LogIndex struct {
	dir string
	id  string

	mu   sync.Mutex
	day  string
	db   *sql.DB
	stop bool
}

// logIndexIDFile holds the index's id, and a day's file is logIndexDay.
const logIndexIDFile = "index.id"

var logIndexDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.db$`)

// rowid packs a line's job, its number and its level, since a contentless table can
// keep nothing else about a row: job<<28 | line<<2 | level.
const (
	logRowLineShift = 2
	logRowJobShift  = 28
	logRowLevelMask = 1<<logRowLineShift - 1
	logRowLineMask  = 1<<(logRowJobShift-logRowLineShift) - 1
)

// logIndexSchema is a day file's whole schema. contentless: the text is tokenised and
// dropped. columnsize=0 saves a row a line that nothing here reads.
var logIndexSchema = []string{
	`CREATE VIRTUAL TABLE IF NOT EXISTS lines USING fts5(text, content='', columnsize=0, tokenize='trigram')`,
	`CREATE TABLE IF NOT EXISTS jobs (
		job INTEGER PRIMARY KEY, lease TEXT NOT NULL UNIQUE, first_at INTEGER NOT NULL
	) STRICT`,
	`CREATE TABLE IF NOT EXISTS chunks (
		job INTEGER NOT NULL, first_line INTEGER NOT NULL, lines INTEGER NOT NULL,
		step INTEGER NOT NULL, step_name TEXT NOT NULL, received_at INTEGER NOT NULL,
		PRIMARY KEY (job, first_line)
	) STRICT, WITHOUT ROWID`,
}

// OpenLogIndex opens the index under dir for writing, creating it and its id if this is
// its first use. Only the controller does: a standby holds nothing, and a promoted one
// holds what nodes send it.
func OpenLogIndex(dir string) (*LogIndex, error) {
	if err := (durablefile.Installer{}).MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state: create the log index directory: %w", err)
	}

	id, err := readOrMakeLogIndexID(dir)
	if err != nil {
		return nil, err
	}

	return &LogIndex{dir: dir, id: id}, nil
}

// readOrMakeLogIndexID is the id on disk, or a new one. A file that cannot be read is
// not absent, and a new id would have every node re-ship everything.
func readOrMakeLogIndexID(dir string) (string, error) {
	data, err := regularfile.ReadFile(filepath.Join(dir, logIndexIDFile), 128, regularfile.Options{NoFollow: true})
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}

		return "", errors.New("state: the log index's id file is empty")
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("state: read the log index's id: %w", err)
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("state: make the log index's id: %w", err)
	}

	id := hex.EncodeToString(raw)
	if _, err := (durablefile.Installer{}).Install(dir, logIndexIDFile, 0o600, func(w io.Writer) error {
		_, err := io.WriteString(w, id+"\n")

		return err
	}); err != nil {
		return "", fmt.Errorf("state: record the log index's id: %w", err)
	}

	return id, nil
}

// ID names this index.
func (x *LogIndex) ID() string { return x.id }

// Close closes the file it writes.
func (x *LogIndex) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()

	x.stop = true
	if x.db == nil {
		return nil
	}

	err := x.db.Close()
	x.db = nil

	return err
}

// dayDB is the file of day, the one it writes, opening it if the day has turned. Called
// with mu held.
func (x *LogIndex) dayDB(ctx context.Context, day string) (*sql.DB, error) {
	if x.stop {
		return nil, errors.New("state: the log index is closed")
	}

	if x.db != nil && x.day == day {
		return x.db, nil
	}

	// BOTH TOGETHER: a next day that cannot be opened leaves no day being written, and Retain reads
	// the name as saying there is a file to close.
	if x.db != nil {
		_ = x.db.Close()
		x.db, x.day = nil, ""
	}

	db, err := sql.Open("sqlite", dsn(filepath.Join(x.dir, day+".db"), "busy_timeout(5000)", "journal_mode(WAL)",
		"synchronous(NORMAL)"))
	if err != nil {
		return nil, fmt.Errorf("state: open a day of the log index: %w", err)
	}

	db.SetMaxOpenConns(1)

	if err := createLogDay(ctx, db); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("state: create a day of the log index: %w", err)
	}

	x.db, x.day = db, day

	return db, nil
}

// createLogDay makes a day's schema in one transaction, so a search beside it finds all of it or
// none of it, and what it finds with only part of it is a damaged day.
func createLogDay(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range logIndexSchema {
		if err := logIndexExec(ctx, tx, stmt); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// logExecer is what logIndexExec runs a statement on.
type logExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// logIndexExec runs one of this file's statements. A table the trigram index's
// virtual table makes is not in sqlc's catalogue, so none of them can be generated.
func logIndexExec(ctx context.Context, db logExecer, stmt string, args ...any) error {
	_, err := db.ExecContext(ctx, stmt, args...) //billet:ignore rawsql // the log index is not the ledger, and sqlc has no catalogue entry for a virtual table

	return err
}

// LogIndexChunk is one chunk to index: whose, where its lines begin, and when it arrived,
// on this control plane's clock, which is what a search's times are in.
type LogIndexChunk struct {
	Lease      string
	FirstLine  int64
	Chunk      *joblog.Chunk
	ReceivedAt time.Time
}

// ErrLogLineRange says a chunk's lines lie past what the index's rowid can number.
var ErrLogLineRange = errors.New("state: a chunk's lines are past what the log index can number")

// Add indexes one chunk into the file of the day it arrived. A chunk this day's file
// holds already is a repeat, which is what a node re-shipping sends, and is ignored. All
// of it or none, in one transaction of this file's own.
func (x *LogIndex) Add(ctx context.Context, c LogIndexChunk) error {
	lines := int64(len(c.Chunk.Lines))
	if c.FirstLine < 0 || c.FirstLine+lines > joblog.MaxLines {
		return ErrLogLineRange
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	db, err := x.dayDB(ctx, c.ReceivedAt.UTC().Format(time.DateOnly))
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin a log index write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	at := c.ReceivedAt.UnixMilli()

	if err := logIndexExec(ctx, tx, `INSERT OR IGNORE INTO jobs (lease, first_at) VALUES (?, ?)`, c.Lease, at); err != nil {
		return fmt.Errorf("state: record a job in the log index: %w", err)
	}

	var job int64
	if err := logIndexQueryRow(ctx, tx, `SELECT job FROM jobs WHERE lease = ?`, c.Lease).Scan(&job); err != nil {
		return fmt.Errorf("state: read a job in the log index: %w", err)
	}

	added, err := logIndexExecCount(ctx, tx, `INSERT OR IGNORE INTO chunks (job, first_line, lines, step, step_name, received_at)
		VALUES (?, ?, ?, ?, ?, ?)`, job, c.FirstLine, lines, c.Chunk.Step, c.Chunk.StepName, at)
	if err != nil {
		return fmt.Errorf("state: record a chunk in the log index: %w", err)
	}

	if added == 0 {
		return nil
	}

	for i := range c.Chunk.Lines {
		text := c.Chunk.Lines[i].Text
		row := job<<logRowJobShift | (c.FirstLine+int64(i))<<logRowLineShift | int64(joblog.LevelOf(text))

		if err := logIndexExec(ctx, tx, `INSERT INTO lines (rowid, text) VALUES (?, ?)`, row, text); err != nil {
			return fmt.Errorf("state: index a line: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit a log index write: %w", err)
	}

	return nil
}

// logIndexExecCount is logIndexExec, saying how many rows it changed.
func logIndexExecCount(ctx context.Context, tx *sql.Tx, stmt string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, stmt, args...) //billet:ignore rawsql // the log index is not the ledger, and sqlc has no catalogue entry for a virtual table
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

// logRowQuerier is what logIndexQueryRow asks.
type logRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func logIndexQueryRow(ctx context.Context, db logRowQuerier, stmt string, args ...any) *sql.Row {
	return db.QueryRowContext(ctx, stmt, args...) //billet:ignore rawsql // the log index is not the ledger, and sqlc has no catalogue entry for a virtual table
}

// Retain removes whole days older than before, then the oldest while the files take more
// than maxBytes in all, and says how many it removed. The day it is writing stays while it
// is inside the retention, so a bound smaller than one day's lines costs the days before it
// and no more; past the retention it goes like any other, or an idle controller would keep
// its last day for ever. Whether a reader has a removed file open does not matter: it reads
// on from what it opened.
func (x *LogIndex) Retain(before time.Time, maxBytes int64) (int, error) {
	x.mu.Lock()
	defer x.mu.Unlock()

	days, err := logIndexDays(x.dir)
	if err != nil {
		return 0, err
	}

	cutoff := before.UTC().Format(time.DateOnly)
	removed := 0

	var total int64
	sizes := make(map[string]int64, len(days))

	for _, day := range days {
		sizes[day] = logIndexDaySize(x.dir, day)
		total += sizes[day]
	}

	for _, day := range days {
		if day == x.day && day >= cutoff {
			continue
		}

		if day >= cutoff && total <= maxBytes {
			break
		}

		if day == x.day {
			if x.db != nil {
				_ = x.db.Close()
			}

			x.db, x.day = nil, ""
		}

		for _, suffix := range []string{".db", ".db-wal", ".db-shm"} {
			if err := os.Remove(filepath.Join(x.dir, day+suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, fmt.Errorf("state: remove a day of the log index: %w", err)
			}
		}

		total -= sizes[day]
		removed++
	}

	return removed, nil
}

// logIndexDaySize is what a day's files take, a file that cannot be read counting as
// none: the next pass reads it again.
func logIndexDaySize(dir, day string) int64 {
	var total int64

	for _, suffix := range []string{".db", ".db-wal"} {
		if info, err := os.Stat(filepath.Join(dir, day+suffix)); err == nil {
			total += info.Size()
		}
	}

	return total
}

// logIndexDays is the days the directory holds, oldest first, by name.
func logIndexDays(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("state: read the log index directory: %w", err)
	}

	var days []string

	for _, e := range entries {
		if logIndexDay.MatchString(e.Name()) {
			days = append(days, strings.TrimSuffix(e.Name(), ".db"))
		}
	}

	slices.Sort(days)

	return days, nil
}

// LogHit is one line a query matched: whose it is, its number and level, which step it
// was in, and when its chunk arrived on the control plane's clock.
type LogHit struct {
	Lease      string
	Line       int64
	Level      joblog.Level
	Step       int64
	StepName   string
	ReceivedAt time.Time
}

// LogSearch is one search of the index, read-only.
type LogSearch struct {
	// Match is the FTS5 query, which BuildLogMatch writes from a person's.
	Match string
	// Since is the oldest day a file is opened for. A file holds a day's arrivals, so a
	// hit can be a little older than this, and the caller compares ReceivedAt.
	Since time.Time
	// MaxScan bounds the rows read in all.
	MaxScan int
	// Visit is told each hit, newest job first within a day and newest day first. False
	// stops the search.
	Visit func(LogHit) bool
}

// ErrLogIndexAbsent says this host holds no log index at all, which is could-not-tell,
// not no matches.
var ErrLogIndexAbsent = errors.New("state: this host holds no log index")

// SearchLogIndex runs a search over the index under dir, and says whether its bound
// stopped it. It opens each file for reading only, without taking anything the writer
// needs, so it runs beside a control plane or after one has stopped.
func SearchLogIndex(ctx context.Context, dir string, s LogSearch) (partial bool, err error) {
	days, err := logIndexDays(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, ErrLogIndexAbsent
	}

	if err != nil {
		return false, err
	}

	first := s.Since.UTC().Format(time.DateOnly)
	scanned := 0

	for _, day := range slices.Backward(days) {
		if day < first {
			break
		}

		more, stopped, err := searchLogDay(ctx, filepath.Join(dir, day+".db"), s, &scanned)
		if err != nil {
			return false, err
		}

		if stopped {
			return more, nil
		}
	}

	return false, nil
}

// logJobs reads a day's jobs and chunks as the hits reach them.
type logJobs struct {
	db     *sql.DB
	leases map[int64]string
	chunks map[int64][]logChunkRow
}

type logChunkRow struct {
	firstLine, lines, step int64
	stepName               string
	receivedAt             int64
}

// logDaySchema is how many tables a day's file holds and how many of them are the one a search
// reads, from one statement, so both describe one moment.
func logDaySchema(ctx context.Context, db *sql.DB) (tables, lines int, err error) {
	row := logIndexQueryRow(ctx, db, `SELECT count(*), count(CASE WHEN name = 'lines' THEN 1 END) FROM sqlite_master`)
	err = row.Scan(&tables, &lines)

	return tables, lines, err
}

// matchLogDay runs a search of one day, and answers its rows, or no rows and no error for a day with
// nothing in it: removed by the retention since its name was read, or created a moment ago with its
// schema not yet committed. Neither held a line, so neither ends a search of the other days.
//
// WHAT A FAILED MATCH MET IS DECIDED FROM ONE LOOK. createLogDay is one transaction, so a look finds
// no tables, or all of them, or a damaged day with only some, which may have held lines and is the
// failure it is. A schema committing after the match failed is why finding it whole means matching
// again rather than reporting the error.
func matchLogDay(path string, match func() (*sql.Rows, error),
	schema func() (tables, lines int, err error),
) (*sql.Rows, error) {
	rows, err := match()
	if err == nil {
		return rows, nil
	}

	if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
		return nil, nil
	}

	if !strings.Contains(err.Error(), "no such table") {
		return nil, err
	}

	tables, lines, schemaErr := schema()

	switch {
	case schemaErr != nil:
		return nil, err
	case tables == 0:
		return nil, nil
	case lines > 0:
		return match()
	default:
		return nil, err
	}
}

// searchLogDay searches one file. stopped says the search is over, and more whether its
// bound is what ended it, as against the caller's Visit.
func searchLogDay(ctx context.Context, path string, s LogSearch, scanned *int) (more, stopped bool, err error) {
	// mode=ro, OR A MISSING FILE IS CREATED: a retention removing this day while the search
	// reached it would leave an empty file behind, which no later pass would remove.
	db, err := sql.Open("sqlite", dsnWith(path, map[string]string{"mode": "ro"}, "busy_timeout(5000)", "query_only(ON)"))
	if err != nil {
		return false, false, fmt.Errorf("state: open a day of the log index: %w", err)
	}
	defer db.Close()

	// TWO CONNECTIONS, because the rows of the match stay open while a hit's job and
	// chunks are read on the other.
	db.SetMaxOpenConns(2)

	match := func() (*sql.Rows, error) {
		return db.QueryContext(ctx, `SELECT rowid FROM lines WHERE lines MATCH ? ORDER BY rowid DESC`, s.Match) //billet:ignore rawsql // the log index is not the ledger, and sqlc has no catalogue entry for a virtual table
	}

	rows, err := matchLogDay(path, match, func() (int, int, error) { return logDaySchema(ctx, db) })
	if err != nil {
		return false, false, fmt.Errorf("state: search a day of the log index: %w", err)
	}

	if rows == nil {
		return false, false, nil
	}
	defer rows.Close()

	jobs := &logJobs{db: db, leases: map[int64]string{}, chunks: map[int64][]logChunkRow{}}

	for rows.Next() {
		var row int64
		if err := rows.Scan(&row); err != nil {
			return false, false, fmt.Errorf("state: read a log index hit: %w", err)
		}

		if *scanned >= s.MaxScan {
			return true, true, nil
		}

		*scanned++

		hit, ok, err := jobs.hit(ctx, row)
		if err != nil {
			return false, false, err
		}

		if ok && !s.Visit(hit) {
			return false, true, nil
		}
	}

	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("state: search a day of the log index: %w", err)
	}

	return false, false, nil
}

// hit is what a row says, with its job's lease and its chunk. A row whose job or chunk is
// not found is left out rather than made up: one transaction wrote all three.
func (j *logJobs) hit(ctx context.Context, row int64) (LogHit, bool, error) {
	job := row >> logRowJobShift
	line := (row >> logRowLineShift) & logRowLineMask

	lease, known := j.leases[job]
	if !known {
		err := logIndexQueryRow(ctx, j.db, `SELECT lease FROM jobs WHERE job = ?`, job).Scan(&lease)
		if errors.Is(err, sql.ErrNoRows) {
			return LogHit{}, false, nil
		}

		if err != nil {
			return LogHit{}, false, fmt.Errorf("state: read a job in the log index: %w", err)
		}

		j.leases[job] = lease
	}

	chunks, loaded := j.chunks[job]
	if !loaded {
		var err error
		if chunks, err = j.readChunks(ctx, job); err != nil {
			return LogHit{}, false, err
		}

		j.chunks[job] = chunks
	}

	i, found := slices.BinarySearchFunc(chunks, line, func(c logChunkRow, l int64) int {
		switch {
		case l < c.firstLine:
			return 1
		case l >= c.firstLine+c.lines:
			return -1
		default:
			return 0
		}
	})
	if !found {
		return LogHit{}, false, nil
	}

	c := &chunks[i]

	return LogHit{Lease: lease, Line: line, Level: joblog.Level(row & logRowLevelMask), Step: c.step,
		StepName: c.stepName, ReceivedAt: time.UnixMilli(c.receivedAt).UTC()}, true, nil
}

// readChunks is a job's chunks in line order.
func (j *logJobs) readChunks(ctx context.Context, job int64) ([]logChunkRow, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT first_line, lines, step, step_name, received_at FROM chunks WHERE job = ? ORDER BY first_line`, job) //billet:ignore rawsql // the log index is not the ledger, and sqlc has no catalogue entry for a virtual table
	if err != nil {
		return nil, fmt.Errorf("state: read a job's chunks in the log index: %w", err)
	}
	defer rows.Close()

	var out []logChunkRow

	for rows.Next() {
		var c logChunkRow
		if err := rows.Scan(&c.firstLine, &c.lines, &c.step, &c.stepName, &c.receivedAt); err != nil {
			return nil, fmt.Errorf("state: read a job's chunks in the log index: %w", err)
		}

		out = append(out, c)
	}

	return out, rows.Err()
}

// ErrLogQuery says a person's query cannot be searched for. Its text is the operator's
// own, so quoting it is no leak.
var ErrLogQuery = errors.New("a query this index cannot search")

// logIndexMinTerm is the shortest term a trigram can find, in characters, which is what the
// tokenizer walks: a shorter one matches nothing and says nothing (measured 2026-11-04 on ASCII),
// which would read as no such line.
const logIndexMinTerm = 3

// BuildLogMatch writes the FTS5 query for terms a line must hold and terms it must not,
// each quoted so nothing in one is read as an operator. A line must hold at least one
// term, because a query of only exclusions is a syntax error (measured 2026-11-04), and a
// term of fewer than three characters is refused, since a trigram cannot find it and the
// answer would read as no such line.
func BuildLogMatch(must, mustNot []string) (string, error) {
	if len(must) == 0 {
		return "", fmt.Errorf("%w: it needs at least one term to find, not only ones to exclude", ErrLogQuery)
	}

	quote := func(term string) (string, error) {
		if utf8.RuneCountInString(term) < logIndexMinTerm {
			return "", fmt.Errorf("%w: %q is under %d characters, which a trigram index cannot find", ErrLogQuery, term,
				logIndexMinTerm)
		}

		return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`, nil
	}

	var b strings.Builder

	for i, term := range must {
		q, err := quote(term)
		if err != nil {
			return "", err
		}

		if i > 0 {
			b.WriteByte(' ')
		}

		b.WriteString(q)
	}

	for _, term := range mustNot {
		q, err := quote(term)
		if err != nil {
			return "", err
		}

		b.WriteString(" NOT " + q)
	}

	return b.String(), nil
}
