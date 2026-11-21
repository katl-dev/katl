package agent

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	upgradeSnapshotDirectory = "/var/lib/etcd/katl-snapshots"
	// retainedUpgradeSnapshots keeps the rollback point of the latest upgrade
	// and one earlier one, bounding node-local use to about twice the database.
	retainedUpgradeSnapshots = 2
)

// pruneUpgradeSnapshots removes all but the newest retained pre-upgrade
// snapshots and returns the logical paths that remain, newest first. Only
// files named after Katl upgrade operations are considered, so snapshots an
// operator saved in the same directory are never removed. A snapshot that an
// active or recovery-required operation still references is always retained.
func (e *Executor) pruneUpgradeSnapshots() ([]string, error) {
	directory := rootedRuntimePath(e.Root, upgradeSnapshotDirectory)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type candidate struct {
		name     string
		modified time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, OperationKindKubeadmUpgrade+"-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate{name: name, modified: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].modified.Equal(candidates[j].modified) {
			return candidates[i].modified.After(candidates[j].modified)
		}
		return candidates[i].name > candidates[j].name
	})

	// Without a complete view of referencing operations nothing is safe to delete.
	protected, err := e.referencedUpgradeSnapshots()
	if err != nil {
		retained := make([]string, 0, len(candidates))
		for _, c := range candidates {
			retained = append(retained, path.Join(upgradeSnapshotDirectory, c.name))
		}
		return retained, fmt.Errorf("read operations referencing etcd snapshots: %w", err)
	}
	var retained []string
	var errs []error
	for i, c := range candidates {
		logical := path.Join(upgradeSnapshotDirectory, c.name)
		if i < retainedUpgradeSnapshots || protected[logical] {
			retained = append(retained, logical)
			continue
		}
		if err := os.Remove(filepath.Join(directory, c.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			retained = append(retained, logical)
			errs = append(errs, err)
		}
	}
	return retained, errors.Join(errs...)
}

func (e *Executor) referencedUpgradeSnapshots() (map[string]bool, error) {
	records, err := e.Store.List()
	if err != nil {
		return nil, err
	}
	referenced := map[string]bool{}
	for _, record := range records {
		if record.Terminal && !record.RecoveryRequired {
			continue
		}
		if update := record.KubernetesSysextUpdate; update != nil && update.SnapshotStorageLocation != "" {
			referenced[path.Clean(update.SnapshotStorageLocation)] = true
		}
	}
	return referenced, nil
}
