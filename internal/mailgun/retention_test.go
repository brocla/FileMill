package mailgun

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"filemill/internal/store"
)

// retentionFixture wires an engine and publisher into a Service with no Mailgun
// endpoint — the sweep never sends mail.
func newRetentionFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	f := newDeliveryFixture(t)
	return f
}

// age registers a delivery record created at the given time.
func (f *deliveryFixture) age(submissionID int64, outputIndex int, fileID string, created time.Time) {
	f.engine.deliveries[deliveryKey{submissionID, outputIndex}] = store.Delivery{
		SubmissionID: submissionID, OutputIndex: outputIndex,
		FileID: fileID, Link: "https://docs.google.com/" + fileID, CreatedAt: created,
	}
}

// Files past the retention horizon are deleted from Drive and marked, so the
// sender's data does not live there forever.
func TestSweepDeletesExpiredFilesOnly(t *testing.T) {
	f := newDeliveryFixture(t)
	now := time.Now().UTC()
	f.age(1, 0, "old-file", now.Add(-31*24*time.Hour))
	f.age(2, 0, "fresh-file", now.Add(-2*24*time.Hour))

	if err := f.service.sweepExpired(context.Background()); err != nil {
		t.Fatalf("sweepExpired: %v", err)
	}

	if len(f.publisher.deleted) != 1 || f.publisher.deleted[0] != "old-file" {
		t.Fatalf("deleted = %v, want only old-file", f.publisher.deleted)
	}
	if !f.engine.sweptDeliveries[deliveryKey{1, 0}] {
		t.Error("the swept record must be marked deleted")
	}
	if f.engine.sweptDeliveries[deliveryKey{2, 0}] {
		t.Error("a fresh record must not be marked deleted")
	}
}

// A record already swept must not be offered again — otherwise every sweep
// would re-delete the same files forever.
func TestSweepDoesNotRepeatItself(t *testing.T) {
	f := newDeliveryFixture(t)
	f.age(1, 0, "old-file", time.Now().UTC().Add(-31*24*time.Hour))

	for i := 0; i < 3; i++ {
		if err := f.service.sweepExpired(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if len(f.publisher.deleted) != 1 {
		t.Errorf("deleted %d times, want 1", len(f.publisher.deleted))
	}
}

// One file that cannot be deleted must not stop the sweep: the rest of the
// batch still has to be cleaned up, and the failure is retried next time.
func TestSweepSkipsFailingDeleteAndContinues(t *testing.T) {
	f := newDeliveryFixture(t)
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	f.age(1, 0, "stuck-file", old)
	f.age(2, 0, "other-file", old)
	f.publisher.deleteErr = fmt.Errorf("drive unavailable")

	if err := f.service.sweepExpired(context.Background()); err != nil {
		t.Fatalf("a failing delete must be skipped, not returned: %v", err)
	}

	if len(f.publisher.deleted) != 2 {
		t.Errorf("the sweep must attempt every expired file; attempted %v", f.publisher.deleted)
	}
	if f.engine.sweptDeliveries[deliveryKey{1, 0}] {
		t.Error("a record whose delete failed must not be marked deleted — it has to be retried")
	}
	if !strings.Contains(f.logs.String(), "drive unavailable") {
		t.Errorf("the failure must be logged; got %q", f.logs.String())
	}
}

// With no publisher configured there is nothing to sweep, and asking Google
// anything would be wrong.
func TestSweepIsInertWithoutAPublisher(t *testing.T) {
	f := newDeliveryFixture(t)
	f.service.publisher = nil
	f.age(1, 0, "old-file", time.Now().UTC().Add(-31*24*time.Hour))

	if err := f.service.sweepExpired(context.Background()); err != nil {
		t.Fatalf("sweepExpired: %v", err)
	}
	if len(f.publisher.deleted) != 0 {
		t.Errorf("nothing may be deleted without a publisher; got %v", f.publisher.deleted)
	}
}
