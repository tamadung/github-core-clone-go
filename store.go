// store.go — the forge's in-memory store: repos, objects, refs, issues, pulls,
// labels and comments. No disk writes at request time; everything lives in maps
// and is deterministic (timestamps derive from a sequence, never the clock).
package main

import (
	"sort"
	"strings"
	"time"
)

const BaseEpoch = 1700000000

// tsFor renders a sequence number as an ISO-8601 UTC timestamp, the way
// JavaScript's Date.toISOString does (always three fractional digits, 'Z').
func tsFor(seq int) string {
	return time.Unix(BaseEpoch+int64(seq), 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID         int
	Body       string
	User       string
	CreatedSeq int
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []*Label
	User       string
	CreatedSeq int
	UpdatedSeq int
	Comments   []*Comment
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string // "" means null
	MergeCommitSHA string
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	User           string
	CreatedSeq     int
	UpdatedSeq     int
	Comments       []*Comment
}

type Repo struct {
	ID            int
	Name          string
	Owner         string // "" means null (unknown yet)
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int
	Objects       map[string]GitObject
	Refs          map[string]string
	Issues        []*Issue
	IssueSeq      int
	Pulls         []*Pull
	PullSeq       int
	Labels        []*Label
	LabelSeq      int
	CommentSeq    int
	// Caches, invalidated on push / mutation.
	AdvV0Up     []byte
	AdvRecv     []byte
	ClonePack   map[string][]byte
	LsRefsCache map[string][]byte
	IssueJSON   map[int][]byte
}

var defaultLabels = []struct {
	Name, Color, Description string
}{
	{"bug", "d73a4a", "Something isn't working"},
	{"documentation", "0075ca", "Improvements or additions to documentation"},
	{"duplicate", "cfd3d7", "This issue or pull request already exists"},
	{"enhancement", "a2eeef", "New feature or request"},
	{"good first issue", "7057ff", "Good for newcomers"},
	{"help wanted", "008672", "Extra attention is needed"},
	{"invalid", "e4e669", "This doesn't seem right"},
	{"question", "d876e3", "Further information is requested"},
	{"wontfix", "ffffff", "This will not be worked on"},
}

type Store struct {
	Viewer        string // "" means null (unknown yet)
	Repos         []*Repo
	ByName        map[string]*Repo
	Seq           int
	NextRepoID    int
	NextIssueID   int
	NextPullID    int
	NextLabelID   int
	NextCommentID int
}

func newStore() *Store {
	return &Store{
		ByName:        make(map[string]*Repo),
		NextRepoID:    1,
		NextIssueID:   1,
		NextPullID:    1,
		NextLabelID:   1,
		NextCommentID: 1,
	}
}

func (s *Store) nextSeq() int {
	s.Seq++
	return s.Seq
}

// learnViewer learns the viewer's login from the first owner segment ever seen.
func (s *Store) learnViewer(owner string) bool {
	if owner == "" {
		return false
	}
	if s.Viewer == "" {
		s.Viewer = owner
		return true
	}
	return s.Viewer == owner
}

func (s *Store) createRepo(name, description string, isPrivate bool) *Repo {
	repo := &Repo{
		ID:            s.NextRepoID,
		Name:          name,
		Owner:         s.Viewer,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		CreatedSeq:    s.nextSeq(),
		Objects:       make(map[string]GitObject),
		Refs:          make(map[string]string),
		IssueSeq:      1,
		PullSeq:       1,
		LabelSeq:      1,
		CommentSeq:    1,
		ClonePack:     make(map[string][]byte),
		LsRefsCache:   make(map[string][]byte),
		IssueJSON:     make(map[int][]byte),
	}
	s.NextRepoID++
	for _, def := range defaultLabels {
		repo.Labels = append(repo.Labels, &Label{ID: s.NextLabelID, Name: def.Name, Color: def.Color, Description: def.Description})
		s.NextLabelID++
	}
	s.Repos = append(s.Repos, repo)
	s.ByName[name] = repo
	return repo
}

func (s *Store) findRepoByName(name string) *Repo {
	return s.ByName[name]
}

// findRepo resolves a repo by (owner, name). When the viewer is still unknown,
// the first owner-bearing request binds it. A stranger owner never resolves.
func (s *Store) findRepo(owner, name string) *Repo {
	if owner != "" {
		if s.Viewer == "" {
			s.Viewer = owner
		} else if s.Viewer != owner {
			return nil
		}
	}
	repo := s.findRepoByName(name)
	if repo == nil {
		return nil
	}
	if repo.Owner == "" {
		repo.Owner = s.Viewer
	}
	if owner != "" && repo.Owner != owner {
		return nil
	}
	return repo
}

func (s *Store) repoOwner(repo *Repo) string {
	if repo.Owner != "" {
		return repo.Owner
	}
	return s.Viewer
}

func (s *Store) invalidateGitCaches(repo *Repo) {
	repo.AdvV0Up = nil
	repo.AdvRecv = nil
	repo.ClonePack = make(map[string][]byte)
	repo.LsRefsCache = make(map[string][]byte)
}

func (s *Store) resolveRef(repo *Repo, ref string) string {
	if ref == "" {
		return ""
	}
	if isObjectID(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref
		}
		return ""
	}
	if strings.HasPrefix(ref, "refs/") {
		return repo.Refs[ref]
	}
	if branch, ok := repo.Refs["refs/heads/"+ref]; ok {
		return branch
	}
	if tag, ok := repo.Refs["refs/tags/"+ref]; ok {
		return tag
	}
	if ref == "HEAD" {
		return repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	return ""
}

func (s *Store) headOID(repo *Repo) string {
	if oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		return oid
	}
	return ZeroOID
}

