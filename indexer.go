package indexer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileRecord is the per-file record kept in SQLite.
type FileRecord struct {
	Path      string
	Size      int64
	Mtime     int64
	IndexedAt int64
}

// mtimeSafetyWindow is how far back from the previous scan start a file's
// recorded mtime may lie before we trust it blindly. Files whose mtime falls
// inside the window are re-read, because a same-second, same-length rewrite
// could have happened after they were last read without changing mtime.
const mtimeSafetyWindow = 2

// Indexer incrementally indexes .txt files under sourceDir.
type Indexer struct {
	mu        sync.Mutex
	sourceDir string
	indexDir  string
	dbPath    string
	index     *invertedIndex
	store     *store

	// batchSize caps how many changed files one index commit may cover.
	batchSize int
}

// Open opens (or creates) the indexer state. It never discards the index:
// committed segments are replayed, orphaned partial writes are cleaned up.
func Open(sourceDir, indexDir, dbPath string) (*Indexer, error) {
	idx, err := openIndex(indexDir)
	if err != nil {
		return nil, err
	}
	st, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}
	return &Indexer{
		sourceDir: sourceDir,
		indexDir:  indexDir,
		dbPath:    dbPath,
		index:     idx,
		store:     st,
		batchSize: 100,
	}, nil
}

type scannedFile struct {
	path  string // slash-separated relative path
	abs   string
	size  int64
	mtime int64
}

// Reindex incrementally applies adds, modifications and deletions to both
// stores. Only one Reindex may run at a time.
func (ix *Indexer) Reindex() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	now := time.Now().Unix()
	prevScanStart, err := ix.store.lastScanStart()
	if err != nil {
		return err
	}

	onDisk := map[string]scannedFile{}
	root := ix.sourceDir
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".txt") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		onDisk[rel] = scannedFile{path: rel, abs: path, size: info.Size(), mtime: info.ModTime().Unix()}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	records, err := ix.store.all()
	if err != nil {
		return err
	}
	recorded := map[string]fileRecord{}
	for _, r := range records {
		recorded[r.Path] = r
	}

	var toRead []scannedFile
	for path, f := range onDisk {
		rec, ok := recorded[path]
		switch {
		case !ok:
			toRead = append(toRead, f) // new file
		case rec.Mtime != f.mtime:
			toRead = append(toRead, f) // mtime changed
		case rec.Mtime >= prevScanStart-mtimeSafetyWindow:
			// mtime too close to the previous scan: a same-second rewrite
			// after the last read cannot be ruled out.
			toRead = append(toRead, f)
		}
	}
	var toDelete []string
	for path := range recorded {
		if _, ok := onDisk[path]; !ok {
			toDelete = append(toDelete, path)
		}
	}
	sort.Slice(toRead, func(i, j int) bool { return toRead[i].path < toRead[j].path })
	sort.Strings(toDelete)

	b := newBatch()
	var recs []fileRecord
	var deleted []string
	flush := func() error {
		if err := ix.index.commit(b); err != nil {
			return err
		}
		if err := ix.store.update(recs, deleted, now); err != nil {
			return err
		}
		crashPoint("postStore")
		b = newBatch()
		recs = nil
		deleted = nil
		return nil
	}

	for _, f := range toRead {
		if readHook != nil {
			readHook(f.abs)
		}
		content, err := os.ReadFile(f.abs)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.path, err)
		}
		terms := dedupeTerms(tokenize(content))
		b.add(f.path, terms)
		recs = append(recs, fileRecord{Path: f.path, Size: f.size, Mtime: f.mtime, IndexedAt: now})
		crashPoint("fileProcessed")
		if b.size() >= ix.batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	for _, p := range toDelete {
		b.del(p)
		deleted = append(deleted, p)
		if b.size() >= ix.batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	// Final flush commits the remaining batch and always advances
	// last_scan_start, even when nothing changed.
	return flush()
}

func dedupeTerms(terms []string) []string {
	sort.Strings(terms)
	out := terms[:0]
	for i, t := range terms {
		if i == 0 || t != terms[i-1] {
			out = append(out, t)
		}
	}
	return out
}

// Search returns the sorted paths of files whose current content contains
// term. It never runs concurrently with Reindex.
func (ix *Indexer) Search(term string) []string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	terms := tokenize([]byte(term))
	if len(terms) != 1 {
		return nil
	}
	return ix.index.search(terms[0])
}

// Files returns the file records stored in SQLite.
func (ix *Indexer) Files() []FileRecord {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	records, err := ix.store.all()
	if err != nil {
		return nil
	}
	out := make([]FileRecord, 0, len(records))
	for _, r := range records {
		out = append(out, FileRecord(r))
	}
	return out
}

// Close releases resources.
func (ix *Indexer) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.store != nil {
		return ix.store.close()
	}
	return nil
}
