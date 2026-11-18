// git.go — the git object model and wire formats for the forge.
// SHA-1 objects, trees/commits/tags, packfiles (plain objects, no deltas),
// pkt-line framing and sideband chunking. Every byte mirrors the git wire
// format, so the conformance client can parse every byte this server
// produces, and vice versa.
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
	"strings"
)

type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

type GitObject struct {
	Type ObjectType
	Data []byte
}

const ZeroOID = "0000000000000000000000000000000000000000"

const ConformanceIdentity = "Conformance <conformance@make.fun>"

func isObjectID(s string) bool {
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

// hashObject is SHA-1 over "<type> <size>\0" + the object's bytes.
func hashObject(typ ObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree splits a tree object into its entries, in stored order.
func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries, nil
}

func treeSortKey(mode, name string) string {
	if mode == "40000" {
		return name + "/"
	}
	return name
}

// buildTree serializes entries, sorted the way git sorts them.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i].Mode, sorted[i].Name) < treeSortKey(sorted[j].Mode, sorted[j].Name)
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// parseCommit parses a commit object; it refuses a malformed header layout.
func parseCommit(data []byte) (CommitView, error) {
	var out CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		out.Message = string(data[blank+2:])
	}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		key, value := line, ""
		if sp >= 0 {
			key, value = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			out.Tree = value
		case "parent":
			out.Parents = append(out.Parents, value)
		case "author":
			out.Author = value
		case "committer":
			out.Committer = value
		}
	}
	if !isObjectID(out.Tree) {
		return out, errors.New("commit has no valid tree header")
	}
	for _, p := range out.Parents {
		if !isObjectID(p) {
			return out, errors.New("commit has a bad parent id")
		}
	}
	return out, nil
}

type TagView struct {
	Object  string
	Type    ObjectType
	Tag     string
	Tagger  string
	Message string
}

// parseTag parses an annotated tag object.
func parseTag(data []byte) (TagView, error) {
	var out TagView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		out.Message = string(data[blank+2:])
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			return out, fmt.Errorf("tag header %q has no value", line)
		}
		headers[line[:sp]] = line[sp+1:]
	}
	out.Object = headers["object"]
	typ := headers["type"]
	if !isObjectID(out.Object) {
		return out, errors.New("tag has no valid object header")
	}
	switch ObjectType(typ) {
	case ObjCommit, ObjTree, ObjBlob, ObjTag:
		out.Type = ObjectType(typ)
	default:
		return out, fmt.Errorf("tag has bad type %q", typ)
	}
	out.Tag = headers["tag"]
	out.Tagger = headers["tagger"]
	return out, nil
}

type BuildCommitFields struct {
	Tree      string
	Parents   []string
	Message   string
	Author    string
	Committer string
	Timestamp int64
	Timezone  string
}

// buildCommit builds a commit object's bytes (deterministic: fixed identity
// and timestamp).
func buildCommit(f BuildCommitFields) []byte {
	ts := f.Timestamp
	if ts == 0 {
		ts = 1700000000
	}
	tz := f.Timezone
	if tz == "" {
		tz = "+0000"
	}
	author := f.Author
	if author == "" {
		author = ConformanceIdentity
	}
	committer := f.Committer
	if committer == "" {
		committer = ConformanceIdentity
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "tree %s\n", f.Tree)
	for _, p := range f.Parents {
		fmt.Fprintf(&sb, "parent %s\n", p)
	}
	fmt.Fprintf(&sb, "author %s %d %s\n", author, ts, tz)
	fmt.Fprintf(&sb, "committer %s %d %s\n", committer, ts, tz)
	sb.WriteString("\n")
	sb.WriteString(f.Message)
	return []byte(sb.String())
}

// ---------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------

type ObjectLookup func(oid string) *GitObject

// collectReachable returns every object reachable from roots: commit -> tree
// + parents, tree -> entries (gitlinks skipped), tag -> its target,
// blob -> nothing.
func collectReachable(roots []string, lookup ObjectLookup, allowMissing bool) (map[string]bool, error) {
	seen := map[string]bool{}
	var queue []string
	for _, r := range roots {
		if isObjectID(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			c, err := parseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case ObjTag:
			t, err := parseTag(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, t.Object)
		case ObjTree:
			entries, err := parseTree(obj.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, e.OID)
			}
		}
	}
	return seen, nil
}

