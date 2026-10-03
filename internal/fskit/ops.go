package fskit

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// Synthesized files at the mount root (contract section 5).
const (
	gitfileName     = ".git"
	neverIndexName  = ".metadata_never_index"
	sizeFillWorkers = 8
)

func isSynthesized(path string) bool {
	return path == gitfileName || path == neverIndexName
}

func kindOf(typ string) uint8 {
	switch typ {
	case "dir":
		return kindDir
	case "symlink":
		return kindSymlink
	default:
		return kindFile
	}
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return wireErr(errEINVAL)
	}
	if len(name) > maxNameBytes {
		return wireErr(errENAMETOOLONG)
	}
	return nil
}

func childPathOf(parent, name string) string {
	return model.CleanPath(filepath.Join(parent, name))
}

func (s *Server) node(ino uint64) (inodeView, error) {
	v, ok := s.inodes.get(ino)
	if !ok {
		return inodeView{}, wireErr(errESTALE)
	}
	return v, nil
}

// parentDir resolves a parent number to its path and checks that it is a
// live directory. Caller holds s.ns.
func (s *Server) parentDir(ino uint64, mustExist bool) (string, error) {
	v, err := s.node(ino)
	if err != nil {
		return "", err
	}
	if v.removed != nil {
		return "", wireErr(errENOENT)
	}
	if v.kind != kindDir {
		return "", wireErr(errENOTDIR)
	}
	if mustExist && v.path != "." {
		n, err := s.res.ResolvePath(v.path)
		if err != nil {
			return "", err
		}
		if n.Type() != "dir" {
			return "", wireErr(errENOTDIR)
		}
	}
	return v.path, nil
}

// --- attributes ---

func (s *Server) rootAttr() Attr {
	mt := s.res.CommitTime()
	ns := s.started
	if mt != 0 {
		ns = mt * int64(time.Second)
	}
	return Attr{Ino: inoRoot, Kind: kindDir, Mode: 0o755, Size: 4096, MtimeNs: ns, CtimeNs: ns}
}

func (s *Server) synthAttr(ino uint64, path string) Attr {
	size := uint64(0)
	if path == gitfileName {
		size = uint64(len(s.gitfile))
	}
	return Attr{Ino: ino, Kind: kindFile, Mode: 0o444, Size: size, MtimeNs: s.started, CtimeNs: s.started}
}

func baseTimeNs(gen, commitTime int64) int64 {
	ct := commitTime
	if ct == 0 {
		ct = gen // the FUSE front end's fallback when the commit time is unknown
	}
	return ct * int64(time.Second)
}

func wireMode(mode uint32, kind uint8) uint32 {
	m := mode & 0o7777
	if kind == kindSymlink && m == 0 {
		m = 0o755
	}
	return m
}

// attrOf builds the attribute record for a resolved node. When a base
// file's or symlink's size is not known yet it also returns the node, and
// the caller fills the size outside the namespace lock (fillSize).
func attrOf(ino uint64, n fusefs.ResolvedNode, gen, commitTime int64) (Attr, *model.BaseNode) {
	if n.FromOverlay {
		kind := kindOf(n.Overlay.NodeType())
		a := Attr{Ino: ino, Kind: kind, Mode: wireMode(n.Overlay.Mode, kind), Size: uint64(max(n.Overlay.SizeBytes, 0)), MtimeNs: n.Overlay.MtimeUnixNs, CtimeNs: n.Overlay.CtimeUnixNs}
		if kind == kindDir && a.Size == 0 {
			a.Size = 4096
		}
		return a, nil
	}
	return baseAttr(ino, n.Base, gen, commitTime)
}

func baseAttr(ino uint64, b model.BaseNode, gen, commitTime int64) (Attr, *model.BaseNode) {
	kind := kindOf(b.Type)
	t := baseTimeNs(gen, commitTime)
	a := Attr{Ino: ino, Kind: kind, Mode: wireMode(fusefs.NormalizeMode(b.Mode, b.Type), kind), Size: uint64(max(b.SizeBytes, 0)), MtimeNs: t, CtimeNs: t}
	if kind == kindDir && a.Size == 0 {
		a.Size = 4096
	}
	if kind != kindDir && b.SizeState != "known" && b.ObjectOID != "" {
		node := b
		return a, &node
	}
	return a, nil
}

