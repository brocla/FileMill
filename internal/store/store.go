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

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
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
	if _, err := db.Exec("UPDATE jobs SET status='interrupted', completed_at=? WHERE status='running'", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Create(j Job) error {
	_, err := s.db.Exec("INSERT INTO jobs(id, operation, status, input_name, created_at) VALUES(?,?,?,?,?)", j.ID, j.Operation, "queued", j.InputName, j.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) Next() (*Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRow("SELECT id, operation, status, input_name, message, created_at FROM jobs WHERE status='queued' ORDER BY created_at LIMIT 1")
	var j Job
	var created string
	if err := row.Scan(&j.ID, &j.Operation, &j.Status, &j.InputName, &j.Message, &created); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	res, err := tx.Exec("UPDATE jobs SET status='running', started_at=? WHERE id=? AND status='queued'", time.Now().UTC().Format(time.RFC3339Nano), j.ID)
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
	j.Status = "running"
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
