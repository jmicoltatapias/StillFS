package logging

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestInfoWritesStructuredRecord(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	logger.Info("ready", map[string]interface{}{"address": ":9000"})

	var record map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("log record is not valid JSON: %v", err)
	}
	if record["level"] != "info" || record["message"] != "ready" || record["address"] != ":9000" {
		t.Fatalf("unexpected log record: %#v", record)
	}
}
