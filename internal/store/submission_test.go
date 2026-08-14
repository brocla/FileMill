package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// oldSchemaDB writes a database with the pre-migration shape: message_id alone
// carries the UNIQUE constraint. Built by hand rather than by an older Open, so
// the migration is tested against the schema that actually shipped and not
// against whatever the current code happens to produce.
//
// The rows mirror the live store at the time this was written: submissions with
// child jobs and deliveries, plus one submission that failed before its commit
// point (expected_jobs=0, no child rows) — an inert row the migration must
// carry across rather than tidy away.
func oldSchemaDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE email_submissions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL UNIQUE,
 sender TEXT NOT NULL, recipient TEXT NOT NULL, subject TEXT NOT NULL,
 expected_jobs INTEGER NOT NULL DEFAULT 0, delivered_at TEXT
)`,
		`CREATE TABLE email_submission_jobs (
 submission_id INTEGER NOT NULL, attachment_index INTEGER NOT NULL, job_id TEXT NOT NULL UNIQUE,
 PRIMARY KEY(submission_id, attachment_index), FOREIGN KEY(submission_id) REFERENCES email_submissions(id)
)`,
		`CREATE TABLE email_deliveries (
 submission_id INTEGER NOT NULL, output_index INTEGER NOT NULL,
 file_id TEXT NOT NULL, link TEXT NOT NULL, created_at TEXT NOT NULL, deleted_at TEXT,
 PRIMARY KEY(submission_id, output_index), FOREIGN KEY(submission_id) REFERENCES email_submissions(id)
)`,
		// Non-contiguous ids, as the live store has: the child tables point at
		// these values, so the migration must preserve them exactly.
		`INSERT INTO email_submissions(id,message_id,sender,recipient,subject,expected_jobs,delivered_at)
 VALUES(1,'<a@mail>','from@example.com','iwk@mill.test','one',1,'2026-08-01T00:00:00Z'),
       (24,'<b@mail>','from@example.com','iwk@mill.test','orphan',0,NULL),
       (62,'<c@mail>','from@example.com','vwk@mill.test','dual',1,'2026-08-13T23:34:36Z')`,
		`INSERT INTO email_submission_jobs(submission_id,attachment_index,job_id)
 VALUES(1,0,'job-1'),(62,0,'job-62')`,
		`INSERT INTO email_deliveries(submission_id,output_index,file_id,link,created_at)
 VALUES(62,0,'file-62','https://docs.google.com/62','2026-08-13T23:34:36Z')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("build old schema: %v", err)
		}
	}
	return path
}

// uniqueIndexColumns returns the column lists of every unique index on a table,
// which is how the migration recognizes the old schema and how these tests
// confirm the new one.
func (s *Store) uniqueIndexColumns(table string) ([][]string, error) {
	return uniqueIndexColumns(s.db, table)
}

// One email addressed to two FileMill addresses is two units of work: each
// recipient routes to its own operation and earns its own reply. Keyed on
// Message-Id alone the second recipient silently reused the first's submission,
// found its attachment already submitted, and no job was ever created for it.
func TestBeginEmailSeparatesRecipientsOfOneMessage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	first, created, err := s.BeginEmail("<same@mail>", "from@example.com", "vwk@mill.test", "dual send")
	if err != nil || !created {
		t.Fatalf("first recipient: created %t, err %v; want a new submission", created, err)
	}
	second, created, err := s.BeginEmail("<same@mail>", "from@example.com", "iwk@mill.test", "dual send")
	if err != nil {
		t.Fatalf("second recipient: %v", err)
	}
	if !created {
		t.Error("a second recipient of the same message must be a new submission, not a repeat")
	}
	if second == first {
		t.Fatalf("both recipients share submission %d; each needs its own", first)
	}
}

// The whole point of the key: Mailgun retries the same delivery after a failure
// or a slow response, and a retry must resume the existing submission rather
// than duplicate the job. Splitting the key by recipient must not weaken this,
// because a retry repeats the recipient too.
func TestBeginEmailStillCoalescesRetriesOfOneDelivery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	first, created, err := s.BeginEmail("<same@mail>", "from@example.com", "iwk@mill.test", "subject")
	if err != nil || !created {
		t.Fatalf("first attempt: created %t, err %v", created, err)
	}
	again, created, err := s.BeginEmail("<same@mail>", "from@example.com", "iwk@mill.test", "subject")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if created {
		t.Error("a retry of the same delivery must not report a new submission")
	}
	if again != first {
		t.Fatalf("retry got submission %d, want the original %d", again, first)
	}
}

