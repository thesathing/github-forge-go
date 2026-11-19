// Pure git object, pack and pkt-line helpers. No I/O, standard library only.
// Every function here is deterministic: same bytes in, same bytes out.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

const ZeroOID = "0000000000000000000000000000000000000000"

type ObjType uint8

const (
	ObjCommit ObjType = 1
	ObjTree   ObjType = 2
	ObjBlob   ObjType = 3
	ObjTag    ObjType = 4
)

func (t ObjType) String() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	case ObjTag:
		return "tag"
	}
	return "unknown"
}

func typeCode(s string) (ObjType, bool) {
	switch s {
	case "commit":
		return ObjCommit, true
	case "tree":
		return ObjTree, true
	case "blob":
		return ObjBlob, true
	case "tag":
		return ObjTag, true
	}
	return 0, false
}

type GitObject struct {
	Type ObjType
	Data []byte
}

var hex40 = func() func(string) bool {
	return func(s string) bool {
		if len(s) != 40 {
			return false
		}
		for i := 0; i < 40; i++ {
			c := s[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
}()

func IsObjectID(v string) bool { return hex40(v) }

func HashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ.String(), len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------- trees

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("bad tree")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("bad tree")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("bad tree")
		}
		out = append(out, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return out, nil
}

func treeSortKey(e TreeEntry) []byte {
	if e.Mode == "40000" {
		return []byte(e.Name + "/")
	}
	return []byte(e.Name)
}

func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return bytes.Compare(treeSortKey(sorted[i]), treeSortKey(sorted[j])) < 0
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------- commits / tags

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func ParseCommit(data []byte) CommitView {
	var c CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
		c.Message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		var k, v string
		if sp < 0 {
			k = string(line)
		} else {
			k, v = string(line[:sp]), string(line[sp+1:])
		}
		switch k {
		case "tree":
			c.Tree = v
		case "parent":
			c.Parents = append(c.Parents, v)
		case "author":
			c.Author = v
		case "committer":
			c.Committer = v
		}
	}
	return c
}

func BuildCommit(tree string, parents []string, author, committer, message string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	fmt.Fprintf(&buf, "author %s\ncommitter %s\n\n%s", author, committer, message)
	return buf.Bytes()
}

type TagView struct {
	Object  string
	Type    ObjType
	Tag     string
	Tagger  string
	Message string
}

func ParseTag(data []byte) TagView {
	var t TagView
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
		t.Message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		k, v := string(line[:sp]), string(line[sp+1:])
		switch k {
		case "object":
			t.Object = v
		case "type":
			if tc, ok := typeCode(v); ok {
				t.Type = tc
			}
		case "tag":
			t.Tag = v
		case "tagger":
			t.Tagger = v
		}
	}
	if t.Type == 0 {
		t.Type = ObjCommit
	}
	return t
}

// ---------------------------------------------------------------- pkt-line

type Packet struct {
	Kind    byte // 'd' data, 'f' flush, 'e' delim, 'r' response-end
	Payload []byte
}

var (
	pktFlush = []byte("0000")
	pktDelim = []byte("0001")
)

func PktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	hexLen := fmt.Sprintf("%04x", len(payload)+4)
	copy(out, hexLen)
	copy(out[4:], payload)
	return out
}

func PktLineStr(s string) []byte { return PktLine([]byte(s)) }

func PktLines(items [][]byte) []byte {
	var buf bytes.Buffer
	for _, it := range items {
		buf.Write(it)
	}
	return buf.Bytes()
}

