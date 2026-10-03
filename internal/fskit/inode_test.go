package fskit

import "testing"

func TestInodeTableMoveRetiresStaleMappings(t *testing.T) {
	tb := newInodeTable()
	a := tb.assign("a", kindDir, true)
	ax := tb.assign("a/x", kindFile, true)
	// "b" and "b/y" vanished in a snapshot change but are still mapped.
	b := tb.assign("b", kindDir, true)
	by := tb.assign("b/y", kindFile, false)

	tb.move("a", "b")

	if v, _ := tb.lookupPath("b"); v.ino != a {
		t.Fatalf("b = ino %d, want %d", v.ino, a)
	}
	if v, _ := tb.lookupPath("b/x"); v.ino != ax {
		t.Fatalf("b/x = ino %d, want %d", v.ino, ax)
	}
	if _, ok := tb.lookupPath("a"); ok {
		t.Fatal("a still mapped")
	}
	old, ok := tb.get(b)
	if !ok || old.path != "" || old.removed == nil {
		t.Fatalf("the replaced b is %+v, want a removed item", old)
	}
	if _, ok := tb.get(by); ok {
		t.Fatal("an unheld stale item was not freed")
	}
	if ino := tb.assign("b/y", kindFile, false); ino == by {
		t.Fatal("a retired number was reused")
	}
}

func TestInodeTableNumbers(t *testing.T) {
	tb := newInodeTable()
	if v, ok := tb.lookupPath("."); !ok || v.ino != inoRoot {
		t.Fatalf("root = %+v", v)
	}
	first := tb.assign("f", kindFile, false)
	if first != firstIno {
		t.Fatalf("first number %d, want %d", first, firstIno)
	}
	if again := tb.assign("f", kindFile, true); again != first {
		t.Fatalf("same path got %d then %d", first, again)
	}
	tb.forget(first)
	if again := tb.assign("f", kindFile, false); again != first {
		t.Fatal("forget of a live item changed its number")
	}
	tb.forget(inoRoot)
	if _, ok := tb.get(inoRoot); !ok {
		t.Fatal("forget removed the root")
	}
}
