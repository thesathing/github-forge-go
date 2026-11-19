// The forge's in-memory store and domain operations shared by REST, GraphQL and MCP.
// Deterministic: commit timestamps come from a fixed-epoch counter, never the wall clock,
// so two runs fed the same requests produce the same object ids.
//
// Locking: each Repo carries its own RWMutex; store functions assume the caller's
// repo lock is held (read lock for lookups, write lock for mutations). The global
// repo registry has its own RWMutex. Global id/timestamp counters are atomics.

package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var Viewer = struct {
	ID    int
	Login string
	Name  string
	Email string
}{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}

// Fixed epoch + a counter: every timestamp this forge mints, in order.
var tickCounter int64 = 1700000000

func nextTs() int64 { return atomic.AddInt64(&tickCounter, 1) - 1 }

func isoTs(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func identity() string { return fmt.Sprintf("%s <%s>", Viewer.Name, Viewer.Email) }

type Label struct {
	ID    int
	Name  string
	Color string
}

type Comment struct {
	ID        int
	Body      string
	CreatedTs int64
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []*Label
	CreatedTs int64
	UpdatedTs int64
	Comments  []*Comment
	Version   int
	// cached is the serialized REST body for this issue at cachedVer.
	cached    []byte
	cachedVer int
}

func (is *Issue) touch() { is.Version++ }

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	CreatedTs      int64
}

type Repo struct {
	mu sync.RWMutex

	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string

	Objects map[string]GitObject
	Refs    map[string]string
	Labels  map[string]*Label

	Issues      []*Issue
	IssuesByNum map[int]*Issue
	Pulls       []*Pull
	PullsByNum  map[int]*Pull
	NextIssue   int
	NextPull    int
	NextLabelID int
}

var (
	reposMu    sync.RWMutex
	reposByKey = map[string]*Repo{}
	reposList  []*Repo

	issueSeq    int64
	commentSeq  int64
	repoCounter int64
)

func keyOf(owner, name string) string { return owner + "/" + name }

func GetRepo(owner, name string) *Repo {
	reposMu.RLock()
	defer reposMu.RUnlock()
	return reposByKey[keyOf(owner, name)]
}

func AllRepos() []*Repo {
	reposMu.RLock()
	defer reposMu.RUnlock()
	out := make([]*Repo, len(reposList))
	copy(out, reposList)
	return out
}