func (s *Store) getOrCreateLabel(repo *Repo, name string) *Label {
	for _, label := range repo.Labels {
		if label.Name == name {
			return label
		}
	}
	label := &Label{ID: s.NextLabelID, Name: name, Color: "ededed", Description: ""}
	s.NextLabelID++
	repo.Labels = append(repo.Labels, label)
	return label
}

func (s *Store) createIssue(repo *Repo, title, body string, labelNames []string) *Issue {
	labels := make([]*Label, 0, len(labelNames))
	for _, name := range labelNames {
		labels = append(labels, s.getOrCreateLabel(repo, name))
	}
	issue := &Issue{
		ID:         s.NextIssueID,
		Number:     repo.IssueSeq,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     labels,
		User:       s.Viewer,
		CreatedSeq: s.nextSeq(),
		Comments:   []*Comment{},
	}
	s.NextIssueID++
	repo.IssueSeq++
	issue.UpdatedSeq = s.Seq
	repo.Issues = append(repo.Issues, issue)
	return issue
}

func (s *Store) createPull(repo *Repo, title, body, headRef, baseRef string) *Pull {
	headSha := s.resolveRef(repo, headRef)
	baseSha := s.resolveRef(repo, baseRef)
	if headSha == "" || baseSha == "" {
		return nil
	}
	pull := &Pull{
		ID:         s.NextPullID,
		Number:     repo.PullSeq,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    headRef,
		BaseRef:    baseRef,
		HeadSha:    headSha,
		BaseSha:    baseSha,
		User:       s.Viewer,
		CreatedSeq: s.nextSeq(),
		Comments:   []*Comment{},
	}
	s.NextPullID++
	repo.PullSeq++
	pull.UpdatedSeq = s.Seq
	repo.Pulls = append(repo.Pulls, pull)
	return pull
}

type blobRef struct {
	OID  string
	Mode string
}

// blobAtPath is the tree oid at `path` inside `treeOid`, or nil when absent.
func (s *Store) blobAtPath(repo *Repo, treeOID, path string) *blobRef {
	parts := strings.Split(path, "/")
	var filtered []string
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	current := treeOID
	for i, part := range filtered {
		object, ok := repo.Objects[current]
		if !ok || object.Type != ObjTree {
			return nil
		}
		entries, err := parseTree(object.Data)
		if err != nil {
			return nil
		}
		var entry *TreeEntry
		for j := range entries {
			if entries[j].Name == part {
				entry = &entries[j]
				break
			}
		}
		if entry == nil {
			return nil
		}
		if i == len(filtered)-1 {
			if entry.Mode == "40000" {
				return nil
			}
			return &blobRef{OID: entry.OID, Mode: entry.Mode}
		}
		if entry.Mode != "40000" {
			return nil
		}
		current = entry.OID
	}
	return nil
}

// flattenTree is a flat map of every file path -> {oid, mode} reachable from a tree.
func (s *Store) flattenTree(repo *Repo, treeOID, prefix string) map[string]blobRef {
	out := map[string]blobRef{}
	object, ok := repo.Objects[treeOID]
	if !ok || object.Type != ObjTree {
		return out
	}
	entries, err := parseTree(object.Data)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			for p, v := range s.flattenTree(repo, entry.OID, path) {
				out[p] = v
			}
		} else if entry.Mode != "160000" {
			out[path] = blobRef{OID: entry.OID, Mode: entry.Mode}
		}
	}
	return out
}

