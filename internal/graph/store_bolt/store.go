package store_bolt

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	bbolt "go.etcd.io/bbolt"

	"github.com/zzet/gortex/internal/graph"
)

// Store is a bbolt-backed implementation of graph.Store.
//
// All node/edge state lives on disk in the buckets enumerated in
// bucket_layout.go. The struct holds a single *bbolt.DB plus a tiny
// in-memory mutex used only to serialize the (read-then-write) call
// pattern of SetEdgeProvenance against concurrent identity-revision
// readers — bbolt itself takes care of write serialization, so
// AddNode / AddEdge / AddBatch / EvictFile / EvictRepo do not need
// our help to be race-free.
type Store struct {
	db *bbolt.DB

	// provMu serialises the read-modify-write of SetEdgeProvenance
	// (load the stored edge, compare hashes, rewrite). Without it
	// two concurrent provenance bumps could both observe the
	// pre-change Origin and double-charge the revision counter.
	provMu sync.Mutex

	// resolveMu is the resolver-coordination mutex returned by
	// ResolveMutex. Held by cross-repo / temporal / external resolver
	// passes to keep their edge mutations from interleaving. Separate
	// from provMu since the two protect different invariants.
	resolveMu sync.Mutex
}

// Compile-time assertion: *Store satisfies graph.Store.
var _ graph.Store = (*Store)(nil)

// Open opens (or creates) a bbolt database at path and ensures every
// bucket the schema needs exists.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("store_bolt: open %q: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range allBuckets {
			if _, e := tx.CreateBucketIfNotExists(name); e != nil {
				return fmt.Errorf("create bucket %q: %w", name, e)
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// ResolveMutex returns the resolver-coordination mutex. Held by
// cross-repo / temporal / external resolver passes to serialise edge
// mutations. Separate from provMu (which protects SetEdgeProvenance's
// read-modify-write) since the two guard different invariants.
func (s *Store) ResolveMutex() *sync.Mutex { return &s.resolveMu }

// Close closes the underlying bbolt DB.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// -- encoding helpers ---------------------------------------------------
//
// Earlier revisions of this file used `gob.NewEncoder` once per record.
// That pattern emits the full type-definition prologue (~200-400 bytes
// of metadata for Node / Edge) for EVERY encoded value because a fresh
// encoder has no remembered type state — multiplied by the millions of
// nodes/edges in a large repo's graph, that's hundreds of MB of
// redundant bytes flowing through the BTree on bulk load and a
// proportional commit-time penalty. Switched to a hand-rolled,
// length-prefixed binary codec that pays no per-instance prologue and
// allocates only the value bytes themselves.
//
// Format (version=1, varint-len-prefixed strings, fixed-width ints,
// gob-encoded Meta blob — Meta is rare and small enough that the per-
// item gob hit is not the bottleneck):
//
//   Node (version 1):
//     u8   version (=1)
//     varint+bytes  ID, Kind, Name, QualName, FilePath, Language,
//                   RepoPrefix, WorkspaceID, ProjectID, AbsoluteFilePath
//     varint        StartLine, EndLine
//     varint+bytes  Meta (gob; len=0 when nil/empty)
//
//   Edge (version 1):
//     u8   version (=1)
//     varint+bytes  From, To, Kind, FilePath
//     varint        Line
//     8 bytes f64   Confidence (IEEE 754 big-endian)
//     varint+bytes  ConfidenceLabel, Origin, Tier
//     u8            CrossRepo (0 or 1)
//     varint+bytes  Meta (gob; len=0 when nil/empty)
//
// Schema evolution: bump the version byte and branch on it in decode.

const nodeFormatVersion byte = 1
const edgeFormatVersion byte = 1

// encodeBuf is reused across encodes within a single transaction to
// avoid per-record allocation. Each Get() returns a buffer reset to
// length 0 but with its underlying capacity intact.
var encodeBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)
		return &b
	},
}

func getEncBuf() *[]byte {
	bp := encodeBufPool.Get().(*[]byte)
	*bp = (*bp)[:0]
	return bp
}

func putEncBuf(bp *[]byte) {
	// Drop oversized buffers so an outlier Meta blob doesn't pin a
	// giant slab in the pool slot forever.
	if cap(*bp) > 8192 {
		return
	}
	encodeBufPool.Put(bp)
}

// appendVarintLen writes a varint length followed by the bytes.
func appendVarintLen(buf []byte, b []byte) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(b)))
	buf = append(buf, tmp[:n]...)
	buf = append(buf, b...)
	return buf
}

// appendStr is appendVarintLen for strings — saves the []byte cast.
func appendStr(buf []byte, s string) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(s)))
	buf = append(buf, tmp[:n]...)
	buf = append(buf, s...)
	return buf
}

func appendVarint(buf []byte, v int64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutVarint(tmp[:], v)
	return append(buf, tmp[:n]...)
}

func readStr(b []byte) (string, []byte, error) {
	l, n := binary.Uvarint(b)
	if n <= 0 {
		return "", nil, errors.New("store_bolt: short varint")
	}
	if uint64(len(b)-n) < l {
		return "", nil, errors.New("store_bolt: short string")
	}
	return string(b[n : n+int(l)]), b[n+int(l):], nil
}

func readBytes(b []byte) ([]byte, []byte, error) {
	l, n := binary.Uvarint(b)
	if n <= 0 {
		return nil, nil, errors.New("store_bolt: short varint")
	}
	if uint64(len(b)-n) < l {
		return nil, nil, errors.New("store_bolt: short bytes")
	}
	out := make([]byte, l)
	copy(out, b[n:n+int(l)])
	return out, b[n+int(l):], nil
}

