package fskit

import (
	"strings"
	"sync"
)

// inodeTable numbers items for the life of a mount.
//
//   - The root is 2; every other number starts at 3 and is never reused.
//   - A path keeps its number, also after forget, so `stat` gives the same
//     st_ino every time. A rename moves numbers with the items.
//   - A removed or replaced item leaves the path table but keeps its number
//     and its data (a removedItem) until FSKit reclaims it (forget).
type inodeTable struct {
	mu     sync.Mutex
	byIno  map[uint64]*inode
	byPath map[string]*inode
	next   uint64
}

type inode struct {
	ino     uint64
	path    string // "" once removed
	kind    uint8
	held    bool // the kernel holds an item for it (looked up, not forgotten)
	removed *removedItem
}

// inodeView is a copy of an inode's fields, safe to use without the lock.
type inodeView struct {
	ino     uint64
	path    string
	kind    uint8
	removed *removedItem
}

func newInodeTable() *inodeTable {
	t := &inodeTable{
		byIno:  map[uint64]*inode{},
		byPath: map[string]*inode{},
		next:   firstIno,
	}
	root := &inode{ino: inoRoot, path: ".", kind: kindDir, held: true}
	t.byIno[inoRoot] = root
	t.byPath["."] = root
	return t
}

// assign returns the number for path, allocating one the first time.
func (t *inodeTable) assign(path string, kind uint8, held bool) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.byPath[path]; ok {
		n.kind = kind
		if held {
			n.held = true
		}
		return n.ino
	}
	n := &inode{ino: t.next, path: path, kind: kind, held: held}
	t.next++
	t.byIno[n.ino] = n
	t.byPath[path] = n
	return n.ino
}

func (t *inodeTable) get(ino uint64) (inodeView, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.byIno[ino]
	if !ok {
		return inodeView{}, false
	}
	return inodeView{ino: n.ino, path: n.path, kind: n.kind, removed: n.removed}, true
}

func (t *inodeTable) lookupPath(path string) (inodeView, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.byPath[path]
	if !ok {
		return inodeView{}, false
	}
	return inodeView{ino: n.ino, path: n.path, kind: n.kind, removed: n.removed}, true
}

// detach takes path (and every mapped path below it) out of the path table.
// The item at path keeps rec as its data until forget; descendants keep an
// attribute-only record. Items the kernel does not hold are freed at once.
func (t *inodeTable) detach(path string, rec *removedItem) {
	t.mu.Lock()
	var free []*removedItem
	for p, n := range t.byPath {
		if p != path && !strings.HasPrefix(p, path+"/") {
			continue
		}
		if n.ino == inoRoot {
			continue
		}
		delete(t.byPath, p)
		n.path = ""
		if p == path && rec != nil {
			n.removed = rec
		} else {
			n.removed = &removedItem{attr: Attr{Ino: n.ino, Kind: n.kind}}
		}
		if !n.held {
			delete(t.byIno, n.ino)
			free = append(free, n.removed)
		}
	}
	t.mu.Unlock()
	for _, r := range free {
		r.close()
	}
}

// move renames path oldPath, and everything mapped below it, to newPath.
// Callers detach whatever was at newPath first.
func (t *inodeTable) move(oldPath, newPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var moving []*inode
	for p, n := range t.byPath {
		if p == oldPath || strings.HasPrefix(p, oldPath+"/") {
			moving = append(moving, n)
		}
	}
	for _, n := range moving {
		delete(t.byPath, n.path)
	}
	for _, n := range moving {
		n.path = newPath + strings.TrimPrefix(n.path, oldPath)
		t.byPath[n.path] = n
	}
}

// forget records that the kernel let go of ino. A removed item is freed; a
// live one keeps its number with its path.
func (t *inodeTable) forget(ino uint64) {
	t.mu.Lock()
	n, ok := t.byIno[ino]
	if !ok || ino == inoRoot {
		t.mu.Unlock()
		return
	}
	n.held = false
	var rec *removedItem
	if n.removed != nil {
		delete(t.byIno, ino)
		rec = n.removed
	}
	t.mu.Unlock()
	if rec != nil {
		rec.close()
	}
}

func (t *inodeTable) closeAll() {
	t.mu.Lock()
	var recs []*removedItem
	for _, n := range t.byIno {
		if n.removed != nil {
			recs = append(recs, n.removed)
		}
	}
	t.mu.Unlock()
	for _, r := range recs {
		r.close()
	}
}

// size reports how many numbers are live. Used by tests.
func (t *inodeTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byIno)
}