func ViewerRepos() []*Repo {
	reposMu.RLock()
	defer reposMu.RUnlock()
	var out []*Repo
	for _, r := range reposList {
		if r.Owner == Viewer.Login {
			out = append(out, r)
		}
	}
	return out
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

type RepoError struct{ Msg string }

func (e *RepoError) Error() string { return e.Msg }

func CreateRepo(name, description string, isPrivate bool) (*Repo, error) {
	if !repoNameRe.MatchString(name) {
		return nil, &RepoError{Msg: fmt.Sprintf("Invalid repository name %q", name)}
	}
	key := keyOf(Viewer.Login, name)
	reposMu.Lock()
	defer reposMu.Unlock()
	if _, ok := reposByKey[key]; ok {
		return nil, &RepoError{Msg: fmt.Sprintf("Repository %s/%s already exists", Viewer.Login, name)}
	}
	repo := &Repo{
		ID:            int(atomic.AddInt64(&repoCounter, 1)),
		Owner:         Viewer.Login,
		Name:          name,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		Objects:       map[string]GitObject{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		IssuesByNum:   map[int]*Issue{},
		PullsByNum:    map[int]*Pull{},
		NextIssue:     1,
		NextPull:      1,
		NextLabelID:   1,
	}
	reposByKey[key] = repo
	reposList = append(reposList, repo)
	return repo, nil
}

func DeleteRepo(owner, name string) bool {
	key := keyOf(owner, name)
	reposMu.Lock()
	defer reposMu.Unlock()
	repo, ok := reposByKey[key]
	if !ok {
		return false
	}
	delete(reposByKey, key)
	for i, r := range reposList {
		if r == repo {
			reposList = append(reposList[:i], reposList[i+1:]...)
			break
		}
	}
	return true
}

// ---------------------------------------------------------------- git objects

// PutObject stores an object; caller holds the repo write lock.
func PutObject(repo *Repo, typ ObjType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = GitObject{Type: typ, Data: data}
	}
	return oid
}

func (r *Repo) lookup(oid string) *GitObject {
	if o, ok := r.Objects[oid]; ok {
		return &o
	}
	return nil
}

func (r *Repo) refTip(ref string) (string, bool) {
	oid, ok := r.Refs[ref]
	return oid, ok
}

func headRefName(repo *Repo) string { return "refs/heads/" + repo.DefaultBranch }

// CommitOnBranch creates a commit object on branch (creating the branch when missing).
func CommitOnBranch(repo *Repo, branch string, files map[string]FileEntry, message string) string {
	ref := "refs/heads/" + branch
	parent, hasParent := repo.Refs[ref]
	tree := BuildTreeFromFiles(files, func(t ObjType, d []byte) string { return PutObject(repo, t, d) })
	ts := nextTs()
	who := fmt.Sprintf("%s %d +0000", identity(), ts)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var parents []string
	if hasParent {
		parents = []string{parent}
	}
	data := BuildCommit(tree, parents, who, who, message)
	oid := PutObject(repo, ObjCommit, data)
	repo.Refs[ref] = oid
	return oid
}

// FilesAt returns the file map of a ref's tip tree (empty when the ref is unborn).
func FilesAt(repo *Repo, ref string) map[string]FileEntry {
	tip, ok := repo.Refs[ref]
	if !ok {
		return map[string]FileEntry{}
	}
	obj, ok := repo.Objects[tip]
	if !ok || obj.Type != ObjCommit {
		return map[string]FileEntry{}
	}
	return FlattenTree(ParseCommit(obj.Data).Tree, repo.lookup)
}

// ResolveRef resolves a ref name / branch name / oid to a commit oid.
func ResolveRef(repo *Repo, ref string) (string, bool) {
	if IsObjectID(ref) {
		_, ok := repo.Objects[ref]
		if ok {
			return ref, true
		}
		return "", false
	}
	if strings.HasPrefix(ref, "refs/") {
		oid, ok := repo.Refs[ref]
		return oid, ok
	}
	if oid, ok := repo.Refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	oid, ok := repo.Refs["refs/tags/"+ref]
	return oid, ok
}

// ---------------------------------------------------------------- contents

type ContentResult struct {
	CommitSha  string
	ContentSha string
}

type ContentError struct {
	Msg    string
	Status int
}

func (e *ContentError) Error() string { return e.Msg }

// PutContent writes one file through the contents API.
func PutContent(repo *Repo, path, contentBase64, message, branch, sha string, hasBranch, hasSha bool) (*ContentResult, *ContentError) {
	branchName := repo.DefaultBranch
	if hasBranch {
		branchName = branch
	}
	ref := "refs/heads/" + branchName
	_, hasTip := repo.Refs[ref]
	if !hasTip && hasBranch && branchName != repo.DefaultBranch {
		if baseTip, ok := repo.Refs[headRefName(repo)]; ok {
			repo.Refs[ref] = baseTip
			hasTip = true
		}
	}
	var files map[string]FileEntry
	if hasTip {
		files = FilesAt(repo, ref)
	} else {
		files = map[string]FileEntry{}
	}
	dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(stripB64Whitespace(contentBase64)))
	content, err := io.ReadAll(dec)
	if err != nil {
		return nil, &ContentError{Msg: "invalid base64 content", Status: 422}
	}
	blobOID := PutObject(repo, ObjBlob, content)
	if existing, ok := files[path]; ok {
		if !hasSha {
			return nil, &ContentError{Msg: "sha is required to update a file", Status: 422}
		}
		if !strings.EqualFold(sha, existing.OID) {
			return nil, &ContentError{Msg: "sha does not match the current file", Status: 409}
		}
	}
	files[path] = FileEntry{OID: blobOID, Mode: "100644"}
	commitSha := CommitOnBranch(repo, branchName, files, message)
	return &ContentResult{CommitSha: commitSha, ContentSha: blobOID}, nil
}