func readVarint(b []byte) (int64, []byte, error) {
	v, n := binary.Varint(b)
	if n <= 0 {
		return 0, nil, errors.New("store_bolt: short varint")
	}
	return v, b[n:], nil
}

// encodeMetaBlob is the lone gob path that survived the rewrite. Meta
// is a map[string]any with caller-defined value types; gob handles the
// dynamic-typing case for free where the rest of the schema is
// statically known. It runs only when meta is non-empty so the common
// "no meta" node/edge pays zero codec overhead.
func encodeMetaBlob(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(m); err != nil {
		return nil, fmt.Errorf("encode meta: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeMetaBlob(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	m := make(map[string]any)
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode meta: %w", err)
	}
	return m, nil
}

func encodeNode(n *graph.Node) ([]byte, error) {
	if n == nil {
		return nil, errors.New("store_bolt: nil node")
	}
	metaBlob, err := encodeMetaBlob(n.Meta)
	if err != nil {
		return nil, fmt.Errorf("encode node %q: %w", n.ID, err)
	}
	bp := getEncBuf()
	defer putEncBuf(bp)
	buf := *bp
	buf = append(buf, nodeFormatVersion)
	buf = appendStr(buf, n.ID)
	buf = appendStr(buf, string(n.Kind))
	buf = appendStr(buf, n.Name)
	buf = appendStr(buf, n.QualName)
	buf = appendStr(buf, n.FilePath)
	buf = appendStr(buf, n.Language)
	buf = appendStr(buf, n.RepoPrefix)
	buf = appendStr(buf, n.WorkspaceID)
	buf = appendStr(buf, n.ProjectID)
	buf = appendStr(buf, n.AbsoluteFilePath)
	buf = appendVarint(buf, int64(n.StartLine))
	buf = appendVarint(buf, int64(n.EndLine))
	buf = appendVarintLen(buf, metaBlob)
	// Return a fresh slice that bbolt can safely keep across the
	// transaction commit — we don't want it pointing into a pooled
	// buffer that's about to be reset for the next call.
	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf // restore for pool reuse
	return out, nil
}

func decodeNode(b []byte) (*graph.Node, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if b[0] != nodeFormatVersion {
		return nil, fmt.Errorf("store_bolt: unknown node format version %d", b[0])
	}
	b = b[1:]
	n := &graph.Node{}
	var (
		s   string
		blb []byte
		v   int64
		err error
	)
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.ID = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.Kind = graph.NodeKind(s)
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.Name = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.QualName = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.FilePath = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.Language = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.RepoPrefix = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.WorkspaceID = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.ProjectID = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	n.AbsoluteFilePath = s
	if v, b, err = readVarint(b); err != nil {
		return nil, err
	}
	n.StartLine = int(v)
	if v, b, err = readVarint(b); err != nil {
		return nil, err
	}
	n.EndLine = int(v)
	if blb, _, err = readBytes(b); err != nil {
		return nil, err
	}
	if n.Meta, err = decodeMetaBlob(blb); err != nil {
		return nil, err
	}
	return n, nil
}

func encodeEdge(e *graph.Edge) ([]byte, error) {
	if e == nil {
		return nil, errors.New("store_bolt: nil edge")
	}
	metaBlob, err := encodeMetaBlob(e.Meta)
	if err != nil {
		return nil, fmt.Errorf("encode edge %s->%s: %w", e.From, e.To, err)
	}
	bp := getEncBuf()
	defer putEncBuf(bp)
	buf := *bp
	buf = append(buf, edgeFormatVersion)
	buf = appendStr(buf, e.From)
	buf = appendStr(buf, e.To)
	buf = appendStr(buf, string(e.Kind))
	buf = appendStr(buf, e.FilePath)
	buf = appendVarint(buf, int64(e.Line))
	var confBuf [8]byte
	binary.BigEndian.PutUint64(confBuf[:], floatBits(e.Confidence))
	buf = append(buf, confBuf[:]...)
	buf = appendStr(buf, e.ConfidenceLabel)
	buf = appendStr(buf, e.Origin)
	buf = appendStr(buf, e.Tier)
	if e.CrossRepo {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	buf = appendVarintLen(buf, metaBlob)
	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf
	return out, nil
}

func decodeEdge(b []byte) (*graph.Edge, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if b[0] != edgeFormatVersion {
		return nil, fmt.Errorf("store_bolt: unknown edge format version %d", b[0])
	}
	b = b[1:]
	e := &graph.Edge{}
	var (
		s   string
		blb []byte
		v   int64
		err error
	)
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	e.From = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	e.To = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	e.Kind = graph.EdgeKind(s)
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	e.FilePath = s
	if v, b, err = readVarint(b); err != nil {
		return nil, err
	}
	e.Line = int(v)
	if len(b) < 8 {
		return nil, errors.New("store_bolt: short confidence")
	}
	e.Confidence = bitsFloat(binary.BigEndian.Uint64(b[:8]))
	b = b[8:]
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	e.ConfidenceLabel = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	e.Origin = s
	if s, b, err = readStr(b); err != nil {
		return nil, err
	}
	e.Tier = s
	if len(b) < 1 {
		return nil, errors.New("store_bolt: short cross_repo")
	}
	e.CrossRepo = b[0] != 0
	b = b[1:]
	if blb, _, err = readBytes(b); err != nil {
		return nil, err
	}
	if e.Meta, err = decodeMetaBlob(blb); err != nil {
		return nil, err
	}
	return e, nil
}

// floatBits / bitsFloat wrap math.Float64bits/Float64frombits so the
// encode/decode paths stay one-liners.
func floatBits(f float64) uint64    { return math.Float64bits(f) }
func bitsFloat(b uint64) float64    { return math.Float64frombits(b) }

// edgeKey builds a stable, lexicographically-prefix-scannable binary key
// from the identity tuple (from, to, kind, filePath, line). Each
// variable-length component is prefixed with a 2-byte big-endian length
// so the encoding is uniquely decodable. The single edges bucket is
// keyed by this; the per-endpoint adjacency indexes embed it after the
// endpoint ID and a NUL separator.
func edgeKey(e *graph.Edge) []byte {
	if e == nil {
		return nil
	}
	parts := [][]byte{
		[]byte(e.From),
		[]byte(e.To),
		[]byte(e.Kind),
		[]byte(e.FilePath),
	}
	size := 0
	for _, p := range parts {
		size += 2 + len(p)
	}
	size += 4 // line int32
	buf := make([]byte, 0, size)
	for _, p := range parts {
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(p)))
		buf = append(buf, lb[:]...)
		buf = append(buf, p...)
	}
	var line [4]byte
	binary.BigEndian.PutUint32(line[:], uint32(e.Line))
	buf = append(buf, line[:]...)
	return buf
}

