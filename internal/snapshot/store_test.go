package snapshot

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(context.Background(), filepath.Join(dir, "snap.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPublishAndGetNode(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "abc123", SizeState: "known", SizeBytes: 42},
		{RepoID: "r", Path: "src", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "src/main.go", Type: "file", Mode: 0o644, ObjectOID: "def456", SizeState: "known", SizeBytes: 100},
	}
	gen, err := s.PublishGeneration(ctx, "head1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected gen 1, got %d", gen)
	}

	n, ok := s.GetNode(gen, "README.md")
	if !ok {
		t.Fatal("expected README.md")
	}
	if n.SizeBytes != 42 || n.ObjectOID != "abc123" {
		t.Fatalf("wrong node: %+v", n)
	}

	_, ok = s.GetNode(gen, "nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestListChildren(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "a1", SizeState: "known", SizeBytes: 1},
		{RepoID: "r", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "b1", SizeState: "known", SizeBytes: 2},
		{RepoID: "r", Path: "sub", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "sub/c.txt", Type: "file", Mode: 0o644, ObjectOID: "c1", SizeState: "known", SizeBytes: 3},
	}
	gen, err := s.PublishGeneration(ctx, "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}

	// Root children
	children, err := s.ListChildren(gen, ".")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range children {
		names[c.Path] = true
	}
	if !names["a.txt"] || !names["b.txt"] || !names["sub"] {
		t.Fatalf("expected a.txt, b.txt, sub in root children, got %v", names)
	}
	if names["sub/c.txt"] {
		t.Fatal("sub/c.txt should not be a root child")
	}

	// Sub children
	children, err = s.ListChildren(gen, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Path != "sub/c.txt" {
		t.Fatalf("expected [sub/c.txt], got %v", children)
	}
}

func TestGenerationCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "f.txt", Type: "file", Mode: 0o644, ObjectOID: "x", SizeState: "known"},
	}
	g1, _ := s.PublishGeneration(ctx, "h1", "main", nodes)
	g2, _ := s.PublishGeneration(ctx, "h2", "main", nodes)
	g3, _ := s.PublishGeneration(ctx, "h3", "main", nodes)

	if g1 != 1 || g2 != 2 || g3 != 3 {
		t.Fatalf("unexpected generations: %d %d %d", g1, g2, g3)
	}

	if err := s.PruneGenerations(ctx, g2); err != nil {
		t.Fatal(err)
	}
	// Generation 1 should be cleaned up once the caller publishes the view.
	_, ok := s.GetNode(1, "f.txt")
	if ok {
		t.Fatal("gen 1 should be cleaned up")
	}
	// Generation 2 should still exist (gen-1)
	_, ok = s.GetNode(2, "f.txt")
	if !ok {
		t.Fatal("gen 2 should still exist")
	}
}

func TestCurrentGeneration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	gen, err := s.CurrentGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 0 {
		t.Fatalf("expected 0 for empty store, got %d", gen)
	}

	nodes := []model.BaseNode{{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}}
	s.PublishGeneration(ctx, "h1", "main", nodes)

	gen, err = s.CurrentGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected 1, got %d", gen)
	}
}

func TestUpdateSizeUsesGenerationObjectOIDIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "shared", SizeState: "unknown"},
		{RepoID: "r", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "other", SizeState: "unknown"},
	}
	gen, err := s.PublishGeneration(ctx, "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}

	var id, parent, notUsed int
	var detail string
	err = s.db.QueryRowContext(ctx, `EXPLAIN QUERY PLAN UPDATE base_nodes SET size_bytes=?, size_state='known' WHERE generation=? AND object_oid=?`, 42, gen, "shared").Scan(&id, &parent, &notUsed, &detail)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "idx_base_nodes_gen_oid") {
		t.Fatalf("UpdateSize plan = %q, want idx_base_nodes_gen_oid", detail)
	}

	if err := s.UpdateSizes(ctx, gen, map[string]int64{"shared": 42, "other": 7}); err != nil {
		t.Fatal(err)
	}
	n, ok := s.GetNode(gen, "a.txt")
	if !ok {
		t.Fatal("a.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 42 {
		t.Fatalf("a.txt size = %s/%d, want known/42", n.SizeState, n.SizeBytes)
	}
	n, ok = s.GetNode(gen, "b.txt")
	if !ok {
		t.Fatal("b.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 7 {
		t.Fatalf("b.txt size = %s/%d, want known/7", n.SizeState, n.SizeBytes)
	}
}

func TestNewDropsLegacyTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.sqlite")
	db, err := meta.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE learned_path_stats (
			path TEXT PRIMARY KEY,
			access_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE blob_cache_index (
			object_oid TEXT PRIMARY KEY,
			cache_path TEXT NOT NULL
		);
	`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	assertTableMissing := func(name string) {
		t.Helper()
		row := s.db.QueryRowContext(context.Background(), `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name)
		var got string
		if err := row.Scan(&got); err == nil {
			t.Fatalf("table %q should be dropped", name)
		} else if err != sql.ErrNoRows {
			t.Fatalf("lookup table %q: %v", name, err)
		}
	}

	assertTableMissing("learned_path_stats")
	assertTableMissing("blob_cache_index")
}

