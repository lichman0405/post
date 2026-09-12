package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The verdict is asked for twice — as the final message (validated by the
// harness against the schema) and as RESULT.json — and a Reviewer that
// satisfies only the first loses a perfectly good verdict. T0102's did exactly
// that: an approving verdict went into StructuredOutput and RESULT.json was
// never written, so collect refused with "no such file" and a re-review of
// correct work was the only alternative. Asking in prose is not a mechanism.
func TestVerdictRecoveredFromTheSessionLog(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "worker.log")
	// A stream-json session: noise, a tool call, then the harness's final
	// `result` event carrying the structured output as a JSON string.
	content := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"not a verdict"}]}}`,
		`{"type":"result","subtype":"success","result":"{\"task_id\":\"T0102\",\"verdict\":\"approve\",\"summary\":\"ok\",\"findings\":[],\"risks\":[]}"}`,
	}, "\n")
	if err := os.WriteFile(log, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := verdictFromSessionLog(log)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the harness's final structured output was not recovered")
	}
	if !strings.Contains(string(got), `"verdict":"approve"`) {
		t.Errorf("recovered the wrong document: %s", got)
	}
}

// The neighbour: a log whose final message is not a verdict must NOT be
// mistaken for one, or an unrelated run would be recorded as evidence.
func TestVerdictNotRecoveredFromANonVerdictLog(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "worker.log")
	content := `{"type":"result","subtype":"success","result":"{\"task_id\":\"T0102\",\"summary\":\"no verdict field\"}"}`
	if err := os.WriteFile(log, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := verdictFromSessionLog(log); err != nil || ok {
		t.Fatalf("a document without a verdict field was accepted as one (ok=%v, err=%v)", ok, err)
	}
	// A missing log is not an error either — it just recovers nothing.
	if _, ok, err := verdictFromSessionLog(filepath.Join(dir, "absent.log")); err != nil || ok {
		t.Fatalf("a missing log produced ok=%v err=%v, want false/nil", ok, err)
	}
}
