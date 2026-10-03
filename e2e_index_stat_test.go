//go:build linux

package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestE2EGitFirstStatusDownloadsNothing checks that a fresh mount answers git
// status, git diff and stat for every file without downloading file contents,
// and that real edits still show up.
func TestE2EGitFirstStatusDownloadsNothing(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)
	remoteURL := createLocalTestRepo(t)
	bare := strings.TrimPrefix(remoteURL, "file://")
	// Serve blob sizes the way Pando's server does.
	run(t, "", "git", "-C", bare, "config", "transfer.advertiseObjectInfo", "true")
	repo := newMountedE2ERepoFrom(t, remoteURL)

	hydrated := func() int64 {
		t.Helper()
		st, err := repo.svc.Status(context.Background(), repoName)
		if err != nil {
			t.Fatal(err)
		}
		return st.HydratedBlobCount
	}

	assertGitStatus(t, repo.mountPath, map[string]string{})
	if n := hydrated(); n != 0 {
		t.Fatalf("first git status downloaded %d blobs, want 0", n)
	}
	gitCmdQuiet(t, repo.mountPath, "diff", "--quiet")
	if n := hydrated(); n != 0 {
		t.Fatalf("git diff downloaded %d blobs, want 0", n)
	}

	// Every file reports its real size before anything reads it.
	want := map[string]int64{}
	for line := range strings.SplitSeq(strings.TrimSpace(run(t, "", "git", "-C", bare, "ls-tree", "-r", "-l", "main")), "\n") {
		meta, path, _ := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		want[path] = size
	}
	seen := 0
	err := filepath.WalkDir(repo.mountPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repo.mountPath, path)
		if rel == ".git" {
			return nil
		}
		size, tracked := want[rel]
		if !tracked {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Size() != size {
			t.Errorf("%s size = %d, want %d", rel, info.Size(), size)
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Fatalf("stat saw %d tracked files, want %d", seen, len(want))
	}
	if n := hydrated(); n != 0 {
		t.Fatalf("stat of every file downloaded %d blobs, want 0", n)
	}

	// Real edits still show up.
	readmePath := filepath.Join(repo.mountPath, "README.md")
	f, err := os.OpenFile(readmePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("appended in the mount\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.mountPath, "notes.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo.mountPath, "packages", "miniflare", "package.json")); err != nil {
		t.Fatal(err)
	}
	assertGitStatus(t, repo.mountPath, map[string]string{
		"README.md":                       " M",
		"notes.txt":                       "??",
		"packages/miniflare/package.json": " D",
	})
	diff := gitCmd(t, repo.mountPath, "diff", "README.md")
	if !strings.Contains(diff, "+appended in the mount") || !strings.Contains(diff, " # Test Repo") {
		t.Fatalf("git diff README.md =\n%s", diff)
	}
	if n := hydrated(); n > 2 {
		t.Fatalf("an edit and a delete downloaded %d blobs, want at most 2", n)
	}
}
