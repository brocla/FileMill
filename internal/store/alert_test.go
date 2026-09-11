package store

import (
	"path/filepath"
	"testing"
	"time"

	"filemill/internal/alert"
)

var _ alert.Ledger = (*Store)(nil)

// countAlertSends reads the raw row count, so a test can see pruning that
// AlertSendsSince's window would hide. Test-only.
func (s *Store) countAlertSends() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM alert_sends").Scan(&n)
	return n, err
}

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestLastAlertUnknownCategoryIsZero(t *testing.T) {
	s, _ := openTestStore(t)

	sentAt, suppressed, err := s.LastAlert("job-systemic")
	if err != nil || !sentAt.IsZero() || suppressed != 0 {
		t.Fatalf("LastAlert = %v, %d, %v; want zero values", sentAt, suppressed, err)
	}
}

// A global cap can hold back a category that has never sent, so the count
// must be storable before there is any send time.
func TestAlertSuppressedBeforeAnySend(t *testing.T) {
	s, _ := openTestStore(t)

	for range 3 {
		if err := s.RecordAlertSuppressed("publish"); err != nil {
			t.Fatal(err)
		}
	}
	sentAt, suppressed, err := s.LastAlert("publish")
	if err != nil || !sentAt.IsZero() || suppressed != 3 {
		t.Fatalf("LastAlert = %v, %d, %v; want no send time and 3 suppressed", sentAt, suppressed, err)
	}
}

// Recording a send and clearing the backlog are separate steps: the send is
// recorded before it is attempted, to keep the caps exact, but the count of
// held-back alerts survives until one actually goes out.
func TestRecordAlertSentKeepsSuppressedUntilCleared(t *testing.T) {
	s, _ := openTestStore(t)
	at := time.Date(2026, 9, 11, 9, 30, 15, 123456789, time.UTC)

	if err := s.RecordAlertSuppressed("delivery"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlertSent("delivery", at); err != nil {
		t.Fatal(err)
	}
	sentAt, suppressed, err := s.LastAlert("delivery")
	if err != nil {
		t.Fatal(err)
	}
	if !sentAt.Equal(at) || suppressed != 1 {
		t.Fatalf("LastAlert = %v, %d; want %v, 1", sentAt, suppressed, at)
	}

	if err := s.ClearSuppressed("delivery"); err != nil {
		t.Fatal(err)
	}
	if _, suppressed, _ := s.LastAlert("delivery"); suppressed != 0 {
		t.Fatalf("suppressed after ClearSuppressed = %d, want 0", suppressed)
	}
	// Other categories are untouched.
	if other, _, _ := s.LastAlert("intake"); !other.IsZero() {
		t.Errorf("an unrelated category picked up a send time: %v", other)
	}
}

// The window is exclusive of its start, so a send leaves it exactly 24h (or
// 1h) after it went out. Times differing only in fractional seconds must
// still order correctly, which RFC3339Nano's trimmed zeros would break.
func TestAlertSendsSinceIsExclusiveAndOrdered(t *testing.T) {
	s, _ := openTestStore(t)
	whole := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	half := whole.Add(500 * time.Millisecond)
	later := whole.Add(time.Minute)

	for _, at := range []time.Time{later, whole, half} {
		if err := s.RecordAlertSent("intake", at); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.AlertSendsSince(whole)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Equal(half) || !got[1].Equal(later) {
		t.Fatalf("AlertSendsSince(%v) = %v; want [%v %v]", whole, got, half, later)
	}
}

// Rows are kept for 24h, the longest window the caps read, and pruned on the
// next write.
func TestAlertSendsArePrunedAfter24h(t *testing.T) {
	s, _ := openTestStore(t)
	t0 := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	for _, at := range []time.Time{t0, t0.Add(time.Hour), t0.Add(24 * time.Hour)} {
		if err := s.RecordAlertSent("restart", at); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.countAlertSends()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("alert_sends holds %d rows, want 2 (the send 24h old pruned)", n)
	}
	got, err := s.AlertSendsSince(t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Equal(t0.Add(time.Hour)) {
		t.Fatalf("kept sends = %v, want the two newest", got)
	}
}

// Surviving a restart is the whole reason the throttle lives in SQLite.
func TestAlertStateSurvivesReopen(t *testing.T) {
	s, path := openTestStore(t)
	at := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	if err := s.RecordAlertSent("restart", at); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlertSuppressed("restart"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	sentAt, suppressed, err := reopened.LastAlert("restart")
	if err != nil || !sentAt.Equal(at) || suppressed != 1 {
		t.Fatalf("after reopen LastAlert = %v, %d, %v; want %v, 1", sentAt, suppressed, err, at)
	}
	sends, err := reopened.AlertSendsSince(at.Add(-time.Minute))
	if err != nil || len(sends) != 1 {
		t.Fatalf("after reopen AlertSendsSince = %v, %v; want the one send", sends, err)
	}
}
