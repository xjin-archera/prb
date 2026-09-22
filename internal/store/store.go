// Package store persists reviews, comments and chat in one SQLite file.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Comment struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Side        string `json:"side"`
	Severity    string `json:"severity"`
	Body        string `json:"body"`
	Include     bool   `json:"include"`
	AIGenerated bool   `json:"ai_generated"`
}

type Cut struct {
	Finding string `json:"finding"`
	Reason  string `json:"reason"`
}

// Resolved is an earlier finding that a follow-up review judged addressed.
type Resolved struct {
	Finding string `json:"finding"`
	Note    string `json:"note"`
}

type Result struct {
	Verdict         string     `json:"verdict"`
	VerifiedLocally string     `json:"verified_locally"`
	SummaryBody     string     `json:"summary_body"`
	Comments        []Comment  `json:"comments"`
	Cut             []Cut      `json:"cut"`
	Resolved        []Resolved `json:"resolved,omitempty"`
	LGTM            bool       `json:"lgtm"`
}

type Posted struct {
	URL        string `json:"url"`
	Event      string `json:"event"`
	At         int64  `json:"at"`
	Inline     int    `json:"inline"`
	Unanchored int    `json:"unanchored"`
	Cleanup    string `json:"cleanup,omitempty"`
}

type ChatMessage struct {
	Role string `json:"role"` // user | assistant
	Text string `json:"text"`
	At   int64  `json:"at"`
}

// Status values.
const (
	Idle    = "idle"
	Queued  = "queued"
	Running = "running"
	Done    = "done"
	Failed  = "failed"
	PostedS = "posted"
)

type Review struct {
	Repo       string
	Number     int
	Status     string
	HeadSHA    string
	Worktree   string
	StartedAt  int64
	FinishedAt int64
	Error      string
	Result     *Result
	Posted     *Posted
	CostUSD    float64
	DurationS  float64
	SessionID  string
	PrevSHA    string // head of the previous round when this review is a follow-up
	Rounds     int    // completed review rounds (1 = first review)
	Tampered   string // files the reviewer modified in the worktree (reverted by the app), one per line
	Chat       []ChatMessage
}

func (r Review) Key() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// IncludedCount is the number of comments that will be posted.
func (r Review) IncludedCount() int {
	if r.Result == nil {
		return 0
	}
	n := 0
	for _, c := range r.Result.Comments {
		if c.Include {
			n++
		}
	}
	return n
}

