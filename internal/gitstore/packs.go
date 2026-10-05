package gitstore

import (
	"context"
	"path/filepath"
	"time"
)

// Every fetch into a blobless clone adds a pack. Git's own answer, `git
// maintenance run --auto` after each fetch, repacks while other fetches run,
// and a fetch that starts during the swap dies ("in the commit graph file but
// not in the object database"), failing the read that asked for the blob:
// 20 of 480 lazy fetches, 8 at a time, measured 2026-10-04. So the daemon's
// clones have automatic maintenance off, and the store merges the packs
// itself while none of its fetches run.

// maxPacks is git's gc.autoPackLimit.
const maxPacks = 50

var maintenanceSettings = [][2]string{
	{"maintenance.auto", "false"},
	{"gc.auto", "0"},
}

func configureMaintenance(ctx context.Context, gitDir string) error {
	for _, setting := range maintenanceSettings {
		if _, err := runGit(ctx, gitDir, "config", "--local", setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

// duringFetch runs fn, which may fetch, while the packs are not being merged.
// fn must not call duringFetch again: a waiting merge would block it.
func (p *batchPool) duringFetch(fn func()) {
	p.packs.RLock()
	defer p.packs.RUnlock()
	fn()
}

// mergePacks merges the clone's packs into one once there are more than
// maxPacks. It waits for running fetches and holds new ones back meanwhile;
// a merge of a 50,000-object clone takes under a second.
func (s *Store) mergePacks(gitDir string, p *batchPool) {
	packs, _ := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "*.pack"))
	if len(packs) <= maxPacks || !p.merging.CompareAndSwap(false, true) {
		return
	}
	defer p.merging.Store(false)
	p.packs.Lock()
	defer p.packs.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := runGit(ctx, gitDir, "repack", "-a", "-d", "-q"); err != nil {
		s.logger.Warn("merging packs failed", "git_dir", gitDir, "packs", len(packs), "error", err)
	}
}
