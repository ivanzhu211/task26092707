package indexer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// crashHook, when non-nil, is invoked at named points inside commit paths.
// Tests use it to simulate power loss via os.Exit in a helper process.
var crashHook func(point string)

func crashPoint(point string) {
	if crashHook != nil {
		crashHook(point)
	}
}

// readHook, when non-nil, is invoked with the absolute path of every file
// whose content is read from disk. Tests use it to observe read behavior.
var readHook func(path string)

const (
	manifestName = "manifest"
	segmentMagic = "IDX1"
	tmpSuffix    = ".tmp"
)

// batch accumulates one commit's worth of index mutations.
type batch struct {
	deleted map[string]bool     // paths whose previous postings are invalidated
	added   map[string][]string // path -> distinct terms
}

func newBatch() *batch {
	return &batch{deleted: map[string]bool{}, added: map[string][]string{}}
}

func (b *batch) del(path string) {
	if _, ok := b.added[path]; !ok {
		b.deleted[path] = true
	}
}

func (b *batch) add(path string, terms []string) {
	delete(b.deleted, path)
	b.added[path] = terms
}

func (b *batch) size() int { return len(b.deleted) + len(b.added) }

// invertedIndex is an append-only, segment-based inverted index.
// A commit appends one segment file and atomically replaces the manifest;
// a crash before the manifest swap loses the whole uncommitted batch.
type invertedIndex struct {
	dir      string
	segments []string
	next     int
	// mem mirrors exactly the committed on-disk state.
	memTerms map[string]map[string]bool // term -> paths
	memDocs  map[string]map[string]bool // path -> terms
}

type manifestData struct {
	Next     int      `json:"next"`
	Segments []string `json:"segments"`
}