// outEdgeIdxKey: fromID + 0x00 + edgeKey
func outEdgeIdxKey(fromID string, ek []byte) []byte {
	buf := make([]byte, 0, len(fromID)+1+len(ek))
	buf = append(buf, fromID...)
	buf = append(buf, 0x00)
	buf = append(buf, ek...)
	return buf
}

// inEdgeIdxKey: toID + 0x00 + edgeKey
func inEdgeIdxKey(toID string, ek []byte) []byte {
	buf := make([]byte, 0, len(toID)+1+len(ek))
	buf = append(buf, toID...)
	buf = append(buf, 0x00)
	buf = append(buf, ek...)
	return buf
}

// scopedKey: prefix + 0x00 + nodeID — used by the kind/file/repo/name
// node indexes whose values are empty (presence is the data).
func scopedKey(prefix, nodeID string) []byte {
	buf := make([]byte, 0, len(prefix)+1+len(nodeID))
	buf = append(buf, prefix...)
	buf = append(buf, 0x00)
	buf = append(buf, nodeID...)
	return buf
}

// -- write paths --------------------------------------------------------

// AddNode inserts or replaces n in the graph. Idempotent on a stable
// (ID) key — re-adding the same node leaves NodeCount unchanged but
// refreshes every per-attribute index (kind, file, repo, name,
// qualname) in case the values drifted.
func (s *Store) AddNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		return s.putNodeTx(tx, n)
	})
}

// putNodeTx is the shared write path used by AddNode and AddBatch.
// Removes any stale per-attribute index rows from a prior version of
// the same node before writing the fresh ones.
func (s *Store) putNodeTx(tx *bbolt.Tx, n *graph.Node) error {
	if n == nil || n.ID == "" {
		return nil
	}
	nodes := tx.Bucket(bucketNodes)
	idKey := []byte(n.ID)

	// Clear any stale index rows from a prior write under this ID.
	if existing := nodes.Get(idKey); existing != nil {
		old, err := decodeNode(existing)
		if err == nil && old != nil {
			s.removeNodeIndexes(tx, old)
		}
	}

	enc, err := encodeNode(n)
	if err != nil {
		return err
	}
	if err := nodes.Put(idKey, enc); err != nil {
		return err
	}
	return s.addNodeIndexes(tx, n)
}

// addNodeIndexes writes every per-attribute index row for n.
func (s *Store) addNodeIndexes(tx *bbolt.Tx, n *graph.Node) error {
	if n.Kind != "" {
		if err := tx.Bucket(bucketIdxNodeKind).Put(scopedKey(string(n.Kind), n.ID), nil); err != nil {
			return err
		}
	}
	if n.FilePath != "" {
		if err := tx.Bucket(bucketIdxNodeFile).Put(scopedKey(n.FilePath, n.ID), nil); err != nil {
			return err
		}
	}
	if n.RepoPrefix != "" {
		if err := tx.Bucket(bucketIdxNodeRepo).Put(scopedKey(n.RepoPrefix, n.ID), nil); err != nil {
			return err
		}
	}
	if n.Name != "" {
		if err := tx.Bucket(bucketIdxNodeName).Put(scopedKey(n.Name, n.ID), nil); err != nil {
			return err
		}
	}
	if n.QualName != "" {
		if err := tx.Bucket(bucketIdxNodeQual).Put([]byte(n.QualName), []byte(n.ID)); err != nil {
			return err
		}
	}
	return nil
}

// removeNodeIndexes deletes every per-attribute index row for n.
func (s *Store) removeNodeIndexes(tx *bbolt.Tx, n *graph.Node) {
	if n.Kind != "" {
		_ = tx.Bucket(bucketIdxNodeKind).Delete(scopedKey(string(n.Kind), n.ID))
	}
	if n.FilePath != "" {
		_ = tx.Bucket(bucketIdxNodeFile).Delete(scopedKey(n.FilePath, n.ID))
	}
	if n.RepoPrefix != "" {
		_ = tx.Bucket(bucketIdxNodeRepo).Delete(scopedKey(n.RepoPrefix, n.ID))
	}
	if n.Name != "" {
		_ = tx.Bucket(bucketIdxNodeName).Delete(scopedKey(n.Name, n.ID))
	}
	if n.QualName != "" {
		// Only clear the qualname row if it actually points at this node —
		// two distinct nodes with the same QualName can coexist if the
		// caller never enforces uniqueness; we conservatively wipe only
		// the matching row.
		b := tx.Bucket(bucketIdxNodeQual)
		if v := b.Get([]byte(n.QualName)); v != nil && string(v) == n.ID {
			_ = b.Delete([]byte(n.QualName))
		}
	}
}

