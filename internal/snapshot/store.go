package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS repo_state (
	  key TEXT PRIMARY KEY,
	  value TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS base_nodes (
	  generation INTEGER NOT NULL,
	  path TEXT NOT NULL,
	  parent_path TEXT NOT NULL DEFAULT '',
	  type TEXT NOT NULL,
	  mode INTEGER NOT NULL,
	  object_oid TEXT NOT NULL,
	  size_state TEXT NOT NULL,
	  size_bytes INTEGER NOT NULL DEFAULT 0,
	  PRIMARY KEY (generation, path)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_base_nodes_gen_parent ON base_nodes(generation, parent_path);`,
	`CREATE INDEX IF NOT EXISTS idx_base_nodes_gen_oid ON base_nodes(generation, object_oid);`,
	`DROP TABLE IF EXISTS learned_path_stats;`,
	`DROP TABLE IF EXISTS blob_cache_index;`,
}

type Store struct {
	db *sql.DB
}

type stateQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func New(ctx context.Context, path string) (*Store, error) {
	db, err := meta.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if err := meta.ExecMigrations(ctx, db, migrations); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) PublishGeneration(ctx context.Context, headOID string, ref string, nodes []model.BaseNode) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	gen, err := s.nextGenerationTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM base_nodes WHERE generation=?`, gen); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO base_nodes(generation, path, parent_path, type, mode, object_oid, size_state, size_bytes) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, n := range nodes {
		parentPath := parentDir(n.Path)
		if _, err := stmt.ExecContext(ctx, gen, n.Path, parentPath, n.Type, n.Mode, n.ObjectOID, n.SizeState, n.SizeBytes); err != nil {
			return 0, err
		}
	}
	if err := upsertState(ctx, tx, "current_generation", fmt.Sprintf("%d", gen)); err != nil {
		return 0, err
	}
	if err := upsertState(ctx, tx, "head_oid", headOID); err != nil {
		return 0, err
	}
	if err := upsertState(ctx, tx, "head_ref", ref); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return gen, nil
}

func (s *Store) PruneGenerations(ctx context.Context, keepFrom int64) error {
	if keepFrom <= 1 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM base_nodes WHERE generation < ?`, keepFrom)
	return err
}

func (s *Store) CurrentGeneration(ctx context.Context) (int64, error) {
	gen, ok, err := stateInt64(ctx, s.db, "current_generation")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	return gen, nil
}

// ReadState returns persisted HEAD OID, ref, and generation from the snapshot
// database. Used by status commands that run outside the daemon process.
func (s *Store) ReadState(ctx context.Context) (headOID, headRef string, generation int64, err error) {
	gen, err := s.CurrentGeneration(ctx)
	if err != nil {
		return "", "", 0, err
	}
	headOID, _, _ = stateValue(ctx, s.db, "head_oid")
	headRef, _, _ = stateValue(ctx, s.db, "head_ref")
	return headOID, headRef, gen, nil
}

func (s *Store) UpdateHEADRef(ctx context.Context, ref string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repo_state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "head_ref", ref)
	return err
}

func (s *Store) GetNode(generation int64, path string) (model.BaseNode, bool) {
	n, ok, _ := s.LookupNode(context.Background(), generation, path)
	return n, ok
}

func (s *Store) LookupNode(ctx context.Context, generation int64, path string) (model.BaseNode, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT path, type, mode, object_oid, size_state, size_bytes FROM base_nodes WHERE generation=? AND path=?`, generation, path)
	var n model.BaseNode
	if err := row.Scan(&n.Path, &n.Type, &n.Mode, &n.ObjectOID, &n.SizeState, &n.SizeBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.BaseNode{}, false, nil
		}
		return model.BaseNode{}, false, err
	}
	return n, true, nil
}

