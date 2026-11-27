// Package linkrecovery owns the response to a required management link that
// systemd-networkd reports without carrier while sysfs reports carrier and
// operstate up, which leaves the link without a DHCP lease (#529, #811).
// Its per-boot record is the only account of the response; boot health reads
// it to explain a link's state.
package linkrecovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Dir holds one record per reconfigured link. It lives under /run, so each
// boot starts without records and a restarted owner keeps the boot's records.
const Dir = "/run/katl/link-recovery"

type Outcome string

const (
	// Attempted means the attempt was claimed but its result was not recorded,
	// as when the owner stopped while networkctl ran.
	Attempted    Outcome = "attempted"
	Reconfigured Outcome = "reconfigured"
	Failed       Outcome = "failed"
)

// Record is one link's reconfiguration in this boot.
type Record struct {
	Link    string    `json:"link"`
	At      time.Time `json:"at"`
	Outcome Outcome   `json:"outcome"`
	Error   string    `json:"error,omitempty"`
}

const (
	symptom        = "systemd-networkd reported no carrier while sysfs reported carrier up"
	presentSymptom = "systemd-networkd reports no carrier while sysfs reports carrier up"
)

// nextStep follows an attempt whose success is not known. It matches the
// next steps of other boot-health diagnostics.
const nextStep = "Inspect 'journalctl -b -u katl-link-recovery' on the node. If the link stays offline, reboot from the node's console with 'systemctl reboot'; 'katlctl node reboot <node>' works only while another management link reaches the node"

// String describes the record for an operator as a sentence, naming the link
// once and ending with what to do next.
func (r Record) String() string {
	switch r.Outcome {
	case Reconfigured:
		return fmt.Sprintf("Required link %s was reconfigured once in this boot after %s. No action is needed while the node is healthy", r.Link, symptom)
	case Failed:
		return fmt.Sprintf("Required link %s could not be reconfigured after %s: %s. %s", r.Link, symptom, r.Error, nextStep)
	default:
		return fmt.Sprintf("Required link %s reconfiguration was attempted once in this boot after %s; its result was not recorded. %s", r.Link, symptom, nextStep)
	}
}

// Summary describes every record, or returns "" when there are none.
func Summary(records []Record) string {
	descriptions := make([]string, 0, len(records))
	for _, record := range records {
		descriptions = append(descriptions, record.String())
	}
	return strings.Join(descriptions, ". ")
}

// Read returns the records in dir sorted by link. A missing directory means
// no link was reconfigured in this boot.
func Read(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read link recovery records: %w", err)
	}
	var records []Record
	for _, entry := range entries {
		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read link recovery record: %w", err)
		}
		record := Record{Link: entry.Name(), Outcome: Attempted}
		// A claim interrupted before its first write leaves an empty record.
		if len(data) > 0 {
			if err := json.Unmarshal(data, &record); err != nil {
				return nil, fmt.Errorf("decode link recovery record %s: %w", entry.Name(), err)
			}
		}
		record.Link = entry.Name()
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b Record) int { return strings.Compare(a.Link, b.Link) })
	return records, nil
}

func recorded(dir, link string) bool {
	_, err := os.Lstat(filepath.Join(dir, link))
	return err == nil
}

// claim creates the link's record for this boot. Exclusive creation makes the
// claim, so it reports false when an attempt was already made.
func claim(dir string, record Record) (bool, error) {
	if !filepath.IsLocal(record.Link) || filepath.Base(record.Link) != record.Link || strings.HasPrefix(record.Link, ".") {
		return false, fmt.Errorf("unsafe link name %q", record.Link)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("record attempt: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, record.Link), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record attempt: %w", err)
	}
	data, err := json.Marshal(record)
	if err == nil {
		_, err = file.Write(data)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// Without a record there is no attempt, so give up the claim.
		_ = os.Remove(filepath.Join(dir, record.Link))
		return false, fmt.Errorf("record attempt: %w", err)
	}
	return true, nil
}

// update replaces a claimed record. The rename keeps the claim in place, so a
// concurrent claim still sees the attempt.
func update(dir string, record Record) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+record.Link+".")
	if err != nil {
		return fmt.Errorf("record outcome: %w", err)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("record outcome: %w", err)
	}
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return fmt.Errorf("record outcome: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("record outcome: %w", err)
	}
	if err := os.Rename(temp.Name(), filepath.Join(dir, record.Link)); err != nil {
		return fmt.Errorf("record outcome: %w", err)
	}
	return nil
}
