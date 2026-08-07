package mailgun

import (
	"net/http"
	"strings"
	"testing"
)

func routedService(engine Engine) *Service {
	return &Service{
		engine:   engine,
		signKey:  "k",
		maxBytes: 1 << 20,
		routes:   map[string]string{"workerlist@mill.keywind.cc": "workerlist"},
		allowed:  map[string]bool{},
		log:      discardLogger(),
	}
}

// twoAttachmentIntake drives intake directly with a two-attachment message.
//
// The webhook layer now refuses a message carrying more than one processable
// attachment, so this can no longer arrive through receive. intake itself stays
// group-shaped — its job is to build an N-job submission atomically — and that
// atomicity is exactly what these tests pin down, so they exercise it at its own
// boundary rather than through the routing policy above it.
func twoAttachmentIntake(t *testing.T, s *Service) error {
	t.Helper()
	r := signedMultipart(t, "k",
		map[string]string{
			"recipient":  "workerlist@mill.keywind.cc",
			"sender":     "kevin@example.com",
			"subject":    "Schedules",
			"Message-Id": "<orig-1@example.com>",
		},
		map[string][]byte{
			"attachment-1": []byte("one"),
			"attachment-2": []byte("two"),
		})
	if err := parseForm(r); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	return s.intake(r, "workerlist", attachments(r))
}

// The submission must only be finalized (expected_jobs set) after every job
// mapping is durable, so the delivery loop never observes a half-built group.
func TestIntakeSetsExpectedAsFinalCommit(t *testing.T) {
	fake := newFakeEngine()
	s := routedService(fake)

	if err := twoAttachmentIntake(t, s); err != nil {
		t.Fatalf("intake: %v", err)
	}

	if len(fake.calls) == 0 || fake.calls[len(fake.calls)-1] != "SetEmailExpected" {
		t.Fatalf("SetEmailExpected must be the last call; got %v", fake.calls)
	}
	adds, expects := 0, 0
	for _, c := range fake.calls {
		switch {
		case c == "SetEmailExpected":
			expects++
			if adds != 2 {
				t.Fatalf("SetEmailExpected happened after %d AddEmailJob calls, want 2; got %v", adds, fake.calls)
			}
		case strings.HasPrefix(c, "AddEmailJob"):
			adds++
		}
	}
	if expects != 1 {
		t.Fatalf("SetEmailExpected called %d times, want 1; got %v", expects, fake.calls)
	}
	if fake.expected[1] != 2 {
		t.Fatalf("expected_jobs = %d, want 2", fake.expected[1])
	}
}

// A Mailgun retry of the same message must resume the submission, not duplicate
// its jobs.
func TestIntakeResumeIsIdempotent(t *testing.T) {
	fake := newFakeEngine()
	s := routedService(fake)

	if err := twoAttachmentIntake(t, s); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if fake.submitCount != 2 {
		t.Fatalf("first delivery submitted %d jobs, want 2", fake.submitCount)
	}

	if err := twoAttachmentIntake(t, s); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if fake.submitCount != 2 {
		t.Fatalf("retry submitted extra jobs: total %d, want 2", fake.submitCount)
	}
	if len(fake.subs) != 1 {
		t.Fatalf("retry created a second submission group: %d groups", len(fake.subs))
	}
}

// A failure creating a job is our fault, so the handler must return 500 to make
// Mailgun retry (which then heals the submission).
func TestIntakeSubmitFailureReturns500(t *testing.T) {
	fake := newFakeEngine()
	fake.failSubmitAfter = 0 // fail on the first Submit
	s := routedService(fake)

	r := signedMultipart(t, "k",
		map[string]string{"recipient": "workerlist@mill.keywind.cc", "sender": "kevin@example.com", "Message-Id": "<orig-2@example.com>"},
		map[string][]byte{"attachment-1": []byte("one")})

	status, _ := s.receive(r)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if _, ok := fake.expected[1]; ok {
		t.Fatal("submission was finalized despite a submit failure")
	}
}