// AddEdge inserts e, idempotent on the (from, to, kind, filePath, line)
// identity tuple. Re-adding the same logical edge with an upgraded
// Origin replaces the stored value and bumps the identity-revision
// counter.
func (s *Store) AddEdge(e *graph.Edge) {
	if e == nil {
		return
	}
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		_, _, err := s.putEdgeTx(tx, e)
		return err
	})
}

// putEdgeTx is the shared write path used by AddEdge and AddBatch.
// Returns (inserted, originChanged, err) so the caller can update the
// edge-identity-revision counter.
func (s *Store) putEdgeTx(tx *bbolt.Tx, e *graph.Edge) (inserted, originChanged bool, err error) {
	if e == nil {
		return false, false, nil
	}
	ek := edgeKey(e)
	edges := tx.Bucket(bucketEdges)
	prev := edges.Get(ek)
	if prev != nil {
		// An existing edge with the same identity tuple lives here. We
		// replace it in place; the only signal we need to surface is
		// whether the Origin changed.
		old, derr := decodeEdge(prev)
		if derr == nil && old != nil && old.Origin != e.Origin {
			originChanged = true
		}
	} else {
		inserted = true
	}
	enc, eerr := encodeEdge(e)
	if eerr != nil {
		return false, false, eerr
	}
	if err := edges.Put(ek, enc); err != nil {
		return false, false, err
	}
	if err := tx.Bucket(bucketIdxEdgeOut).Put(outEdgeIdxKey(e.From, ek), nil); err != nil {
		return false, false, err
	}
	if err := tx.Bucket(bucketIdxEdgeIn).Put(inEdgeIdxKey(e.To, ek), nil); err != nil {
		return false, false, err
	}
	if originChanged {
		if err := bumpEdgeIdentityRevisions(tx); err != nil {
			return false, false, err
		}
	}
	return inserted, originChanged, nil
}

// AddBatch inserts every node and edge in a single bbolt write
// transaction — the on-disk analogue of *Graph's bulk fast-path.
// addBatchChunkSize bounds the number of mutations per bbolt
// transaction. bbolt's commit phase has to rebalance every dirty page
// in the transaction, so one giant Update over 100k+ items pays an
// O(N log N) commit penalty that dwarfs steady-state write time. Empty
// rule of thumb from upstream: 5–20k mutations per Tx is the sweet
// spot where commit overhead amortises without the dirty set ballooning.
const addBatchChunkSize = 5000

