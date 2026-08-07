package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"filemill/internal/contract"
)

// writeTransformers writes a transformers.yaml under root/config so Open can
// load it. The commands are never executed by Submit, so dummies suffice.
func writeTransformers(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, "config")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transformers.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// readJob reads back the job.json Submit wrote for a job id.
func readJob(t *testing.T, root, id string) contract.Job {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "data", "jobs", id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var j contract.Job
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

// TestSubmitThreadsTransformerOptionsIntoJob is the keystone for the layout
// feature: an operation that declares options in transformers.yaml must have
// those options land in the job.json the transformer reads, while an operation
// with none must still get an empty object (not null), preserving today's
// contract for optionless transformers like copy_rename.
func TestSubmitThreadsTransformerOptionsIntoJob(t *testing.T) {
	root := t.TempDir()
	writeTransformers(t, root, `transformers:
  - operation: with_opts
    command: ["python.exe", "x.py"]
    options: { layout: sheets }
    extensions: [txt]
  - operation: no_opts
    command: ["python.exe", "x.py"]
    extensions: [txt]
`)
	app, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	src := filepath.Join(root, "input.txt")
	if err := os.WriteFile(src, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	// An operation with declared options threads them into job.json.
	id, err := app.Submit("with_opts", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := readJob(t, root, id).Options["layout"]; got != "sheets" {
		t.Errorf("with_opts: job.Options[layout] = %v, want \"sheets\"", got)
	}

	// An operation with no options still serializes an empty object, not null,
	// so the contract seen by optionless transformers is unchanged.
	id2, err := app.Submit("no_opts", src)
	if err != nil {
		t.Fatal(err)
	}
	opts := readJob(t, root, id2).Options
	if opts == nil {
		t.Errorf("no_opts: job.Options is null, want an empty object {}")
	}
	if len(opts) != 0 {
		t.Errorf("no_opts: job.Options = %v, want empty", opts)
	}
}
