package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filemill/internal/alert"
	"filemill/internal/contract"
	"filemill/internal/store"
)

// main.go hands the App to the alert Emailer as its throttle ledger.
var _ alert.Ledger = (*App)(nil)

// fakeTransformerArg, as the test binary's first argument, makes it act as a
// transformer instead of running tests. See TestMain.
const fakeTransformerArg = "-filemill-fake-transformer"

// TestMain lets execute's tests run real transformer processes without Python
// or scripts: fakeTransformer points a transformer's command back at this test
// binary, which acts out the behavior named after fakeTransformerArg. The
// check comes before m.Run, so a normal test run never reaches it.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == fakeTransformerArg {
		os.Exit(actOut(os.Args[2]))
	}
	os.Exit(m.Run())
}

// actOut is the fake transformer. It runs in the job's workspace, as execute
// starts it there, and returns its exit code.
func actOut(mode string) int {
	switch mode {
	case "succeed":
		writeResult(contract.Version, true, "done")
		return 0
	case "reject": // the contract's way to turn an input down
		writeResult(contract.Version, false, "not a worker list")
		return 0
	case "reject-exit": // turned down, and exits nonzero as well
		writeResult(contract.Version, false, "not a worker list")
		return 1
	case "crash":
		fmt.Fprintln(os.Stderr, "Traceback: boom")
		return 1
	case "no-result":
		return 0
	case "bad-version":
		writeResult("0", true, "done")
		return 0
	case "succeed-exit": // result and exit code contradict each other
		writeResult(contract.Version, true, "done")
		return 3
	case "hang":
		time.Sleep(time.Minute) // killed by the job timeout long before this
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown fake transformer mode %q\n", mode)
	return 2
}

func writeResult(version string, success bool, message string) {
	b, err := json.Marshal(contract.Result{ContractVersion: version, Success: success, Message: message})
	if err == nil {
		err = os.WriteFile("result.json", b, 0644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

// fakeTransformer is a transformer command that runs this test binary as the
// fake transformer acting out mode.
func fakeTransformer(t *testing.T, mode string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, fakeTransformerArg, mode}
}

// fakeReporter records every alert, so a test can assert which failures
// report and which stay silent.
type fakeReporter struct {
	mu     sync.Mutex
	alerts []alert.Alert
	notify chan struct{} // when set, signalled (without blocking) on each report
}

func (r *fakeReporter) Report(a alert.Alert) {
	r.mu.Lock()
	r.alerts = append(r.alerts, a)
	r.mu.Unlock()
	if r.notify != nil {
		select {
		case r.notify <- struct{}{}:
		default:
		}
	}
}

func (r *fakeReporter) reports() []alert.Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.alerts)
}

// newTestApp opens an App with one operation, "fake", that runs command on
// .txt inputs, and gives it a fakeReporter.
func newTestApp(t *testing.T, command ...string) (*App, *fakeReporter) {
	t.Helper()
	root := t.TempDir()
	quoted := make([]string, len(command))
	for i, c := range command {
		quoted[i] = yamlQuote(c)
	}
	writeTransformers(t, root, fmt.Sprintf("transformers:\n  - operation: fake\n    command: [%s]\n    extensions: [txt]\n", strings.Join(quoted, ", ")))
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	rep := &fakeReporter{}
	a.SetReporter(rep)
	return a, rep
}

// yamlQuote single-quotes s, where YAML has no backslash escapes, so a
// Windows path survives as written.
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// claimJob submits input.txt to "fake" and claims it, as Run would, leaving
// the job running and ready for execute.
func claimJob(t *testing.T, a *App) store.Job {
	t.Helper()
	src := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit("fake", src); err != nil {
		t.Fatal(err)
	}
	j, err := a.store.Next()
	if err != nil || j == nil {
		t.Fatalf("claim: job %v, err %v", j, err)
	}
	return *j
}