// AddBatch inserts nodes and edges in chunked transactions. Each chunk
// commits independently; readers see the writes in chunk granularity
// rather than as one atomic batch, but the indexer only calls AddBatch
// from a single goroutine during a cold-index pass so that's not a
// correctness concern. Splitting the writes keeps bbolt's
// dirty-page set bounded and the commit phase predictable on large
// loads (the alternative is a single Update over millions of mutations,
// which we measured at 4+ minutes for a 120k-node / 514k-edge graph).
func (s *Store) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	for i := 0; i < len(nodes); i += addBatchChunkSize {
		end := min(i+addBatchChunkSize, len(nodes))
		chunk := nodes[i:end]
		_ = s.db.Update(func(tx *bbolt.Tx) error {
			for _, n := range chunk {
				if n == nil {
					continue
				}
				if err := s.putNodeTx(tx, n); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for i := 0; i < len(edges); i += addBatchChunkSize {
		end := min(i+addBatchChunkSize, len(edges))
		chunk := edges[i:end]
		_ = s.db.Update(func(tx *bbolt.Tx) error {
			for _, e := range chunk {
				if e == nil {
					continue
				}
				if _, _, err := s.putEdgeTx(tx, e); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

// SetEdgeProvenance rewrites the persisted edge with a new Origin and
// bumps the identity-revision counter when the change is real. Returns
// false when newOrigin is the same as the stored Origin (no-op).
func (s *Store) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	s.provMu.Lock()
	defer s.provMu.Unlock()
	var changed bool
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		ek := edgeKey(e)
		edges := tx.Bucket(bucketEdges)
		raw := edges.Get(ek)
		if raw == nil {
			return nil
		}
		stored, derr := decodeEdge(raw)
		if derr != nil || stored == nil {
			return derr
		}
		if stored.Origin == newOrigin {
			return nil
		}
		stored.Origin = newOrigin
		// Mirror the in-memory contract: Tier is a pure projection of
		// Origin (graph.ResolvedBy), and we re-derive it only when it
		// was already populated.
		if stored.Tier != "" {
			stored.Tier = graph.ResolvedBy(newOrigin)
		}
		// Also mutate the caller's pointer so the test that inspects
		// `e.Origin` after the call sees the new value (mirrors the
		// in-memory store, which keeps a single pointer per edge).
		e.Origin = newOrigin
		if e.Tier != "" {
			e.Tier = graph.ResolvedBy(newOrigin)
		}
		enc, eerr := encodeEdge(stored)
		if eerr != nil {
			return eerr
		}
		if err := edges.Put(ek, enc); err != nil {
			return err
		}
		if err := bumpEdgeIdentityRevisions(tx); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed
}

// ReindexEdge moves an edge from (From, oldTo) to (From, e.To). Used by
// the indexer after a To-side relink. We delete the old key tuple
// outright and reinsert with the current e — origin/meta are preserved
// because the caller hands us the still-valid struct.
func (s *Store) ReindexEdge(e *graph.Edge, oldTo string) {
	if e == nil {
		return
	}
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		return s.reindexEdgeTx(tx, e, oldTo)
	})
}

// reindexEdgeTx is the per-edge mutation logic factored out of
// ReindexEdge so ReindexEdges can call it inside its own batched
// transaction without one Update-per-edge overhead.
func (s *Store) reindexEdgeTx(tx *bbolt.Tx, e *graph.Edge, oldTo string) error {
	// Build the old key by temporarily swapping To back.
	newTo := e.To
	e.To = oldTo
	oldKey := edgeKey(e)
	e.To = newTo
	edges := tx.Bucket(bucketEdges)
	_ = edges.Delete(oldKey)
	_ = tx.Bucket(bucketIdxEdgeOut).Delete(outEdgeIdxKey(e.From, oldKey))
	_ = tx.Bucket(bucketIdxEdgeIn).Delete(inEdgeIdxKey(oldTo, oldKey))
	_, _, err := s.putEdgeTx(tx, e)
	return err
}

// reindexChunkSize bounds the number of edge re-binds per bbolt
// transaction. Same sweet spot as addBatchChunkSize for the same
// reason: bbolt's commit phase pays per dirty page, so one giant Tx
// over thousands of mutations is O(N log N). 5000 amortises per-tx
// overhead while keeping the dirty set bounded.
const reindexChunkSize = 5000

// ReindexEdges chunks the batch into reindexChunkSize-mutation
// transactions and runs each inside one bbolt Update — folding 10k
// resolver-pass mutations from 10k commits down to 2.
func (s *Store) ReindexEdges(batch []graph.EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	for i := 0; i < len(batch); i += reindexChunkSize {
		end := min(i+reindexChunkSize, len(batch))
		chunk := batch[i:end]
		_ = s.db.Update(func(tx *bbolt.Tx) error {
			for _, r := range chunk {
				if r.Edge == nil {
					continue
				}
				if err := s.reindexEdgeTx(tx, r.Edge, r.OldTo); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

// setEdgeProvenanceTx is the per-edge SetEdgeProvenance body factored
// out so the batch variant can call it inside one Tx. Returns true
// when the stored Origin actually changed (callers tally for the
// revision counter). Mirrors the in-memory contract: caller's *Edge
// pointer is also mutated so post-call inspection sees the new
// Origin / re-derived Tier.
func (s *Store) setEdgeProvenanceTx(tx *bbolt.Tx, e *graph.Edge, newOrigin string) (bool, error) {
	if e == nil {
		return false, nil
	}
	ek := edgeKey(e)
	edges := tx.Bucket(bucketEdges)
	raw := edges.Get(ek)
	if raw == nil {
		return false, nil
	}
	stored, derr := decodeEdge(raw)
	if derr != nil || stored == nil {
		return false, derr
	}
	if stored.Origin == newOrigin {
		return false, nil
	}
	stored.Origin = newOrigin
	if stored.Tier != "" {
		stored.Tier = graph.ResolvedBy(newOrigin)
	}
	e.Origin = newOrigin
	if e.Tier != "" {
		e.Tier = graph.ResolvedBy(newOrigin)
	}
	enc, eerr := encodeEdge(stored)
	if eerr != nil {
		return false, eerr
	}
	if err := edges.Put(ek, enc); err != nil {
		return false, err
	}
	return true, nil
}

// SetEdgeProvenanceBatch chunks the batch the same way ReindexEdges
// does and bumps the persistent identity-revision counter per actual
// change, keeping the in-memory SetEdgeProvenance's per-edge "real
// change?" semantics intact while collapsing the disk-side write
// amplification.
func (s *Store) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	if len(batch) == 0 {
		return 0
	}
	s.provMu.Lock()
	defer s.provMu.Unlock()
	totalChanged := 0
	for i := 0; i < len(batch); i += reindexChunkSize {
		end := min(i+reindexChunkSize, len(batch))
		chunk := batch[i:end]
		chunkChanged := 0
		_ = s.db.Update(func(tx *bbolt.Tx) error {
			for _, u := range chunk {
				if u.Edge == nil {
					continue
				}
				ok, err := s.setEdgeProvenanceTx(tx, u.Edge, u.NewOrigin)
				if err != nil {
					return err
				}
				if ok {
					chunkChanged++
					// Bump in-tx so a crash mid-chunk leaves the
					// revision counter consistent with the partial
					// edges actually persisted.
					if err := bumpEdgeIdentityRevisions(tx); err != nil {
						return err
					}
				}
			}
			return nil
		})
		totalChanged += chunkChanged
	}
	return totalChanged
}

// RemoveEdge drops the edge with the given (from, to, kind) tuple.
// Returns true when something was actually removed. Because the
// identity tuple includes FilePath and Line, multiple edges may share
// the same (from, to, kind); we walk the out-edge index for this from-
// node and delete every match.
func (s *Store) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	var removed bool
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		outIdx := tx.Bucket(bucketIdxEdgeOut)
		edges := tx.Bucket(bucketEdges)
		inIdx := tx.Bucket(bucketIdxEdgeIn)
		prefix := append([]byte(from), 0x00)
		c := outIdx.Cursor()
		// We can't delete while iterating safely; collect first.
		var toDelete [][]byte
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			ek := k[len(prefix):]
			raw := edges.Get(ek)
			if raw == nil {
				continue
			}
			e, derr := decodeEdge(raw)
			if derr != nil || e == nil {
				continue
			}
			if e.To == to && e.Kind == kind {
				cp := make([]byte, len(ek))
				copy(cp, ek)
				toDelete = append(toDelete, cp)
			}
		}
		for _, ek := range toDelete {
			if err := edges.Delete(ek); err != nil {
				return err
			}
			if err := outIdx.Delete(outEdgeIdxKey(from, ek)); err != nil {
				return err
			}
			if err := inIdx.Delete(inEdgeIdxKey(to, ek)); err != nil {
				return err
			}
			removed = true
		}
		return nil
	})
	return removed
}

// EvictFile drops every node whose FilePath equals filePath plus every
// edge touching one of those nodes. Returns (nodesRemoved, edgesRemoved).
func (s *Store) EvictFile(filePath string) (int, int) {
	if filePath == "" {
		return 0, 0
	}
	var nRemoved, eRemoved int
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		ids := s.collectIDsByScopedPrefix(tx, bucketIdxNodeFile, filePath)
		nRemoved, eRemoved = s.evictNodesByID(tx, ids)
		return nil
	})
	return nRemoved, eRemoved
}

// EvictRepo drops every node whose RepoPrefix equals repoPrefix plus
// every edge touching one of those nodes.
func (s *Store) EvictRepo(repoPrefix string) (int, int) {
	if repoPrefix == "" {
		return 0, 0
	}
	var nRemoved, eRemoved int
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		ids := s.collectIDsByScopedPrefix(tx, bucketIdxNodeRepo, repoPrefix)
		nRemoved, eRemoved = s.evictNodesByID(tx, ids)
		return nil
	})
	return nRemoved, eRemoved
}

// collectIDsByScopedPrefix walks a scoped index bucket (kind / file /
// repo / name) for the rows whose prefix equals `prefix` and returns
// the node IDs encoded after the NUL separator.
func (s *Store) collectIDsByScopedPrefix(tx *bbolt.Tx, bucketName []byte, prefix string) []string {
	b := tx.Bucket(bucketName)
	if b == nil {
		return nil
	}
	pfx := append([]byte(prefix), 0x00)
	var ids []string
	c := b.Cursor()
	for k, _ := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, _ = c.Next() {
		ids = append(ids, string(k[len(pfx):]))
	}
	return ids
}

// evictNodesByID deletes the listed nodes (plus their index rows and
// every adjacent edge). Returns (nodesRemoved, edgesRemoved).
func (s *Store) evictNodesByID(tx *bbolt.Tx, ids []string) (int, int) {
	if len(ids) == 0 {
		return 0, 0
	}
	nodes := tx.Bucket(bucketNodes)
	edges := tx.Bucket(bucketEdges)
	outIdx := tx.Bucket(bucketIdxEdgeOut)
	inIdx := tx.Bucket(bucketIdxEdgeIn)

	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}

	nRemoved := 0
	for _, id := range ids {
		raw := nodes.Get([]byte(id))
		if raw == nil {
			continue
		}
		n, derr := decodeNode(raw)
		if derr == nil && n != nil {
			s.removeNodeIndexes(tx, n)
		}
		if err := nodes.Delete([]byte(id)); err != nil {
			continue
		}
		nRemoved++
	}

	// Collect every edge whose endpoint is in idSet — we walk both
	// adjacency indexes so an edge whose endpoints are *both* evicted
	// is still counted exactly once.
	type edgeRow struct {
		key  []byte
		from string
		to   string
	}
	seen := make(map[string]edgeRow)
	collect := func(idx *bbolt.Bucket) {
		c := idx.Cursor()
		for _, id := range ids {
			pfx := append([]byte(id), 0x00)
			for k, _ := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, _ = c.Next() {
				ek := k[len(pfx):]
				raw := edges.Get(ek)
				if raw == nil {
					continue
				}
				e, derr := decodeEdge(raw)
				if derr != nil || e == nil {
					continue
				}
				cp := make([]byte, len(ek))
				copy(cp, ek)
				seen[string(cp)] = edgeRow{key: cp, from: e.From, to: e.To}
			}
		}
	}
	collect(outIdx)
	collect(inIdx)

	for _, row := range seen {
		_ = edges.Delete(row.key)
		_ = outIdx.Delete(outEdgeIdxKey(row.from, row.key))
		_ = inIdx.Delete(inEdgeIdxKey(row.to, row.key))
	}
	return nRemoved, len(seen)
}

