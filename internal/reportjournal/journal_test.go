package reportjournal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBatchPersistsAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node-12", "reports")
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	bytes := map[int][2]int64{12: {1024, 2048}, 19: {9, 7}}
	b, err := j.Append(bytes)
	if err != nil {
		t.Fatal(err)
	}
	bytes[12] = [2]int64{999, 999}
	j2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := j2.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !reflect.DeepEqual(pending[0], b) {
		t.Fatalf("reopened batches = %+v; expected %+v", pending, b)
	}
	if pending[0].Traffic[12] != [2]int64{1024, 2048} {
		t.Fatal("journal reused mutable caller traffic")
	}
	info, err := os.Stat(filepath.Join(dir, b.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("batch permissions = %v", info.Mode().Perm())
	}
	if err := j2.Acknowledge(b.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = j.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after ack=%+v, err=%v", pending, err)
	}
}

func TestJournalDistinctBatchesAndBadInputs(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := j.Append(map[int][2]int64{1: {1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.Append(map[int][2]int64{1: {1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("duplicate batch IDs")
	}
	for _, invalid := range []map[int][2]int64{nil, {0: {1, 2}}, {1: {-1, 2}}} {
		if _, err := j.Append(invalid); err == nil {
			t.Fatalf("accepted invalid %v", invalid)
		}
	}
	if err := j.Acknowledge("../../escape"); err == nil {
		t.Fatal("path traversal accepted")
	}
	p, err := j.Pending()
	if err != nil || len(p) != 2 {
		t.Fatalf("pending %+v err %v", p, err)
	}
}

func TestCorruptJournalFailsClosed(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.Append(map[int][2]int64{7: {123, 456}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.path(b.ID), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Pending(); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("corrupt pending batch should block replay, got %v", err)
	}
}

func TestJournalRecoversFullyWrittenTempAfterCrash(t *testing.T) {
	dir := t.TempDir()
	batch := Batch{
		ID:        "0123456789abcdef0123456789abcdef",
		CreatedAt: 42,
		Traffic:   map[int][2]int64{7: {10, 20}},
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pending-interrupted"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := j.Pending()
	if err != nil || len(pending) != 1 || pending[0].ID != batch.ID {
		t.Fatalf("recovered pending=%+v err=%v", pending, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".pending-interrupted")); !os.IsNotExist(err) {
		t.Fatalf("temporary file not removed, err=%v", err)
	}
}

func TestJournalRefusesTruncatedCrashTemp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".pending-corrupt"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("crash-truncated journal must block startup and require recovery")
	}
}
