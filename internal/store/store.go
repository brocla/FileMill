package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Job struct {
	ID, Operation, Status, InputName, Message string
	CreatedAt                                 time.Time
}
type Store struct{ db *sql.DB }

type EmailSubmission struct {
	ID                                    int64
	Sender, Recipient, Subject, MessageID string
	Expected                              int
	Jobs                                  []EmailJob
}
type EmailJob struct {
	Index int
	Job   Job
}

// Job status values. Lifecycle: queued -> running -> succeeded|failed.
// interrupted marks a job that was still running when the process stopped.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite allows only one writer. Serialize all access through a single
	// connection so the webhook handler, worker, and delivery goroutines can't
	// collide with SQLITE_BUSY. busy_timeout makes a lock wait briefly rather
	// than fail (covering a second process too, e.g. `submit` during `run`);
	// WAL keeps reads from blocking the writer.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, operation TEXT NOT NULL, status TEXT NOT NULL, input_name TEXT NOT NULL,
 message TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, started_at TEXT, completed_at TEXT
); CREATE INDEX IF NOT EXISTS jobs_status_created ON jobs(status, created_at);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS email_submissions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL UNIQUE,
 sender TEXT NOT NULL, recipient TEXT NOT NULL, subject TEXT NOT NULL,
 expected_jobs INTEGER NOT NULL DEFAULT 0, delivered_at TEXT
); CREATE TABLE IF NOT EXISTS email_submission_jobs (
 submission_id INTEGER NOT NULL, attachment_index INTEGER NOT NULL, job_id TEXT NOT NULL UNIQUE,
 PRIMARY KEY(submission_id, attachment_index), FOREIGN KEY(submission_id) REFERENCES email_submissions(id)
);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("UPDATE jobs SET status=?, completed_at=? WHERE status=?", StatusInterrupted, time.Now().UTC().Format(time.RFC3339Nano), StatusRunning); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) BeginEmail(messageID, sender, recipient, subject string) (int64, bool, error) {
	res, err := s.db.Exec("INSERT OR IGNORE INTO email_submissions(message_id,sender,recipient,subject) VALUES(?,?,?,?)", messageID, sender, recipient, subject)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	var id int64
	err = s.db.QueryRow("SELECT id FROM email_submissions WHERE message_id=?", messageID).Scan(&id)
	return id, n == 1, err
}
func (s *Store) SetEmailExpected(id int64, count int) error {
	_, err := s.db.Exec("UPDATE email_submissions SET expected_jobs=? WHERE id=?", count, id)
	return err
}
func (s *Store) EmailHasJob(id int64, index int) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM email_submission_jobs WHERE submission_id=? AND attachment_index=?", id, index).Scan(&n)
	return n > 0, err
}
func (s *Store) AddEmailJob(id int64, index int, jobID string) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO email_submission_jobs(submission_id,attachment_index,job_id) VALUES(?,?,?)", id, index, jobID)
	return err
}
func (s *Store) PendingEmails() ([]EmailSubmission, error) {
	// Read the submissions fully and close the result set before querying each
	// one's jobs. Holding the outer rows open across the per-submission queries
	// would deadlock under SetMaxOpenConns(1).
	rows, err := s.db.Query("SELECT id,message_id,sender,recipient,subject,expected_jobs FROM email_submissions WHERE delivered_at IS NULL AND expected_jobs>0")
	if err != nil {
		return nil, err
	}
	var subs []EmailSubmission
	for rows.Next() {
		var e EmailSubmission
		if err := rows.Scan(&e.ID, &e.MessageID, &e.Sender, &e.Recipient, &e.Subject, &e.Expected); err != nil {
			rows.Close()
			return nil, err
		}
		subs = append(subs, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var out []EmailSubmission
	for _, e := range subs {
		jr, err := s.db.Query("SELECT j.id,j.operation,j.status,j.input_name,j.message,j.created_at,esj.attachment_index FROM email_submission_jobs esj JOIN jobs j ON j.id=esj.job_id WHERE esj.submission_id=? ORDER BY esj.attachment_index", e.ID)
		if err != nil {
			return nil, err
		}
		for jr.Next() {
			var x EmailJob
			var c string
			if err := jr.Scan(&x.Job.ID, &x.Job.Operation, &x.Job.Status, &x.Job.InputName, &x.Job.Message, &c, &x.Index); err != nil {
				jr.Close()
				return nil, err
			}
			x.Job.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
			e.Jobs = append(e.Jobs, x)
		}
		if err := jr.Err(); err != nil {
			jr.Close()
			return nil, err
		}
		jr.Close()
		if len(e.Jobs) == e.Expected {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *Store) MarkEmailDelivered(id int64) error {
	_, err := s.db.Exec("UPDATE email_submissions SET delivered_at=? WHERE id=?", time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Create(j Job) error {
	_, err := s.db.Exec("INSERT INTO jobs(id, operation, status, input_name, created_at) VALUES(?,?,?,?,?)", j.ID, j.Operation, StatusQueued, j.InputName, j.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) Next() (*Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRow("SELECT id, operation, status, input_name, message, created_at FROM jobs WHERE status=? ORDER BY created_at LIMIT 1", StatusQueued)
	var j Job
	var created string
	if err := row.Scan(&j.ID, &j.Operation, &j.Status, &j.InputName, &j.Message, &created); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	res, err := tx.Exec("UPDATE jobs SET status=?, started_at=? WHERE id=? AND status=?", StatusRunning, time.Now().UTC().Format(time.RFC3339Nano), j.ID, StatusQueued)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, fmt.Errorf("could not claim job %s", j.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.Status = StatusRunning
	return &j, nil
}
func (s *Store) Complete(id, status, message string) error {
	_, err := s.db.Exec("UPDATE jobs SET status=?, message=?, completed_at=? WHERE id=?", status, message, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) Get(id string) (Job, error) {
	var j Job
	var c string
	err := s.db.QueryRow("SELECT id,operation,status,input_name,message,created_at FROM jobs WHERE id=?", id).Scan(&j.ID, &j.Operation, &j.Status, &j.InputName, &j.Message, &c)
	if err != nil {
		return Job{}, err
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
	return j, nil
}
