package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filemill/internal/contract"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--info" {
		fmt.Println(`{"name":"Uppercase","version":"1.0","inputs":["txt"],"outputs":["txt"],"options":{}}`)
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
	content, err := os.ReadFile(source)
	if err != nil {
		fail(err.Error())
		return
	}
	destination := filepath.Join(job.OutputDirectory, "uppercase-"+filepath.Base(job.InputFiles[0].Name))
	if err := os.WriteFile(destination, []byte(strings.ToUpper(string(content))), 0644); err != nil {
		fail(err.Error())
		return
	}
	write(contract.Result{ContractVersion: contract.Version, Success: true, Message: "File uppercased", OutputFiles: []contract.OutputFile{{Name: filepath.Base(destination), Path: filepath.ToSlash(destination)}}, Details: map[string]any{}})
}
func fail(message string) {
	write(contract.Result{ContractVersion: contract.Version, Success: false, Message: message, ErrorCode: "UPPERCASE_FAILED", Details: map[string]any{}})
	os.Exit(1)
}
func write(r contract.Result) {
	b, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile("result.json", b, 0644)
}