// collectReachableDepth is reachability with a commit-depth limit: depth
// counts commits from the roots (depth 1 = only the root commits, their trees
// and blobs, no parents). It returns the included object ids and the shallow
// boundary (included commits at exactly depth whose parents were left out).
func collectReachableDepth(roots []string, lookup ObjectLookup, depth int) (map[string]bool, []string) {
	included := map[string]bool{}
	var boundary []string
	type item struct {
		oid   string
		level int
	}
	var queue []item
	for _, r := range roots {
		if isObjectID(r) {
			queue = append(queue, item{r, 1})
		}
	}
	for len(queue) > 0 {
		it := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if included[it.oid] {
			continue
		}
		obj := lookup(it.oid)
		if obj == nil {
			continue
		}
		included[it.oid] = true
		switch obj.Type {
		case ObjCommit:
			c, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			if !included[c.Tree] {
				queue = append(queue, item{c.Tree, it.level})
			}
			if it.level < depth {
				for _, p := range c.Parents {
					queue = append(queue, item{p, it.level + 1})
				}
			} else if len(c.Parents) > 0 {
				boundary = append(boundary, it.oid)
			}
		case ObjTag:
			t, err := parseTag(obj.Data)
			if err != nil {
				continue
			}
			queue = append(queue, item{t.Object, it.level})
		case ObjTree:
			entries, err := parseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				if !included[e.OID] {
					queue = append(queue, item{e.OID, it.level})
				}
			}
		}
	}
	return included, boundary
}

// isAncestor is true when ancestor is reachable from tip (or equals it).
func isAncestor(ancestor, tip string, lookup ObjectLookup) bool {
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
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
			c, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			for _, p := range c.Parents {
				if p == ancestor {
					return true
				}
				queue = append(queue, p)
			}
		case ObjTag:
			t, err := parseTag(obj.Data)
			if err != nil {
				continue
			}
			queue = append(queue, t.Object)
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// packfiles (version 2, plain objects, no deltas)
// ---------------------------------------------------------------------------

var typeCode = map[ObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]ObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func writePackObjectHeader(typ ObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining >>= 4
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining >>= 7
	}
	out = append(out, first)
	return out
}

