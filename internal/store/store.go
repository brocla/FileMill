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

// Delivery records one output file published to Google Drive. It is written
// before the reply that carries the link, so it serves three purposes at once:
// a retry finds it and skips re-uploading, the delivery loop reads the link out
// of it, and the retention sweep gets the file id and age it needs to delete
// the file later.
//
// It is keyed per output file, not per submission: one job may declare several
// output files, and each becomes its own Drive file.
type Delivery struct {
	SubmissionID int64
	OutputIndex  int
	FileID, Link string
	CreatedAt    time.Time
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
	// Published Drive files, one row per output file. The sweep sets deleted_at
	// rather than removing the row, so a swept record is never offered twice and
	// what was published stays inspectable.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS email_deliveries (
 submission_id INTEGER NOT NULL, output_index INTEGER NOT NULL,
 file_id TEXT NOT NULL, link TEXT NOT NULL, created_at TEXT NOT NULL, deleted_at TEXT,
 PRIMARY KEY(submission_id, output_index), FOREIGN KEY(submission_id) REFERENCES email_submissions(id)
); CREATE INDEX IF NOT EXISTS email_deliveries_sweep ON email_deliveries(deleted_at, created_at);`)
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

// PutDelivery records a published Drive file. It is the commit point for an
// upload: once this returns, no retry will upload that output again. INSERT OR
// IGNORE makes a repeated write harmless rather than overwriting a link that
// has already been mailed out.
func (s *Store) PutDelivery(submissionID int64, outputIndex int, fileID, link string) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO email_deliveries(submission_id,output_index,file_id,link,created_at) VALUES(?,?,?,?,?)",
		submissionID, outputIndex, fileID, link, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// Delivery returns the record for one output, if it has been published. A
// swept (deleted) record still counts: the file is gone from Drive, but the
// submission was delivered and must never be re-uploaded.
func (s *Store) Delivery(submissionID int64, outputIndex int) (Delivery, bool, error) {
	d := Delivery{SubmissionID: submissionID, OutputIndex: outputIndex}
	var created string
	err := s.db.QueryRow("SELECT file_id,link,created_at FROM email_deliveries WHERE submission_id=? AND output_index=?", submissionID, outputIndex).
		Scan(&d.FileID, &d.Link, &created)
	if err == sql.ErrNoRows {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return d, true, nil
}

// ExpiredDeliveries returns published files created before cutoff that the
// retention sweep has not yet deleted.
func (s *Store) ExpiredDeliveries(cutoff time.Time) ([]Delivery, error) {
	rows, err := s.db.Query("SELECT submission_id,output_index,file_id,link,created_at FROM email_deliveries WHERE deleted_at IS NULL AND created_at<? ORDER BY created_at",
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var created string
		if err := rows.Scan(&d.SubmissionID, &d.OutputIndex, &d.FileID, &d.Link, &created); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDeliveryDeleted records that the sweep removed the Drive file.
func (s *Store) MarkDeliveryDeleted(submissionID int64, outputIndex int) error {
	_, err := s.db.Exec("UPDATE email_deliveries SET deleted_at=? WHERE submission_id=? AND output_index=?",
		time.Now().UTC().Format(time.RFC3339Nano), submissionID, outputIndex)
	return err
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