// ListChildren returns direct children of parentPath using a path-based lookup
// (no inode join, no collision risk).
func (s *Store) ListChildren(generation int64, parentPath string) ([]model.BaseNode, error) {
	pp := model.CleanPath(parentPath)
	rows, err := s.db.Query(`SELECT path, type, mode, object_oid, size_state, size_bytes FROM base_nodes WHERE generation=? AND parent_path=? ORDER BY path`, generation, pp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.BaseNode
	for rows.Next() {
		var n model.BaseNode
		if err := rows.Scan(&n.Path, &n.Type, &n.Mode, &n.ObjectOID, &n.SizeState, &n.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GenerationChange is one path that differs between two generations. Old is
// nil for an added path and New is nil for a removed one.
type GenerationChange struct {
	Path string
	Old  *model.BaseNode
	New  *model.BaseNode
}

// DiffGenerations lists the paths whose type, mode or object differ between
// two generations, sorted by path. Front ends that must replay a server-side
// change (the FSKit write-through) use it.
func (s *Store) DiffGenerations(ctx context.Context, oldGen, newGen int64) ([]GenerationChange, error) {
	const cols = `n.path, n.type, n.mode, n.object_oid, n.size_state, n.size_bytes, o.type, o.mode, o.object_oid, o.size_state, o.size_bytes`
	var out []GenerationChange
	scan := func(rows *sql.Rows, removed bool) error {
		defer rows.Close()
		for rows.Next() {
			var a model.BaseNode
			var bType, bOID, bState sql.NullString
			var bMode, bSize sql.NullInt64
			if err := rows.Scan(&a.Path, &a.Type, &a.Mode, &a.ObjectOID, &a.SizeState, &a.SizeBytes, &bType, &bMode, &bOID, &bState, &bSize); err != nil {
				return err
			}
			c := GenerationChange{Path: a.Path}
			var b *model.BaseNode
			if bType.Valid {
				b = &model.BaseNode{Path: a.Path, Type: bType.String, Mode: uint32(bMode.Int64), ObjectOID: bOID.String, SizeState: bState.String, SizeBytes: bSize.Int64}
			}
			if removed {
				c.Old = &a
			} else {
				c.New = &a
				c.Old = b
			}
			out = append(out, c)
		}
		return rows.Err()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM base_nodes n LEFT JOIN base_nodes o ON o.generation=? AND o.path=n.path
		WHERE n.generation=? AND (o.path IS NULL OR o.type<>n.type OR o.mode<>n.mode OR o.object_oid<>n.object_oid)`, oldGen, newGen)
	if err != nil {
		return nil, err
	}
	if err := scan(rows, false); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT `+cols+` FROM base_nodes n LEFT JOIN base_nodes o ON o.generation=? AND o.path=n.path
		WHERE n.generation=? AND o.path IS NULL`, newGen, oldGen)
	if err != nil {
		return nil, err
	}
	if err := scan(rows, true); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// UpdateSize sets the size for a blob after hydration. Updates all rows with
// the given OID in the current generation so stat() returns the correct size
// without waiting for a full re-index.
func (s *Store) UpdateSize(generation int64, objectOID string, size int64) {
	_ = s.UpdateSizes(context.Background(), generation, map[string]int64{objectOID: size})
}

func (s *Store) UpdateSizes(ctx context.Context, generation int64, sizes map[string]int64) error {
	if len(sizes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE base_nodes SET size_bytes=?, size_state='known' WHERE generation=? AND object_oid=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for oid, size := range sizes {
		if _, err := stmt.ExecContext(ctx, size, generation, oid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// KnownSizes is every blob size the generation knows, by object id, so a new
// generation need not ask the remote again for blobs it shares.
func (s *Store) KnownSizes(ctx context.Context, generation int64) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT object_oid, size_bytes FROM base_nodes WHERE generation=? AND size_state='known' AND object_oid!='' AND type IN ('file','symlink')`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var oid string
		var size int64
		if err := rows.Scan(&oid, &size); err != nil {
			return nil, err
		}
		out[oid] = size
	}
	return out, rows.Err()
}

// UnknownSizeOIDs is the blob ids whose size the generation still lacks.
func (s *Store) UnknownSizeOIDs(ctx context.Context, generation int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT object_oid FROM base_nodes WHERE generation=? AND size_state!='known' AND object_oid!='' AND type IN ('file','symlink')`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var oid string
		if err := rows.Scan(&oid); err != nil {
			return nil, err
		}
		out = append(out, oid)
	}
	return out, rows.Err()
}

func (s *Store) nextGenerationTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	gen, ok, err := stateInt64(ctx, tx, "current_generation")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 1, nil
	}
	return gen + 1, nil
}

func stateValue(ctx context.Context, q stateQueryer, key string) (string, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT value FROM repo_state WHERE key=?`, key)
	var val string
	if err := row.Scan(&val); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return val, true, nil
}

func stateInt64(ctx context.Context, q stateQueryer, key string) (int64, bool, error) {
	val, ok, err := stateValue(ctx, q, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	var parsed int64
	if _, err := fmt.Sscanf(val, "%d", &parsed); err != nil {
		return 0, true, err
	}
	return parsed, true, nil
}

func upsertState(ctx context.Context, tx *sql.Tx, key string, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO repo_state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// parentDir returns the parent path for a given path, using "." as the root parent.
func parentDir(path string) string {
	if path == "." || path == "/" || path == "" {
		return ""
	}
	d := filepath.Dir(path)
	if d == "/" {
		return "."
	}
	return d
}
