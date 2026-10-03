package fskit

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func TestHelloUnknownOpAndLimits(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	rep, _ := c.call(map[string]any{"op": "hello", "version": 3, "build": "ext-1"}, nil)
	if rep.Errno != 0 || rep.Version != ProtocolVersion || rep.MaxRead != MaxRead || rep.MaxWrite != MaxWrite || rep.Build != "test" {
		t.Fatalf("hello reply = %+v", rep)
	}
	// The daemon answers with its own version whatever the extension sent.
	rep, _ = c.call(map[string]any{"op": "hello", "version": 2}, nil)
	if rep.Version != ProtocolVersion {
		t.Fatalf("hello with version 2 = %+v", rep)
	}
	if errno := c.op("truncate", map[string]any{"ino": 2}); errno != errENOSYS {
		t.Fatalf("unknown op errno = %d, want ENOSYS", errno)
	}
	readme := c.mustLookup(inoRoot, "README.md")
	if _, errno := c.read(readme.Ino, 0, MaxRead+1); errno != errEINVAL {
		t.Fatalf("read over max_read errno = %d, want EINVAL", errno)
	}
	if _, errno := c.getattr(999999); errno != errESTALE {
		t.Fatalf("getattr of an unknown ino errno = %d, want ESTALE", errno)
	}
	if _, errno := c.lookup(inoRoot, "a/b"); errno != errEINVAL {
		t.Fatalf("lookup with a slash errno = %d, want EINVAL", errno)
	}
	if _, errno := c.lookup(inoRoot, strings.Repeat("x", 256)); errno != errENAMETOOLONG {
		t.Fatalf("lookup of a long name errno = %d, want ENAMETOOLONG", errno)
	}
}

func TestMalformedHeaderClosesConnection(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	conn, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeFrame(conn, []byte("not json"), nil); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil {
		t.Fatal("connection stayed open after a malformed header")
	}
	// An oversized frame length also closes the connection.
	conn2, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	var lens [8]byte
	binary.BigEndian.PutUint32(lens[:4], maxFrameLen+1)
	conn2.Write(lens[:])
	conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn2.Read(b[:]); err == nil {
		t.Fatal("connection stayed open after an oversized frame")
	}
}

func TestLookupReadBaseFilesAndRoot(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	root, errno := c.getattr(inoRoot)
	if errno != 0 || root.Ino != inoRoot || root.Kind != kindDir {
		t.Fatalf("root getattr = %+v errno %d", root, errno)
	}
	readme := c.mustLookup(inoRoot, "README.md")
	if readme.Ino < firstIno || readme.Kind != kindFile || readme.Mode != 0o644 {
		t.Fatalf("README.md attr = %+v", readme)
	}
	if readme.Size != uint64(len(h.files["README.md"])) {
		t.Fatalf("README.md size = %d, want %d (an unknown size must be learned, not reported as 0)", readme.Size, len(h.files["README.md"]))
	}
	if got := c.readAll(readme.Ino); got != h.files["README.md"] {
		t.Fatalf("README.md content = %q", got)
	}
	run := c.mustLookup(c.mustLookup(inoRoot, "bin").Ino, "run.sh")
	if run.Mode != 0o755 {
		t.Fatalf("bin/run.sh mode = %o, want 755", run.Mode)
	}
	guide := c.mustLookup(c.mustLookup(inoRoot, "docs").Ino, "guide.md")
	data, errno := c.read(guide.Ino, 11, 10)
	if errno != 0 || string(data) != "guide line" {
		t.Fatalf("partial read = %q errno %d", data, errno)
	}
	data, errno = c.read(guide.Ino, int64(guide.Size)+10, 10)
	if errno != 0 || len(data) != 0 {
		t.Fatalf("read past the end = %q errno %d", data, errno)
	}
	if _, errno := c.lookup(inoRoot, "missing"); errno != errENOENT {
		t.Fatalf("lookup missing errno = %d, want ENOENT", errno)
	}
	if _, errno := c.lookup(readme.Ino, "x"); errno != errENOTDIR {
		t.Fatalf("lookup under a file errno = %d, want ENOTDIR", errno)
	}
	src := c.mustLookup(inoRoot, "src")
	if _, errno := c.read(src.Ino, 0, 10); errno != errEISDIR {
		t.Fatalf("read of a directory errno = %d, want EISDIR", errno)
	}
	link := c.mustLookup(inoRoot, "link")
	if link.Kind != kindSymlink || link.Size != uint64(len("README.md")) {
		t.Fatalf("link attr = %+v", link)
	}
	rep, _ := c.call(map[string]any{"op": "readlink", "ino": link.Ino}, nil)
	if rep.Errno != 0 || rep.Target == nil || *rep.Target != "README.md" {
		t.Fatalf("readlink = %+v", rep)
	}
	if errno := c.op("readlink", map[string]any{"ino": readme.Ino}); errno != errEINVAL {
		t.Fatalf("readlink of a file errno = %d, want EINVAL", errno)
	}
}

