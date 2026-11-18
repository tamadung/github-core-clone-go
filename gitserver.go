// gitserver.go — the git smart-HTTP surface: ref advertisement (protocol v0
// and v2), upload-pack (fetch, ls-refs, shallow) and receive-pack (push with
// report-status). Every byte follows the git wire format the conformance
// client parses.
package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const agentName = "forge/1.0"

const uploadV0Caps = "symref=HEAD:refs/heads/main multi_ack_detailed side-band-64k thin-pack ofs-delta shallow agent=forge/1.0"
const receiveV0Caps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"

func v2Advertisement() []byte {
	return encodePktLines([]pktItem{
		pktData("version 2\n"),
		pktData("agent=" + agentName + "\n"),
		pktData("ls-refs=unborn\n"),
		pktData("fetch=shallow\n"),
		pktData("object-format=sha1\n"),
		pktFlushItem(),
	})
}

func sortedRefs(repo *Repo) []struct {
	Name string
	OID  string
} {
	out := make([]struct {
		Name string
		OID  string
	}, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, struct {
			Name string
			OID  string
		}{name, oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// peeledTarget resolves a tag object to its ultimate commit, or "" when it is
// not a tag.
func peeledTarget(repo *Repo, oid string) string {
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjTag {
		return ""
	}
	tag, err := parseTag(object.Data)
	if err != nil {
		return ""
	}
	current := tag.Object
	for i := 0; i < 16; i++ {
		next, ok := repo.Objects[current]
		if !ok {
			return ""
		}
		if next.Type == ObjCommit {
			return current
		}
		if next.Type == ObjTag {
			t, err := parseTag(next.Data)
			if err != nil {
				return ""
			}
			current = t.Object
			continue
		}
		return ""
	}
	return ""
}

// v0Advertisement is a v0 ref advertisement for one service, with the
// `# service=` preamble.
func v0Advertisement(repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = uploadV0Caps
	} else {
		caps = receiveV0Caps
	}
	var items []pktItem
	items = append(items, pktData("# service="+service+"\n"))
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		items = append(items, pktData(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		head := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if head == "" {
			head = refs[0].OID
		}
		items = append(items, pktData(head+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			items = append(items, pktData(ref.OID+" "+ref.Name+"\n"))
			if strings.HasPrefix(ref.Name, "refs/tags/") {
				if peeled := peeledTarget(repo, ref.OID); peeled != "" {
					items = append(items, pktData(peeled+" "+ref.Name+"^{}\n"))
				}
			}
		}
	}
	items = append(items, pktFlushItem())
	return encodePktLines(items)
}

func jsonNotFound() *Response {
	return &Response{Status: 404, ContentType: "application/json", Body: []byte(`{"message":"Not Found"}`)}
}

// handleInfoRefs serves GET /{owner}/{repo}.git/info/refs?service=...
func handleInfoRefs(store *Store, owner, repoName, service, gitProtocol string) *Response {
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return jsonNotFound()
	}
	repo := store.findRepo(owner, repoName)
	if repo == nil {
		return jsonNotFound()
	}
	if service == "git-upload-pack" && gitProtocol == "version=2" {
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v2Advertisement()}
	}
	var cache []byte
	if service == "git-upload-pack" {
		cache = repo.AdvV0Up
	} else {
		cache = repo.AdvRecv
	}
	if cache != nil {
		return &Response{Status: 200, ContentType: "application/x-" + service + "-advertisement", Body: cache}
	}
	body := v0Advertisement(repo, service)
	if service == "git-upload-pack" {
		repo.AdvV0Up = body
	} else {
		repo.AdvRecv = body
	}
	return &Response{Status: 200, ContentType: "application/x-" + service + "-advertisement", Body: body}
}

// ---------------------------------------------------------------------------
// upload-pack v2: ls-refs and fetch
// ---------------------------------------------------------------------------

type v2Request struct {
	command string
	args    []string
}

func parseV2Body(body []byte) (v2Request, error) {
	packets, err := decodePktLines(body)
	if err != nil {
		return v2Request{}, err
	}
	var req v2Request
	afterDelim := false
	for _, packet := range packets {
		if packet.Kind == "delim" {
			afterDelim = true
			continue
		}
		if packet.Kind == "flush" {
			break
		}
		if packet.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		if !afterDelim {
			if strings.HasPrefix(line, "command=") {
				req.command = line[len("command="):]
			}
		} else {
			req.args = append(req.args, line)
		}
	}
	return req, nil
}

func lsRefsResponse(repo *Repo, args []string) []byte {
	cacheKey := strings.Join(args, "|")
	if cached, ok := repo.LsRefsCache[cacheKey]; ok {
		return cached
	}
	peel := containsArg(args, "peel")
	symrefs := containsArg(args, "symrefs")
	unborn := containsArg(args, "unborn")
	var prefixes []string
	for _, a := range args {
		if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var items []pktItem
	headOID := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if keep("HEAD") {
		if headOID != "" {
			if symrefs {
				items = append(items, pktData(headOID+" HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
			} else {
				items = append(items, pktData(headOID+" HEAD\n"))
			}
		} else if unborn {
			if symrefs {
				items = append(items, pktData("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
			} else {
				items = append(items, pktData("unborn HEAD\n"))
			}
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if peel && strings.HasPrefix(ref.Name, "refs/tags/") {
			if peeled := peeledTarget(repo, ref.OID); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		items = append(items, pktData(line+"\n"))
	}
	items = append(items, pktFlushItem())
	out := encodePktLines(items)
	repo.LsRefsCache[cacheKey] = out
	return out
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

type fetchPlan struct {
	wants      []string
	wantRefs   []struct{ ref, oid string }
	haves      []string
	oldShallow []string
	depth      int // 0 means no deepen
}

func planFetch(repo *Repo, args []string) fetchPlan {
	var plan fetchPlan
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			oid := strings.TrimSpace(arg[5:])
			if isObjectID(oid) {
				plan.wants = append(plan.wants, oid)
			}
		case strings.HasPrefix(arg, "want-ref "):
			ref := strings.TrimSpace(arg[len("want-ref "):])
			if oid, ok := repo.Refs[ref]; ok {
				plan.wantRefs = append(plan.wantRefs, struct{ ref, oid string }{ref, oid})
				plan.wants = append(plan.wants, oid)
			}
		case strings.HasPrefix(arg, "have "):
			oid := strings.TrimSpace(arg[5:])
			if isObjectID(oid) {
				plan.haves = append(plan.haves, oid)
			}
		case strings.HasPrefix(arg, "shallow "):
			oid := strings.TrimSpace(arg[8:])
			if isObjectID(oid) {
				plan.oldShallow = append(plan.oldShallow, oid)
			}
		case strings.HasPrefix(arg, "deepen "):
			n, err := strconv.Atoi(strings.TrimSpace(arg[7:]))
			if err == nil && n > 0 {
				plan.depth = n
			}
		}
	}
	return plan
}

// collectHeld is the objects the client already holds, from its `have` lines —
// but a shallow clone holds only the boundary commits themselves (plus their
// trees and blobs), never their parents. `stopAt` names those boundary
// commits, so a deepen fetch still delivers the parent commits it asks for.
func collectHeld(repo *Repo, haves []string, stopAt map[string]bool) map[string]bool {
	held := map[string]bool{}
	queue := append([]string{}, haves...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if held[oid] {
			continue
		}
		object, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		held[oid] = true
		switch object.Type {
		case ObjCommit:
			commit, err := parseCommit(object.Data)
			if err != nil {
				continue
			}
			queue = append(queue, commit.Tree)
			if !stopAt[oid] {
				queue = append(queue, commit.Parents...)
			}
		case ObjTag:
			tag, err := parseTag(object.Data)
			if err != nil {
				continue
			}
			queue = append(queue, tag.Object)
		case ObjTree:
			entries, err := parseTree(object.Data)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.Mode != "160000" {
					queue = append(queue, entry.OID)
				}
			}
		}
	}
	return held
}

func fetchPackSet(repo *Repo, plan fetchPlan) (objects []GitObject, boundary, unshallow []string) {
	lookup := func(oid string) *GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			o := obj
			return &o
		}
		return nil
	}
	var included map[string]bool
	if plan.depth != 0 {
		var b []string
		included, b = collectReachableDepth(plan.wants, lookup, plan.depth)
		boundary = b
	} else {
		var err error
		included, err = collectReachable(plan.wants, lookup, true)
		if err != nil {
			included = map[string]bool{}
		}
	}
	if len(plan.haves) > 0 {
		stopAt := map[string]bool{}
		for _, o := range plan.oldShallow {
			stopAt[o] = true
		}
		held := collectHeld(repo, plan.haves, stopAt)
		for oid := range held {
			delete(included, oid)
		}
	}
	for oid := range included {
		if object, ok := repo.Objects[oid]; ok {
			objects = append(objects, object)
		}
	}
	if len(plan.oldShallow) > 0 {
		boundarySet := map[string]bool{}
		for _, b := range boundary {
			boundarySet[b] = true
		}
		for _, old := range plan.oldShallow {
			if included[old] && !boundarySet[old] {
				unshallow = append(unshallow, old)
			}
		}
	}
	return objects, boundary, unshallow
}

// cachedFetch is one fetch plan's full result: the pack plus the shallow and
// unshallow lines the response must repeat. Caching the whole result (not
// just the pack) lets a repeated fetch skip the graph walk in fetchPackSet
// entirely — clones of the same tip repeat, so the walk is pure waste.
type cachedFetch struct {
	pack      []byte
	boundary  []string
	unshallow []string
}

// fetchCacheKey identifies a fetch plan: the wanted tips, depth, and what the
// client already holds. Sorted so the same plan always keys the same way.
func fetchCacheKey(plan fetchPlan) string {
	wants := append([]string{}, plan.wants...)
	sort.Strings(wants)
	haves := append([]string{}, plan.haves...)
	sort.Strings(haves)
	shallow := append([]string{}, plan.oldShallow...)
	sort.Strings(shallow)
	return fmt.Sprintf("%s|%d|%s|%s", strings.Join(wants, ","), plan.depth, strings.Join(haves, ","), strings.Join(shallow, ","))
}

// fetchCached returns the pack, boundary, and unshallow lines for a plan,
// computing them (graph walk + pack write) only on a cache miss. A cache hit
// skips fetchPackSet entirely.
func fetchCached(repo *Repo, plan fetchPlan) (pack []byte, boundary, unshallow []string) {
	key := fetchCacheKey(plan)
	if c, ok := repo.ClonePack[key]; ok {
		return c.pack, c.boundary, c.unshallow
	}
	objects, boundary, unshallow := fetchPackSet(repo, plan)
	pack = writePack(objects)
	repo.ClonePack[key] = cachedFetch{pack: pack, boundary: boundary, unshallow: unshallow}
	return pack, boundary, unshallow
}

func fetchV2Response(repo *Repo, plan fetchPlan) []byte {
	pack, boundary, unshallow := fetchCached(repo, plan)
	var parts [][]byte
	var ackLines []pktItem
	ackLines = append(ackLines, pktData("acknowledgments\n"))
	if len(plan.haves) > 0 {
		acked := false
		for _, have := range plan.haves {
			if _, ok := repo.Objects[have]; ok {
				ackLines = append(ackLines, pktData("ACK "+have+"\n"))
				acked = true
			}
		}
		if !acked {
			ackLines = append(ackLines, pktData("NAK\n"))
		} else {
			ackLines = append(ackLines, pktData("ready\n"))
		}
	} else {
		ackLines = append(ackLines, pktData("NAK\n"))
	}
	ackLines = append(ackLines, pktDelimItem())
	parts = append(parts, encodePktLines(ackLines))
	if len(plan.wantRefs) > 0 {
		var lines []pktItem
		lines = append(lines, pktData("wanted-refs\n"))
		for _, w := range plan.wantRefs {
			lines = append(lines, pktData(w.oid+" "+w.ref+"\n"))
		}
		lines = append(lines, pktDelimItem())
		parts = append(parts, encodePktLines(lines))
	}
	if plan.depth != 0 || len(unshallow) > 0 {
		var lines []pktItem
		lines = append(lines, pktData("shallow-info\n"))
		for _, oid := range boundary {
			lines = append(lines, pktData("shallow "+oid+"\n"))
		}
		for _, oid := range unshallow {
			lines = append(lines, pktData("unshallow "+oid+"\n"))
		}
		lines = append(lines, pktDelimItem())
		parts = append(parts, encodePktLines(lines))
	}
	parts = append(parts, encodePktLines([]pktItem{pktData("packfile\n")}))
	for _, chunk := range sidebandChunks(1, pack) {
		parts = append(parts, chunk)
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func fetchV0Response(repo *Repo, plan fetchPlan) []byte {
	pack, boundary, unshallow := fetchCached(repo, plan)
	var parts [][]byte
	var ackLines []pktItem
	if len(plan.haves) > 0 {
		acked := false
		for _, have := range plan.haves {
			if _, ok := repo.Objects[have]; ok {
				ackLines = append(ackLines, pktData("ACK "+have+"\n"))
				acked = true
			}
		}
		if !acked {
			ackLines = append(ackLines, pktData("NAK\n"))
		}
	} else {
		ackLines = append(ackLines, pktData("NAK\n"))
	}
	for _, oid := range boundary {
		ackLines = append(ackLines, pktData("shallow "+oid+"\n"))
	}
	for _, oid := range unshallow {
		ackLines = append(ackLines, pktData("unshallow "+oid+"\n"))
	}
	parts = append(parts, encodePktLines(ackLines))
	for _, chunk := range sidebandChunks(1, pack) {
		parts = append(parts, chunk)
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func handleUploadPack(store *Store, owner, repoName string, body []byte, gitProtocol string) *Response {
	repo := store.findRepo(owner, repoName)
	if repo == nil {
		return jsonNotFound()
	}
	packets, err := decodePktLines(body)
	if err != nil {
		return &Response{Status: 400, ContentType: "text/plain", Body: []byte("malformed pkt-line stream")}
	}
	firstText := ""
	for _, p := range packets {
		if p.Kind == "data" {
			firstText = string(p.Payload)
			break
		}
	}
	isV2 := gitProtocol == "version=2" || strings.HasPrefix(firstText, "command=")
	if isV2 {
		request, err := parseV2Body(body)
		if err != nil {
			return &Response{Status: 400, ContentType: "text/plain", Body: []byte("malformed pkt-line stream")}
		}
		if request.command == "ls-refs" {
			out := lsRefsResponse(repo, request.args)
			return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out}
		}
		if request.command == "fetch" {
			plan := planFetch(repo, request.args)
			out := fetchV2Response(repo, plan)
			return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out}
		}
		return &Response{Status: 400, ContentType: "text/plain", Body: []byte("unknown command " + request.command)}
	}
	// Protocol v0 fetch: want/have/deepen/shallow lines in one or two sections.
	var args []string
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		args = append(args, strings.TrimSuffix(string(packet.Payload), "\n"))
	}
	plan := planFetch(repo, args)
	out := fetchV0Response(repo, plan)
	return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out}
}

// ---------------------------------------------------------------------------
// receive-pack (push)
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func handleReceivePack(store *Store, owner, repoName string, body []byte) *Response {
	repo := store.findRepo(owner, repoName)
	if repo == nil {
		return jsonNotFound()
	}
	// Split the body: pkt-line command section (up to the first flush), then the pack.
	packStart := len(body)
	offset := 0
	var commandLines []string
	malformed := false
	for offset < len(body) {
		if offset+4 > len(body) {
			malformed = true
			break
		}
		length, err := strconvParseHex4(body[offset : offset+4])
		if err != nil || length < 0 {
			malformed = true
			break
		}
		if length == 0 {
			packStart = offset + 4
			break
		}
		if offset+length > len(body) {
			malformed = true
			break
		}
		commandLines = append(commandLines, strings.TrimSuffix(string(body[offset+4:offset+length]), "\n"))
		offset += length
	}

	var commands []pushCommand
	sideband := false
	if !malformed {
		for i, line := range commandLines {
			if i == 0 {
				if nul := strings.IndexByte(line, 0); nul >= 0 {
					caps := line[nul+1:]
					for _, cap := range strings.Split(caps, " ") {
						if cap == "side-band-64k" {
							sideband = true
						}
					}
					line = line[:nul]
				}
			}
			parts := strings.Split(line, " ")
			if len(parts) != 3 || !isObjectID(parts[0]) || !isObjectID(parts[1]) {
				malformed = true
				break
			}
			commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		}
	}

	var reportLines []pktItem
	unpackOK := true
	if malformed || len(commands) == 0 {
		unpackOK = false
		reportLines = append(reportLines, pktData("unpack error\n"))
		for _, command := range commands {
			reportLines = append(reportLines, pktData("ng "+command.ref+" unpacker error\n"))
		}
	} else {
		packBytes := body[packStart:]
		needsPack := false
		for _, c := range commands {
			if c.newOID != ZeroOID {
				needsPack = true
			}
		}
		if needsPack && len(packBytes) > 0 {
			lookup := func(oid string) *GitObject {
				if obj, ok := repo.Objects[oid]; ok {
					o := obj
					return &o
				}
				return nil
			}
			objects, _, err := readPack(packBytes, lookup)
			if err != nil {
				unpackOK = false
				reportLines = append(reportLines, pktData("unpack error\n"))
				for _, command := range commands {
					reportLines = append(reportLines, pktData("ng "+command.ref+" unpacker error\n"))
				}
			} else {
				for oid, object := range objects {
					repo.Objects[oid] = object
				}
			}
		}
		if unpackOK {
			reportLines = append(reportLines, pktData("unpack ok\n"))
			changed := false
			for _, command := range commands {
				current, hasCurrent := repo.Refs[command.ref]
				if command.newOID == ZeroOID {
					// Delete.
					if !hasCurrent {
						reportLines = append(reportLines, pktData("ng "+command.ref+" remote ref does not exist\n"))
					} else if command.oldOID != ZeroOID && command.oldOID != current {
						reportLines = append(reportLines, pktData("ng "+command.ref+" non-fast-forward\n"))
					} else {
						delete(repo.Refs, command.ref)
						changed = true
						reportLines = append(reportLines, pktData("ok "+command.ref+"\n"))
					}
					continue
				}
				target, ok := repo.Objects[command.newOID]
				if !ok {
					reportLines = append(reportLines, pktData("ng "+command.ref+" object not found\n"))
					continue
				}
				if strings.HasPrefix(command.ref, "refs/heads/") && target.Type != ObjCommit {
					reportLines = append(reportLines, pktData("ng "+command.ref+" not a commit\n"))
					continue
				}
				if strings.HasPrefix(command.ref, "refs/tags/") && target.Type != ObjCommit && target.Type != ObjTag {
					reportLines = append(reportLines, pktData("ng "+command.ref+" not a commit or tag\n"))
					continue
				}
				if !hasCurrent {
					if command.oldOID != ZeroOID {
						reportLines = append(reportLines, pktData("ng "+command.ref+" non-fast-forward\n"))
						continue
					}
					repo.Refs[command.ref] = command.newOID
					changed = true
					reportLines = append(reportLines, pktData("ok "+command.ref+"\n"))
					continue
				}
				if command.oldOID != current {
					reportLines = append(reportLines, pktData("ng "+command.ref+" non-fast-forward\n"))
					continue
				}
				lookup := func(oid string) *GitObject {
					if obj, ok := repo.Objects[oid]; ok {
						o := obj
						return &o
					}
					return nil
				}
				if !isAncestor(current, command.newOID, lookup) {
					reportLines = append(reportLines, pktData("ng "+command.ref+" non-fast-forward\n"))
					continue
				}
				repo.Refs[command.ref] = command.newOID
				changed = true
				reportLines = append(reportLines, pktData("ok "+command.ref+"\n"))
			}
			if changed {
				store.invalidateGitCaches(repo)
			}
		}
	}

	inner := encodePktLines(reportLines)
	var out []byte
	if sideband {
		var parts [][]byte
		for _, chunk := range sidebandChunks(1, inner) {
			parts = append(parts, chunk)
		}
		parts = append(parts, pktFlush)
		out = bytes.Join(parts, nil)
	} else {
		out = inner
	}
	return &Response{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: out}
}
