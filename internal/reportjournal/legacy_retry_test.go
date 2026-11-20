package reportjournal

import "testing"

func TestLegacyRetryDurableTransitions(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.Append(map[int][2]int64{1: {10, 20}})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.MarkLegacyRetryable(b.ID); err != nil {
		t.Fatal(err)
	}
	j, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := j.LegacyRetryable()
	if err != nil || len(p) != 1 || p[0].ID != b.ID || p[0].Traffic[1] != [2]int64{10, 20} {
		t.Fatalf("retry: %v %v", p, err)
	}
	if err := j.BeginLegacyRetry(b.ID); err != nil {
		t.Fatal(err)
	}
	j, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err = j.LegacyRetryable()
	if err != nil || len(p) != 0 {
		t.Fatalf("ambiguous replay: %v %v", p, err)
	}
	p, err = j.Pending()
	if err != nil || len(p) != 1 || p[0].ID != b.ID {
		t.Fatalf("audit missing: %v %v", p, err)
	}
}