func openIndex(dir string) (*invertedIndex, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ix := &invertedIndex{
		dir:      dir,
		next:     1,
		memTerms: map[string]map[string]bool{},
		memDocs:  map[string]map[string]bool{},
	}
	mf := manifestData{Next: 1}
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err == nil {
		if err := json.Unmarshal(data, &mf); err != nil {
			return nil, fmt.Errorf("corrupt manifest: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	ix.next = mf.Next
	ix.segments = mf.Segments
	for _, seg := range mf.Segments {
		if err := ix.loadSegment(seg); err != nil {
			return nil, err
		}
	}
	// Remove orphaned segment files never referenced by a committed manifest.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{manifestName: true}
	for _, s := range mf.Segments {
		live[s] = true
	}
	for _, e := range entries {
		if !live[e.Name()] && strings.HasPrefix(e.Name(), "seg-") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return ix, nil
}

func (ix *invertedIndex) loadSegment(name string) error {
	data, err := os.ReadFile(filepath.Join(ix.dir, name))
	if err != nil {
		return err
	}
	b, err := decodeSegment(data)
	if err != nil {
		return fmt.Errorf("segment %s: %w", name, err)
	}
	ix.apply(b)
	return nil
}

// apply reflects a committed batch into the in-memory index.
func (ix *invertedIndex) apply(b *batch) {
	for path := range b.deleted {
		ix.removeDoc(path)
	}
	for path, terms := range b.added {
		ix.removeDoc(path)
		for _, term := range terms {
			if ix.memTerms[term] == nil {
				ix.memTerms[term] = map[string]bool{}
			}
			ix.memTerms[term][path] = true
			if ix.memDocs[path] == nil {
				ix.memDocs[path] = map[string]bool{}
			}
			ix.memDocs[path][term] = true
		}
	}
}

func (ix *invertedIndex) removeDoc(path string) {
	for term := range ix.memDocs[path] {
		delete(ix.memTerms[term], path)
		if len(ix.memTerms[term]) == 0 {
			delete(ix.memTerms, term)
		}
	}
	delete(ix.memDocs, path)
}

// commit durably and atomically publishes b, then updates the in-memory index.
func (ix *invertedIndex) commit(b *batch) error {
	if b.size() == 0 {
		return nil
	}
	crashPoint("preSegment")
	name := fmt.Sprintf("seg-%06d", ix.next)
	final := filepath.Join(ix.dir, name)
	tmp := final + tmpSuffix
	if err := writeFileSync(tmp, encodeSegment(b)); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if err := syncDir(ix.dir); err != nil {
		return err
	}
	crashPoint("postSegment")
	mf := manifestData{Next: ix.next + 1, Segments: append(append([]string{}, ix.segments...), name)}
	mdata, err := json.Marshal(mf)
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(ix.dir, manifestName+tmpSuffix), mdata); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(ix.dir, manifestName+tmpSuffix), filepath.Join(ix.dir, manifestName)); err != nil {
		return err
	}
	if err := syncDir(ix.dir); err != nil {
		return err
	}
	crashPoint("postManifest")
	ix.next = mf.Next
	ix.segments = mf.Segments
	ix.apply(b)
	return nil
}

func (ix *invertedIndex) search(term string) []string {
	paths := make([]string, 0, len(ix.memTerms[term]))
	for p := range ix.memTerms[term] {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func (ix *invertedIndex) close() error { return nil }

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// --- segment encoding ---

func encodeSegment(b *batch) []byte {
	var buf bytes.Buffer
	buf.WriteString(segmentMagic)
	deleted := make([]string, 0, len(b.deleted))
	for p := range b.deleted {
		deleted = append(deleted, p)
	}
	sort.Strings(deleted)
	writeU32(&buf, uint32(len(deleted)))
	for _, p := range deleted {
		writeStr(&buf, p)
	}
	added := make([]string, 0, len(b.added))
	for p := range b.added {
		added = append(added, p)
	}
	sort.Strings(added)
	writeU32(&buf, uint32(len(added)))
	for _, p := range added {
		writeStr(&buf, p)
		writeU32(&buf, uint32(len(b.added[p])))
		for _, t := range b.added[p] {
			writeStr(&buf, t)
		}
	}
	return buf.Bytes()
}

func decodeSegment(data []byte) (*batch, error) {
	if len(data) < len(segmentMagic) || string(data[:len(segmentMagic)]) != segmentMagic {
		return nil, fmt.Errorf("bad magic")
	}
	r := bytes.NewReader(data[len(segmentMagic):])
	b := newBatch()
	nd, err := readU32(r)
	if err != nil {
		return nil, err
	}
	for i := uint32(0); i < nd; i++ {
		p, err := readStr(r)
		if err != nil {
			return nil, err
		}
		b.deleted[p] = true
	}
	na, err := readU32(r)
	if err != nil {
		return nil, err
	}
	for i := uint32(0); i < na; i++ {
		p, err := readStr(r)
		if err != nil {
			return nil, err
		}
		nt, err := readU32(r)
		if err != nil {
			return nil, err
		}
		terms := make([]string, 0, nt)
		for j := uint32(0); j < nt; j++ {
			t, err := readStr(r)
			if err != nil {
				return nil, err
			}
			terms = append(terms, t)
		}
		b.added[p] = terms
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("trailing bytes")
	}
	return b, nil
}

func writeU32(buf *bytes.Buffer, v uint32) {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	buf.Write(tmp[:])
}

func writeStr(buf *bytes.Buffer, s string) {
	writeU32(buf, uint32(len(s)))
	buf.WriteString(s)
}

func readU32(r *bytes.Reader) (uint32, error) {
	var tmp [4]byte
	if _, err := r.Read(tmp[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(tmp[:]), nil
}

func readStr(r *bytes.Reader) (string, error) {
	n, err := readU32(r)
	if err != nil {
		return "", err
	}
	if uint32(r.Len()) < n {
		return "", fmt.Errorf("short string")
	}
	buf := make([]byte, n)
	if _, err := r.Read(buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