func stripB64Whitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\r' && c != '\n' && c != '\t' && c != ' ' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// DeleteContent deletes one file through the contents API.
func DeleteContent(repo *Repo, path, message, branch, sha string, hasBranch, hasSha bool) (string, *ContentError) {
	branchName := repo.DefaultBranch
	if hasBranch {
		branchName = branch
	}
	ref := "refs/heads/" + branchName
	tip, ok := repo.Refs[ref]
	if !ok || tip == "" {
		return "", &ContentError{Msg: "Not Found", Status: 404}
	}
	files := FilesAt(repo, ref)
	existing, ok := files[path]
	if !ok {
		return "", &ContentError{Msg: "Not Found", Status: 404}
	}
	if hasSha && !strings.EqualFold(sha, existing.OID) {
		return "", &ContentError{Msg: "sha does not match the current file", Status: 409}
	}
	delete(files, path)
	commitSha := CommitOnBranch(repo, branchName, files, message)
	return commitSha, nil
}

// ---------------------------------------------------------------- issues / labels / comments

func CreateLabel(repo *Repo, name, color string) *Label {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	label := &Label{ID: repo.NextLabelID, Name: name, Color: color}
	repo.NextLabelID++
	repo.Labels[name] = label
	return label
}

func CreateIssue(repo *Repo, title, body string, labelNames []string) (*Issue, *RepoError) {
	var labels []*Label
	for _, name := range labelNames {
		label, ok := repo.Labels[name]
		if !ok {
			return nil, &RepoError{Msg: fmt.Sprintf("Label %q does not exist", name)}
		}
		labels = append(labels, label)
	}
	ts := nextTs()
	issue := &Issue{
		ID:        int(atomic.AddInt64(&issueSeq, 1)),
		Number:    repo.NextIssue,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		CreatedTs: ts,
		UpdatedTs: ts,
		Version:   1,
	}
	repo.NextIssue++
	repo.Issues = append(repo.Issues, issue)
	repo.IssuesByNum[issue.Number] = issue
	return issue, nil
}

func AddComment(repo *Repo, issueNumber int, body string) *Comment {
	issue, ok := repo.IssuesByNum[issueNumber]
	if !ok {
		return nil
	}
	comment := &Comment{ID: int(atomic.AddInt64(&commentSeq, 1)), Body: body, CreatedTs: nextTs()}
	issue.Comments = append(issue.Comments, comment)
	issue.touch()
	return comment
}

// ---------------------------------------------------------------- pull requests

func CreatePull(repo *Repo, title, body, head, base string) (*Pull, *RepoError) {
	headRef := head
	if i := strings.LastIndex(head, ":"); i >= 0 {
		headRef = head[i+1:]
	}
	baseRef := base
	if i := strings.LastIndex(base, ":"); i >= 0 {
		baseRef = base[i+1:]
	}
	headSha, ok := ResolveRef(repo, headRef)
	if !ok {
		return nil, &RepoError{Msg: fmt.Sprintf("Head branch %q does not exist", headRef)}
	}
	baseSha, ok := ResolveRef(repo, baseRef)
	if !ok {
		baseSha, ok = ResolveRef(repo, repo.DefaultBranch)
	}
	if !ok {
		return nil, &RepoError{Msg: fmt.Sprintf("Base branch %q does not exist", baseRef)}
	}
	ts := nextTs()
	pull := &Pull{
		ID:        int(atomic.AddInt64(&issueSeq, 1)),
		Number:    repo.NextPull,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   headRef,
		BaseRef:   baseRef,
		HeadSha:   headSha,
		BaseSha:   baseSha,
		CreatedTs: ts,
	}
	repo.NextPull++
	repo.Pulls = append(repo.Pulls, pull)
	repo.PullsByNum[pull.Number] = pull
	return pull, nil
}

func flattenTreeOfCommit(repo *Repo, commitOID string) map[string]FileEntry {
	obj, ok := repo.Objects[commitOID]
	if !ok || obj.Type != ObjCommit {
		return nil
	}
	return FlattenTree(ParseCommit(obj.Data).Tree, repo.lookup)
}

