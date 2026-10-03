package gitstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

// fsmonitorInitHook answers git's fsmonitor query (protocol version 2) with a
// token and no changed paths. It is only used while marking a fresh index.
const fsmonitorInitHook = "#!/bin/sh\nprintf 'artifact-fs-init\\0'\n"

// markIndexFsmonitorValid marks every regular entry of a freshly built index
// as unchanged in git's fsmonitor extension.
//
// `read-tree` leaves entries with no stat data, so the first `git status` in a
// mount would lstat every file and then read every file to compare contents,
// which downloads every blob of a blobless clone. A fresh index matches the
// base snapshot exactly, and ArtifactFS's fsmonitor hook reports every path
// the overlay has changed since then. So git can trust the entries the hook
// does not report and skip both the lstat and the read.
//
// Call it only right after the index was built from the snapshot's commit and
// before anything else uses it. Gitlinks stay unmarked, as git requires.
func markIndexFsmonitorValid(ctx context.Context, gitDir string) error {
	scratch, err := os.MkdirTemp(filepath.Dir(gitDir), ".fsmonitor-init-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	hook := filepath.Join(scratch, "fsmonitor-init")
	if err := os.WriteFile(hook, []byte(fsmonitorInitHook), 0o755); err != nil {
		return err
	}
	staged, err := runGit(ctx, gitDir, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	var paths bytes.Buffer
	for record := range strings.SplitSeq(staged, "\x00") {
		meta, path, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[0] == "160000" || fields[2] != "0" {
			continue
		}
		paths.WriteString(path)
		paths.WriteByte(0)
	}
	if paths.Len() == 0 {
		return nil
	}
	// The hook runs with the work tree as its cwd. The scratch folder stands in
	// for it; update-index never looks at work tree files with these options.
	// Nothing here may download a blob, so lazy fetching is off.
	env := []string{"GIT_WORK_TREE=" + scratch, "GIT_NO_LAZY_FETCH=1"}
	fsmonitor := "core.fsmonitor=" + shellScriptQuote(hook)
	if err := runGitWithInput(ctx, gitDir, env, &paths, "-c", fsmonitor, "update-index", "--fsmonitor-valid", "-z", "--stdin"); err != nil {
		return err
	}
	// Marking entries drops the cache tree that read-tree wrote; write-tree
	// rebuilds it from the index alone and keeps the fsmonitor extension.
	// --missing-ok stops it from checking (and lazily fetching) every blob.
	return runGitWithInput(ctx, gitDir, env, nil, "-c", fsmonitor, "write-tree", "--missing-ok")
}

// runGitWithInput runs a local git command that needs no credentials, feeding
// it stdin and discarding stdout.
func runGitWithInput(ctx context.Context, gitDir string, extraEnv []string, stdin *bytes.Buffer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	configureCancelableCommand(cmd)
	cmd.Env = append(append(os.Environ(), "GIT_DIR="+gitDir), extraEnv...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	errBuf := &boundedGitError{limit: maxGitErrorBytes}
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		msg := auth.RedactString(strings.TrimSpace(errBuf.String()))
		if msg == "" {
			msg = auth.RedactString(err.Error())
		}
		return errors.New(msg)
	}
	return nil
}

func (s *Store) markFreshIndex(ctx context.Context, gitDir string, repoName string) {
	if err := markIndexFsmonitorValid(ctx, gitDir); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("marking the fresh index fsmonitor-valid failed; the first git status will read every file", "repo", repoName, "error", err)
	}
}