// -- point lookups ------------------------------------------------------

func (s *Store) GetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	var out *graph.Node
	_ = s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketNodes).Get([]byte(id))
		if raw == nil {
			return nil
		}
		// Copy the bytes out before decode — bbolt invalidates them
		// once the txn ends, but decoding inside the txn is fine.
		n, derr := decodeNode(raw)
		if derr == nil {
			out = n
		}
		return nil
	})
	return out
}

func (s *Store) GetNodeByQualName(qualName string) *graph.Node {
	if qualName == "" {
		return nil
	}
	var id string
	_ = s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketIdxNodeQual).Get([]byte(qualName))
		if v != nil {
			id = string(v)
		}
		return nil
	})
	if id == "" {
		return nil
	}
	return s.GetNode(id)
}

// -- name + scope queries ---------------------------------------------

func (s *Store) FindNodesByName(name string) []*graph.Node {
	if name == "" {
		return nil
	}
	var out []*graph.Node
	_ = s.db.View(func(tx *bbolt.Tx) error {
		ids := s.collectIDsByScopedPrefix(tx, bucketIdxNodeName, name)
		out = make([]*graph.Node, 0, len(ids))
		nodes := tx.Bucket(bucketNodes)
		for _, id := range ids {
			raw := nodes.Get([]byte(id))
			if raw == nil {
				continue
			}
			n, derr := decodeNode(raw)
			if derr == nil && n != nil {
				out = append(out, n)
			}
		}
		return nil
	})
	return out
}