func TestSynthesizedRootFiles(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	git := c.mustLookup(inoRoot, ".git")
	want := "gitdir: " + h.repo.GitDir + "\n"
	if got := c.readAll(git.Ino); got != want {
		t.Fatalf(".git content = %q, want %q", got, want)
	}
	never := c.mustLookup(inoRoot, ".metadata_never_index")
	if never.Size != 0 || never.Kind != kindFile {
		t.Fatalf(".metadata_never_index attr = %+v", never)
	}
	entries, _ := c.listAll(inoRoot, 0, true)
	got := names(entries)
	for _, n := range []string{".git", ".metadata_never_index", "README.md", "src"} {
		if !slices.Contains(got, n) {
			t.Fatalf("root listing %v lacks %s", got, n)
		}
	}
	if errno := c.unlink(inoRoot, ".git"); errno != errEPERM {
		t.Fatalf("unlink .git errno = %d, want EPERM", errno)
	}
	if _, errno := c.create("create", inoRoot, ".git", nil); errno != errEEXIST {
		t.Fatalf("create .git errno = %d, want EEXIST", errno)
	}
	if _, errno := c.write(git.Ino, 0, []byte("x")); errno != errEPERM {
		t.Fatalf("write .git errno = %d, want EPERM", errno)
	}
	if errno := c.rename(inoRoot, "README.md", inoRoot, ".metadata_never_index"); errno != errEPERM {
		t.Fatalf("rename onto .metadata_never_index errno = %d, want EPERM", errno)
	}
}

