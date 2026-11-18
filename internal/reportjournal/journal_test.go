package reportjournal

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBatchPersistsAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node-12", "reports")
	j, err := Open(dir)
	if err != nil { t.Fatal(err) }
	bytes := map[int][2]int64{12: {1024, 2048}, 19: {9, 7}}
	b, err := j.Append(bytes)
	if err != nil { t.Fatal(err) }
	bytes[12] = [2]int64{999, 999}
	j2, err := Open(dir)
	if err != nil { t.Fatal(err) }
	pending, err := j2.Pending()
	if err != nil { t.Fatal(err) }
	if len(pending) != 1 || !reflect.DeepEqual(pending[0], b) {
		t.Fatalf("reopened batches = %+v; expected %+v", pending, b)
	}
	if pending[0].Traffic[12] != [2]int64{1024, 2048} {
		t.Fatal("journal reused mutable caller traffic")
	}
	info, err := os.Stat(filepath.Join(dir, b.ID+".json"))
	if err != nil { t.Fatal(err) }
	if info.Mode().Perm() != 0o600 { t.Fatalf("batch permissions = %v", info.Mode().Perm()) }
	if err := j2.Acknowledge(b.ID); err != nil { t.Fatal(err) }
	pending, err = j.Pending()
	if err != nil || len(pending) != 0 { t.Fatalf("pending after ack=%+v, err=%v", pending, err) }
}

func TestJournalDistinctBatchesAndBadInputs(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil { t.Fatal(err) }
	a, err := j.Append(map[int][2]int64{1: {1, 2}})
	if err != nil { t.Fatal(err) }
	b, err := j.Append(map[int][2]int64{1: {1, 2}})
	if err != nil { t.Fatal(err) }
	if a.ID == b.ID { t.Fatal("duplicate batch IDs") }
	for _, invalid := range []map[int][2]int64{nil, {0: {1, 2}}, {1: {-1, 2}}} {
		if _, err := j.Append(invalid); err == nil { t.Fatalf("accepted invalid %v", invalid) }
	}
	if err := j.Acknowledge("../../escape"); err == nil { t.Fatal("path traversal accepted") }
	p, err := j.Pending()
	if err != nil || len(p) != 2 { t.Fatalf("pending %+v err %v", p, err) }
}

func TestCorruptJournalFailsClosed(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil { t.Fatal(err) }
	b, err := j.Append(map[int][2]int64{7: {123, 456}})
	if err != nil { t.Fatal(err) }
	if err := os.WriteFile(j.path(b.ID), []byte("{not-json"), 0o600); err != nil { t.Fatal(err) }
	if _, err := j.Pending(); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("corrupt pending batch should block replay, got %v", err)
	}
}