// DecodePktLines decodes a whole buffer of pkt-lines. Throws on bad framing.
func DecodePktLines(b []byte) ([]Packet, error) {
	var out []Packet
	off := 0
	for off < len(b) {
		if off+4 > len(b) {
			return nil, errors.New("truncated pkt-line")
		}
		head := string(b[off : off+4])
		var ln int
		if _, err := fmt.Sscanf(head, "%04x", &ln); err != nil {
			return nil, fmt.Errorf("bad pkt-line header %q", head)
		}
		// Sscanf %x accepts non-hex too; validate strictly.
		for _, c := range head {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return nil, fmt.Errorf("bad pkt-line header %q", head)
			}
		}
		switch ln {
		case 0:
			out = append(out, Packet{Kind: 'f'})
			off += 4
		case 1:
			out = append(out, Packet{Kind: 'e'})
			off += 4
		case 2:
			out = append(out, Packet{Kind: 'r'})
			off += 4
		default:
			if ln < 4 || off+ln > len(b) {
				return nil, errors.New("truncated pkt-line")
			}
			out = append(out, Packet{Kind: 'd', Payload: b[off+4 : off+ln]})
			off += ln
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- packfile

func readVarintSize(buf []byte, start int) (size int, first byte, next int, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack truncated")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	off := start + 1
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, 0, errors.New("pack truncated")
		}
		b = buf[off]
		off++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, first, off, nil
}

func readOfsBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("pack truncated")
	}
	b := buf[start]
	off := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, errors.New("pack truncated")
		}
		b = buf[off]
		off++
		value = ((value + 1) << 7) + int(b&0x7f)
	}
	return value, off, nil
}

// inflateAt inflates the zlib stream starting at offset (ending before end).
// Returns the data and the offset just past the stream.
func inflateAt(buf []byte, offset, end int) ([]byte, int, error) {
	if offset > end || offset > len(buf) {
		return nil, 0, errors.New("pack truncated")
	}
	limit := end
	if limit > len(buf) {
		limit = len(buf)
	}
	r := bytes.NewReader(buf[offset:limit])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := limit - offset - r.Len()
	return data, offset + consumed, nil
}

func ApplyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[off]
			off++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, tgtSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff, copySize := 0, 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copyOff += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || written+copySize > tgtSize {
				return nil, errors.New("delta copy out of range")
			}
			copy(out[written:], base[copyOff:copyOff+copySize])
			written += copySize
		} else if op != 0 {
			n := int(op & 0x7f)
			if off+n > len(delta) || written+n > tgtSize {
				return nil, errors.New("delta truncated")
			}
			copy(out[written:], delta[off:off+n])
			off += n
			written += n
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if written != tgtSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type PackRead struct {
	Objects map[string]GitObject
	Count   int
}

// ReadPack reads a packfile (v2/v3), resolving OFS_DELTA and REF_DELTA.
func ReadPack(pack []byte, resolveBase func(oid string) *GitObject) (*PackRead, error) {
	buf := pack
	if len(buf) < 32 || string(buf[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := uint32(buf[4])<<24 | uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(uint32(buf[8])<<24 | uint32(buf[9])<<16 | uint32(buf[10])<<8 | uint32(buf[11]))
	trailerStart := len(buf) - 20
	trailer := hex.EncodeToString(buf[trailerStart:])
	sum := sha1.Sum(buf[:trailerStart])
	if trailer != hex.EncodeToString(sum[:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	type raw struct {
		idx       int
		offset    int
		typeCode  int
		size      int
		data      []byte
		baseOff   int
		baseOID   string
		hasBaseOf bool
		hasBaseRf bool
	}
	raws := make([]*raw, 0, count)
	byOffset := make(map[int]*raw, count)
	off := 12
	for i := 0; i < count; i++ {
		entryOffset := off
		size, first, next, err := readVarintSize(buf, off)
		if err != nil {
			return nil, err
		}
		off = next
		tc := int((first >> 4) & 0x07)
		e := &raw{idx: i, offset: entryOffset, typeCode: tc, size: size}
		switch tc {
		case 6:
			base, next2, err := readOfsBase(buf, off)
			if err != nil {
				return nil, err
			}
			off = next2
			e.baseOff = entryOffset - base
			e.hasBaseOf = true
		case 7:
			if off+20 > len(buf) {
				return nil, errors.New("pack truncated")
			}
			e.baseOID = hex.EncodeToString(buf[off : off+20])
			e.hasBaseRf = true
			off += 20
		default:
			if tc < 1 || tc > 4 {
				return nil, fmt.Errorf("unknown pack type %d", tc)
			}
		}
		data, next3, err := inflateAt(buf, off, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		e.data = data
		off = next3
		raws = append(raws, e)
		byOffset[entryOffset] = e
	}
	if off != trailerStart {
		return nil, errors.New("pack trailing bytes")
	}
	objects := make(map[string]GitObject, count)
	type resolved struct {
		typ  ObjType
		data []byte
		oid  string
	}
	done := make(map[int]*resolved, count)
	byOID := make(map[string]int, count)
	resolving := make(map[int]bool, count)
	var resolve func(e *raw) (*resolved, error)
	resolve = func(e *raw) (*resolved, error) {
		if r, ok := done[e.offset]; ok {
			return r, nil
		}
		if resolving[e.offset] {
			return nil, errors.New("delta cycle")
		}
		resolving[e.offset] = true
		var typ ObjType
		var data []byte
		var err error
		switch {
		case e.hasBaseOf:
			baseEntry, ok := byOffset[e.baseOff]
			if !ok {
				return nil, errors.New("ofs-delta base missing")
			}
			base, err2 := resolve(baseEntry)
			if err2 != nil {
				return nil, err2
			}
			typ = base.typ
			data, err = ApplyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
		case e.hasBaseRf:
			if idx, ok := byOID[e.baseOID]; ok {
				base, err2 := resolve(raws[idx])
				if err2 != nil {
					return nil, err2
				}
				typ = base.typ
				data, err = ApplyDelta(base.data, e.data)
				if err != nil {
					return nil, err
				}
			} else {
				var ext *GitObject
				if resolveBase != nil {
					ext = resolveBase(e.baseOID)
				}
				if ext == nil {
					return nil, fmt.Errorf("ref-delta base %s missing", e.baseOID)
				}
				typ = ext.Type
				data, err = ApplyDelta(ext.Data, e.data)
				if err != nil {
					return nil, err
				}
			}
		default:
			typ = ObjType(e.typeCode)
			data = e.data
		}
		oid := HashObject(typ, data)
		r := &resolved{typ: typ, data: data, oid: oid}
		done[e.offset] = r
		byOID[oid] = e.idx
		delete(resolving, e.offset)
		return r, nil
	}
	for _, e := range raws {
		r, err := resolve(e)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[r.oid]; !ok {
			objects[r.oid] = GitObject{Type: r.typ, Data: r.data}
		}
	}
	return &PackRead{Objects: objects, Count: count}, nil
}

func writePackObjectHeader(typ ObjType, size int) []byte {
	code := int(typ)
	var out []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining /= 16
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining /= 128
	}
	out = append(out, first)
	return out
}

// Deflate cache: the clone workload sends the same objects to 50 concurrent
// clients; deflating once instead of 50x is a large win. Bounded, FIFO eviction.
var deflateCache = newDeflateCache(4096)

type deflateCacheT struct {
	mu    sync.Mutex
	m     map[string][]byte
	order []string
	max   int
}

func newDeflateCache(max int) *deflateCacheT {
	return &deflateCacheT{m: make(map[string][]byte), max: max}
}

func (c *deflateCacheT) get(oid string, data []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit, ok := c.m[oid]; ok {
		return hit
	}
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	out := buf.Bytes()
	if len(c.m) >= c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.m, oldest)
	}
	c.m[oid] = out
	c.order = append(c.order, oid)
	return out
}

// WritePack builds a plain-object packfile (no deltas).
func WritePack(objects []PackedObject) []byte {
	var body bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	hdr[4] = 0
	hdr[5] = 0
	hdr[6] = 0
	hdr[7] = 2
	n := len(objects)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	body.Write(hdr)
	for _, o := range objects {
		body.Write(writePackObjectHeader(o.Type, len(o.Data)))
		oid := o.OID
		if oid == "" {
			oid = HashObject(o.Type, o.Data)
		}
		body.Write(deflateCache.get(oid, o.Data))
	}
	out := body.Bytes()
	sum := sha1.Sum(out)
	out = append(out, sum[:]...)
	return out
}

type PackedObject struct {
	Type ObjType
	Data []byte
	OID  string
}

func PackChecksumOK(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	actual := hex.EncodeToString(pack[len(pack)-20:])
	sum := sha1.Sum(pack[:len(pack)-20])
	return actual == hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- reachability

type ObjectLookup func(oid string) *GitObject

// Reachable returns objects reachable from roots, with a commit-depth limit.
// Shallow holds boundary commits included at max depth that still have parents.
func Reachable(roots []string, lookup ObjectLookup, maxCommitDepth int) (objects map[string]bool, shallow []string) {
	seen := make(map[string]bool)
	type item struct {
		oid   string
		depth int
	}
	var queue []item
	for _, r := range roots {
		if IsObjectID(r) {
			queue = append(queue, item{r, 0})
		}
	}
	for len(queue) > 0 {
		it := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		oid, depth := it.oid, it.depth
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			c := ParseCommit(obj.Data)
			seen[c.Tree] = true
			if treeObj := lookup(c.Tree); treeObj != nil {
				collectTree(treeObj.Data, lookup, seen)
			}
			if depth+1 < maxCommitDepth {
				for _, p := range c.Parents {
					queue = append(queue, item{p, depth + 1})
				}
			} else if len(c.Parents) > 0 {
				shallow = append(shallow, oid)
			}
		case ObjTag:
			t := ParseTag(obj.Data)
			queue = append(queue, item{t.Object, depth})
		case ObjTree:
			collectTree(obj.Data, lookup, seen)
		}
	}
	return seen, shallow
}

func collectTree(data []byte, lookup ObjectLookup, seen map[string]bool) {
	entries, err := ParseTree(data)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Mode == "160000" {
			continue
		}
		if seen[e.OID] {
			continue
		}
		obj := lookup(e.OID)
		if obj == nil {
			continue
		}
		seen[e.OID] = true
		if obj.Type == ObjTree {
			collectTree(obj.Data, lookup, seen)
		}
	}
}

func Closure(roots []string, lookup ObjectLookup) map[string]bool {
	objs, _ := Reachable(roots, lookup, int(^uint(0)>>1))
	return objs
}

func IsAncestor(ancestor, tip string, lookup ObjectLookup) bool {
	if ancestor == tip {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			stack = append(stack, ParseCommit(obj.Data).Parents...)
		case ObjTag:
			stack = append(stack, ParseTag(obj.Data).Object)
		}
	}
	return false
}

// FlattenTree flattens a tree into path -> {oid, mode}.
func FlattenTree(treeOID string, lookup ObjectLookup) map[string]FileEntry {
	out := make(map[string]FileEntry)
	flattenTreeInto(treeOID, lookup, out, "")
	return out
}

func flattenTreeInto(treeOID string, lookup ObjectLookup, out map[string]FileEntry, prefix string) {
	obj := lookup(treeOID)
	if obj == nil || obj.Type != ObjTree {
		return
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			flattenTreeInto(e.OID, lookup, out, path)
		} else {
			out[path] = FileEntry{OID: e.OID, Mode: e.Mode}
		}
	}
}

type FileEntry struct {
	OID  string
	Mode string
}

// BuildTreeFromFiles builds trees from a flat file map; returns the root tree oid.
func BuildTreeFromFiles(files map[string]FileEntry, put func(typ ObjType, data []byte) string) string {
	type node struct {
		files map[string]FileEntry
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: map[string]FileEntry{}, dirs: map[string]*node{}} }
	root := mk()
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, path := range keys {
		entry := files[path]
		segs := splitPath(path)
		n := root
		for i := 0; i < len(segs)-1; i++ {
			name := segs[i]
			child, ok := n.dirs[name]
			if !ok {
				child = mk()
				n.dirs[name] = child
			}
			n = child
		}
		n.files[segs[len(segs)-1]] = entry
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []TreeEntry
		for name, e := range n.files {
			entries = append(entries, TreeEntry{Mode: e.Mode, OID: e.OID, Name: name})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", OID: build(child), Name: name})
		}
		return put(ObjTree, BuildTree(entries))
	}
	return build(root)
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	out = append(out, p[start:])
	return out
}
