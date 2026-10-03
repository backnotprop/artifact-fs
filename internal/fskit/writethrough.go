package fskit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// MaxWriteThroughFile is the largest file ServerChanges replays; a bigger
// one is left to the kernel's attribute timeout and the next open.
const MaxWriteThroughFile = 64 << 20

// ServerChanges turns the difference between two snapshot generations into
// write-through changes. Paths with an overlay entry are local edits and are
// skipped; the overlay reconcile after the generation switch decides them.
// Deletes come first, deepest path first, then writes in path order.
func ServerChanges(ctx context.Context, diff []snapshot.GenerationChange, ov model.OverlayStore, h model.Hydrator, repo model.RepoConfig) ([]Change, error) {
	var deletes, writes []Change
	for _, d := range diff {
		if _, local, err := ov.Lookup(ctx, d.Path); err != nil {
			return nil, err
		} else if local {
			continue
		}
		typeChanged := d.Old != nil && d.New != nil && d.Old.Type != d.New.Type
		if d.Old != nil && (d.New == nil || typeChanged) {
			deletes = append(deletes, Change{Path: d.Path, Delete: true})
		}
		if d.New == nil {
			continue
		}
		switch d.New.Type {
		case "file":
			if d.New.SizeState == "known" && d.New.SizeBytes > MaxWriteThroughFile {
				continue
			}
			data, err := readBlob(ctx, h, repo, *d.New)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.Path, err)
			}
			if data == nil {
				continue
			}
			writes = append(writes, Change{Path: d.Path, Data: data, Mode: os.FileMode(d.New.Mode & 0o777)})
		case "symlink":
			target, err := h.ReadBlob(ctx, repo, *d.New, maxSymlinkTargetSz)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.Path, err)
			}
			writes = append(writes, Change{Path: d.Path, Symlink: string(target)})
		}
	}
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].Path > deletes[j].Path })
	return append(deletes, writes...), nil
}

// readBlob reads a whole base file, or returns nil when it is over
// MaxWriteThroughFile.
func readBlob(ctx context.Context, h model.Hydrator, repo model.RepoConfig, n model.BaseNode) ([]byte, error) {
	f, size, err := h.OpenHydrated(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if size > MaxWriteThroughFile {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxWriteThroughFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxWriteThroughFile {
		return nil, nil
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

// WriteThroughPrefix starts the name of every temp file the daemon writes
// through a mount.
const WriteThroughPrefix = ".pando-wt-"

// Change is one server-side change to apply through a mount.
type Change struct {
	// Path is relative to the repo root, with forward slashes.
	Path string
	// Delete removes the file or symlink (or an empty directory).
	Delete bool
	// Data is the new file content.
	Data []byte
	// Mode holds permission bits for a file; 0 means 0o644.
	Mode os.FileMode
	// Symlink, when set, makes the change a symlink with this target.
	Symlink string
}

// WriteThrough applies server-side changes through the mount, so the kernel
// sees them happen and drops what it cached. On macOS 26 an FSKit volume has
// no other way to tell the kernel that a file changed underneath it.
//
// Each file is written to a temp file named WriteThroughPrefix+random in the
// same directory and renamed over the target; a delete is an unlink. On a
// read-only mount the server accepts exactly these paths while the change is
// in flight. WriteThrough holds no server lock while it runs, because every
// syscall comes back to this server as a request.
func (s *Server) WriteThrough(ctx context.Context, changes []Change) error {
	if s.cfg.MountPoint == "" {
		return errors.New("fskit: write-through needs the mount point")
	}
	return writeThrough(ctx, s.cfg.MountPoint, changes, s.allow)
}

// WriteThroughMount applies changes through any mounted repo at mountPoint
// (the FUSE front end too), with no read-only exception.
func WriteThroughMount(ctx context.Context, mountPoint string, changes []Change) error {
	return writeThrough(ctx, mountPoint, changes, nil)
}

func writeThrough(ctx context.Context, mountPoint string, changes []Change, allow *allowList) error {
	for _, c := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := applyChange(mountPoint, c, allow); err != nil {
			return fmt.Errorf("write-through %s: %w", c.Path, err)
		}
	}
	return nil
}

func applyChange(mountPoint string, c Change, allow *allowList) error {
	rel, err := cleanRelative(c.Path)
	if err != nil {
		return err
	}
	target := filepath.Join(mountPoint, filepath.FromSlash(rel))
	paths := append(ancestors(rel), rel)
	var tmpRel string
	if !c.Delete {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		tmpRel = path.Join(path.Dir(rel), WriteThroughPrefix+hex.EncodeToString(b[:]))
		paths = append(paths, tmpRel)
	}
	if allow != nil {
		allow.add(paths)
		defer allow.remove(paths)
	}
	if c.Delete {
		err := os.Remove(target)
		switch {
		case err == nil, errors.Is(err, os.ErrNotExist):
			return nil
		case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
			// A directory that still holds local files stays.
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(mountPoint, filepath.FromSlash(tmpRel))
	if c.Symlink != "" {
		if err := os.Symlink(c.Symlink, tmp); err != nil {
			return err
		}
	} else {
		mode := c.Mode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		if _, err := f.Write(c.Data); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return err
		}
		// The umask may have narrowed the mode at create.
		if err := os.Chmod(tmp, mode); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// cleanRelative normalizes a repo-relative path and refuses one that leaves
// the repo or names the root.
func cleanRelative(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q is absolute", p)
	}
	rel := path.Clean(filepath.ToSlash(p))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path %q is outside the repo", p)
	}
	if model.CleanPath(rel) != rel {
		return "", fmt.Errorf("path %q is not clean", p)
	}
	return rel, nil
}

// ancestors returns the parent directories of rel, nearest the root first.
func ancestors(rel string) []string {
	var out []string
	dir := path.Dir(rel)
	for dir != "." && dir != "/" {
		out = append([]string{dir}, out...)
		dir = path.Dir(dir)
	}
	return out
}
