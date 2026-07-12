package contract

const Version = "1"

type Job struct {
	ContractVersion string         `json:"contract_version"`
	JobID           string         `json:"job_id"`
	Operation       string         `json:"operation"`
	InputFiles      []InputFile    `json:"input_files"`
	OutputDirectory string         `json:"output_directory"`
	Options         map[string]any `json:"options"`
}

type InputFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type Result struct {
	ContractVersion string         `json:"contract_version"`
	Success         bool           `json:"success"`
	Message         string         `json:"message"`
	ErrorCode       string         `json:"error_code,omitempty"`
	OutputFiles     []OutputFile   `json:"output_files,omitempty"`
	Details         map[string]any `json:"details"`
}

type OutputFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}