// writePack builds a packfile from plain objects (no deltas). The trailer is
// SHA-1 over all prior bytes.
func writePack(objects []GitObject) []byte {
	var body bytes.Buffer
	body.WriteString("PACK")
	body.Write([]byte{0, 0, 0, 2})
	body.Write([]byte{byte(len(objects) >> 24), byte(len(objects) >> 16), byte(len(objects) >> 8), byte(len(objects))})
	for _, obj := range objects {
		body.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		zw.Write(obj.Data)
		zw.Close()
		body.Write(zb.Bytes())
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

// inflateAt decompresses one zlib stream starting at offset and reports the
// bytes and the offset just past the stream. It relies on bytes.Reader being
// an io.ByteReader, so the flate decoder consumes exactly the stream's bytes.
func inflateAt(pack []byte, offset int, limit int) ([]byte, int, error) {
	br := bytes.NewReader(pack[offset:limit])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		zr.Close()
		return nil, 0, err
	}
	zr.Close()
	consumed := (limit - offset) - br.Len()
	return data, offset + consumed, nil
}

func readVarintSize(pack []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(pack) {
		return 0, 0, 0, errors.New("pack varint out of range")
	}
	b := pack[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	off := start + 1
	for b&0x80 != 0 {
		if off >= len(pack) {
			return 0, 0, 0, errors.New("pack varint out of range")
		}
		b = pack[off]
		off++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, off, first, nil
}

func readOfsDeltaBase(pack []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(pack) {
		return 0, 0, errors.New("pack ofs-delta out of range")
	}
	b := pack[start]
	off := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if off >= len(pack) {
			return 0, 0, errors.New("pack ofs-delta out of range")
		}
		b = pack[off]
		off++
		value = ((value + 1) << 7) + int(b&0x7f)
	}
	return value, off, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size truncated")
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
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOffset, copySize := 0, 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					copyOffset += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			lit := int(op & 0x7f)
			copy(out[written:written+lit], delta[off:off+lit])
			off += lit
			written += lit
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

type packEntry struct {
	offset    int
	typeCode  int
	size      int
	data      []byte
	baseOff   int
	baseOID   string
	resolved  []byte
	resType   ObjectType
	oid       string
	hasRes    bool
}

// readPack reads a packfile (version 2 or 3), resolving OFS_DELTA and
// REF_DELTA entries. resolveBase supplies a base object a thin pack leaves out.
func readPack(pack []byte, resolveBase ObjectLookup) (map[string]GitObject, int, error) {
	if len(pack) < 32 {
		return nil, 0, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[0:4]) != "PACK" {
		return nil, 0, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, 0, fmt.Errorf("unsupported pack version %d", version)
	}
	declared := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	sum := sha1.Sum(pack[:trailerStart])
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(pack[trailerStart:]) {
		return nil, 0, errors.New("pack trailer does not match its bytes")
	}

	raw := make([]*packEntry, 0, declared)
	byOffset := map[int]*packEntry{}
	offset := 12
	for i := 0; i < declared; i++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return nil, 0, err
		}
		offset = next
		tc := int((first >> 4) & 0x07)
		entry := &packEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, next2, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, 0, err
			}
			offset = next2
			entry.baseOff = entryOffset - base
		} else if tc == 7 {
			if offset+20 > len(pack) {
				return nil, 0, errors.New("pack ref-delta out of range")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if codeType[tc] == "" {
			return nil, 0, fmt.Errorf("pack entry %d has unknown type code %d", i, tc)
		}
		data, next3, err := inflateAt(pack, offset, trailerStart)
		if err != nil {
			return nil, 0, err
		}
		if len(data) != size {
			return nil, 0, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(data))
		}
		entry.data = data
		offset = next3
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, 0, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	objects := map[string]GitObject{}
	byOID := map[string]*packEntry{}
	resolving := map[int]bool{}
	deltaCount := 0

	var resolve func(e *packEntry) (ObjectType, []byte, string, error)
	resolve = func(e *packEntry) (ObjectType, []byte, string, error) {
		if e.hasRes {
			return e.resType, e.resolved, e.oid, nil
		}
		if resolving[e.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", e.offset)
		}
		resolving[e.offset] = true
		var typ ObjectType
		var data []byte
		if e.typeCode == 6 {
			deltaCount++
			baseEntry, ok := byOffset[e.baseOff]
			if !ok {
				return "", nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", e.baseOff)
			}
			bt, bd, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = bt
			data, err = applyDelta(bd, e.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if e.typeCode == 7 {
			deltaCount++
			if baseEntry, ok := byOID[e.baseOID]; ok {
				bt, bd, _, err := resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = bt
				data, err = applyDelta(bd, e.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				baseObj := resolveBase(e.baseOID)
				if baseObj == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is not in the pack or the store", e.baseOID)
				}
				typ = baseObj.Type
				var err error
				data, err = applyDelta(baseObj.Data, e.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			typ = codeType[e.typeCode]
			data = e.data
		}
		oid := hashObject(typ, data)
		e.resolved = data
		e.resType = typ
		e.oid = oid
		e.hasRes = true
		delete(resolving, e.offset)
		byOID[oid] = e
		return typ, data, oid, nil
	}

	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, 0, err
		}
		objects[oid] = GitObject{Type: typ, Data: data}
	}
	return objects, deltaCount, nil
}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")
var pktEnd = []byte("0002")

// pktLine is one pkt-line carrying a text payload.
func pktLine(text string) []byte {
	payload := []byte(text)
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	out = append(out, payload...)
	return out
}

type pktItem struct {
	kind string // "data", "flush", "delim", "end"
	text string
}

func pktFlushItem() pktItem  { return pktItem{kind: "flush"} }
func pktDelimItem() pktItem  { return pktItem{kind: "delim"} }
func pktData(text string) pktItem { return pktItem{kind: "data", text: text} }

// encodePktLines encodes a list of text lines / flush / delim into a stream.
func encodePktLines(items []pktItem) []byte {
	var out []byte
	for _, it := range items {
		switch it.kind {
		case "flush":
			out = append(out, pktFlush...)
		case "delim":
			out = append(out, pktDelim...)
		case "end":
			out = append(out, pktEnd...)
		default:
			out = append(out, pktLine(it.text)...)
		}
	}
	return out
}

type GitPacket struct {
	Kind    string // "data", "flush", "delim", "end"
	Payload []byte
}

// decodePktLines decodes a complete pkt-line stream.
func decodePktLines(b []byte) ([]GitPacket, error) {
	var packets []GitPacket
	off := 0
	for off < len(b) {
		if off+4 > len(b) {
			return nil, errors.New("pkt-line is truncated")
		}
		length, err := strconvParseHex4(b[off : off+4])
		if err != nil || length < 0 {
			return nil, errors.New("pkt-line has a bad length prefix")
		}
		switch length {
		case 0:
			packets = append(packets, GitPacket{Kind: "flush"})
			off += 4
		case 1:
			packets = append(packets, GitPacket{Kind: "delim"})
			off += 4
		case 2:
			packets = append(packets, GitPacket{Kind: "end"})
			off += 4
		default:
			if off+length > len(b) {
				return nil, errors.New("pkt-line is truncated")
			}
			packets = append(packets, GitPacket{Kind: "data", Payload: b[off+4 : off+length]})
			off += length
		}
	}
	return packets, nil
}

func strconvParseHex4(b []byte) (int, error) {
	n := 0
	for _, c := range b {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n |= int(c-'A') + 10
		default:
			return 0, errors.New("bad hex")
		}
	}
	return n, nil
}

// sidebandChunks wraps raw bytes in sideband pkt-lines: each packet is
// length + band byte + data, chunked so no packet exceeds 65520 bytes.
const sidebandChunk = 65515

func sidebandChunks(band byte, data []byte) [][]byte {
	var chunks [][]byte
	if len(data) == 0 {
		prefix := []byte(fmt.Sprintf("%04x", 5))
		chunks = append(chunks, append(prefix, band))
		return chunks
	}
	for off := 0; off < len(data); {
		end := off + sidebandChunk
		if end > len(data) {
			end = len(data)
		}
		slice := data[off:end]
		prefix := []byte(fmt.Sprintf("%04x", len(slice)+5))
		chunk := make([]byte, 0, len(slice)+5)
		chunk = append(chunk, prefix...)
		chunk = append(chunk, band)
		chunk = append(chunk, slice...)
		chunks = append(chunks, chunk)
		off = end
	}
	return chunks
}