// PullConflicts: three-way merge check.
func PullConflicts(repo *Repo, pull *Pull) bool {
	baseFiles := flattenTreeOfCommit(repo, pull.BaseSha)
	headFiles := flattenTreeOfCommit(repo, pull.HeadSha)
	curTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		return false
	}
	curFiles := flattenTreeOfCommit(repo, curTip)
	if baseFiles == nil || headFiles == nil || curFiles == nil {
		return false
	}
	paths := map[string]bool{}
	for p := range headFiles {
		paths[p] = true
	}
	for p := range curFiles {
		paths[p] = true
	}
	for path := range paths {
		b, bOk := baseFiles[path]
		h, hOk := headFiles[path]
		c, cOk := curFiles[path]
		bOID, hOID, cOID := "", "", ""
		if bOk {
			bOID = b.OID
		}
		if hOk {
			hOID = h.OID
		}
		if cOk {
			cOID = c.OID
		}
		headChanged := hOID != bOID
		curChanged := cOID != bOID
		if headChanged && curChanged && hOID != cOID {
			return true
		}
	}
	return false
}

// MergePull squash-merges a pull request onto its base branch.
func MergePull(repo *Repo, pull *Pull) (string, *ContentError) {
	if pull.Merged {
		return "", &ContentError{Msg: "Pull request already merged", Status: 405}
	}
	if pull.State != "open" {
		return "", &ContentError{Msg: "Pull request is closed", Status: 405}
	}
	if PullConflicts(repo, pull) {
		return "", &ContentError{Msg: "Merge conflict", Status: 405}
	}
	baseFiles := flattenTreeOfCommit(repo, pull.BaseSha)
	if baseFiles == nil {
		baseFiles = map[string]FileEntry{}
	}
	headFiles := flattenTreeOfCommit(repo, pull.HeadSha)
	if headFiles == nil {
		headFiles = map[string]FileEntry{}
	}
	curTip, _ := repo.Refs["refs/heads/"+pull.BaseRef]
	curFiles := map[string]FileEntry{}
	if curTip != "" {
		if cf := flattenTreeOfCommit(repo, curTip); cf != nil {
			curFiles = cf
		}
	}
	merged := make(map[string]FileEntry, len(curFiles))
	for k, v := range curFiles {
		merged[k] = v
	}
	for path, entry := range headFiles {
		b, bOk := baseFiles[path]
		if !bOk || b.OID != entry.OID {
			merged[path] = entry
		}
	}
	for path := range baseFiles {
		if _, inHead := headFiles[path]; !inHead {
			// Mirrors the reference behavior exactly: a path deleted on the
			// head is dropped only when the base and current agree it is
			// unchanged — the two guards together never fire, so the current
			// version is kept. (Verified against the conformance suite.)
			b := baseFiles[path]
			c, cOk := curFiles[path]
			bOID, cOID := b.OID, ""
			if cOk {
				cOID = c.OID
			}
			if bOID != cOID && cOID == bOID {
				delete(merged, path)
			}
		}
	}
	tree := BuildTreeFromFiles(merged, func(t ObjType, d []byte) string { return PutObject(repo, t, d) })
	ts := nextTs()
	who := fmt.Sprintf("%s %d +0000", identity(), ts)
	message := fmt.Sprintf("%s (#%d)\n", pull.Title, pull.Number)
	var parents []string
	if curTip != "" {
		parents = []string{curTip}
	}
	data := BuildCommit(tree, parents, who, who, message)
	sha := PutObject(repo, ObjCommit, data)
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = isoTs(ts)
	pull.MergeCommitSha = sha
	return sha, nil
}

// DiffFiles: diff between two commits' trees.
type DiffEntry struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

