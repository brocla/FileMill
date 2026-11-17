package contract

import (
	"encoding/json"
	"testing"
)

// message_html is optional: a transformer that sends it gets an HTML reply,
// and one that doesn't decodes exactly as before.
func TestResultDecodesOptionalMessageHTML(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"present", `{"contract_version":"1","success":true,"message":"ok","message_html":"<p>ok</p>"}`, "<p>ok</p>"},
		{"absent", `{"contract_version":"1","success":true,"message":"ok"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r Result
			if err := json.Unmarshal([]byte(tc.raw), &r); err != nil {
				t.Fatal(err)
			}
			if r.MessageHTML != tc.want || r.Message != "ok" {
				t.Errorf("decoded %+v, want MessageHTML %q and Message %q", r, tc.want, "ok")
			}
		})
	}
}
