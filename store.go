package indexer

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// store is the SQLite-backed record of indexed files. It is the ops-query
// source of truth for which files exist, with what size/mtime, and when they
// were indexed. Records are only written AFTER the corresponding index batch
// commit, so a record always implies committed postings.
type store struct {
	db *sql.DB
}

type fileRecord struct {
	Path      string
	Size      int64
	Mtime     int64
	IndexedAt int64
}

func openStore(dbPath string) (*store, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	stmts := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`CREATE TABLE IF NOT EXISTS files (
			path TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			mtime INTEGER NOT NULL,
			indexed_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("init store: %w", err)
		}
	}
	return &store{db: db}, nil
}

func (s *store) all() ([]fileRecord, error) {
	rows, err := s.db.Query(`SELECT path, size, mtime, indexed_at FROM files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fileRecord
	for rows.Next() {
		var r fileRecord
		if err := rows.Scan(&r.Path, &r.Size, &r.Mtime, &r.IndexedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// update atomically upserts recs, deletes paths, and records scanStart as the
// last scan start. Called once per committed index batch.
func (s *store) update(recs []fileRecord, deleted []string, scanStart int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	up, err := tx.Prepare(`INSERT INTO files (path, size, mtime, indexed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, indexed_at=excluded.indexed_at`)
	if err != nil {
		return err
	}
	defer up.Close()
	for _, r := range recs {
		if _, err := up.Exec(r.Path, r.Size, r.Mtime, r.IndexedAt); err != nil {
			return err
		}
	}
	del, err := tx.Prepare(`DELETE FROM files WHERE path = ?`)
	if err != nil {
		return err
	}
	defer del.Close()
	for _, p := range deleted {
		if _, err := del.Exec(p); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('last_scan_start', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(scanStart)); err != nil {
		return err
	}
	return tx.Commit()
}

// lastScanStart returns the recorded start time of the previous scan, or 0.
func (s *store) lastScanStart() (int64, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'last_scan_start'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int64
	if _, err := fmt.Sscan(v, &n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *store) close() error { return s.db.Close() }
