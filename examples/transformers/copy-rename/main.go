package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filemill/internal/contract"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--info" {
		fmt.Println(`{"name":"Copy and Rename","version":"1.0","inputs":["txt"],"outputs":["txt"],"options":{}}`)
		return
	}
	if len(os.Args) != 2 {
		fail("expected job.json path")
		return
	}
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err.Error())
		return
	}
	var job contract.Job
	if err := json.Unmarshal(b, &job); err != nil {
		fail(err.Error())
		return
	}
	if len(job.InputFiles) != 1 {
		fail("exactly one input file is required")
		return
	}
	source := filepath.FromSlash(job.InputFiles[0].Path)
	destination := filepath.Join(job.OutputDirectory, "copied-"+filepath.Base(job.InputFiles[0].Name))
	if err := copyFile(source, destination); err != nil {
		fail(err.Error())
		return
	}
	write(contract.Result{ContractVersion: contract.Version, Success: true, Message: "File copied and renamed", OutputFiles: []contract.OutputFile{{Name: filepath.Base(destination), Path: filepath.ToSlash(destination)}}, Details: map[string]any{}})
}
func fail(message string) {
	write(contract.Result{ContractVersion: contract.Version, Success: false, Message: message, ErrorCode: "COPY_FAILED", Details: map[string]any{}})
	os.Exit(1)
}
func write(r contract.Result) {
	b, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile("result.json", b, 0644)
}
func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