// Two clients race to create one name: exactly one wins and the other gets
// EEXIST, for every kind and for mixed kinds.
func TestCreateRaceExactlyOneWins(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	a, b := h.client(), h.client()
	ops := [][2]string{{"create", "create"}, {"mkdir", "mkdir"}, {"symlink", "symlink"}, {"create", "mkdir"}, {"symlink", "create"}}
	for round := 0; round < 60; round++ {
		pair := ops[round%len(ops)]
		name := fmt.Sprintf("race-%d", round)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var errs [2]int32
		for i, c := range []*wireClient{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				extra := map[string]any{"mode": 0o644, "target": "README.md"}
				_, errs[i] = c.create(pair[i], inoRoot, name, extra)
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, e := range errs {
			switch e {
			case 0:
				wins++
			case errEEXIST:
			default:
				t.Fatalf("round %d (%v): errno %d", round, pair, e)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d (%v): %d winners, errnos %v", round, pair, wins, errs)
		}
	}
	// Names that exist only in the base snapshot are taken too.
	for _, op := range []string{"create", "mkdir", "symlink"} {
		if _, errno := a.create(op, inoRoot, "README.md", map[string]any{"target": "x"}); errno != errEEXIST {
			t.Fatalf("%s over a base file errno = %d, want EEXIST", op, errno)
		}
		if _, errno := a.create(op, inoRoot, "src", map[string]any{"target": "x"}); errno != errEEXIST {
			t.Fatalf("%s over a base directory errno = %d, want EEXIST", op, errno)
		}
	}
	// A removed base name is free again.
	if errno := a.unlink(inoRoot, "README.md"); errno != 0 {
		t.Fatalf("unlink README.md errno %d", errno)
	}
	if _, errno := a.create("create", inoRoot, "README.md", nil); errno != 0 {
		t.Fatalf("create over a removed base file errno = %d", errno)
	}
}

func TestUnlinkWhileOpen(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	f := c.mustCreate(inoRoot, "tmp.txt", "hello")
	if errno := c.unlink(inoRoot, "tmp.txt"); errno != 0 {
		t.Fatalf("unlink errno %d", errno)
	}
	if _, errno := c.lookup(inoRoot, "tmp.txt"); errno != errENOENT {
		t.Fatalf("lookup after unlink errno = %d, want ENOENT", errno)
	}
	if got := c.readAll(f.Ino); got != "hello" {
		t.Fatalf("read after unlink = %q", got)
	}
	c.mustWrite(f.Ino, 5, []byte(" world"))
	if got := c.readAll(f.Ino); got != "hello world" {
		t.Fatalf("read after a write to the removed file = %q", got)
	}
	if a, errno := c.getattr(f.Ino); errno != 0 || a.Size != 11 || a.Ino != f.Ino {
		t.Fatalf("getattr of the removed file = %+v errno %d", a, errno)
	}
	entries, _ := c.listAll(inoRoot, 0, false)
	if slices.Contains(names(entries), "tmp.txt") {
		t.Fatal("removed file still listed")
	}
	c.forget(f.Ino)
	if _, errno := c.getattr(f.Ino); errno != errESTALE {
		t.Fatalf("getattr after forget errno = %d, want ESTALE", errno)
	}
	if _, errno := c.read(f.Ino, 0, 10); errno != errESTALE {
		t.Fatalf("read after forget errno = %d, want ESTALE", errno)
	}

	// A base file that was never copied into the overlay.
	readme := c.mustLookup(inoRoot, "README.md")
	if errno := c.unlink(inoRoot, "README.md"); errno != 0 {
		t.Fatalf("unlink README.md errno %d", errno)
	}
	if got := c.readAll(readme.Ino); got != h.files["README.md"] {
		t.Fatalf("read of the removed base file = %q", got)
	}
	c.mustWrite(readme.Ino, 0, []byte("##"))
	if got := c.readAll(readme.Ino); got != "##"+h.files["README.md"][2:] {
		t.Fatalf("read after writing the removed base file = %q", got)
	}
	c.forget(readme.Ino)
	if _, errno := c.getattr(readme.Ino); errno != errESTALE {
		t.Fatalf("getattr after forget errno = %d, want ESTALE", errno)
	}

	// unlink and rmdir check the kind.
	if errno := c.unlink(inoRoot, "src"); errno != errEPERM {
		t.Fatalf("unlink of a directory errno = %d, want EPERM", errno)
	}
	if errno := c.rmdir(inoRoot, "link"); errno != errENOTDIR {
		t.Fatalf("rmdir of a symlink errno = %d, want ENOTDIR", errno)
	}
	if errno := c.rmdir(inoRoot, "src"); errno != errENOTEMPTY {
		t.Fatalf("rmdir of a non-empty directory errno = %d, want ENOTEMPTY", errno)
	}
	d := c.mustMkdir(inoRoot, "gone")
	if errno := c.rmdir(inoRoot, "gone"); errno != 0 {
		t.Fatalf("rmdir errno %d", errno)
	}
	if a, errno := c.getattr(d.Ino); errno != 0 || a.Kind != kindDir {
		t.Fatalf("getattr of a removed directory = %+v errno %d", a, errno)
	}
	if entries, _ := c.listAll(d.Ino, 0, false); len(entries) != 2 {
		t.Fatalf("listing of a removed directory = %v", names(entries))
	}
	if _, errno := c.create("create", d.Ino, "x", nil); errno != errENOENT {
		t.Fatalf("create in a removed directory errno = %d, want ENOENT", errno)
	}
}

func TestRenameFilesAndDirectories(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()

	// A file renamed over an existing file replaces it in one step; the
	// replaced item lives on until forget, and numbers move with items.
	a := c.mustCreate(inoRoot, "a.txt", "AAA")
	b := c.mustCreate(inoRoot, "b.txt", "BBB")
	if errno := c.rename(inoRoot, "a.txt", inoRoot, "b.txt"); errno != 0 {
		t.Fatalf("rename a over b errno %d", errno)
	}
	if got := c.mustLookup(inoRoot, "b.txt"); got.Ino != a.Ino {
		t.Fatalf("b.txt ino after rename = %d, want a's %d", got.Ino, a.Ino)
	}
	if _, errno := c.lookup(inoRoot, "a.txt"); errno != errENOENT {
		t.Fatalf("a.txt after rename errno = %d", errno)
	}
	if got := c.readAll(a.Ino); got != "AAA" {
		t.Fatalf("content at b.txt = %q", got)
	}
	if got := c.readAll(b.Ino); got != "BBB" {
		t.Fatalf("replaced file content = %q, want BBB until forget", got)
	}
	c.forget(b.Ino)
	if _, errno := c.getattr(b.Ino); errno != errESTALE {
		t.Fatalf("replaced file after forget errno = %d", errno)
	}

	// Over a base file, keeping the base file readable through its number.
	readme := c.mustLookup(inoRoot, "README.md")
	c.mustCreate(inoRoot, "new.md", "new readme\n")
	if errno := c.rename(inoRoot, "new.md", inoRoot, "README.md"); errno != 0 {
		t.Fatalf("rename over README.md errno %d", errno)
	}
	if got := c.readAll(c.mustLookup(inoRoot, "README.md").Ino); got != "new readme\n" {
		t.Fatalf("README.md after rename = %q", got)
	}
	if got := c.readAll(readme.Ino); got != h.files["README.md"] {
		t.Fatalf("replaced base README.md = %q", got)
	}

	// A base file moved to a new name keeps its number.
	util := c.mustLookup(c.mustLookup(inoRoot, "src").Ino, "util.go")
	if errno := c.rename(c.mustLookup(inoRoot, "src").Ino, "util.go", inoRoot, "util.go"); errno != 0 {
		t.Fatalf("rename src/util.go errno %d", errno)
	}
	if got := c.mustLookup(inoRoot, "util.go"); got.Ino != util.Ino {
		t.Fatalf("util.go ino = %d, want %d", got.Ino, util.Ino)
	}
	if got := c.readAll(util.Ino); got != h.files["src/util.go"] {
		t.Fatalf("moved base file content = %q", got)
	}

	// Directory renames move every number below them.
	d := c.mustMkdir(inoRoot, "d")
	x := c.mustCreate(d.Ino, "x", "xx")
	if errno := c.rename(inoRoot, "d", inoRoot, "e"); errno != 0 {
		t.Fatalf("rename d to e errno %d", errno)
	}
	e := c.mustLookup(inoRoot, "e")
	if e.Ino != d.Ino {
		t.Fatalf("e ino = %d, want %d", e.Ino, d.Ino)
	}
	if got := c.mustLookup(e.Ino, "x"); got.Ino != x.Ino {
		t.Fatalf("e/x ino = %d, want %d", got.Ino, x.Ino)
	}
	if got := c.readAll(x.Ino); got != "xx" {
		t.Fatalf("e/x content = %q", got)
	}
	// A base directory with base files below it.
	nested := c.mustLookup(inoRoot, "nested")
	aDir := c.mustLookup(nested.Ino, "a")
	bTxt := c.mustLookup(aDir.Ino, "b.txt")
	if errno := c.rename(inoRoot, "nested", inoRoot, "moved"); errno != 0 {
		t.Fatalf("rename base directory errno %d", errno)
	}
	moved := c.mustLookup(inoRoot, "moved")
	if moved.Ino != nested.Ino {
		t.Fatalf("moved ino = %d, want %d", moved.Ino, nested.Ino)
	}
	if got := c.mustLookup(c.mustLookup(moved.Ino, "a").Ino, "b.txt"); got.Ino != bTxt.Ino {
		t.Fatalf("moved/a/b.txt ino = %d, want %d", got.Ino, bTxt.Ino)
	}
	if got := c.readAll(bTxt.Ino); got != h.files["nested/a/b.txt"] {
		t.Fatalf("moved/a/b.txt content = %q", got)
	}

	// Directory over an empty directory replaces it.
	c.mustMkdir(inoRoot, "empty2")
	f := c.mustMkdir(inoRoot, "f")
	if errno := c.rename(inoRoot, "f", inoRoot, "empty2"); errno != 0 {
		t.Fatalf("rename dir over empty dir errno %d", errno)
	}
	if got := c.mustLookup(inoRoot, "empty2"); got.Ino != f.Ino {
		t.Fatalf("empty2 ino = %d, want %d", got.Ino, f.Ino)
	}

	// Error rules.
	cases := []struct {
		from, to    string
		want        int32
		description string
	}{
		{description: "missing source", from: "nope", to: "x", want: errENOENT},
		{description: "dir over non-empty dir", from: "e", to: "src", want: errENOTEMPTY},
		{description: "dir over file", from: "e", to: "README.md", want: errENOTDIR},
		{description: "file over dir", from: "README.md", to: "src", want: errEISDIR},
	}
	for _, tc := range cases {
		if errno := c.rename(inoRoot, tc.from, inoRoot, tc.to); errno != tc.want {
			t.Fatalf("%s: errno %d, want %d", tc.description, errno, tc.want)
		}
	}
	if errno := c.rename(inoRoot, "e", e.Ino, "inside"); errno != errEINVAL {
		t.Fatalf("dir into itself errno = %d, want EINVAL", errno)
	}
	if errno := c.rename(inoRoot, "README.md", inoRoot, "README.md"); errno != 0 {
		t.Fatalf("rename onto itself errno = %d", errno)
	}
}

func TestReaddirPagingLargeDirectory(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	dir := c.mustMkdir(inoRoot, "big")
	const n = 1200
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("entry-%04d", i)
		c.mustCreate(dir.Ino, name, "")
		want = append(want, name)
	}
	sort.Strings(want)

	plain, v1 := c.listAll(dir.Ino, 100, false)
	if plain[0].Name != "." || plain[1].Name != ".." || plain[0].Ino != dir.Ino || plain[1].Ino != inoRoot {
		t.Fatalf("plain listing starts %+v %+v", plain[0], plain[1])
	}
	if got := names(plain[2:]); !slices.Equal(got, want) {
		t.Fatalf("plain listing has %d names, want %d in order", len(got), len(want))
	}
	withAttrs, v2 := c.listAll(dir.Ino, 128, true)
	if got := names(withAttrs); !slices.Equal(got, want) {
		t.Fatalf("attr listing has %d names, want %d in order", len(got), len(want))
	}
	for _, e := range withAttrs {
		if e.Attr == nil || e.Attr.Ino != e.Ino || e.Ino < firstIno {
			t.Fatalf("attr entry %+v", e)
		}
	}
	if v1 != v2 {
		t.Fatalf("verifier changed with no change: %d then %d", v1, v2)
	}
	// Cookies mean the same position in both modes.
	if withAttrs[0].NextCookie != plain[2].NextCookie {
		t.Fatalf("cookie of the first child differs between modes: %d and %d", withAttrs[0].NextCookie, plain[2].NextCookie)
	}

	// A listing in progress stays consistent while the directory changes.
	rep, _ := c.call(map[string]any{"op": "readdir", "ino": dir.Ino, "cookie": 0, "verifier": 0, "max_entries": 300, "with_attrs": true}, nil)
	first := *rep.Entries
	ver := rep.Verifier
	c.mustCreate(dir.Ino, "aaa-new", "")
	if errno := c.unlink(dir.Ino, "entry-0500"); errno != 0 {
		t.Fatalf("unlink errno %d", errno)
	}
	rest := first
	cookie := first[len(first)-1].NextCookie
	for {
		rep, _ = c.call(map[string]any{"op": "readdir", "ino": dir.Ino, "cookie": cookie, "verifier": ver, "max_entries": 300, "with_attrs": true}, nil)
		if rep.Errno != 0 {
			t.Fatalf("page after a change errno %d", rep.Errno)
		}
		if rep.Verifier != ver {
			t.Fatalf("verifier changed inside a listing")
		}
		rest = append(rest, *rep.Entries...)
		if *rep.EOF {
			break
		}
		cookie = (*rep.Entries)[len(*rep.Entries)-1].NextCookie
	}
	if got := names(rest); !slices.Equal(got, want) {
		t.Fatalf("listing in progress changed underneath: %d names", len(got))
	}
	// A new listing sees the change, with a new verifier.
	after, v3 := c.listAll(dir.Ino, 500, true)
	if v3 == ver {
		t.Fatal("verifier did not change after the directory changed")
	}
	got := names(after)
	if !slices.Contains(got, "aaa-new") || slices.Contains(got, "entry-0500") || len(got) != n {
		t.Fatalf("new listing is wrong: %d names", len(got))
	}
	// A cookie with a verifier the daemon never issued, or one it no longer
	// has while the directory changed, is a bad cookie.
	rep, _ = c.call(map[string]any{"op": "readdir", "ino": dir.Ino, "cookie": 5, "verifier": 12345, "max_entries": 10, "with_attrs": false}, nil)
	if rep.Errno != errBadCookie {
		t.Fatalf("stale cookie errno = %d, want %d", rep.Errno, errBadCookie)
	}
	// Base directories list with sizes learned, never 0 for a non-empty file.
	many, _ := c.listAll(c.mustLookup(inoRoot, "many").Ino, 16, true)
	if len(many) != 40 {
		t.Fatalf("many/ has %d entries", len(many))
	}
	for _, e := range many {
		if want := uint64(len(h.files["many/"+e.Name])); e.Attr.Size != want {
			t.Fatalf("many/%s size %d, want %d", e.Name, e.Attr.Size, want)
		}
	}
	if errno := c.op("readdir", map[string]any{"ino": c.mustLookup(inoRoot, "README.md").Ino}); errno != errENOTDIR {
		t.Fatalf("readdir of a file errno = %d, want ENOTDIR", errno)
	}
}

func TestSetattrAndSymlink(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	f := c.mustCreate(inoRoot, "s.txt", "0123456789")
	setattr := func(ino uint64, fields map[string]any) reply {
		fields["op"] = "setattr"
		fields["ino"] = ino
		rep, _ := c.call(fields, nil)
		return rep
	}
	rep := setattr(f.Ino, map[string]any{"size": 4})
	if rep.Errno != 0 || !slices.Equal(*rep.Consumed, []string{"size"}) || rep.Attr.Size != 4 {
		t.Fatalf("truncate = %+v", rep)
	}
	if got := c.readAll(f.Ino); got != "0123" {
		t.Fatalf("content after truncate = %q", got)
	}
	rep = setattr(f.Ino, map[string]any{"mode": 0o755})
	if rep.Errno != 0 || !slices.Equal(*rep.Consumed, []string{"mode"}) || rep.Attr.Mode != 0o755 {
		t.Fatalf("chmod = %+v", rep)
	}
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC).UnixNano()
	rep = setattr(f.Ino, map[string]any{"mtime_ns": mtime, "atime_ns": mtime})
	if rep.Errno != 0 || !slices.Equal(*rep.Consumed, []string{"mtime", "atime"}) || rep.Attr.MtimeNs != mtime {
		t.Fatalf("utimes = %+v", rep)
	}
	if a, _ := c.getattr(f.Ino); a.MtimeNs != mtime || a.Mode != 0o755 {
		t.Fatalf("getattr after setattr = %+v", a)
	}
	// A base file: chmod copies it into the overlay and keeps its content.
	run := c.mustLookup(c.mustLookup(inoRoot, "bin").Ino, "run.sh")
	rep = setattr(run.Ino, map[string]any{"mode": 0o700})
	if rep.Errno != 0 || rep.Attr.Mode != 0o700 {
		t.Fatalf("chmod of a base file = %+v", rep)
	}
	if got := c.readAll(run.Ino); got != h.files["bin/run.sh"] {
		t.Fatalf("base file content after chmod = %q", got)
	}
	// Size on a directory.
	if rep := setattr(c.mustLookup(inoRoot, "src").Ino, map[string]any{"size": 0}); rep.Errno != errEISDIR {
		t.Fatalf("truncate of a directory errno = %d", rep.Errno)
	}

	// symlink creates; readlink reads; mode and times on a symlink are not
	// consumed and are not errors.
	l, errno := c.create("symlink", inoRoot, "l", map[string]any{"target": "s.txt"})
	if errno != 0 || l.Kind != kindSymlink || l.Size != 5 {
		t.Fatalf("symlink = %+v errno %d", l, errno)
	}
	r, _ := c.call(map[string]any{"op": "readlink", "ino": l.Ino}, nil)
	if r.Target == nil || *r.Target != "s.txt" {
		t.Fatalf("readlink = %+v", r)
	}
	if got := c.mustLookup(inoRoot, "l"); got.Ino != l.Ino || got.Kind != kindSymlink {
		t.Fatalf("lookup l = %+v", got)
	}
	if _, errno := c.create("symlink", inoRoot, "l", map[string]any{"target": "x"}); errno != errEEXIST {
		t.Fatalf("second symlink errno = %d, want EEXIST", errno)
	}
	if _, errno := c.create("symlink", inoRoot, "l2", map[string]any{"target": strings.Repeat("t", 4097)}); errno != errENAMETOOLONG {
		t.Fatalf("long symlink target errno = %d", errno)
	}
	rep = setattr(l.Ino, map[string]any{"mode": 0o700, "mtime_ns": mtime})
	if rep.Errno != 0 || len(*rep.Consumed) != 0 {
		t.Fatalf("setattr of a symlink = %+v", rep)
	}
	// The root takes no mode or times, without an error.
	rep = setattr(inoRoot, map[string]any{"mode": 0o700})
	if rep.Errno != 0 || len(*rep.Consumed) != 0 {
		t.Fatalf("setattr of the root = %+v", rep)
	}
	// Setattr on a removed file applies to its kept data.
	if errno := c.unlink(inoRoot, "s.txt"); errno != 0 {
		t.Fatal(errno)
	}
	rep = setattr(f.Ino, map[string]any{"size": 2, "mode": 0o600})
	if rep.Errno != 0 || rep.Attr.Size != 2 || rep.Attr.Mode != 0o600 {
		t.Fatalf("setattr of a removed file = %+v", rep)
	}
}

