package session

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id          TEXT PRIMARY KEY,
		kind        TEXT NOT NULL,
		prompt      TEXT NOT NULL,
		options     TEXT NOT NULL,
		context     TEXT NOT NULL,
		message_id  INTEGER NOT NULL DEFAULT 0,
		task_tag    TEXT,
		created_at  INTEGER NOT NULL,
		expires_at  INTEGER NOT NULL,
		status      TEXT NOT NULL DEFAULT 'pending',
		reply_choice TEXT,
		reply_text   TEXT,
		resolved_at  INTEGER,
		orphaned     INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
	CREATE INDEX IF NOT EXISTS idx_sessions_message ON sessions(message_id);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Idempotent migration for the orphaned column on older DBs.
	_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN orphaned INTEGER NOT NULL DEFAULT 0`)
	return &Store{db: db}, nil
}

func (s *Store) Save(ses *Session) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions(id,kind,prompt,options,context,task_tag,created_at,expires_at,status)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		ses.ID, string(ses.Kind), ses.Prompt,
		strings.Join(ses.Options, "\x1f"),
		ses.Context, ses.TaskTag,
		ses.CreatedAt.Unix(), ses.ExpiresAt.Unix(),
		string(StatusPending))
	return err
}

// SaveIfAbsent inserts the session only if its ID is not already present.
// Returns true if inserted, false if a row with this ID existed.
// Used by the pipeline HTTP path to allow idempotent retry with a stable ID.
func (s *Store) SaveIfAbsent(ses *Session) (bool, error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO sessions(id,kind,prompt,options,context,task_tag,created_at,expires_at,status)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		ses.ID, string(ses.Kind), ses.Prompt,
		strings.Join(ses.Options, "\x1f"),
		ses.Context, ses.TaskTag,
		ses.CreatedAt.Unix(), ses.ExpiresAt.Unix(),
		string(StatusPending))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) UpdateMessageID(id string, msgID int64) error {
	_, err := s.db.Exec(`UPDATE sessions SET message_id = ? WHERE id = ?`, msgID, id)
	return err
}

func (s *Store) Resolve(id string, r Reply) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET status=?, reply_choice=?, reply_text=?, resolved_at=?
		 WHERE id=? AND status='pending'`,
		string(r.Status), r.Choice, r.Text, time.Now().Unix(), id)
	return err
}

// MarkOrphaned flips a session to orphaned=1 without changing status.
// Used during rehydration so we know this session's original waiter is gone.
func (s *Store) MarkOrphaned(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET orphaned = 1 WHERE id = ?`, id)
	return err
}

// ExpireOverdue sets status='timeout' on any pending row whose expires_at has passed.
// Returns the number of rows expired. Call once on startup.
func (s *Store) ExpireOverdue() (int, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`UPDATE sessions SET status='timeout', resolved_at=?
		 WHERE status='pending' AND expires_at < ?`, now, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

type PendingRow struct {
	ID        string
	Kind      Kind
	Prompt    string
	Options   []string
	Context   string
	MessageID int64
	TaskTag   string
	CreatedAt time.Time
	ExpiresAt time.Time
	Orphaned  bool
}

func (s *Store) LoadPending() ([]PendingRow, error) {
	rows, err := s.db.Query(
		`SELECT id,kind,prompt,options,context,message_id,task_tag,created_at,expires_at,orphaned
		 FROM sessions WHERE status='pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingRow
	for rows.Next() {
		var r PendingRow
		var opts string
		var created, expires int64
		var orphaned int
		var kind string
		if err := rows.Scan(&r.ID, &kind, &r.Prompt, &opts, &r.Context,
			&r.MessageID, &r.TaskTag, &created, &expires, &orphaned); err != nil {
			return nil, err
		}
		r.Kind = Kind(kind)
		if opts != "" {
			r.Options = strings.Split(opts, "\x1f")
		}
		r.CreatedAt = time.Unix(created, 0)
		r.ExpiresAt = time.Unix(expires, 0)
		r.Orphaned = orphaned == 1
		out = append(out, r)
	}
	return out, nil
}

// ResolvedRow is what GetResolved returns for pipeline poll-based retrieval.
type ResolvedRow struct {
	ID         string
	Status     Status
	Choice     string
	Text       string
	Orphaned   bool
	ResolvedAt time.Time
}

var ErrNotFound = errors.New("session not found")

// Get returns the current state of any session, pending or resolved.
func (s *Store) Get(id string) (ResolvedRow, error) {
	var r ResolvedRow
	var status, choice, text string
	var resolvedAt sql.NullInt64
	var orphaned int
	row := s.db.QueryRow(
		`SELECT id, status, reply_choice, reply_text, resolved_at, orphaned
		 FROM sessions WHERE id = ?`, id)
	if err := row.Scan(&r.ID, &status, &choice, &text, &resolvedAt, &orphaned); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	r.Status = Status(status)
	r.Choice = choice
	r.Text = text
	r.Orphaned = orphaned == 1
	if resolvedAt.Valid {
		r.ResolvedAt = time.Unix(resolvedAt.Int64, 0)
	}
	return r, nil
}