// A store carrying the old single-column constraint must come forward without
// losing anything. Ids are the load-bearing part: both child tables reference
// email_submissions(id) and those references are unenforced (foreign_keys is
// off), so a rebuild that let AUTOINCREMENT reassign ids would silently detach
// every job and delivery rather than fail.
func TestOpenMigratesOldSubmissionSchemaPreservingRows(t *testing.T) {
	path := oldSchemaDB(t)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open old-schema database: %v", err)
	}
	defer s.Close()

	cols, err := s.uniqueIndexColumns("email_submissions")
	if err != nil {
		t.Fatal(err)
	}
	var pairFound, messageOnlyFound bool
	for _, c := range cols {
		switch {
		case len(c) == 2 && c[0] == "message_id" && c[1] == "recipient":
			pairFound = true
		case len(c) == 1 && c[0] == "message_id":
			messageOnlyFound = true
		}
	}
	if !pairFound {
		t.Errorf("migrated schema must be unique on (message_id, recipient); got %v", cols)
	}
	if messageOnlyFound {
		t.Errorf("the message_id-only constraint must be gone; got %v", cols)
	}

	// Every row survives, ids included — the orphan (24) as much as the rest.
	for _, want := range []struct {
		id        int64
		messageID string
		recipient string
		expected  int
	}{
		{1, "<a@mail>", "iwk@mill.test", 1},
		{24, "<b@mail>", "iwk@mill.test", 0},
		{62, "<c@mail>", "vwk@mill.test", 1},
	} {
		var messageID, recipient string
		var expected int
		err := s.db.QueryRow("SELECT message_id,recipient,expected_jobs FROM email_submissions WHERE id=?", want.id).
			Scan(&messageID, &recipient, &expected)
		if err != nil {
			t.Fatalf("submission %d did not survive: %v", want.id, err)
		}
		if messageID != want.messageID || recipient != want.recipient || expected != want.expected {
			t.Errorf("submission %d = %q/%q/%d, want %q/%q/%d",
				want.id, messageID, recipient, expected, want.messageID, want.recipient, want.expected)
		}
	}

	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM email_submissions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("row count changed: got %d, want 3", count)
	}

	// The child rows must still join, which is the thing id preservation buys.
	var jobID string
	if err := s.db.QueryRow(`SELECT j.job_id FROM email_submission_jobs j
 JOIN email_submissions e ON e.id=j.submission_id WHERE e.message_id='<c@mail>'`).Scan(&jobID); err != nil {
		t.Fatalf("job row lost its submission: %v", err)
	}
	if jobID != "job-62" {
		t.Errorf("joined job = %q, want job-62", jobID)
	}
	var fileID string
	if err := s.db.QueryRow(`SELECT d.file_id FROM email_deliveries d
 JOIN email_submissions e ON e.id=d.submission_id WHERE e.message_id='<c@mail>'`).Scan(&fileID); err != nil {
		t.Fatalf("delivery row lost its submission: %v", err)
	}
	if fileID != "file-62" {
		t.Errorf("joined delivery = %q, want file-62", fileID)
	}

	// The migration exists to allow this, so prove it on the migrated store.
	if _, created, err := s.BeginEmail("<c@mail>", "from@example.com", "iwk@mill.test", "dual"); err != nil || !created {
		t.Errorf("the second recipient of a migrated message must insert: created %t, err %v", created, err)
	}

	// A new submission must not collide with a preserved id. Copying explicit
	// ids into an AUTOINCREMENT table should carry sqlite_sequence forward;
	// assert it rather than trust it.
	id, _, err := s.BeginEmail("<new@mail>", "from@example.com", "iwk@mill.test", "after")
	if err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	if id <= 62 {
		t.Errorf("new submission got id %d, which reuses a migrated id; sqlite_sequence did not carry forward", id)
	}
}

// Opening twice must be a no-op the second time: the detector sees the new
// shape and skips. A migration that ran again would rebuild a table it has
// already rebuilt, on every worker start.
func TestOpenMigrationIsIdempotent(t *testing.T) {
	path := oldSchemaDB(t)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer again.Close()

	var count int
	if err := again.db.QueryRow("SELECT COUNT(*) FROM email_submissions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("second open changed the rows: got %d, want 3", count)
	}
	cols, err := again.uniqueIndexColumns("email_submissions")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || len(cols[0]) != 2 {
		t.Errorf("second open must leave exactly the pair constraint; got %v", cols)
	}
}

// A database created fresh gets the new shape directly, with no migration step.
func TestOpenCreatesPairConstraintOnFreshDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cols, err := s.uniqueIndexColumns("email_submissions")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || len(cols[0]) != 2 || cols[0][0] != "message_id" || cols[0][1] != "recipient" {
		t.Errorf("fresh database must be unique on (message_id, recipient); got %v", cols)
	}
}
