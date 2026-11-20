// Package reportjournal contains a durable local spool for report batches.
// Replay MUST NOT be enabled until the XBoard panel implements atomic
// batch-id deduplication. An ambiguous HTTP failure may already be charged.
package reportjournal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Batch contains raw, unmultiplied user bytes. XBoard applies node rate.
type Batch struct {
	ID        string           `json:"id"`
	CreatedAt int64            `json:"created_at"`
	Traffic   map[int][2]int64 `json:"traffic"`
}

// Journal is scoped to one node, not an entire Machine process.
type Journal struct{ dir string }

func Open(dir string) (*Journal, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("empty report journal directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	j := &Journal{dir: dir}
	if err := j.recoverIncompleteRenames(); err != nil {
		return nil, fmt.Errorf("recover journal: %w", err)
	}
	return j, nil
}

// recoverIncompleteRenames salvages complete temporary files left by a
// crash between fsync and rename. A partial temp blocks initialization
// instead of silently discarding potentially unbilled traffic.
func (j *Journal) recoverIncompleteRenames() error {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		src := filepath.Join(j.dir, entry.Name())
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		var batch Batch
		if err := json.Unmarshal(raw, &batch); err != nil {
			return fmt.Errorf("incomplete journal temp %q requires manual recovery: %w", entry.Name(), err)
		}
		if !validID(batch.ID) {
			return fmt.Errorf("invalid batch ID in incomplete journal temp %q", entry.Name())
		}
		if _, err := copyTraffic(batch.Traffic); err != nil {
			return fmt.Errorf("invalid counters in journal temp %q: %w", entry.Name(), err)
		}
		dst := j.path(batch.ID)
		if previous, err := os.ReadFile(dst); err == nil {
			if string(previous) != string(raw) {
				return fmt.Errorf("different payload exists for batch %s", batch.ID)
			}
			if err := os.Remove(src); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		} else if err := os.Rename(src, dst); err != nil {
			return err
		}
		if err := syncDirectory(j.dir); err != nil {
			return err
		}
	}
	return nil
}

func newID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func copyTraffic(traffic map[int][2]int64) (map[int][2]int64, error) {
	if len(traffic) == 0 {
		return nil, errors.New("no traffic to journal")
	}
	copied := make(map[int][2]int64, len(traffic))
	for uid, v := range traffic {
		if uid <= 0 || v[0] < 0 || v[1] < 0 {
			return nil, fmt.Errorf("invalid traffic counter for uid %d", uid)
		}
		copied[uid] = v
	}
	return copied, nil
}

// Append atomically journals one new batch and fsyncs it before returning.
// The caller must not treat this as a panel receipt or replay without
// server-side idempotency. An append error means storage is not guaranteed.
func (j *Journal) Append(traffic map[int][2]int64) (Batch, error) {
	copied, err := copyTraffic(traffic)
	if err != nil {
		return Batch{}, err
	}
	id, err := newID()
	if err != nil {
		return Batch{}, err
	}
	batch := Batch{ID: id, CreatedAt: time.Now().Unix(), Traffic: copied}
	raw, err := json.Marshal(batch)
	if err != nil {
		return Batch{}, err
	}
	f, err := os.CreateTemp(j.dir, ".pending-*")
	if err != nil {
		return Batch{}, fmt.Errorf("create temporary batch: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return Batch{}, err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return Batch{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return Batch{}, err
	}
	if err := f.Close(); err != nil {
		return Batch{}, err
	}
	if err := os.Rename(tmp, j.path(id)); err != nil {
		return Batch{}, err
	}
	// Once rename succeeds, do not signal a zero batch even if directory
	// fsync fails: the file may already exist. The caller must not restore
	// those counters into another new batch, which would double-charge.
	if err := syncDirectory(j.dir); err != nil {
		return batch, fmt.Errorf("sync persisted batch directory: %w", err)
	}
	return batch, nil
}

func (j *Journal) path(id string) string { return filepath.Join(j.dir, id+".json") }

// Pending returns saved batches in stable order. Corrupted files cause a
// hard error rather than a silent loss of potentially billable traffic.
func (j *Journal) Pending() ([]Batch, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Batch, 0)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !validID(id) {
			return nil, fmt.Errorf("invalid journal filename %q", name)
		}
		data, err := os.ReadFile(j.path(id))
		if err != nil {
			return nil, err
		}
		var b Batch
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		if b.ID != id {
			return nil, fmt.Errorf("batch ID mismatch in %s", name)
		}
		if _, err := copyTraffic(b.Traffic); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", name, err)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].CreatedAt == out[k].CreatedAt {
			return out[i].ID < out[k].ID
		}
		return out[i].CreatedAt < out[k].CreatedAt
	})
	return out, nil
}

// Acknowledge removes a batch ONLY after an explicit, idempotent panel
// receipt. Never use an ordinary 200 response on the legacy reporting API.
func (j *Journal) Acknowledge(id string) error {
	return j.remove(id)
}

// RemoveLegacyAudit discards an audit record after the legacy API reports
// success. It must never be used to acknowledge an idempotent batch.
func (j *Journal) RemoveLegacyAudit(id string) error {
	return j.remove(id)
}

func (j *Journal) remove(id string) error {
	if !validID(id) {
		return errors.New("invalid batch ID")
	}
	if err := os.Remove(j.path(id)); err != nil {
		return err
	}
	return syncDirectory(j.dir)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// MarkLegacyRetryable moves a proven-unsent batch out of ambiguous audit
// storage. A crash before this transition leaves it ambiguous, never replayed.
func (j *Journal) MarkLegacyRetryable(id string) error {
	if !validID(id) {
		return errors.New("invalid batch ID")
	}
	if err := os.Rename(j.path(id), filepath.Join(j.dir, id+".retry")); err != nil {
		return err
	}
	return syncDirectory(j.dir)
}

// LegacyRetryable returns only batches explicitly classified as unsent.
// Existing .json audit records are never implicitly promoted.
func (j *Journal) LegacyRetryable() ([]Batch, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var out []Batch
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".retry") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".retry")
		if !validID(id) {
			return nil, errors.New("invalid retry batch ID")
		}
		raw, err := os.ReadFile(filepath.Join(j.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var b Batch
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		if b.ID != id {
			return nil, errors.New("retry batch ID mismatch")
		}
		if _, err := copyTraffic(b.Traffic); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].CreatedAt == out[k].CreatedAt {
			return out[i].ID < out[k].ID
		}
		return out[i].CreatedAt < out[k].CreatedAt
	})
	return out, nil
}

// BeginLegacyRetry makes a retry ambiguous durably BEFORE sending it.
// If killed after this point, operators reconcile rather than replay.
func (j *Journal) BeginLegacyRetry(id string) error {
	if !validID(id) {
		return errors.New("invalid batch ID")
	}
	if _, err := os.Lstat(j.path(id)); err == nil {
		return errors.New("ambiguous batch already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(filepath.Join(j.dir, id+".retry"), j.path(id)); err != nil {
		return err
	}
	return syncDirectory(j.dir)
}