// countLines counts lines in a blob (a trailing newline makes no extra line).
func (s *Store) countLines(repo *Repo, oid string) int {
	object, ok := repo.Objects[oid]
	if !ok {
		return 0
	}
	data := object.Data
	if len(data) == 0 {
		return 0
	}
	nl := 0
	for _, b := range data {
		if b == '\n' {
			nl++
		}
	}
	if data[len(data)-1] == '\n' {
		return nl
	}
	return nl + 1
}

type diffFile struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
}

// diffTrees is the file diff between two trees.
func (s *Store) diffTrees(repo *Repo, baseTreeOID, headTreeOID string) []diffFile {
	base := s.flattenTree(repo, baseTreeOID, "")
	head := s.flattenTree(repo, headTreeOID, "")
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var files []diffFile
	for _, path := range sorted {
		b, inBase := base[path]
		h, inHead := head[path]
		if inBase && !inHead {
			deletions := s.countLines(repo, b.OID)
			files = append(files, diffFile{Filename: path, Status: "removed", Additions: 0, Deletions: deletions, Changes: deletions})
		} else if !inBase && inHead {
			additions := s.countLines(repo, h.OID)
			files = append(files, diffFile{Filename: path, Status: "added", Additions: additions, Deletions: 0, Changes: additions})
		} else if inBase && inHead && b.OID != h.OID {
			additions := s.countLines(repo, h.OID)
			deletions := s.countLines(repo, b.OID)
			files = append(files, diffFile{Filename: path, Status: "modified", Additions: additions, Deletions: deletions, Changes: additions + deletions})
		}
	}
	return files
}

// mergeable is the three-way mergeability check: a PR conflicts when a file
// it changes was also changed on the base tip (relative to the PR's base)
// with different content.
func (s *Store) mergeable(pull *Pull, repo *Repo) bool {
	baseTipOID := s.resolveRef(repo, pull.BaseRef)
	if baseTipOID == "" {
		return false
	}
	baseTip, ok := repo.Objects[baseTipOID]
	head, ok2 := repo.Objects[pull.HeadSha]
	base, ok3 := repo.Objects[pull.BaseSha]
	if !ok || baseTip.Type != ObjCommit || !ok2 || head.Type != ObjCommit || !ok3 || base.Type != ObjCommit {
		return false
	}
	baseTipCommit, err := parseCommit(baseTip.Data)
	if err != nil {
		return false
	}
	headCommit, err := parseCommit(head.Data)
	if err != nil {
		return false
	}
	baseCommit, err := parseCommit(base.Data)
	if err != nil {
		return false
	}
	prFiles := s.diffTrees(repo, baseCommit.Tree, headCommit.Tree)
	baseTipFlat := s.flattenTree(repo, baseTipCommit.Tree, "")
	baseFlat := s.flattenTree(repo, baseCommit.Tree, "")
	headFlat := s.flattenTree(repo, headCommit.Tree, "")
	for _, file := range prFiles {
		inBase, hasBase := baseFlat[file.Filename]
		inHead, hasHead := headFlat[file.Filename]
		inTip, hasTip := baseTipFlat[file.Filename]
		var baseOID, headOID, tipOID string
		if hasBase {
			baseOID = inBase.OID
		}
		if hasHead {
			headOID = inHead.OID
		}
		if hasTip {
			tipOID = inTip.OID
		}
		// Changed on base tip AND changed in the PR AND the two changes differ.
		if baseOID != tipOID && baseOID != headOID && tipOID != headOID {
			return false
		}
	}
	return true
}

