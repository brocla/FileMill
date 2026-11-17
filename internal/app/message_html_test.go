package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filemill/internal/store"
)

// A succeeded job keeps both versions of its report, so delivery can send the
// HTML beside the text.
func TestExecuteStoresMessageHTMLOfASucceededJob(t *testing.T) {
	a, _ := newTestApp(t, fakeTransformer(t, "succeed-html")...)
	j := claimJob(t, a)

	a.execute(context.Background(), j)

	got, err := a.Job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusSucceeded || got.Message != fakeReport || got.MessageHTML != fakeReportHTML {
		t.Errorf("job = %s %q %q, want succeeded with both messages", got.Status, got.Message, got.MessageHTML)
	}
}

// A failed job's reply stays plain text, whatever its result.json carried.
func TestExecuteDropsMessageHTMLOfAFailedJob(t *testing.T) {
	a, _ := newTestApp(t, fakeTransformer(t, "reject-html")...)
	j := claimJob(t, a)

	a.execute(context.Background(), j)

	got, err := a.Job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed || got.MessageHTML != "" {
		t.Errorf("job = %s with HTML %q, want failed with none", got.Status, got.MessageHTML)
	}
}

// A report's whole text in the log would be one long line of \n escapes per
// job. The log keeps the first line and marks the cut; the full text is in the
// database and the reply. The HTML is never logged.
func TestFinishLogsOnlyTheFirstLineOfAMultiLineMessage(t *testing.T) {
	a, _ := newTestApp(t, fakeTransformer(t, "succeed-html")...)
	j := claimJob(t, a)

	a.execute(context.Background(), j)

	log, err := os.ReadFile(filepath.Join(a.data, "logs", "filemill.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), `message="All checks passed..."`) {
		t.Errorf("log must hold the first line, marked as cut; got:\n%s", log)
	}
	for _, leak := range []string{"CONTINUOUS COVERAGE", "<table>"} {
		if strings.Contains(string(log), leak) {
			t.Errorf("log must not contain %q; got:\n%s", leak, log)
		}
	}
}

func TestFirstLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"done", "done"},
		{"first\nsecond", "first..."},
		{"first\r\nsecond", "first..."},
		{"only line\n", "only line"},
		{"", ""},
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