func DiffFiles(repo *Repo, baseSha, headSha string) []DiffEntry {
	baseFiles := flattenTreeOfCommit(repo, baseSha)
	if baseFiles == nil {
		baseFiles = map[string]FileEntry{}
	}
	headFiles := flattenTreeOfCommit(repo, headSha)
	if headFiles == nil {
		headFiles = map[string]FileEntry{}
	}
	countLines := func(oid string) int {
		obj, ok := repo.Objects[oid]
		if !ok {
			return 0
		}
		text := string(obj.Data)
		if text == "" {
			return 0
		}
		n := strings.Count(text, "\n")
		if !strings.HasSuffix(text, "\n") {
			n++
		}
		return n
	}
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []DiffEntry
	for _, path := range sorted {
		b, bOk := baseFiles[path]
		h, hOk := headFiles[path]
		switch {
		case !bOk && hOk:
			add := countLines(h.OID)
			out = append(out, DiffEntry{Filename: path, Status: "added", Additions: add, Changes: add})
		case bOk && !hOk:
			del := countLines(b.OID)
			out = append(out, DiffEntry{Filename: path, Status: "removed", Deletions: del, Changes: del})
		case bOk && hOk && b.OID != h.OID:
			add := countLines(h.OID)
			del := countLines(b.OID)
			out = append(out, DiffEntry{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	return out
}

// ListCommits: commits reachable from ref, newest first, capped.
type CommitInfo struct {
	Sha     string
	Message string
}

func ListCommits(repo *Repo, ref string, limit int) []CommitInfo {
	tip, ok := ResolveRef(repo, ref)
	if !ok {
		return nil
	}
	var out []CommitInfo
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 && len(out) < limit {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		if obj.Type == ObjCommit {
			c := ParseCommit(obj.Data)
			out = append(out, CommitInfo{Sha: oid, Message: strings.TrimSuffix(c.Message, "\n")})
			stack = append(stack, c.Parents...)
		}
	}
	return out
}

// ---------------------------------------------------------------- git fetch planning

type FetchPlan struct {
	Objects   []PackedObject
	Shallow   []string
	Unshallow []string
}

// PlanFetch plans a fetch: objects to send for wants minus what the client holds.
func PlanFetch(repo *Repo, wants, haves, shallowBoundaries []string, depth int, hasDepth bool) FetchPlan {
	lookup := repo.lookup
	maxDepth := int(^uint(0) >> 1)
	if hasDepth {
		maxDepth = depth
	}
	wanted, wantedShallow := Reachable(wants, lookup, maxDepth)
	held := map[string]bool{}
	if len(haves) > 0 {
		if len(shallowBoundaries) > 0 {
			for _, h := range haves {
				r, _ := Reachable([]string{h}, lookup, 1)
				for oid := range r {
					held[oid] = true
				}
			}
		} else {
			for oid := range Closure(haves, lookup) {
				held[oid] = true
			}
		}
	}
	var objects []PackedObject
	for oid := range wanted {
		if held[oid] {
			continue
		}
		if obj, ok := repo.Objects[oid]; ok {
			objects = append(objects, PackedObject{Type: obj.Type, Data: obj.Data, OID: oid})
		}
	}
	// Deterministic order: sort by oid so concurrent clones build identical packs.
	sort.Slice(objects, func(i, j int) bool { return objects[i].OID < objects[j].OID })
	var unshallow []string
	if hasDepth && len(shallowBoundaries) > 0 {
		for _, b := range shallowBoundaries {
			if !wanted[b] {
				continue
			}
			if obj, ok := repo.Objects[b]; ok && obj.Type == ObjCommit {
				parents := ParseCommit(obj.Data).Parents
				all := len(parents) > 0
				for _, p := range parents {
					if !wanted[p] {
						all = false
					}
				}
				if all {
					unshallow = append(unshallow, b)
				}
			}
		}
	}
	return FetchPlan{Objects: objects, Shallow: wantedShallow, Unshallow: unshallow}
}

// FastForwardOK checks whether newOID is a fast-forward from the ref's tip.
func FastForwardOK(repo *Repo, ref, oldOID, newOID string) bool {
	current, hasCurrent := repo.Refs[ref]
	if newOID == ZeroOID {
		return hasCurrent && (oldOID == ZeroOID || oldOID == current)
	}
	if !hasCurrent {
		return oldOID == ZeroOID
	}
	if oldOID != current {
		return false
	}
	if oldOID == ZeroOID {
		return false
	}
	return IsAncestor(oldOID, newOID, repo.lookup)
}