func TestStableInodesAndForget(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	first := c.mustLookup(inoRoot, "README.md")
	if again := c.mustLookup(inoRoot, "README.md"); again.Ino != first.Ino {
		t.Fatalf("second lookup ino %d, want %d", again.Ino, first.Ino)
	}
	// forget of a live item keeps its number with its path.
	c.forget(first.Ino)
	if again := c.mustLookup(inoRoot, "README.md"); again.Ino != first.Ino {
		t.Fatalf("lookup after forget ino %d, want %d", again.Ino, first.Ino)
	}
	if a, errno := c.getattr(first.Ino); errno != 0 || a.Ino != first.Ino {
		t.Fatalf("getattr after forget = %+v errno %d", a, errno)
	}
	// Listing assigns the same numbers lookup does.
	entries, _ := c.listAll(inoRoot, 0, true)
	for _, e := range entries {
		if e.Name == "README.md" && e.Ino != first.Ino {
			t.Fatalf("listing ino %d, lookup ino %d", e.Ino, first.Ino)
		}
	}
	// A new file at a removed name gets a new number; numbers never repeat.
	seen := map[uint64]bool{first.Ino: true}
	for i := 0; i < 5; i++ {
		a := c.mustCreate(inoRoot, "cycle.txt", "x")
		if seen[a.Ino] || a.Ino < firstIno {
			t.Fatalf("ino %d reused or reserved", a.Ino)
		}
		seen[a.Ino] = true
		if errno := c.unlink(inoRoot, "cycle.txt"); errno != 0 {
			t.Fatal(errno)
		}
		c.forget(a.Ino)
	}
	// Forget of unknown numbers, the root and synthesized files is harmless.
	c.forget(inoRoot, 424242, c.mustLookup(inoRoot, ".git").Ino)
	if _, errno := c.getattr(inoRoot); errno != 0 {
		t.Fatal("root gone after forget")
	}
	// Removed items the kernel never held are freed at once.
	before := h.srv.inodes.size()
	c.mustCreate(inoRoot, "unheld.txt", "")
	ino := c.mustLookup(inoRoot, "unheld.txt").Ino
	c.forget(ino)
	if errno := c.unlink(inoRoot, "unheld.txt"); errno != 0 {
		t.Fatal(errno)
	}
	if after := h.srv.inodes.size(); after != before {
		t.Fatalf("inode table grew from %d to %d after a forgotten item was removed", before, after)
	}
}