// mergePull squash-merges a pull: one new commit on the base tip whose tree
// is the head tree. Returns the merge commit oid, or "" when it cannot merge.
func (s *Store) mergePull(repo *Repo, pull *Pull) string {
	if pull.Merged || pull.State != "open" {
		return ""
	}
	if !s.mergeable(pull, repo) {
		return ""
	}
	baseTipOID := s.resolveRef(repo, pull.BaseRef)
	if baseTipOID == "" {
		return ""
	}
	head, ok := repo.Objects[pull.HeadSha]
	if !ok || head.Type != ObjCommit {
		return ""
	}
	headCommit, err := parseCommit(head.Data)
	if err != nil {
		return ""
	}
	message := "Merge pull request #" + itoa(pull.Number) + " from " + s.repoOwner(repo) + "/" + pull.HeadRef
	commitData := buildCommit(BuildCommitFields{
		Tree:      headCommit.Tree,
		Parents:   []string{baseTipOID},
		Message:   message,
		Author:    ConformanceIdentity,
		Committer: ConformanceIdentity,
		Timestamp: BaseEpoch,
	})
	oid := hashObject(ObjCommit, commitData)
	repo.Objects[oid] = GitObject{Type: ObjCommit, Data: commitData}
	repo.Refs["refs/heads/"+pull.BaseRef] = oid
	pull.Merged = true
	pull.State = "closed"
	pull.MergedAt = tsFor(s.nextSeq())
	pull.MergeCommitSHA = oid
	pull.UpdatedSeq = s.Seq
	s.invalidateGitCaches(repo)
	return oid
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

type commitFile struct {
	Data []byte
	Mode string
}

// commitFiles builds a commit from a full tree path map. Returns the new
// commit oid. `parent` is the branch tip ("" for the first commit).
func (s *Store) commitFiles(repo *Repo, files map[string]commitFile, message, parent string) string {
	byDir := map[string]map[string]blobRef{}
	for path, file := range files {
		blobOID := hashObject(ObjBlob, file.Data)
		repo.Objects[blobOID] = GitObject{Type: ObjBlob, Data: file.Data}
		slash := strings.LastIndex(path, "/")
		dir, name := "", path
		if slash >= 0 {
			dir, name = path[:slash], path[slash+1:]
		}
		if byDir[dir] == nil {
			byDir[dir] = map[string]blobRef{}
		}
		byDir[dir][name] = blobRef{OID: blobOID, Mode: file.Mode}
	}
	var buildDir func(dir string) string
	buildDir = func(dir string) string {
		var entries []TreeEntry
		if direct, ok := byDir[dir]; ok {
			for name, v := range direct {
				entries = append(entries, TreeEntry{Mode: v.Mode, Name: name, OID: v.OID})
			}
		}
		subdirs := map[string]bool{}
		for key := range byDir {
			if key == dir {
				continue
			}
			if dir == "" && !strings.Contains(key, "/") {
				subdirs[key] = true
			} else if strings.HasPrefix(key, dir+"/") && !strings.Contains(key[len(dir)+1:], "/") {
				subdirs[key] = true
			}
		}
		for sub := range subdirs {
			name := sub
			if dir != "" {
				name = sub[len(dir)+1:]
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: buildDir(sub)})
		}
		treeData := buildTree(entries)
		treeOID := hashObject(ObjTree, treeData)
		repo.Objects[treeOID] = GitObject{Type: ObjTree, Data: treeData}
		return treeOID
	}
	rootTree := buildDir("")
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	commitData := buildCommit(BuildCommitFields{
		Tree:      rootTree,
		Parents:   parents,
		Message:   message,
		Author:    ConformanceIdentity,
		Committer: ConformanceIdentity,
		Timestamp: BaseEpoch,
	})
	commitOID := hashObject(ObjCommit, commitData)
	repo.Objects[commitOID] = GitObject{Type: ObjCommit, Data: commitData}
	return commitOID
}

// treeFileMap is the full file map of a tree.
func (s *Store) treeFileMap(repo *Repo, treeOID string) map[string]commitFile {
	out := map[string]commitFile{}
	for path, entry := range s.flattenTree(repo, treeOID, "") {
		if object, ok := repo.Objects[entry.OID]; ok {
			out[path] = commitFile{Data: object.Data, Mode: entry.Mode}
		}
	}
	return out
}

type commitRef struct {
	OID    string
	Commit CommitView
}

// commitsFrom returns the commits reachable from `ref`, newest-first
// (topological order via DFS).
func (s *Store) commitsFrom(repo *Repo, ref string) []commitRef {
	tip := s.resolveRef(repo, ref)
	if tip == "" {
		return nil
	}
	var out []commitRef
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := repo.Objects[oid]
		if !ok || object.Type != ObjCommit {
			continue
		}
		commit, err := parseCommit(object.Data)
		if err != nil {
			continue
		}
		out = append(out, commitRef{OID: oid, Commit: commit})
		for _, parent := range commit.Parents {
			stack = append(stack, parent)
		}
	}
	return out
}
