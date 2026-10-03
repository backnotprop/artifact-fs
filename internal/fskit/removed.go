package fskit

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// removedItem keeps a removed or replaced item usable until FSKit reclaims
// it, so a process that still has the file open keeps reading and writing.
//
// An overlay file is pinned by an open descriptor on its backing file, which
// the overlay unlinks. A base file is kept as its blob (the blob cache is
// content-addressed, so it stays valid) and copied into a private, already
// unlinked scratch file only if someone writes to it.
type removedItem struct {
	mu        sync.Mutex
	attr      Attr
	sizeKnown bool
	file      *os.File
	base      *model.BaseNode
	target    string
}

// blobEnv is what a removed item needs to reach base blobs.
type blobEnv struct {
	hydrator   model.Hydrator
	repo       model.RepoConfig
	scratchDir string
}

func (r *removedItem) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	f := r.file
	r.file = nil
	r.base = nil
	r.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

func (r *removedItem) getattr(ctx context.Context, env blobEnv) (Attr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.attr
	switch {
	case r.file != nil:
		st, err := r.file.Stat()
		if err != nil {
			return Attr{}, err
		}
		a.Size = uint64(st.Size())
	case r.base != nil && !r.sizeKnown:
		if r.base.Type == "symlink" {
			data, err := env.hydrator.ReadBlob(ctx, env.repo, *r.base, maxSymlinkTargetSz)
			if err != nil {
				return Attr{}, err
			}
			r.attr.Size = uint64(len(data))
		} else {
			_, size, err := env.hydrator.EnsureHydrated(ctx, env.repo, *r.base)
			if err != nil {
				return Attr{}, err
			}
			r.attr.Size = uint64(size)
		}
		r.sizeKnown = true
		a.Size = r.attr.Size
	}
	return a, nil
}

func (r *removedItem) read(ctx context.Context, env blobEnv, off int64, size int) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.attr.Kind {
	case kindDir:
		return nil, wireErr(errEISDIR)
	case kindSymlink:
		return nil, wireErr(errEINVAL)
	}
	if r.file != nil {
		return readAt(r.file, off, size)
	}
	if r.base != nil {
		f, _, err := env.hydrator.OpenHydrated(ctx, env.repo, *r.base)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readAt(f, off, size)
	}
	return nil, nil
}

// materialize gives the item a private writable file, copying the base blob
// if there is one. Caller holds r.mu.
func (r *removedItem) materialize(ctx context.Context, env blobEnv) error {
	if r.file != nil {
		return nil
	}
	dir := env.scratchDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "removed-*")
	if err != nil {
		return err
	}
	// Unlink at once: the data lives exactly as long as the descriptor.
	_ = os.Remove(f.Name())
	if r.base != nil {
		src, _, err := env.hydrator.OpenHydrated(ctx, env.repo, *r.base)
		if err != nil {
			f.Close()
			return err
		}
		_, err = io.Copy(f, src)
		src.Close()
		if err != nil {
			f.Close()
			return err
		}
	}
	r.file = f
	r.base = nil
	r.sizeKnown = true
	return nil
}

func (r *removedItem) write(ctx context.Context, env blobEnv, off int64, data []byte) (int, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attr.Kind != kindFile {
		return 0, 0, wireErr(errEISDIR)
	}
	if err := r.materialize(ctx, env); err != nil {
		return 0, 0, err
	}
	n, err := r.file.WriteAt(data, off)
	if err != nil {
		return n, 0, err
	}
	now := time.Now().UnixNano()
	r.attr.MtimeNs, r.attr.CtimeNs = now, now
	st, err := r.file.Stat()
	if err != nil {
		return n, 0, err
	}
	return n, uint64(st.Size()), nil
}

func (r *removedItem) truncate(ctx context.Context, env blobEnv, size int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.attr.Kind {
	case kindDir:
		return wireErr(errEISDIR)
	case kindSymlink:
		return wireErr(errEINVAL)
	}
	if err := r.materialize(ctx, env); err != nil {
		return err
	}
	if err := r.file.Truncate(size); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	r.attr.MtimeNs, r.attr.CtimeNs = now, now
	return nil
}

func (r *removedItem) setMode(mode uint32) {
	r.mu.Lock()
	r.attr.Mode = mode & 0o7777
	r.attr.CtimeNs = time.Now().UnixNano()
	r.mu.Unlock()
}

func (r *removedItem) setMtime(ns int64) {
	r.mu.Lock()
	r.attr.MtimeNs = ns
	r.attr.CtimeNs = time.Now().UnixNano()
	r.mu.Unlock()
}

func (r *removedItem) readlink(ctx context.Context, env blobEnv) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attr.Kind != kindSymlink {
		return "", wireErr(errEINVAL)
	}
	if r.base != nil {
		data, err := env.hydrator.ReadBlob(ctx, env.repo, *r.base, maxSymlinkTargetSz)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return r.target, nil
}

// readAt reads up to size bytes at off; a short result means end of file.
func readAt(f *os.File, off int64, size int) ([]byte, error) {
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

func scratchDirFor(repo model.RepoConfig) string {
	if repo.OverlayDir == "" {
		return ""
	}
	return filepath.Join(repo.OverlayDir, "fskit-removed")
}
