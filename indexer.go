package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// batchSize is the maximum number of changed files per index commit.
const batchSize = 100

// recentWindowSec: a file whose mtime is within this many seconds of the
// previous scan's start may have been silently rewritten (same-second,
// equal-length, mtime restored) and must be re-read.
const recentWindowSec = 2

// Test hooks. They are nil in production; tests use them to simulate crashes
// (by panicking) at storage boundaries and to observe file reads.
var (
	// crashHook is invoked at named points between storage writes.
	crashHook func(point string)
	// readHook is invoked after a file's content has been read during a scan.
	readHook func(path string)
)

const (
	pointBeforeCommit = "beforeCommit"
	pointAfterCommit  = "afterCommit"
	pointBeforeSync   = "beforeDBSync"
	pointAfterSync    = "afterDBSync"
)

// Indexer incrementally indexes a source directory of .txt files.
type Indexer struct {
	sourceDir string
	idx       *memIndex
	store     *fileStore
	mu        sync.Mutex
	closed    bool
}

// Open opens (or creates) the indexer state. It recovers from any prior
// crash using only on-disk state: committed index segments are replayed,
// uncommitted ones discarded; SQLite is left as-is and reconciled by the
// next Reindex.
func Open(sourceDir, indexDir, dbPath string) (*Indexer, error) {
	idx, err := loadIndex(indexDir)
	if err != nil {
		return nil, err
	}
	store, err := openFileStore(dbPath)
	if err != nil {
		return nil, err
	}
	return &Indexer{sourceDir: sourceDir, idx: idx, store: store}, nil
}

type diskFile struct {
	size  int64
	mtime int64
}

type change struct {
	path string
	add  bool // false = remove
}

// Reindex incrementally scans the source dir and reflects additions,
// modifications and deletions into both the index and SQLite.
func (ix *Indexer) Reindex() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	scanStart := time.Now().Unix()
	prevScanStart := ix.idx.scanStart

	onDisk, err := ix.scanDir()
	if err != nil {
		return err
	}

	var changes []change
	for path, df := range onDisk {
		entry, ok := ix.idx.docs[path]
		switch {
		case !ok:
			changes = append(changes, change{path, true})
		case entry.Size != df.size || entry.Mtime != df.mtime:
			changes = append(changes, change{path, true})
		case df.mtime >= prevScanStart-recentWindowSec:
			// Untouched on paper, but too recent to trust the 1s mtime.
			changes = append(changes, change{path, true})
		}
	}
	for path := range ix.idx.docs {
		if _, ok := onDisk[path]; !ok {
			changes = append(changes, change{path, false})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })

	for start := 0; start < len(changes); start += batchSize {
		end := start + batchSize
		if end > len(changes) {
			end = len(changes)
		}
		seg := ix.buildSegment(changes[start:end])
		if crashHook != nil {
			crashHook(pointBeforeCommit)
		}
		if err := ix.idx.commit(seg, scanStart); err != nil {
			return err
		}
		if crashHook != nil {
			crashHook(pointAfterCommit)
		}
	}

	if crashHook != nil {
		crashHook(pointBeforeSync)
	}
	// The index's committed doc table is the source of truth; make SQLite
	// mirror it exactly. This also repairs any divergence left by a crash.
	if err := ix.store.reconcile(ix.idx.docs); err != nil {
		return err
	}
	if crashHook != nil {
		crashHook(pointAfterSync)
	}
	return nil
}

// scanDir lists all .txt files under the source dir with size and
// second-resolution mtime.
func (ix *Indexer) scanDir() (map[string]diskFile, error) {
	out := map[string]diskFile{}
	root := ix.sourceDir
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		out[filepath.ToSlash(rel)] = diskFile{size: info.Size(), mtime: info.ModTime().Unix()}
		return nil
	})
	return out, err
}

// buildSegment reads the content of added files and assembles one segment.
func (ix *Indexer) buildSegment(changes []change) *segment {
	seg := &segment{Adds: map[string]docEntry{}}
	for _, c := range changes {
		if !c.add {
			seg.Removes = append(seg.Removes, c.path)
			continue
		}
		full := filepath.Join(ix.sourceDir, filepath.FromSlash(c.path))
		content, err := os.ReadFile(full)
		if err != nil {
			// Vanished or unreadable between walk and read: drop it.
			seg.Removes = append(seg.Removes, c.path)
			continue
		}
		if readHook != nil {
			readHook(c.path)
		}
		info, err := os.Stat(full)
		if err != nil || !utf8.Valid(content) {
			seg.Removes = append(seg.Removes, c.path)
			continue
		}
		seg.Adds[c.path] = docEntry{
			Size:      info.Size(),
			Mtime:     info.ModTime().Unix(),
			IndexedAt: time.Now().Unix(),
			Terms:     dedup(tokenize(string(content))),
		}
	}
	return seg
}

func dedup(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	out := terms[:0]
	for _, t := range terms {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// Search returns the sorted paths of files whose current content contains
// every token of term (a single token in the typical case).
func (ix *Indexer) Search(term string) []string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	tokens := dedup(tokenize(term))
	if len(tokens) == 0 {
		return nil
	}
	result := ix.idx.search(tokens[0])
	for _, t := range tokens[1:] {
		result = intersect(result, ix.idx.search(t))
	}
	return result
}

func intersect(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, s := range b {
		set[s] = struct{}{}
	}
	out := a[:0]
	for _, s := range a {
		if _, ok := set[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Files returns the file records stored in SQLite.
func (ix *Indexer) Files() []FileRecord {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	recs, err := ix.store.list()
	if err != nil {
		return nil
	}
	return recs
}

// Close releases resources.
func (ix *Indexer) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed {
		return nil
	}
	ix.closed = true
	return ix.store.close()
}