// fillSize learns an unknown base size by downloading the blob, as the FUSE
// front end does: a wrong size would be cached by the kernel and clip reads.
func (s *Server) fillSize(ctx context.Context, a *Attr, b *model.BaseNode) error {
	if b == nil {
		return nil
	}
	if b.Type == "symlink" {
		data, err := s.env.hydrator.ReadBlob(ctx, s.env.repo, *b, maxSymlinkTargetSz)
		if err != nil {
			return err
		}
		a.Size = uint64(len(data))
		return nil
	}
	_, size, err := s.env.hydrator.EnsureHydrated(ctx, s.env.repo, *b)
	if err != nil {
		return err
	}
	a.Size = uint64(size)
	return nil
}

// --- operations ---

func (s *Server) opHello(req *request) *reply {
	if req.Version != ProtocolVersion {
		s.log.Warn("fskit: wire version mismatch; the extension will refuse the mount",
			"extension_version", req.Version, "extension_build", req.Build,
			"daemon_version", ProtocolVersion, "daemon_build", s.cfg.Build)
	}
	return &reply{Version: ProtocolVersion, Build: s.cfg.Build, MaxRead: MaxRead, MaxWrite: MaxWrite}
}

func (s *Server) opLookup(ctx context.Context, req *request) (*reply, error) {
	if err := validateName(req.Name); err != nil {
		return nil, err
	}
	s.ns.RLock()
	parent, err := s.parentDir(req.Parent, false)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	child := childPathOf(parent, req.Name)
	if parent == "." && isSynthesized(child) {
		ino := s.inodes.assign(child, kindFile, true)
		s.ns.RUnlock()
		a := s.synthAttr(ino, child)
		return &reply{Attr: &a}, nil
	}
	n, gen, ct, err := s.res.ResolvePathState(child)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	ino := s.inodes.assign(child, kindOf(n.Type()), true)
	a, need := attrOf(ino, n, gen, ct)
	s.ns.RUnlock()
	if err := s.fillSize(ctx, &a, need); err != nil {
		return nil, err
	}
	return &reply{Attr: &a}, nil
}

func (s *Server) opGetattr(ctx context.Context, req *request) (*reply, error) {
	a, err := s.getattr(ctx, req.Ino)
	if err != nil {
		return nil, err
	}
	return &reply{Attr: &a}, nil
}

func (s *Server) getattr(ctx context.Context, ino uint64) (Attr, error) {
	if ino == inoRoot {
		return s.rootAttr(), nil
	}
	s.ns.RLock()
	v, err := s.node(ino)
	if err != nil {
		s.ns.RUnlock()
		return Attr{}, err
	}
	if v.removed != nil {
		s.ns.RUnlock()
		return v.removed.getattr(ctx, s.env)
	}
	if isSynthesized(v.path) {
		s.ns.RUnlock()
		return s.synthAttr(ino, v.path), nil
	}
	n, gen, ct, err := s.res.ResolvePathState(v.path)
	if err != nil {
		s.ns.RUnlock()
		return Attr{}, err
	}
	a, need := attrOf(ino, n, gen, ct)
	s.ns.RUnlock()
	if err := s.fillSize(ctx, &a, need); err != nil {
		return Attr{}, err
	}
	return a, nil
}

