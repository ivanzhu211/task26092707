package indexer

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// FileRecord is the operational record of one indexed file, stored in SQLite.
type FileRecord struct {
	Path      string
	Size      int64
	Mtime     int64
	IndexedAt int64
}

type fileStore struct {
	db *sql.DB
}

func openFileStore(path string) (*fileStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Single connection keeps the embedded database free of locking surprises.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS files (
		path TEXT PRIMARY KEY,
		size INTEGER NOT NULL,
		mtime INTEGER NOT NULL,
		indexed_at INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &fileStore{db: db}, nil
}

// reconcile makes the SQLite table exactly mirror docs (the index's committed
// view). It only touches rows that differ; content is never read here.
func (s *fileStore) reconcile(docs map[string]docEntry) error {
	existing := map[string]FileRecord{}
	rows, err := s.db.Query(`SELECT path, size, mtime, indexed_at FROM files`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r FileRecord
		if err := rows.Scan(&r.Path, &r.Size, &r.Mtime, &r.IndexedAt); err != nil {
			rows.Close()
			return err
		}
		existing[r.Path] = r
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for path := range existing {
		if _, ok := docs[path]; !ok {
			if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, path); err != nil {
				return err
			}
		}
	}
	stmt, err := tx.Prepare(`INSERT INTO files (path, size, mtime, indexed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, indexed_at=excluded.indexed_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for path, d := range docs {
		if r, ok := existing[path]; ok && r.Size == d.Size && r.Mtime == d.Mtime && r.IndexedAt == d.IndexedAt {
			continue
		}
		if _, err := stmt.Exec(path, d.Size, d.Mtime, d.IndexedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *fileStore) list() ([]FileRecord, error) {
	rows, err := s.db.Query(`SELECT path, size, mtime, indexed_at FROM files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileRecord
	for rows.Next() {
		var r FileRecord
		if err := rows.Scan(&r.Path, &r.Size, &r.Mtime, &r.IndexedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *fileStore) close() error { return s.db.Close() }
