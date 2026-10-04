package fskit

import (
	"context"
	"testing"
	"time"
)

// A listing or stat whose sizes the snapshot lacks asks the remote
// (object-info) and downloads nothing; what it learns is saved.
func TestUnknownSizesComeFromTheRemoteNotDownloads(t *testing.T) {
	h := newHarness(t, harnessOptions{sizesUnknown: true, remoteSizes: true, objectInfo: true})
	c := h.client()

	many := c.mustLookup(inoRoot, "many")
	entries, _ := c.listAll(many.Ino, 16, true)
	if len(entries) != 40 {
		t.Fatalf("many/ lists %d entries, want 40", len(entries))
	}
	for _, e := range entries {
		if want := uint64(len(h.files["many/"+e.Name])); e.Attr == nil || e.Attr.Size != want {
			t.Fatalf("many/%s size = %+v, want %d", e.Name, e.Attr, want)
		}
	}
	readme := c.mustLookup(inoRoot, "README.md")
	if readme.Size != uint64(len(h.files["README.md"])) {
		t.Fatalf("README.md size = %d, want %d", readme.Size, len(h.files["README.md"]))
	}
	link := c.mustLookup(inoRoot, "link")
	if link.Size != uint64(len("README.md")) {
		t.Fatalf("link size = %d, want %d", link.Size, len("README.md"))
	}
	if n := h.downloaded(); n != 0 {
		t.Fatalf("listing and stat downloaded %d files, want 0", n)
	}

	n, ok, err := h.snap.LookupNode(context.Background(), h.gen, "many/f007.txt")
	if err != nil || !ok || n.SizeState != "known" || n.SizeBytes != int64(len(h.files["many/f007.txt"])) {
		t.Fatalf("many/f007.txt in the snapshot = %+v (ok %v, err %v), want its size saved", n, ok, err)
	}

	// Reading is what downloads.
	if got := c.readAll(readme.Ino); got != h.files["README.md"] || h.downloaded() != 1 {
		t.Fatalf("read README.md = %q with %d downloads, want its content and 1", got, h.downloaded())
	}
}

// When the remote cannot answer sizes (no object-info), the mount downloads
// the files it lists to learn their sizes: a wrong size would be cached by
// the kernel and cut reads short.
func TestUnknownSizesWithoutObjectInfoAreDownloaded(t *testing.T) {
	h := newHarness(t, harnessOptions{sizesUnknown: true, remoteSizes: true})
	c := h.client()
	entries, _ := c.listAll(c.mustLookup(inoRoot, "many").Ino, 16, true)
	for _, e := range entries {
		if want := uint64(len(h.files["many/"+e.Name])); e.Attr == nil || e.Attr.Size != want {
			t.Fatalf("many/%s size = %+v, want %d", e.Name, e.Attr, want)
		}
	}
	if n := h.downloaded(); n != 40 {
		t.Fatalf("downloaded %d files, want the 40 listed", n)
	}
}

// After a mount, the sizes the snapshot lacks are filled in the background
// from the remote, and saved: a listing then finds them known, and nothing
// is downloaded.
func TestUnknownSizesFillInTheBackground(t *testing.T) {
	h := newHarness(t, harnessOptions{sizesUnknown: true, remoteSizes: true, objectInfo: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unknown, err := h.snap.UnknownSizeOIDs(ctx, h.gen)
	if err != nil || len(unknown) < 40 {
		t.Fatalf("unknown sizes before the fill = %d (err %v), want every blob", len(unknown), err)
	}
	go h.sizes.Run(ctx)

	deadline := time.Now().Add(20 * time.Second)
	for {
		unknown, err = h.snap.UnknownSizeOIDs(ctx, h.gen)
		if err != nil {
			t.Fatal(err)
		}
		if len(unknown) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sizes still unknown after 20 s", len(unknown))
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, ok, err := h.snap.LookupNode(ctx, h.gen, "many/f007.txt")
	if err != nil || !ok || n.SizeBytes != int64(len(h.files["many/f007.txt"])) {
		t.Fatalf("many/f007.txt = %+v (ok %v, err %v), want its size", n, ok, err)
	}
	c := h.client()
	entries, _ := c.listAll(c.mustLookup(inoRoot, "many").Ino, 16, true)
	for _, e := range entries {
		if want := uint64(len(h.files["many/"+e.Name])); e.Attr == nil || e.Attr.Size != want {
			t.Fatalf("many/%s size = %+v, want %d", e.Name, e.Attr, want)
		}
	}
	if d := h.downloaded(); d != 0 {
		t.Fatalf("downloaded %d files, want 0", d)
	}
}