func (s *Server) opRead(ctx context.Context, req *request) ([]byte, error) {
	if req.Size == nil || *req.Size > MaxRead || req.Offset < 0 {
		return nil, wireErr(errEINVAL)
	}
	size := int(*req.Size)
	s.ns.RLock()
	v, err := s.node(req.Ino)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	if v.removed != nil {
		s.ns.RUnlock()
		return v.removed.read(ctx, s.env, req.Offset, size)
	}
	if v.ino == inoRoot {
		s.ns.RUnlock()
		return nil, wireErr(errEISDIR)
	}
	if isSynthesized(v.path) {
		s.ns.RUnlock()
		content := []byte(nil)
		if v.path == gitfileName {
			content = s.gitfile
		}
		if req.Offset >= int64(len(content)) {
			return nil, nil
		}
		end := min(int(req.Offset)+size, len(content))
		return content[req.Offset:end], nil
	}
	n, err := s.res.ResolvePath(v.path)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	switch n.Type() {
	case "dir":
		s.ns.RUnlock()
		return nil, wireErr(errEISDIR)
	case "symlink":
		s.ns.RUnlock()
		return nil, wireErr(errEINVAL)
	}
	if n.FromOverlay {
		f, err := os.Open(n.Overlay.BackingPath)
		s.ns.RUnlock()
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readAt(f, req.Offset, size)
	}
	base := n.Base
	s.ns.RUnlock()
	// The blob cache is content-addressed, so the download runs without the
	// namespace lock and a slow fetch stalls no other operation.
	f, _, err := s.env.hydrator.OpenHydrated(ctx, s.env.repo, base)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAt(f, req.Offset, size)
}

// prefetchBase downloads the blob behind a base file before an operation
// that copies it into the overlay, so the copy under the lock is local.
func (s *Server) prefetchBase(ctx context.Context, ino uint64) error {
	s.ns.RLock()
	v, err := s.node(ino)
	if err != nil || v.removed != nil || isSynthesized(v.path) || v.ino == inoRoot {
		s.ns.RUnlock()
		return nil
	}
	n, err := s.res.ResolvePath(v.path)
	s.ns.RUnlock()
	if err != nil || n.FromOverlay || n.Base.Type != "file" || n.Base.ObjectOID == "" {
		return nil
	}
	_, _, err = s.env.hydrator.EnsureHydrated(ctx, s.env.repo, n.Base)
	return err
}

func (s *Server) opWrite(ctx context.Context, req *request, data []byte) (*reply, error) {
	if len(data) > MaxWrite || req.Offset < 0 {
		return nil, wireErr(errEINVAL)
	}
	if err := s.prefetchBase(ctx, req.Ino); err != nil {
		return nil, err
	}
	s.ns.RLock()
	v, err := s.node(req.Ino)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	if v.removed != nil {
		s.ns.RUnlock()
		n, size, err := v.removed.write(ctx, s.env, req.Offset, data)
		if err != nil {
			return nil, err
		}
		written := uint32(n)
		return &reply{Written: &written, Size: &size}, nil
	}
	defer s.ns.RUnlock()
	if v.ino == inoRoot {
		return nil, wireErr(errEISDIR)
	}
	if isSynthesized(v.path) {
		return nil, wireErr(errEPERM)
	}
	if err := s.checkWritable(v.path); err != nil {
		return nil, err
	}
	n, err := s.res.ResolvePath(v.path)
	if err != nil {
		return nil, err
	}
	switch n.Type() {
	case "dir":
		return nil, wireErr(errEISDIR)
	case "symlink":
		return nil, wireErr(errEINVAL)
	}
	written, err := s.eng.Write(ctx, v.path, req.Offset, data)
	if err != nil {
		return nil, err
	}
	if written != len(data) {
		return nil, wireErr(errEIO)
	}
	var size uint64
	if e, ok, err := s.eng.Overlay.Lookup(ctx, v.path); err == nil && ok {
		size = uint64(max(e.SizeBytes, 0))
	}
	w := uint32(written)
	return &reply{Written: &w, Size: &size}, nil
}

func (s *Server) opReadlink(ctx context.Context, req *request) (*reply, error) {
	s.ns.RLock()
	v, err := s.node(req.Ino)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	if v.removed != nil {
		s.ns.RUnlock()
		t, err := v.removed.readlink(ctx, s.env)
		if err != nil {
			return nil, err
		}
		return &reply{Target: &t}, nil
	}
	if v.ino == inoRoot || isSynthesized(v.path) {
		s.ns.RUnlock()
		return nil, wireErr(errEINVAL)
	}
	n, err := s.res.ResolvePath(v.path)
	s.ns.RUnlock()
	if err != nil {
		return nil, err
	}
	if n.Type() != "symlink" {
		return nil, wireErr(errEINVAL)
	}
	if n.FromOverlay {
		t := n.Overlay.TargetPath
		return &reply{Target: &t}, nil
	}
	data, err := s.env.hydrator.ReadBlob(ctx, s.env.repo, n.Base, maxSymlinkTargetSz)
	if err != nil {
		return nil, err
	}
	t := string(data)
	return &reply{Target: &t}, nil
}

