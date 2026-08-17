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
		routes:   map[string]string{"workerlist@mill.example.com": "workerlist"},
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
			"recipient":  "workerlist@mill.example.com",
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

// One email addressed to two FileMill addresses arrives as two deliveries
// sharing a Message-Id. Each routes to its own operation and owes its own
// reply, so each must become its own submission with its own job. Keyed on the
// Message-Id alone, the second delivery found the first's submission, saw
// attachment 0 already recorded, skipped it, and returned success having
// created nothing — a silent no-reply.
func TestIntakeSeparatesTwoRecipientsOfOneMessage(t *testing.T) {
	fake := newFakeEngine()
	s := routedService(fake)
	s.routes["vwk@mill.example.com"] = "vworker"

	for _, recipient := range []string{"workerlist@mill.example.com", "vwk@mill.example.com"} {
		r := signedMultipart(t, "k",
			map[string]string{
				"recipient":  recipient,
				"sender":     "kevin@example.com",
				"subject":    "dual send",
				"Message-Id": "<one-message@example.com>",
			},
			map[string][]byte{"attachment-1": []byte("pdf")})

		if status, _ := s.receive(r); status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", recipient, status)
		}
	}

	if len(fake.subs) != 2 {
		t.Errorf("one message to two addresses must be two submissions; got %d", len(fake.subs))
	}
	if fake.submitCount != 2 {
		t.Errorf("each recipient owes a job; got %d submitted", fake.submitCount)
	}
	if len(fake.operations) != 2 || fake.operations[0] == fake.operations[1] {
		t.Errorf("each recipient must run its own operation; got %v", fake.operations)
	}
}

// A retry differs from a fan-out only by the recipient, and mail addresses are
// case-insensitive — so the recipient is normalized before it becomes part of
// the key. Without that, Mailgun varying the case between attempts would submit
// the job twice and send two replies.
func TestIntakeNormalizesRecipientCaseInTheKey(t *testing.T) {
	fake := newFakeEngine()
	s := routedService(fake)

	for _, recipient := range []string{"workerlist@mill.example.com", "WorkerList@Mill.Example.COM"} {
		r := signedMultipart(t, "k",
			map[string]string{
				"recipient":  recipient,
				"sender":     "kevin@example.com",
				"Message-Id": "<retried@example.com>",
			},
			map[string][]byte{"attachment-1": []byte("pdf")})

		if status, _ := s.receive(r); status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", recipient, status)
		}
	}

	if len(fake.subs) != 1 {
		t.Errorf("a retry with different casing must reuse the submission; got %d", len(fake.subs))
	}
	if fake.submitCount != 1 {
		t.Errorf("a retry must not submit a second job; got %d", fake.submitCount)
	}
}

// A failure creating a job is our fault, so the handler must return 500 to make
// Mailgun retry (which then heals the submission).
func TestIntakeSubmitFailureReturns500(t *testing.T) {
	fake := newFakeEngine()
	fake.failSubmitAfter = 0 // fail on the first Submit
	s := routedService(fake)

	r := signedMultipart(t, "k",
		map[string]string{"recipient": "workerlist@mill.example.com", "sender": "kevin@example.com", "Message-Id": "<orig-2@example.com>"},
		map[string][]byte{"attachment-1": []byte("one")})

	status, _ := s.receive(r)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if _, ok := fake.expected[1]; ok {
		t.Fatal("submission was finalized despite a submit failure")
	}
}
