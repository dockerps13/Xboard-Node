package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// deviceReportState records only user IDs potentially present on the panel.
// All calls are serialized by the service's single-flight report path.
type deviceReportState struct {
	path  string
	users map[int]bool
}

func openDeviceReportState(cfg *config.Config) (*deviceReportState, error) {
	scope := sha256.Sum256([]byte(cfg.Panel.URL + "\x00" + strconv.Itoa(cfg.Panel.MachineID) + "\x00" + strconv.Itoa(cfg.Panel.NodeID) + "\x00" + cfg.Panel.NodeType))
	if err := os.MkdirAll(cfg.Kernel.ConfigDir, 0700); err != nil {
		return nil, err
	}
	d := &deviceReportState{
		path:  filepath.Join(cfg.Kernel.ConfigDir, fmt.Sprintf("device-recovery-%x.json", scope)),
		users: make(map[int]bool),
	}
	raw, err := os.ReadFile(d.path)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &d.users); err != nil {
		return nil, err
	}
	if d.users == nil {
		return nil, fmt.Errorf("invalid null device recovery state")
	}
	for uid, known := range d.users {
		if uid <= 0 || !known {
			return nil, fmt.Errorf("invalid device recovery entry")
		}
	}
	return d, nil
}

func (d *deviceReportState) snapshot() map[int][]string {
	out := make(map[int][]string, len(d.users))
	for uid := range d.users {
		out[uid] = []string{}
	}
	return out
}

func (d *deviceReportState) update(alive map[int][]string, confirmed bool) error {
	next := make(map[int]bool, len(d.users)+len(alive))
	for uid := range d.users {
		next[uid] = true
	}
	changed := false
	for uid, ips := range alive {
		if uid <= 0 {
			return fmt.Errorf("invalid device user ID")
		}
		if confirmed {
			if len(ips) == 0 && next[uid] {
				delete(next, uid)
				changed = true
			}
		} else if !next[uid] {
			next[uid] = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	dir := filepath.Dir(d.path)
	f, err := os.CreateTemp(dir, ".device-recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), d.path); err != nil {
		return err
	}
	df, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return err
	}
	d.users = next
	return nil
}

func (s *Service) persistDeviceAttempt(alive map[int][]string) error {
	if s.deviceState == nil {
		return nil
	}
	return s.deviceState.update(alive, false)
}

func (s *Service) confirmDeviceAttempt(alive map[int][]string) {
	if s.deviceState != nil {
		if err := s.deviceState.update(alive, true); err != nil {
			nlog.Core().Warn("device receipt received but recovery cleanup failed", "error", err)
			return
		}
	}
	s.tracker.AcknowledgeAliveIPs(alive)
}
