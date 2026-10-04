package gitstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// FetchBlobs brings every missing blob of a blobless clone in one fetch (one
// new pack), and asks the remote for nothing it already has.
func TestFetchBlobsFetchesMissingBlobsInOneRequest(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	run(t, "git", "init", "--bare", "-b", "main", bare)
	run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "true")
	run(t, "git", "init", "-b", "main", work)
	for i := range 30 {
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("f%02d.txt", i)), []byte(fmt.Sprintf("file %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, "git", "-C", work, "add", ".")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "files")
	run(t, "git", "-C", work, "push", "-q", bare, "main")

	clone := filepath.Join(tmp, "clone.git")
	run(t, "git", "clone", "-q", "--bare", "--filter=blob:none", "file://"+bare, clone)
	var oids []string
	for _, line := range strings.Split(strings.TrimSpace(runOutput(t, "git", "--git-dir", clone, "ls-tree", "HEAD")), "\n") {
		oids = append(oids, strings.Fields(line)[2])
	}
	if missing := countMissing(t, clone, oids); missing != 30 {
		t.Fatalf("missing before = %d, want 30", missing)
	}
	packsBefore := countPacks(t, clone)

	cfg := model.RepoConfig{ID: "r", GitDir: clone}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := New(nil)
	defer store.Close()
	asked, err := store.FetchBlobs(ctx, cfg, oids[:20])
	if err != nil || asked != 20 {
		t.Fatalf("FetchBlobs = %d, %v; want 20, nil", asked, err)
	}
	if missing := countMissing(t, clone, oids); missing != 10 {
		t.Fatalf("missing after = %d, want 10", missing)
	}
	if packs := countPacks(t, clone); packs != packsBefore+1 {
		t.Fatalf("packs = %d, want %d (one fetch)", packs, packsBefore+1)
	}
	// The 20 already here are not asked for again.
	asked, err = store.FetchBlobs(ctx, cfg, oids)
	if err != nil || asked != 10 {
		t.Fatalf("second FetchBlobs = %d, %v; want 10, nil", asked, err)
	}
	if missing := countMissing(t, clone, oids); missing != 0 {
		t.Fatalf("missing at the end = %d, want 0", missing)
	}
}

func countMissing(t *testing.T, gitDir string, oids []string) int {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", gitDir, "cat-file", "--batch-check")
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(out), " missing")
}

func countPacks(t *testing.T, gitDir string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}