func TestLargeFileInChunks(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	c := h.client()
	f := c.mustCreate(inoRoot, "large.bin", "")
	want := bytes.Repeat([]byte("0123456789abcdef"), 3*65536+100) // just over 3 MiB
	for off := 0; off < len(want); off += 1 << 20 {
		end := min(off+1<<20, len(want))
		c.mustWrite(f.Ino, int64(off), want[off:end])
	}
	if a, _ := c.getattr(f.Ino); a.Size != uint64(len(want)) {
		t.Fatalf("size %d, want %d", a.Size, len(want))
	}
	if got := c.readAll(f.Ino); got != string(want) {
		t.Fatalf("content differs (%d bytes read)", len(got))
	}
	if _, errno := c.write(f.Ino, 0, make([]byte, MaxWrite+1)); errno != errEINVAL {
		t.Fatalf("write over max_write errno = %d", errno)
	}
}

// slowHydrator delays one blob, to show that a slow download stalls no other
// operation (the first test the diligence report asks for).
type slowHydrator struct {
	model.Hydrator
	slowOID atomic.Value // string
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (s *slowHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	if oid, _ := s.slowOID.Load().(string); oid != "" && node.ObjectOID == oid {
		s.once.Do(func() { close(s.started) })
		<-s.release
	}
	return s.Hydrator.OpenHydrated(ctx, repo, node)
}

func TestSlowReadDoesNotStallOthers(t *testing.T) {
	slow := &slowHydrator{release: make(chan struct{}), started: make(chan struct{})}
	h := newHarness(t, harnessOptions{wrap: func(inner model.Hydrator) model.Hydrator {
		slow.Hydrator = inner
		return slow
	}})
	c := h.client()
	guide := c.mustLookup(c.mustLookup(inoRoot, "docs").Ino, "guide.md")
	n, err := h.srv.res.ResolvePath("docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	slow.slowOID.Store(n.Base.ObjectOID)
	done := make(chan []byte, 1)
	go func() {
		data, _ := c.read(guide.Ino, 0, 64)
		done <- data
	}()
	<-slow.started
	// While that read waits, the same connection serves everything else.
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < 50; i++ {
		c.mustLookup(inoRoot, "README.md")
		c.listAll(inoRoot, 0, true)
		c.mustCreate(inoRoot, fmt.Sprintf("during-%d", i), "x")
		if errno := c.unlink(inoRoot, fmt.Sprintf("during-%d", i)); errno != 0 {
			t.Fatal(errno)
		}
	}
	if time.Now().After(deadline) {
		t.Fatal("operations stalled behind a slow read")
	}
	select {
	case <-done:
		t.Fatal("slow read finished before it was released")
	default:
	}
	close(slow.release)
	if data := <-done; !strings.HasPrefix(h.files["docs/guide.md"], string(data)) || len(data) != 64 {
		t.Fatalf("slow read = %q", data)
	}
}

func TestReadOnlyMountAndWriteThroughAllowList(t *testing.T) {
	h := newHarness(t, harnessOptions{readOnly: true})
	c := h.client()
	if _, errno := c.create("create", inoRoot, "x", nil); errno != errEROFS {
		t.Fatalf("create on a read-only mount errno = %d", errno)
	}
	readme := c.mustLookup(inoRoot, "README.md")
	if _, errno := c.write(readme.Ino, 0, []byte("x")); errno != errEROFS {
		t.Fatalf("write on a read-only mount errno = %d", errno)
	}
	if errno := c.unlink(inoRoot, "README.md"); errno != errEROFS {
		t.Fatalf("unlink on a read-only mount errno = %d", errno)
	}
	// Reads still work.
	if got := c.readAll(readme.Ino); got != h.files["README.md"] {
		t.Fatalf("read on a read-only mount = %q", got)
	}
	// The daemon's own write-through paths are accepted while in flight.
	paths := []string{"README.md", ".pando-wt-0001"}
	h.srv.allow.add(paths)
	tmp := c.mustCreate(inoRoot, ".pando-wt-0001", "server change\n")
	if errno := c.rename(inoRoot, ".pando-wt-0001", inoRoot, "README.md"); errno != 0 {
		t.Fatalf("write-through rename errno %d", errno)
	}
	h.srv.allow.remove(paths)
	if got := c.readAll(tmp.Ino); got != "server change\n" {
		t.Fatalf("README.md after write-through = %q", got)
	}
	if _, errno := c.write(tmp.Ino, 0, []byte("x")); errno != errEROFS {
		t.Fatalf("write after the change finished errno = %d", errno)
	}
}

func TestWriteThroughMountOnAFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes := []Change{
		{Path: "a.md", Data: []byte("new")},
		{Path: "deep/b/c.sh", Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		{Path: "link", Symlink: "a.md"},
		{Path: "gone.txt", Delete: true},
	}
	if err := WriteThroughMount(context.Background(), dir, changes); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(b) != "new" {
		t.Fatalf("a.md = %q", b)
	}
	st, err := os.Stat(filepath.Join(dir, "deep", "b", "c.sh"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("c.sh stat %v err %v", st, err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "link")); target != "a.md" {
		t.Fatalf("link target %q", target)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "**", WriteThroughPrefix+"*"))
	top, _ := filepath.Glob(filepath.Join(dir, WriteThroughPrefix+"*"))
	if len(matches)+len(top) != 0 {
		t.Fatalf("temp files left: %v %v", matches, top)
	}
	for _, bad := range []string{"../x", "/etc/passwd", ".", ""} {
		if err := WriteThroughMount(context.Background(), dir, []Change{{Path: bad, Data: []byte("x")}}); err == nil {
			t.Fatalf("path %q accepted", bad)
		}
	}
}

func TestEnsureMacOSExcludes(t *testing.T) {
	gitDir := t.TempDir()
	path := filepath.Join(gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# git's own comment\n*.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := EnsureMacOSExcludes(gitDir); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "# git's own comment\n*.log\n") {
		t.Fatalf("existing lines changed:\n%s", s)
	}
	if strings.Count(s, excludeBegin) != 1 {
		t.Fatalf("block written more than once:\n%s", s)
	}
	for _, p := range []string{"._*", ".DS_Store", ".fseventsd/", ".Spotlight-V100/", ".Trashes/", ".TemporaryItems/", "/.metadata_never_index"} {
		if !strings.Contains(s, "\n"+p+"\n") {
			t.Fatalf("pattern %s missing:\n%s", p, s)
		}
	}
	// git itself honours the patterns.
	work := t.TempDir()
	gitRun(t, work, "init", "-q")
	if err := EnsureMacOSExcludes(filepath.Join(work, ".git")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"._a", ".DS_Store", ".metadata_never_index", "kept.txt"} {
		os.WriteFile(filepath.Join(work, f), nil, 0o644)
	}
	os.MkdirAll(filepath.Join(work, ".fseventsd"), 0o755)
	os.WriteFile(filepath.Join(work, ".fseventsd", "x"), nil, 0o644)
	status := gitRun(t, work, "status", "--porcelain", "--untracked-files=all")
	if strings.TrimSpace(status) != "?? kept.txt" {
		t.Fatalf("git status shows macOS files:\n%s", status)
	}
}

func TestListenUnixLongPath(t *testing.T) {
	base, err := os.MkdirTemp("", "fsk")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	dir := filepath.Join(base, strings.Repeat("d", 60), strings.Repeat("e", 48))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, SocketName)
	if len(sock) <= maxSocketPath {
		t.Fatalf("test path is only %d bytes", len(sock))
	}
	ln, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(sock); err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket at the long path: %v %v", st, err)
	}
	ln.Close()
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left after close: %v", err)
	}
}

