package store

import (
	"database/sql"
	"fmt"
	"time"
)

// alertSendsRetention is the longest window the alert caps read, the rolling
// 24h of the daily cap. Older rows are pruned on each write.
const alertSendsRetention = 24 * time.Hour

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
	t, err := time.Parse(timeFormat, sentAt.String)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("alert_state %s: %w", category, err)
	}
	return t, suppressed, nil
}

// RecordAlertSent records a send in category at at and prunes sends too old
// for any cap window. The suppressed count is left alone: a send that then
// fails reported nothing, so ClearSuppressed is what zeroes it.
func (s *Store) RecordAlertSent(category string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO alert_state(category,last_sent_at,suppressed) VALUES(?,?,0)
 ON CONFLICT(category) DO UPDATE SET last_sent_at=excluded.last_sent_at`, category, formatTime(at)); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO alert_sends(sent_at) VALUES(?)", formatTime(at)); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM alert_sends WHERE sent_at<=?", formatTime(at.Add(-alertSendsRetention))); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearSuppressed zeroes category's suppressed count, once an email carrying
// it has gone out. A category with no row has nothing to clear.
func (s *Store) ClearSuppressed(category string) error {
	_, err := s.db.Exec("UPDATE alert_state SET suppressed=0 WHERE category=?", category)
	return err
}

// RecordAlertSuppressed counts one held-back alert against category.
func (s *Store) RecordAlertSuppressed(category string) error {
	_, err := s.db.Exec(`INSERT INTO alert_state(category,suppressed) VALUES(?,1)
 ON CONFLICT(category) DO UPDATE SET suppressed=suppressed+1`, category)
	return err
}

// AlertSendsSince returns every alert send after t, oldest first.
func (s *Store) AlertSendsSince(t time.Time) ([]time.Time, error) {
	rows, err := s.db.Query("SELECT sent_at FROM alert_sends WHERE sent_at>? ORDER BY sent_at", formatTime(t))
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
		at, err := time.Parse(timeFormat, raw)
		if err != nil {
			return nil, fmt.Errorf("alert_sends: %w", err)
		}
		out = append(out, at)
	}
	return out, rows.Err()
}
