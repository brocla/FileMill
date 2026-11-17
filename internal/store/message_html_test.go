package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// The reply is built later from PendingEmails, not from the job's workspace, so
// a job's HTML message has to come back out of the database with it.
func TestCompleteRoundTripsMessageHTMLThroughPendingEmails(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.Create(Job{ID: "job", Operation: "check_schedule", InputName: "shift.pdf", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	id, _, err := s.BeginEmail("<m@example.com>", "a@example.com", "b@example.com", "subject")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddEmailJob(id, 0, "job"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmailExpected(id, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete("job", StatusSucceeded, "All checks passed", "<p>All checks passed</p>"); err != nil {
		t.Fatal(err)
	}

	subs, err := s.PendingEmails()
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || len(subs[0].Jobs) != 1 {
		t.Fatalf("PendingEmails = %+v, want one submission with one job", subs)
	}
	got := subs[0].Jobs[0].Job
	if got.Message != "All checks passed" || got.MessageHTML != "<p>All checks passed</p>" {
		t.Errorf("job = %+v, want both messages back", got)
	}
}

// shippedJobsDB writes a database whose jobs table has the shape that shipped
// before message_html, with one completed job in it. Built by hand, like
// oldSchemaDB, so the migration is tested against the real old schema.
func shippedJobsDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE jobs (
 id TEXT PRIMARY KEY, operation TEXT NOT NULL, status TEXT NOT NULL, input_name TEXT NOT NULL,
 message TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, started_at TEXT, completed_at TEXT,
 files_deleted_at TEXT
)`,
		`INSERT INTO jobs(id,operation,status,input_name,message,created_at,completed_at)
 VALUES('old-job','workerlist','succeeded','list.pdf','Worker list spreadsheet created','2026-10-28T04:24:59.454741Z','2026-10-28T04:25:01.3167217Z')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("build old schema: %v", err)
		}
	}
	return path
}

// Opening a database from before message_html adds the column and keeps every
// row, which reads back with no HTML. Opening it again changes nothing.
func TestOpenAddsMessageHTMLColumnKeepingRows(t *testing.T) {
	path := shippedJobsDB(t)
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		got, err := s.Get("old-job")
		s.Close()
		if err != nil {
			t.Fatalf("Get after migration: %v", err)
		}
		if got.Message != "Worker list spreadsheet created" || got.MessageHTML != "" {
			t.Errorf("migrated job = %+v, want its message kept and no HTML", got)
		}
	}
}

// The previous binary, kept for rollback, inserts jobs without naming the new
// column. The column's default has to make that work.
func TestOldStyleInsertWorksAfterMigration(t *testing.T) {
	s, _ := openTestStore(t)
	if _, err := s.db.Exec("INSERT INTO jobs(id, operation, status, input_name, created_at) VALUES('rollback','op','queued','a.txt','2026-11-16T00:00:00.000000000Z')"); err != nil {
		t.Fatalf("an insert that leaves message_html out must still work: %v", err)
	}
}