// opCreate serves create, mkdir and symlink. The overlay checks the name and
// creates the item under one lock, so of two racing creates exactly one wins
// and the other gets EEXIST.
func (s *Server) opCreate(ctx context.Context, req *request, kind uint8) (*reply, error) {
	if err := validateName(req.Name); err != nil {
		return nil, err
	}
	if kind == kindSymlink {
		if req.Target == "" || strings.ContainsRune(req.Target, 0) {
			return nil, wireErr(errEINVAL)
		}
		if len(req.Target) > maxSymlinkTargetSz {
			return nil, wireErr(errENAMETOOLONG)
		}
	}
	s.ns.RLock()
	defer s.ns.RUnlock()
	parent, err := s.parentDir(req.Parent, true)
	if err != nil {
		return nil, err
	}
	child := childPathOf(parent, req.Name)
	if parent == "." && isSynthesized(child) {
		return nil, wireErr(errEEXIST)
	}
	if err := s.checkWritable(child); err != nil {
		return nil, err
	}
	var e model.OverlayEntry
	switch kind {
	case kindFile:
		mode := uint32(0o644)
		if req.Mode != nil {
			mode = *req.Mode & 0o7777
		}
		e, err = s.eng.CreateExclusive(ctx, child, mode)
	case kindDir:
		mode := uint32(0o755)
		if req.Mode != nil {
			mode = *req.Mode & 0o7777
		}
		e, err = s.eng.MkdirExclusive(ctx, child, mode)
	default:
		e, err = s.eng.SymlinkExclusive(ctx, child, req.Target)
	}
	if err != nil {
		return nil, err
	}
	ino := s.inodes.assign(child, kind, true)
	a, _ := attrOf(ino, fusefs.ResolvedNode{FromOverlay: true, Overlay: e}, 0, 0)
	return &reply{Attr: &a}, nil
}

// pin captures a removed or replaced item's data so it outlives its name.
// Caller holds s.ns for writing.
func (s *Server) pin(ino uint64, n fusefs.ResolvedNode) (*removedItem, error) {
	gen, ct := s.res.Generation(), s.res.CommitTime()
	a, need := attrOf(ino, n, gen, ct)
	rec := &removedItem{attr: a, sizeKnown: need == nil}
	switch a.Kind {
	case kindFile:
		if n.FromOverlay {
			if n.Overlay.BackingPath != "" {
				f, err := os.OpenFile(n.Overlay.BackingPath, os.O_RDWR, 0)
				if err != nil {
					f, err = os.Open(n.Overlay.BackingPath)
				}
				if err != nil {
					return nil, err
				}
				rec.file = f
			}
		} else if n.Base.ObjectOID != "" {
			b := n.Base
			rec.base = &b
		}
	case kindSymlink:
		if n.FromOverlay {
			rec.target = n.Overlay.TargetPath
		} else {
			b := n.Base
			rec.base = &b
		}
	}
	return rec, nil
}

func (s *Server) opRemove(ctx context.Context, req *request, dir bool) error {
	if err := validateName(req.Name); err != nil {
		return err
	}
	s.ns.Lock()
	defer s.ns.Unlock()
	parent, err := s.parentDir(req.Parent, false)
	if err != nil {
		return err
	}
	child := childPathOf(parent, req.Name)
	if parent == "." && isSynthesized(child) {
		return wireErr(errEPERM)
	}
	n, err := s.res.ResolvePath(child)
	if err != nil {
		return err
	}
	isDir := n.Type() == "dir"
	switch {
	case dir && !isDir:
		return wireErr(errENOTDIR)
	case !dir && isDir:
		return wireErr(errEPERM)
	}
	if err := s.checkWritable(child); err != nil {
		return err
	}
	var rec *removedItem
	if v, ok := s.inodes.lookupPath(child); ok {
		if rec, err = s.pin(v.ino, n); err != nil {
			return err
		}
	}
	if dir {
		err = s.eng.Rmdir(ctx, child)
		if errors.Is(err, fs.ErrExist) {
			err = wireErr(errENOTEMPTY)
		}
	} else {
		err = s.eng.Unlink(ctx, child)
	}
	if err != nil {
		rec.close()
		return err
	}
	s.inodes.detach(child, rec)
	return nil
}