func TestMountIDs(t *testing.T) {
	if MountIDFor("pando-main") != "pando-main" {
		t.Fatal("a valid repo id changed")
	}
	for _, id := range []string{"has space", strings.Repeat("x", 49), ".hidden", "a/b"} {
		got := MountIDFor(id)
		if !ValidMountID(got) || got == id {
			t.Fatalf("MountIDFor(%q) = %q", id, got)
		}
	}
	t.Setenv("PANDO_FSKIT_DIR", t.TempDir())
	dir, err := PrepareStateDir("repo-1", "My Repo")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, VolumeNameFile)); string(b) != "My Repo\n" {
		t.Fatalf("volname = %q", b)
	}
	if _, err := PrepareStateDir("../evil", "x"); err == nil {
		t.Fatal("invalid mount id accepted")
	}
}

// A merge lands on the server: the daemon builds the new generation, writes
// the server-side changes through the mount (here over the wire, as the
// kernel would deliver them), switches generation, and the overlay
// reconcile leaves nothing dirty.
func TestServerChangeWriteThroughThenReconcile(t *testing.T) {
	h := newHarness(t, harnessOptions{readOnly: true})
	ctx := context.Background()
	c := h.client()
	oldReadme := c.mustLookup(inoRoot, "README.md")

	// The server gets a new commit.
	os.WriteFile(filepath.Join(h.work, "README.md"), []byte("# merged\n"), 0o644)
	os.Remove(filepath.Join(h.work, "src", "util.go"))
	os.MkdirAll(filepath.Join(h.work, "notes"), 0o755)
	os.WriteFile(filepath.Join(h.work, "notes", "new.md"), []byte("new note\n"), 0o644)
	gitRun(t, h.work, "add", "-A")
	gitRun(t, h.work, "commit", "-q", "-m", "merge")
	gitRun(t, h.work, "push", "-q", "origin", "main")

	// The daemon side: fetch, move the branch, build the new generation.
	gitRun(t, h.repo.GitDir, "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/origin/main")
	gitRun(t, h.repo.GitDir, "update-ref", "refs/heads/main", "refs/remotes/origin/main")
	oid, ref, err := h.git.ResolveHEAD(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := h.git.BuildTreeIndex(ctx, h.repo, oid)
	if err != nil {
		t.Fatal(err)
	}
	gen2, err := h.snap.PublishGeneration(ctx, oid, ref, nodes)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := h.snap.DiffGenerations(ctx, h.gen, gen2)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := ServerChanges(ctx, diff, h.ov, h.h, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	var summary []string
	for _, ch := range changes {
		if ch.Delete {
			summary = append(summary, "delete "+ch.Path)
		} else {
			summary = append(summary, "write "+ch.Path+"="+string(ch.Data))
		}
	}
	want := []string{"delete src/util.go", "write README.md=# merged\n", "write notes/new.md=new note\n"}
	if !slices.Equal(summary, want) {
		t.Fatalf("changes = %q, want %q", summary, want)
	}

	// Apply them the way the kernel delivers a write-through: mkdir, a temp
	// file, a write, a rename over the target, an unlink. The mount is
	// read-only, so only the registered paths are accepted.
	for _, ch := range changes {
		paths := append(ancestors(ch.Path), ch.Path)
		tmp := ""
		if !ch.Delete {
			tmp = filepath.ToSlash(filepath.Join(filepath.Dir(ch.Path), WriteThroughPrefix+"t"))
			paths = append(paths, tmp)
		}
		h.srv.allow.add(paths)
		parent := uint64(inoRoot)
		for _, dir := range ancestors(ch.Path) {
			a, errno := c.lookup(parent, filepath.Base(dir))
			if errno == errENOENT {
				a = c.mustMkdir(parent, filepath.Base(dir))
			}
			parent = a.Ino
		}
		if ch.Delete {
			if errno := c.unlink(parent, filepath.Base(ch.Path)); errno != 0 {
				t.Fatalf("unlink %s errno %d", ch.Path, errno)
			}
		} else {
			c.mustCreate(parent, filepath.Base(tmp), string(ch.Data))
			if errno := c.rename(parent, filepath.Base(tmp), parent, filepath.Base(ch.Path)); errno != 0 {
				t.Fatalf("rename onto %s errno %d", ch.Path, errno)
			}
		}
		h.srv.allow.remove(paths)
	}
	if n, _ := h.ov.DirtyCount(ctx); n == 0 {
		t.Fatal("write-through left no overlay entries before the switch")
	}

	// The generation switch, as onHEADChanged does it.
	err = h.res.Transition(func() error {
		if err := h.ov.ReconcileChecked(ctx, func(p string) (model.BaseNode, bool, error) {
			return h.snap.LookupNode(ctx, gen2, p)
		}); err != nil {
			return err
		}
		h.res.SetGeneration(gen2)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := h.ov.DirtyCount(ctx); n != 0 {
		entries, _ := h.ov.ListAll(ctx)
		t.Fatalf("overlay still has %d entries after reconcile: %+v", n, entries)
	}
	if got := c.readAll(c.mustLookup(inoRoot, "README.md").Ino); got != "# merged\n" {
		t.Fatalf("README.md = %q", got)
	}
	if got := c.readAll(oldReadme.Ino); got != h.files["README.md"] {
		t.Fatalf("replaced README.md through its old number = %q", got)
	}
	if _, errno := c.lookup(c.mustLookup(inoRoot, "src").Ino, "util.go"); errno != errENOENT {
		t.Fatalf("src/util.go errno = %d, want ENOENT", errno)
	}
	notes := c.mustLookup(inoRoot, "notes")
	if got := c.readAll(c.mustLookup(notes.Ino, "new.md").Ino); got != "new note\n" {
		t.Fatalf("notes/new.md = %q", got)
	}
	// Local edits are not replayed: a file with an overlay entry is skipped.
	h.srv.allow.add([]string{"local.txt"})
	c.mustCreate(inoRoot, "local.txt", "mine")
	h.srv.allow.remove([]string{"local.txt"})
	skipped, err := ServerChanges(ctx, []snapshot.GenerationChange{{Path: "local.txt", New: &model.BaseNode{Path: "local.txt", Type: "file", ObjectOID: "x"}}}, h.ov, h.h, h.repo)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("a local edit was replayed: %+v %v", skipped, err)
	}
}
