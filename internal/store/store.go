package store

import (
	"database/sql"
	"fmt"
	"strings"
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
	// Unique on the pair, not on message_id alone: one email addressed to two
	// FileMill addresses arrives as two deliveries sharing a Message-Id, and
	// each is its own unit of work. A Mailgun retry repeats the recipient as
	// well, so coalescing retries — the reason the key exists — is unaffected.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS email_submissions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL,
 sender TEXT NOT NULL, recipient TEXT NOT NULL, subject TEXT NOT NULL,
 expected_jobs INTEGER NOT NULL DEFAULT 0, delivered_at TEXT,
 UNIQUE(message_id, recipient)
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
	if err := migrateSubmissionKey(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate email_submissions: %w", err)
	}
	if _, err := db.Exec("UPDATE jobs SET status=?, completed_at=? WHERE status=?", StatusInterrupted, time.Now().UTC().Format(time.RFC3339Nano), StatusRunning); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrateSubmissionKey widens an existing email_submissions from UNIQUE on
// message_id to UNIQUE on (message_id, recipient). It is a no-op on a database
// created by the CREATE TABLE above, so it costs one PRAGMA per start.
//
// The old constraint was declared inline, which SQLite cannot drop in place, so
// this is the standard rebuild: create, copy, drop, rename, in one transaction.
//
// Ids are copied explicitly and that is the load-bearing part. Both child
// tables reference email_submissions(id) and the references are unenforced
// (foreign_keys is never turned on, and SQLite defaults it off), so letting
// AUTOINCREMENT reassign ids would not fail — it would silently detach every
// job and delivery from its submission.
//
// Rows are copied exactly as they stand, including any submission that failed
// before its commit point (expected_jobs=0). Such a row is already invisible to
// PendingEmails; a migration that edits data it does not have to edit is a
// migration that can corrupt data it does not have to touch.
func migrateSubmissionKey(db *sql.DB) error {
	indexes, err := uniqueIndexColumns(db, "email_submissions")
	if err != nil {
		return err
	}
	stale := false
	for _, columns := range indexes {
		if len(columns) == 1 && columns[0] == "message_id" {
			stale = true
		}
	}
	if !stale {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE email_submissions_new (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL,
 sender TEXT NOT NULL, recipient TEXT NOT NULL, subject TEXT NOT NULL,
 expected_jobs INTEGER NOT NULL DEFAULT 0, delivered_at TEXT,
 UNIQUE(message_id, recipient)
)`,
		`INSERT INTO email_submissions_new(id,message_id,sender,recipient,subject,expected_jobs,delivered_at)
 SELECT id,message_id,sender,recipient,subject,expected_jobs,delivered_at FROM email_submissions`,
		`DROP TABLE email_submissions`,
		`ALTER TABLE email_submissions_new RENAME TO email_submissions`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(stmt)[0], err)
		}
	}
	return tx.Commit()
}

// uniqueIndexColumns returns one column list per unique index on a table,
// including the implicit index behind a UNIQUE constraint. Reading the shape
// this way rather than matching text in sqlite_master keeps the migration's
// trigger independent of how the DDL happens to be spelled.
func uniqueIndexColumns(db *sql.DB, table string) ([][]string, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA index_list(%q)", table))
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		// seq, name, unique, origin, partial
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, err
		}
		if unique == 1 {
			names = append(names, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var indexes [][]string
	for _, name := range names {
		rows, err := db.Query(fmt.Sprintf("PRAGMA index_info(%q)", name))
		if err != nil {
			return nil, err
		}
		var columns []string
		for rows.Next() {
			var seqno, cid int
			var column sql.NullString
			if err := rows.Scan(&seqno, &cid, &column); err != nil {
				rows.Close()
				return nil, err
			}
			columns = append(columns, column.String)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		indexes = append(indexes, columns)
	}
	return indexes, nil
}
func (s *Store) BeginEmail(messageID, sender, recipient, subject string) (int64, bool, error) {
	res, err := s.db.Exec("INSERT OR IGNORE INTO email_submissions(message_id,sender,recipient,subject) VALUES(?,?,?,?)", messageID, sender, recipient, subject)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	var id int64
	err = s.db.QueryRow("SELECT id FROM email_submissions WHERE message_id=? AND recipient=?", messageID, recipient).Scan(&id)
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