func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func Now() int64 { return time.Now().Unix() }

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS reviews (
  key TEXT PRIMARY KEY, repo TEXT NOT NULL, number INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'idle', head_sha TEXT NOT NULL DEFAULT '', worktree TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
  result TEXT, posted TEXT, cost_usd REAL NOT NULL DEFAULT 0, duration_s REAL NOT NULL DEFAULT 0,
  session_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS comments (
  id TEXT PRIMARY KEY, review_key TEXT NOT NULL REFERENCES reviews(key) ON DELETE CASCADE, position INTEGER NOT NULL,
  path TEXT NOT NULL, line INTEGER NOT NULL, side TEXT NOT NULL, severity TEXT NOT NULL, body TEXT NOT NULL,
  include INTEGER NOT NULL DEFAULT 1, ai_generated INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS comments_review ON comments(review_key, position);
CREATE TABLE IF NOT EXISTS chat (
  id INTEGER PRIMARY KEY AUTOINCREMENT, review_key TEXT NOT NULL REFERENCES reviews(key) ON DELETE CASCADE,
  role TEXT NOT NULL, text TEXT NOT NULL, at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS chat_review ON chat(review_key, id);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc sqlite is happiest with a single writer connection
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate adds columns introduced after the first schema; SQLite has no ADD COLUMN IF NOT EXISTS.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(reviews)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	for col, ddl := range map[string]string{
		"prev_sha": `ALTER TABLE reviews ADD COLUMN prev_sha TEXT NOT NULL DEFAULT ''`,
		"rounds":   `ALTER TABLE reviews ADD COLUMN rounds INTEGER NOT NULL DEFAULT 0`,
		"tampered": `ALTER TABLE reviews ADD COLUMN tampered TEXT NOT NULL DEFAULT ''`,
	} {
		if !have[col] {
			if _, err := db.Exec(ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// Get returns the review for a PR, or an idle placeholder.
func (s *Store) Get(ctx context.Context, repo string, number int) (Review, error) {
	r := Review{Repo: repo, Number: number, Status: Idle}
	var result, posted sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT status, head_sha, worktree, started_at, finished_at, error, result, posted,
		cost_usd, duration_s, session_id, prev_sha, rounds, tampered FROM reviews WHERE key = ?`, r.Key()).
		Scan(&r.Status, &r.HeadSHA, &r.Worktree, &r.StartedAt, &r.FinishedAt, &r.Error, &result, &posted, &r.CostUSD, &r.DurationS, &r.SessionID, &r.PrevSHA, &r.Rounds, &r.Tampered)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if result.Valid {
		var res Result
		if err := json.Unmarshal([]byte(result.String), &res); err == nil {
			r.Result = &res
		}
	}
	if posted.Valid {
		var p Posted
		if err := json.Unmarshal([]byte(posted.String), &p); err == nil {
			r.Posted = &p
		}
	}
	if r.Result != nil {
		if r.Result.Comments, err = s.comments(ctx, r.Key()); err != nil {
			return r, err
		}
	}
	if r.Chat, err = s.chat(ctx, r.Key()); err != nil {
		return r, err
	}
	return r, nil
}

func (s *Store) comments(ctx context.Context, key string) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, line, side, severity, body, include, ai_generated
		FROM comments WHERE review_key = ? ORDER BY position`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Path, &c.Line, &c.Side, &c.Severity, &c.Body, &c.Include, &c.AIGenerated); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) chat(ctx context.Context, key string) ([]ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, text, at FROM chat WHERE review_key = ? ORDER BY id`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.Role, &m.Text, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Save upserts the review row and replaces its comments. Chat is appended separately.
func (s *Store) Save(ctx context.Context, r Review) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var result, posted any
	if r.Result != nil {
		res := *r.Result
		res.Comments = nil // comments live in their own table
		b, _ := json.Marshal(res)
		result = string(b)
	}
	if r.Posted != nil {
		b, _ := json.Marshal(r.Posted)
		posted = string(b)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO reviews (key, repo, number, status, head_sha, worktree, started_at, finished_at,
		error, result, posted, cost_usd, duration_s, session_id, prev_sha, rounds, tampered) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET status=excluded.status, head_sha=excluded.head_sha, worktree=excluded.worktree,
		started_at=excluded.started_at, finished_at=excluded.finished_at, error=excluded.error, result=excluded.result,
		posted=excluded.posted, cost_usd=excluded.cost_usd, duration_s=excluded.duration_s, session_id=excluded.session_id,
		prev_sha=excluded.prev_sha, rounds=excluded.rounds, tampered=excluded.tampered`,
		r.Key(), r.Repo, r.Number, r.Status, r.HeadSHA, r.Worktree, r.StartedAt, r.FinishedAt, r.Error, result, posted,
		r.CostUSD, r.DurationS, r.SessionID, r.PrevSHA, r.Rounds, r.Tampered)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE review_key = ?`, r.Key()); err != nil {
		return err
	}
	if r.Result != nil {
		for i, c := range r.Result.Comments {
			if c.ID == "" {
				c.ID = NewID()
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO comments (id, review_key, position, path, line, side, severity, body, include, ai_generated)
				VALUES (?,?,?,?,?,?,?,?,?,?)`, c.ID, r.Key(), i, c.Path, c.Line, c.Side, c.Severity, c.Body, c.Include, c.AIGenerated); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) AddChat(ctx context.Context, key string, m ChatMessage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat (review_key, role, text, at) VALUES (?,?,?,?)`, key, m.Role, m.Text, m.At)
	return err
}

func (s *Store) ClearChat(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat WHERE review_key = ?`, key)
	return err
}

// Delete removes a review and everything attached to it.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM reviews WHERE key = ?`, key)
	return err
}

// All returns every stored review keyed by "owner/repo#N" (comments and chat included).
func (s *Store) All(ctx context.Context) (map[string]Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, number FROM reviews`)
	if err != nil {
		return nil, err
	}
	type k struct {
		repo string
		n    int
	}
	var keys []k
	for rows.Next() {
		var kk k
		if err := rows.Scan(&kk.repo, &kk.n); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, kk)
	}
	rows.Close()
	out := map[string]Review{}
	for _, kk := range keys {
		r, err := s.Get(ctx, kk.repo, kk.n)
		if err != nil {
			return nil, err
		}
		out[r.Key()] = r
	}
	return out, nil
}

// FailOrphans marks reviews left running by a previous process as failed.
func (s *Store) FailOrphans(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reviews SET status = 'failed', finished_at = ?,
		error = 'server restarted while this review was running; use Continue or re-run' WHERE status IN ('running','queued')`, Now())
	return err
}
