package store

import (
	"database/sql"
	"fmt"
	"time"
)

// alertTimeFormat is fixed-width, unlike RFC3339Nano, which trims trailing
// zeros from the fraction: "10:00:00Z" sorts after "10:00:00.5Z". The alert
// windows are compared as strings in SQL, so the width matters.
const alertTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// alertSendsRetention is the longest window the alert caps read, the rolling
// 24h of the daily cap. Older rows are pruned on each write.
const alertSendsRetention = 24 * time.Hour

func formatAlertTime(t time.Time) string { return t.UTC().Format(alertTimeFormat) }

// LastAlert returns when category last sent an alert and how many have been
// suppressed since. Zero values mean it has no record; a zero sentAt with a
// count means a global cap held it back before it ever sent.
func (s *Store) LastAlert(category string) (time.Time, int, error) {
	var sentAt sql.NullString
	var suppressed int
	err := s.db.QueryRow("SELECT last_sent_at,suppressed FROM alert_state WHERE category=?", category).Scan(&sentAt, &suppressed)
	if err == sql.ErrNoRows {
		return time.Time{}, 0, nil
	}
	if err != nil {
		return time.Time{}, 0, err
	}
	if !sentAt.Valid {
		return time.Time{}, suppressed, nil
	}
	t, err := time.Parse(alertTimeFormat, sentAt.String)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("alert_state %s: %w", category, err)
	}
	return t, suppressed, nil
}

// RecordAlertSent records a send in category at at, resets its suppressed
// count, and prunes sends too old for any cap window.
func (s *Store) RecordAlertSent(category string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO alert_state(category,last_sent_at,suppressed) VALUES(?,?,0)
 ON CONFLICT(category) DO UPDATE SET last_sent_at=excluded.last_sent_at, suppressed=0`, category, formatAlertTime(at)); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO alert_sends(sent_at) VALUES(?)", formatAlertTime(at)); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM alert_sends WHERE sent_at<=?", formatAlertTime(at.Add(-alertSendsRetention))); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordAlertSuppressed counts one held-back alert against category.
func (s *Store) RecordAlertSuppressed(category string) error {
	_, err := s.db.Exec(`INSERT INTO alert_state(category,suppressed) VALUES(?,1)
 ON CONFLICT(category) DO UPDATE SET suppressed=suppressed+1`, category)
	return err
}

// AlertSendsSince returns every alert send after t, oldest first.
func (s *Store) AlertSendsSince(t time.Time) ([]time.Time, error) {
	rows, err := s.db.Query("SELECT sent_at FROM alert_sends WHERE sent_at>? ORDER BY sent_at", formatAlertTime(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		at, err := time.Parse(alertTimeFormat, raw)
		if err != nil {
			return nil, fmt.Errorf("alert_sends: %w", err)
		}
		out = append(out, at)
	}
	return out, rows.Err()
}
