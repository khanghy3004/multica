package daemon

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDumpSnapshotForReview(t *testing.T) {
	if os.Getenv("DUMP_SNAPSHOT") == "" {
		t.Skip("set DUMP_SNAPSHOT=1")
	}
	list, _, err := listLocalSubagents("claude")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i, la := range list {
		if i >= 3 {
			break
		}
		b, _ := json.MarshalIndent(la, "", "  ")
		t.Logf("%s", b)
	}
}