func (s *Store) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	if name == "" {
		return nil
	}
	all := s.FindNodesByName(name)
	if repoPrefix == "" {
		return all
	}
	out := all[:0]
	for _, n := range all {
		if n != nil && n.RepoPrefix == repoPrefix {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) GetFileNodes(filePath string) []*graph.Node {
	if filePath == "" {
		return nil
	}
	var out []*graph.Node
	_ = s.db.View(func(tx *bbolt.Tx) error {
		ids := s.collectIDsByScopedPrefix(tx, bucketIdxNodeFile, filePath)
		out = make([]*graph.Node, 0, len(ids))
		nodes := tx.Bucket(bucketNodes)
		for _, id := range ids {
			raw := nodes.Get([]byte(id))
			if raw == nil {
				continue
			}
			n, derr := decodeNode(raw)
			if derr == nil && n != nil {
				out = append(out, n)
			}
		}
		return nil
	})
	return out
}

func (s *Store) GetRepoNodes(repoPrefix string) []*graph.Node {
	if repoPrefix == "" {
		return nil
	}
	var out []*graph.Node
	_ = s.db.View(func(tx *bbolt.Tx) error {
		ids := s.collectIDsByScopedPrefix(tx, bucketIdxNodeRepo, repoPrefix)
		out = make([]*graph.Node, 0, len(ids))
		nodes := tx.Bucket(bucketNodes)
		for _, id := range ids {
			raw := nodes.Get([]byte(id))
			if raw == nil {
				continue
			}
			n, derr := decodeNode(raw)
			if derr == nil && n != nil {
				out = append(out, n)
			}
		}
		return nil
	})
	return out
}

// -- edge adjacency ----------------------------------------------------

func (s *Store) GetOutEdges(nodeID string) []*graph.Edge {
	if nodeID == "" {
		return nil
	}
	var out []*graph.Edge
	_ = s.db.View(func(tx *bbolt.Tx) error {
		out = s.collectEdgesByEndpoint(tx, bucketIdxEdgeOut, nodeID)
		return nil
	})
	return out
}

func (s *Store) GetInEdges(nodeID string) []*graph.Edge {
	if nodeID == "" {
		return nil
	}
	var out []*graph.Edge
	_ = s.db.View(func(tx *bbolt.Tx) error {
		out = s.collectEdgesByEndpoint(tx, bucketIdxEdgeIn, nodeID)
		return nil
	})
	return out
}

func (s *Store) collectEdgesByEndpoint(tx *bbolt.Tx, idxBucket []byte, nodeID string) []*graph.Edge {
	idx := tx.Bucket(idxBucket)
	edges := tx.Bucket(bucketEdges)
	prefix := append([]byte(nodeID), 0x00)
	var out []*graph.Edge
	c := idx.Cursor()
	for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
		ek := k[len(prefix):]
		raw := edges.Get(ek)
		if raw == nil {
			continue
		}
		e, derr := decodeEdge(raw)
		if derr == nil && e != nil {
			out = append(out, e)
		}
	}
	return out
}

// -- bulk reads --------------------------------------------------------

func (s *Store) AllNodes() []*graph.Node {
	var out []*graph.Node
	_ = s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		out = make([]*graph.Node, 0, b.Stats().KeyN)
		return b.ForEach(func(_, v []byte) error {
			n, derr := decodeNode(v)
			if derr == nil && n != nil {
				out = append(out, n)
			}
			return nil
		})
	})
	return out
}

func (s *Store) AllEdges() []*graph.Edge {
	var out []*graph.Edge
	_ = s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketEdges)
		out = make([]*graph.Edge, 0, b.Stats().KeyN)
		return b.ForEach(func(_, v []byte) error {
			e, derr := decodeEdge(v)
			if derr == nil && e != nil {
				out = append(out, e)
			}
			return nil
		})
	})
	return out
}

// -- counts and stats --------------------------------------------------

func (s *Store) NodeCount() int {
	var n int
	_ = s.db.View(func(tx *bbolt.Tx) error {
		n = tx.Bucket(bucketNodes).Stats().KeyN
		return nil
	})
	return n
}

func (s *Store) EdgeCount() int {
	var n int
	_ = s.db.View(func(tx *bbolt.Tx) error {
		n = tx.Bucket(bucketEdges).Stats().KeyN
		return nil
	})
	return n
}

func (s *Store) Stats() graph.GraphStats {
	st := graph.GraphStats{
		ByKind:     make(map[string]int),
		ByLanguage: make(map[string]int),
	}
	_ = s.db.View(func(tx *bbolt.Tx) error {
		nodes := tx.Bucket(bucketNodes)
		st.TotalNodes = nodes.Stats().KeyN
		st.TotalEdges = tx.Bucket(bucketEdges).Stats().KeyN
		return nodes.ForEach(func(_, v []byte) error {
			n, derr := decodeNode(v)
			if derr != nil || n == nil {
				return nil
			}
			if n.Kind != "" {
				st.ByKind[string(n.Kind)]++
			}
			if n.Language != "" {
				st.ByLanguage[n.Language]++
			}
			return nil
		})
	})
	return st
}

func (s *Store) RepoStats() map[string]graph.GraphStats {
	out := make(map[string]graph.GraphStats)
	_ = s.db.View(func(tx *bbolt.Tx) error {
		nodes := tx.Bucket(bucketNodes)
		return nodes.ForEach(func(_, v []byte) error {
			n, derr := decodeNode(v)
			if derr != nil || n == nil {
				return nil
			}
			repo := n.RepoPrefix
			st, ok := out[repo]
			if !ok {
				st = graph.GraphStats{
					ByKind:     make(map[string]int),
					ByLanguage: make(map[string]int),
				}
			}
			st.TotalNodes++
			if n.Kind != "" {
				st.ByKind[string(n.Kind)]++
			}
			if n.Language != "" {
				st.ByLanguage[n.Language]++
			}
			out[repo] = st
			return nil
		})
	})
	// Count edges by source node's repo.
	_ = s.db.View(func(tx *bbolt.Tx) error {
		edges := tx.Bucket(bucketEdges)
		nodes := tx.Bucket(bucketNodes)
		return edges.ForEach(func(_, v []byte) error {
			e, derr := decodeEdge(v)
			if derr != nil || e == nil {
				return nil
			}
			raw := nodes.Get([]byte(e.From))
			if raw == nil {
				return nil
			}
			src, derr := decodeNode(raw)
			if derr != nil || src == nil {
				return nil
			}
			st, ok := out[src.RepoPrefix]
			if !ok {
				st = graph.GraphStats{
					ByKind:     make(map[string]int),
					ByLanguage: make(map[string]int),
				}
			}
			st.TotalEdges++
			out[src.RepoPrefix] = st
			return nil
		})
	})
	return out
}