func (s *Server) opRename(ctx context.Context, req *request) error {
	if err := validateName(req.Name); err != nil {
		return err
	}
	if err := validateName(req.NewName); err != nil {
		return err
	}
	if err := s.prefetchRename(ctx, req); err != nil {
		return err
	}
	s.ns.Lock()
	defer s.ns.Unlock()
	oldParent, err := s.parentDir(req.Parent, false)
	if err != nil {
		return err
	}
	newParent, err := s.parentDir(req.NewParent, true)
	if err != nil {
		return err
	}
	oldPath := childPathOf(oldParent, req.Name)
	newPath := childPathOf(newParent, req.NewName)
	if (oldParent == "." && isSynthesized(oldPath)) || (newParent == "." && isSynthesized(newPath)) {
		return wireErr(errEPERM)
	}
	src, err := s.res.ResolvePath(oldPath)
	if err != nil {
		return err
	}
	if oldPath == newPath {
		return nil
	}
	srcType := src.Type()
	if srcType == "dir" && strings.HasPrefix(newPath, oldPath+"/") {
		return wireErr(errEINVAL)
	}
	if err := s.checkWritable(oldPath, newPath); err != nil {
		return err
	}
	var rec *removedItem
	dst, dstErr := s.res.ResolvePath(newPath)
	dstExists := dstErr == nil
	if dstErr != nil && !errors.Is(dstErr, fs.ErrNotExist) {
		return dstErr
	}
	if dstExists {
		dstType := dst.Type()
		if srcType == "dir" && dstType != "dir" {
			return wireErr(errENOTDIR)
		}
		if srcType != "dir" && dstType == "dir" {
			return wireErr(errEISDIR)
		}
		if v, ok := s.inodes.lookupPath(newPath); ok {
			if rec, err = s.pin(v.ino, dst); err != nil {
				return err
			}
		}
	}
	if err := s.eng.Rename(ctx, oldPath, newPath); err != nil {
		rec.close()
		switch {
		case errors.Is(err, fs.ErrInvalid):
			return wireErr(errEINVAL)
		case errors.Is(err, fs.ErrExist):
			return wireErr(errENOTEMPTY)
		}
		return err
	}
	if dstExists {
		s.inodes.detach(newPath, rec)
	}
	s.inodes.move(oldPath, newPath)
	return nil
}

// prefetchRename downloads the base blobs a rename will copy into the
// overlay (a base file, or every base file under a directory) before the
// exclusive lock is taken.
func (s *Server) prefetchRename(ctx context.Context, req *request) error {
	s.ns.RLock()
	oldParent, err := s.parentDir(req.Parent, false)
	if err != nil {
		s.ns.RUnlock()
		return nil
	}
	oldPath := childPathOf(oldParent, req.Name)
	var nodes []model.BaseNode
	if n, err := s.res.ResolvePath(oldPath); err == nil {
		switch {
		case !n.FromOverlay && n.Base.Type == "file" && n.Base.ObjectOID != "":
			nodes = append(nodes, n.Base)
		case n.Type() == "dir":
			nodes, err = s.baseFilesUnder(ctx, oldPath)
			if err != nil {
				s.ns.RUnlock()
				return err
			}
		}
	}
	s.ns.RUnlock()
	return s.hydrateAll(ctx, nodes)
}

// baseFilesUnder lists the visible base files below dir. Caller holds s.ns.
func (s *Server) baseFilesUnder(ctx context.Context, dir string) ([]model.BaseNode, error) {
	var out []model.BaseNode
	var walk func(string) error
	walk = func(p string) error {
		entries, err := s.res.ReaddirTyped(ctx, p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			cp := childPathOf(p, e.Name)
			switch {
			case e.Type == "dir":
				if err := walk(cp); err != nil {
					return err
				}
			case !e.FromOverlay && e.Type == "file" && e.ObjectOID != "":
				out = append(out, model.BaseNode{Path: cp, Type: e.Type, Mode: e.Mode, ObjectOID: e.ObjectOID, SizeState: e.SizeState, SizeBytes: e.SizeBytes})
			}
		}
		return nil
	}
	return out, walk(dir)
}