func TestDiffGenerations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	g1, err := s.PublishGeneration(ctx, "h1", "main", []model.BaseNode{
		{Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{Path: "same.txt", Type: "file", Mode: 0o100644, ObjectOID: "aaa", SizeState: "known"},
		{Path: "edited.txt", Type: "file", Mode: 0o100644, ObjectOID: "bbb", SizeState: "known"},
		{Path: "chmod.sh", Type: "file", Mode: 0o100644, ObjectOID: "ccc", SizeState: "known"},
		{Path: "gone", Type: "dir", Mode: 0o40000, ObjectOID: "t1", SizeState: "known"},
		{Path: "gone/x.txt", Type: "file", Mode: 0o100644, ObjectOID: "ddd", SizeState: "known"},
	})
	if err != nil {
		t.Fatal(err)
	}
	g2, err := s.PublishGeneration(ctx, "h2", "main", []model.BaseNode{
		{Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{Path: "same.txt", Type: "file", Mode: 0o100644, ObjectOID: "aaa", SizeState: "known"},
		{Path: "edited.txt", Type: "file", Mode: 0o100644, ObjectOID: "bbb2", SizeState: "known"},
		{Path: "chmod.sh", Type: "file", Mode: 0o100755, ObjectOID: "ccc", SizeState: "known"},
		{Path: "added.md", Type: "file", Mode: 0o100644, ObjectOID: "eee", SizeState: "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := s.DiffGenerations(ctx, g1, g2)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range diff {
		kind := "changed"
		switch {
		case c.Old == nil:
			kind = "added"
		case c.New == nil:
			kind = "removed"
		}
		got = append(got, kind+" "+c.Path)
	}
	want := []string{"added added.md", "changed chmod.sh", "changed edited.txt", "removed gone", "removed gone/x.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("diff = %v, want %v", got, want)
	}
	if diff[2].Old.ObjectOID != "bbb" || diff[2].New.ObjectOID != "bbb2" {
		t.Fatalf("edited.txt change = %+v %+v", diff[2].Old, diff[2].New)
	}
}

// A folder listing must use the (generation, parent_path) index, not walk
// the whole generation: with ORDER BY path SQLite chose the primary key, and
// every listing of a 50,000-entry snapshot took 8 ms (87 ms under load).
// Children still come back sorted by path.
func TestListChildrenUsesParentIndexAndSorts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	nodes := []model.BaseNode{{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}}
	for _, d := range []string{"z", "a", "m"} {
		nodes = append(nodes, model.BaseNode{RepoID: "r", Path: d, Type: "dir", Mode: 0o755, SizeState: "known"})
		for _, f := range []string{"c.txt", "a.txt", "b.txt"} {
			nodes = append(nodes, model.BaseNode{RepoID: "r", Path: d + "/" + f, Type: "file", Mode: 0o644, ObjectOID: d + f, SizeState: "known", SizeBytes: 1})
		}
	}
	gen, err := s.PublishGeneration(ctx, "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+listChildrenSQL, gen, "m")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	rows.Close()
	if !strings.Contains(strings.Join(plan, "; "), "idx_base_nodes_gen_parent (generation=? AND parent_path=?)") {
		t.Fatalf("ListChildren plan = %q, want the (generation, parent_path) index", plan)
	}
	for _, dir := range []string{".", "m"} {
		children, err := s.ListChildren(gen, dir)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(children); i++ {
			if children[i-1].Path >= children[i].Path {
				t.Fatalf("children of %s not sorted: %v", dir, children)
			}
		}
	}
}
