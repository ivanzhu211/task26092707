package indexer

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// docEntry is the per-document record kept inside the inverted index.
type docEntry struct {
	Size      int64
	Mtime     int64 // unix seconds (1s resolution by design)
	IndexedAt int64 // unix seconds
	Terms     []string
}

// segment is one atomically committed batch of index changes.
type segment struct {
	Adds    map[string]docEntry
	Removes []string
}

// manifest is the only mutable pointer of the index; it is swapped atomically.
type manifest struct {
	ScanStart int64
	Segments  []string
}

const (
	manifestFile  = "manifest"
	manifestTmp   = "manifest.tmp"
	segmentSuffix = ".seg"
	segmentTmp    = ".seg.tmp"
	maxSegments   = 64 // compact when exceeded
)

// memIndex is the in-memory view rebuilt by replaying committed segments.
type memIndex struct {
	dir       string
	postings  map[string]map[string]struct{}
	docs      map[string]docEntry
	segments  []string
	scanStart int64
	nextSeg   int
}

// loadIndex opens the index directory, reads the manifest and replays its
// segments. Anything not referenced by the manifest is uncommitted garbage
// and is ignored (and removed).
func loadIndex(dir string) (*memIndex, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &memIndex{
		dir:      dir,
		postings: map[string]map[string]struct{}{},
		docs:     map[string]docEntry{},
	}
	data, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil // fresh index
		}
		return nil, err
	}
	var mf manifest
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&mf); err != nil {
		return nil, fmt.Errorf("corrupt manifest: %w", err)
	}
	m.scanStart = mf.ScanStart
	m.segments = mf.Segments
	live := map[string]struct{}{}
	maxNum := -1
	for _, name := range mf.Segments {
		live[name] = struct{}{}
		if n := segmentNum(name); n > maxNum {
			maxNum = n
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read segment %s: %w", name, err)
		}
		var seg segment
		if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&seg); err != nil {
			return nil, fmt.Errorf("corrupt segment %s: %w", name, err)
		}
		m.apply(&seg)
	}
	m.nextSeg = maxNum + 1
	// Best-effort cleanup of uncommitted orphans.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if name == manifestFile {
			continue
		}
		if _, ok := live[name]; !ok {
			os.Remove(filepath.Join(dir, name))
		}
	}
	return m, nil
}

func segmentNum(name string) int {
	if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, segmentSuffix) {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(name, "seg-%06d"+segmentSuffix, &n); err != nil {
		return -1
	}
	return n
}

// apply mutates the in-memory index with one committed segment.
func (m *memIndex) apply(seg *segment) {
	for _, p := range seg.Removes {
		m.removeDoc(p)
	}
	// Deterministic order keeps replay reproducible.
	paths := make([]string, 0, len(seg.Adds))
	for p := range seg.Adds {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m.addDoc(p, seg.Adds[p])
	}
}

func (m *memIndex) removeDoc(path string) {
	entry, ok := m.docs[path]
	if !ok {
		return
	}
	for _, t := range entry.Terms {
		set := m.postings[t]
		delete(set, path)
		if len(set) == 0 {
			delete(m.postings, t)
		}
	}
	delete(m.docs, path)
}

func (m *memIndex) addDoc(path string, entry docEntry) {
	m.removeDoc(path)
	m.docs[path] = entry
	for _, t := range entry.Terms {
		set := m.postings[t]
		if set == nil {
			set = map[string]struct{}{}
			m.postings[t] = set
		}
		set[path] = struct{}{}
	}
}

// commit durably and atomically publishes seg: the segment file is written and
// fsynced first, then the manifest is swapped via atomic rename. A crash before
// the rename loses the whole batch; a crash after keeps it entirely.
func (m *memIndex) commit(seg *segment, scanStart int64) error {
	if len(m.segments)+1 > maxSegments {
		if err := m.compact(scanStart); err != nil {
			return err
		}
	}
	name := fmt.Sprintf("seg-%06d%s", m.nextSeg, segmentSuffix)
	if err := writeFileAtomic(m.dir, name, segmentTmp, func(w *bytes.Buffer) error {
		return gob.NewEncoder(w).Encode(seg)
	}); err != nil {
		return err
	}
	m.nextSeg++
	newSegments := append(append([]string{}, m.segments...), name)
	if err := m.writeManifest(&manifest{ScanStart: scanStart, Segments: newSegments}); err != nil {
		return err
	}
	m.apply(seg)
	m.segments = newSegments
	m.scanStart = scanStart
	return nil
}

// compact rewrites all live segments into a single snapshot segment.
func (m *memIndex) compact(scanStart int64) error {
	snap := &segment{Adds: m.docs}
	name := fmt.Sprintf("seg-%06d%s", m.nextSeg, segmentSuffix)
	if err := writeFileAtomic(m.dir, name, segmentTmp, func(w *bytes.Buffer) error {
		return gob.NewEncoder(w).Encode(snap)
	}); err != nil {
		return err
	}
	m.nextSeg++
	if err := m.writeManifest(&manifest{ScanStart: scanStart, Segments: []string{name}}); err != nil {
		return err
	}
	old := m.segments
	m.segments = []string{name}
	m.scanStart = scanStart
	for _, f := range old {
		os.Remove(filepath.Join(m.dir, f))
	}
	return nil
}

func (m *memIndex) writeManifest(mf *manifest) error {
	if err := writeFileAtomic(m.dir, manifestFile, manifestTmp, func(w *bytes.Buffer) error {
		return gob.NewEncoder(w).Encode(mf)
	}); err != nil {
		return err
	}
	return syncDir(m.dir)
}

// writeFileAtomic writes content produced by encode to dir/name via a temp
// file, fsync, and atomic rename.
func writeFileAtomic(dir, name, tmpSuffix string, encode func(w *bytes.Buffer) error) error {
	var buf bytes.Buffer
	if err := encode(&buf); err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+tmpSuffix)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// search returns the sorted paths of documents containing term.
func (m *memIndex) search(term string) []string {
	set := m.postings[term]
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