func (s *Store) RepoPrefixes() []string {
	seen := make(map[string]struct{})
	_ = s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketIdxNodeRepo).Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			// Key shape: prefix + 0x00 + nodeID
			i := bytes.IndexByte(k, 0x00)
			if i <= 0 {
				continue
			}
			seen[string(k[:i])] = struct{}{}
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	return out
}

// -- provenance verification ------------------------------------------

func (s *Store) EdgeIdentityRevisions() int {
	var n int
	_ = s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(metaKeyEdgeIdentityRevisions)
		if len(raw) != 8 {
			return nil
		}
		n = int(binary.BigEndian.Uint64(raw))
		return nil
	})
	return n
}

// VerifyEdgeIdentities sanity-checks that every edge in the canonical
// edges bucket is reachable from both the out- and in-adjacency
// indexes. A missing index row signals a corrupted write.
func (s *Store) VerifyEdgeIdentities() error {
	return s.db.View(func(tx *bbolt.Tx) error {
		edges := tx.Bucket(bucketEdges)
		outIdx := tx.Bucket(bucketIdxEdgeOut)
		inIdx := tx.Bucket(bucketIdxEdgeIn)
		return edges.ForEach(func(k, v []byte) error {
			e, derr := decodeEdge(v)
			if derr != nil || e == nil {
				return nil
			}
			if outIdx.Get(outEdgeIdxKey(e.From, k)) == nil {
				return fmt.Errorf("store_bolt: edge %s->%s missing out-index", e.From, e.To)
			}
			if inIdx.Get(inEdgeIdxKey(e.To, k)) == nil {
				return fmt.Errorf("store_bolt: edge %s->%s missing in-index", e.From, e.To)
			}
			return nil
		})
	})
}

// -- memory estimation -------------------------------------------------

func (s *Store) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	var est graph.RepoMemoryEstimate
	nodes := s.GetRepoNodes(repoPrefix)
	est.NodeCount = len(nodes)
	for _, n := range nodes {
		est.NodeBytes += nodeBytesEstimate(n)
	}
	// Edge accounting: any edge whose From belongs to repoPrefix counts.
	nodeIDs := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		nodeIDs[n.ID] = struct{}{}
	}
	_ = s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketEdges).ForEach(func(_, v []byte) error {
			e, derr := decodeEdge(v)
			if derr != nil || e == nil {
				return nil
			}
			if _, ok := nodeIDs[e.From]; ok {
				est.EdgeCount++
				est.EdgeBytes += edgeBytesEstimate(e)
			}
			return nil
		})
	})
	return est
}

func (s *Store) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	out := make(map[string]graph.RepoMemoryEstimate)
	repoOf := make(map[string]string)
	_ = s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketNodes).ForEach(func(_, v []byte) error {
			n, derr := decodeNode(v)
			if derr != nil || n == nil {
				return nil
			}
			repoOf[n.ID] = n.RepoPrefix
			est := out[n.RepoPrefix]
			est.NodeCount++
			est.NodeBytes += nodeBytesEstimate(n)
			out[n.RepoPrefix] = est
			return nil
		})
	})
	_ = s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketEdges).ForEach(func(_, v []byte) error {
			e, derr := decodeEdge(v)
			if derr != nil || e == nil {
				return nil
			}
			repo, ok := repoOf[e.From]
			if !ok {
				return nil
			}
			est := out[repo]
			est.EdgeCount++
			est.EdgeBytes += edgeBytesEstimate(e)
			out[repo] = est
			return nil
		})
	})
	return out
}

// Per-record byte estimates — these mirror the in-memory store's
// nodeBytes / edgeBytes (struct overhead + string lengths) so the
// numbers stay comparable. Internal helpers, not exported.
const (
	nodeStructOverheadEstimate = uint64(200)
	edgeStructOverheadEstimate = uint64(120)
)

func nodeBytesEstimate(n *graph.Node) uint64 {
	if n == nil {
		return 0
	}
	b := nodeStructOverheadEstimate
	b += uint64(len(n.ID) + len(n.Name) + len(n.QualName) + len(n.FilePath) + len(n.Language) + len(n.RepoPrefix))
	return b
}

func edgeBytesEstimate(e *graph.Edge) uint64 {
	if e == nil {
		return 0
	}
	b := edgeStructOverheadEstimate
	b += uint64(len(e.From) + len(e.To) + len(e.Kind) + len(e.FilePath))
	return b
}

// bumpEdgeIdentityRevisions increments the monotonic counter stored
// in the meta bucket.
func bumpEdgeIdentityRevisions(tx *bbolt.Tx) error {
	b := tx.Bucket(bucketMeta)
	raw := b.Get(metaKeyEdgeIdentityRevisions)
	var n uint64
	if len(raw) == 8 {
		n = binary.BigEndian.Uint64(raw)
	}
	n++
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], n)
	return b.Put(metaKeyEdgeIdentityRevisions, buf[:])
}