func (s *Server) hydrateAll(ctx context.Context, nodes []model.BaseNode) error {
	if len(nodes) == 0 {
		return nil
	}
	sem := make(chan struct{}, sizeFillWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for _, n := range nodes {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if _, _, err := s.env.hydrator.EnsureHydrated(ctx, s.env.repo, n); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}

func (s *Server) opSetattr(ctx context.Context, req *request) (*reply, error) {
	if req.Size != nil || req.Mode != nil {
		// Truncate and chmod of a base file copy it into the overlay.
		if err := s.prefetchBase(ctx, req.Ino); err != nil {
			return nil, err
		}
	}
	consumed := []string{}
	s.ns.RLock()
	v, err := s.node(req.Ino)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	if v.removed != nil {
		s.ns.RUnlock()
		return s.setattrRemoved(ctx, req, v)
	}
	special := v.ino == inoRoot || isSynthesized(v.path)
	if isSynthesized(v.path) && req.Size != nil {
		s.ns.RUnlock()
		return nil, wireErr(errEPERM)
	}
	var typ string
	if !special {
		n, err := s.res.ResolvePath(v.path)
		if err != nil {
			s.ns.RUnlock()
			return nil, err
		}
		typ = n.Type()
	}
	applies := !special && typ != "symlink"
	if req.Size != nil || (applies && (req.Mode != nil || req.MtimeNs != nil)) {
		if v.ino == inoRoot && req.Size != nil {
			s.ns.RUnlock()
			return nil, wireErr(errEISDIR)
		}
		if err := s.checkWritable(v.path); err != nil {
			s.ns.RUnlock()
			return nil, err
		}
	}
	if req.Size != nil {
		switch typ {
		case "dir":
			s.ns.RUnlock()
			return nil, wireErr(errEISDIR)
		case "symlink":
			s.ns.RUnlock()
			return nil, wireErr(errEINVAL)
		}
		if err := s.eng.Truncate(ctx, v.path, int64(*req.Size)); err != nil {
			s.ns.RUnlock()
			return nil, err
		}
		consumed = append(consumed, "size")
	}
	if req.Mode != nil && applies {
		if err := s.eng.SetMode(ctx, v.path, *req.Mode&0o7777); err != nil {
			s.ns.RUnlock()
			return nil, err
		}
		consumed = append(consumed, "mode")
	}
	if req.MtimeNs != nil && applies {
		if err := s.eng.SetMtime(ctx, v.path, time.Unix(0, *req.MtimeNs)); err != nil {
			s.ns.RUnlock()
			return nil, err
		}
		consumed = append(consumed, "mtime")
	}
	if req.AtimeNs != nil {
		consumed = append(consumed, "atime")
	}
	s.ns.RUnlock()
	a, err := s.getattr(ctx, v.ino)
	if err != nil {
		return nil, err
	}
	return &reply{Attr: &a, Consumed: &consumed}, nil
}

func (s *Server) setattrRemoved(ctx context.Context, req *request, v inodeView) (*reply, error) {
	consumed := []string{}
	r := v.removed
	if req.Size != nil {
		if err := r.truncate(ctx, s.env, int64(*req.Size)); err != nil {
			return nil, err
		}
		consumed = append(consumed, "size")
	}
	if req.Mode != nil && r.attr.Kind != kindSymlink {
		r.setMode(*req.Mode)
		consumed = append(consumed, "mode")
	}
	if req.MtimeNs != nil && r.attr.Kind != kindSymlink {
		r.setMtime(*req.MtimeNs)
		consumed = append(consumed, "mtime")
	}
	if req.AtimeNs != nil {
		consumed = append(consumed, "atime")
	}
	a, err := r.getattr(ctx, s.env)
	if err != nil {
		return nil, err
	}
	return &reply{Attr: &a, Consumed: &consumed}, nil
}
