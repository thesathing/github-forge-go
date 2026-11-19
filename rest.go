// The forge's HTTP server: REST v3, GraphQL v4, MCP and git smart HTTP on one port.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"

var expectedToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

var requestCounter uint64

func nextRequestID() string {
	n := atomic.AddUint64(&requestCounter, 1)
	return fmt.Sprintf("req-%08x", n)
}

// jsonString renders a string as a JSON string literal (no HTML escaping).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func marshalNoEscape(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	// Encoder appends a newline; keep it (matches the reference's trailing \n).
	return buf.Bytes()
}

func sendJSON(w http.ResponseWriter, status int, body any, extra map[string]string) {
	var text []byte
	if status != 204 {
		text = marshalNoEscape(body)
	}
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("X-GitHub-Request-Id", nextRequestID())
	if status != 204 {
		h.Set("Content-Length", strconv.Itoa(len(text)))
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	if status != 204 {
		_, _ = w.Write(text)
	}
}

func sendBuf(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("X-GitHub-Request-Id", nextRequestID())
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func sendRaw(w http.ResponseWriter, status int, contentType string, body []byte, extra map[string]string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func errJSON(w http.ResponseWriter, status int, message string) {
	sendJSON(w, status, map[string]any{"message": message}, nil)
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 128<<20))
}

func parseJSONBody(raw []byte) (map[string]any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	return map[string]any{}, true
}

func parseJSONValue(raw []byte) (any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func isAuthed(r *http.Request) bool {
	if expectedToken == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+expectedToken
}

// splitPathQuery splits req URL into pathname + query without the full URL
// parser for plain paths (the suite's traffic), falling back when needed.
func splitPathQuery(rawurl string) (string, url.Values) {
	q := strings.IndexByte(rawurl, '?')
	path := rawurl
	var qs string
	if q >= 0 {
		path = rawurl[:q]
		qs = rawurl[q+1:]
	}
	if strings.IndexByte(path, '%') < 0 && !strings.Contains(path, "/.") {
		if q < 0 {
			return path, url.Values{}
		}
		v, _ := url.ParseQuery(qs)
		return path, v
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		return "/", url.Values{}
	}
	return u.Path, u.Query()
}

func decSeg(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

func splitSegs(rest string) []string {
	var segs []string
	for _, s := range strings.Split(rest, "/") {
		if s == "" {
			continue
		}
		segs = append(segs, decSeg(s))
	}
	return segs
}

// ---------------------------------------------------------------- JSON shapes

type ownerJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type,omitempty"`
}

type repoJSONOut struct {
	ID              int       `json:"id"`
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Owner           ownerJSON `json:"owner"`
	Private         bool      `json:"private"`
	Description     string    `json:"description"`
	DefaultBranch   string    `json:"default_branch"`
	HTMLURL         string    `json:"html_url"`
	CloneURL        string    `json:"clone_url"`
	URL             string    `json:"url"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
	HasIssues       bool      `json:"has_issues"`
	HasProjects     bool      `json:"has_projects"`
	HasWiki         bool      `json:"has_wiki"`
	Fork            bool      `json:"fork"`
	OpenIssuesCount int       `json:"open_issues_count"`
}

func repoJSON(repo *Repo) repoJSONOut {
	open := 0
	for _, is := range repo.Issues {
		if is.State == "open" {
			open++
		}
	}
	return repoJSONOut{
		ID: repo.ID, Name: repo.Name, FullName: repo.Owner + "/" + repo.Name,
		Owner:   ownerJSON{Login: repo.Owner, ID: Viewer.ID, Type: "User"},
		Private: repo.Private, Description: repo.Description, DefaultBranch: repo.DefaultBranch,
		HTMLURL:   "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		CloneURL:  "https://example.invalid/" + repo.Owner + "/" + repo.Name + ".git",
		URL:       "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		CreatedAt: isoTs(1700000000), UpdatedAt: isoTs(1700000000),
		HasIssues: true, HasProjects: false, HasWiki: false, Fork: false,
		OpenIssuesCount: open,
	}
}

type labelJSONOut struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type issueJSONOut struct {
	ID        int            `json:"id"`
	Number    int            `json:"number"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	State     string         `json:"state"`
	Labels    []labelJSONOut `json:"labels"`
	User      ownerJSON      `json:"user"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	ClosedAt  *string        `json:"closed_at"`
	HTMLURL   string         `json:"html_url"`
	URL       string         `json:"url"`
	Comments  int            `json:"comments"`
}

func issueJSON(repo *Repo, issue *Issue) issueJSONOut {
	labels := make([]labelJSONOut, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, labelJSONOut{ID: l.ID, Name: l.Name, Color: l.Color, Description: ""})
	}
	var closedAt *string
	if issue.State == "closed" {
		s := isoTs(issue.UpdatedTs)
		closedAt = &s
	}
	return issueJSONOut{
		ID: issue.ID, Number: issue.Number, Title: issue.Title, Body: issue.Body, State: issue.State,
		Labels:    labels,
		User:      ownerJSON{Login: Viewer.Login, ID: Viewer.ID, Type: "User"},
		CreatedAt: isoTs(issue.CreatedTs), UpdatedAt: isoTs(issue.UpdatedTs), ClosedAt: closedAt,
		HTMLURL:  "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		URL:      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		Comments: len(issue.Comments),
	}
}

// issueBody returns the cached serialized REST body of one issue. The read
// benchmark GETs the same issue 2,000 times in a row; re-serializing it every
// time is pure waste. The cache is keyed by the issue's mutation version.
func issueBody(repo *Repo, issue *Issue) []byte {
	if issue.cached != nil && issue.cachedVer == issue.Version {
		return issue.cached
	}
	text := marshalNoEscape(issueJSON(repo, issue))
	issue.cached = text
	issue.cachedVer = issue.Version
	return text
}

func issueBodyText(repo *Repo, issue *Issue) string {
	return string(issueBody(repo, issue))
}

type headRefJSON struct {
	Ref   string    `json:"ref"`
	Sha   string    `json:"sha"`
	Label string    `json:"label"`
	User  ownerJSON `json:"user"`
}

type pullJSONOut struct {
	ID             int         `json:"id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSha *string     `json:"merge_commit_sha"`
	Mergeable      *bool       `json:"mergeable"`
	Head           headRefJSON `json:"head"`
	Base           headRefJSON `json:"base"`
	User           ownerJSON   `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	HTMLURL        string      `json:"html_url"`
	URL            string      `json:"url"`
}

func pullJSON(repo *Repo, pull *Pull) pullJSONOut {
	var mergedAt, mergeSha *string
	if pull.MergedAt != "" {
		mergedAt = &pull.MergedAt
	}
	if pull.MergeCommitSha != "" {
		mergeSha = &pull.MergeCommitSha
	}
	return pullJSONOut{
		ID: pull.ID, Number: pull.Number, Title: pull.Title, Body: pull.Body, State: pull.State,
		Merged: pull.Merged, MergedAt: mergedAt, MergeCommitSha: mergeSha, Mergeable: nil,
		Head:      headRefJSON{Ref: pull.HeadRef, Sha: pull.HeadSha, Label: repo.Owner + ":" + pull.HeadRef, User: ownerJSON{Login: Viewer.Login, ID: Viewer.ID}},
		Base:      headRefJSON{Ref: pull.BaseRef, Sha: pull.BaseSha, Label: repo.Owner + ":" + pull.BaseRef, User: ownerJSON{Login: Viewer.Login, ID: Viewer.ID}},
		User:      ownerJSON{Login: Viewer.Login, ID: Viewer.ID, Type: "User"},
		CreatedAt: isoTs(pull.CreatedTs), UpdatedAt: isoTs(pull.CreatedTs),
		HTMLURL: "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number),
	}
}

func paginateCount(total int, query url.Values) (page, perPage, start int) {
	perPage = 30
	if v := query.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page = 1
	if v := query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	start = (page - 1) * perPage
	return page, perPage, start
}

func linkHeader(base, path string, query url.Values, pageNum, perPage int, hasNext bool, totalPages int) string {
	if !hasNext && pageNum <= 1 {
		return ""
	}
	q := url.Values{}
	for k, vs := range query {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("per_page", strconv.Itoa(perPage))
	var links []string
	if hasNext {
		q.Set("page", strconv.Itoa(pageNum+1))
		links = append(links, "<"+base+path+"?"+q.Encode()+">; rel=\"next\"")
	}
	last := totalPages
	if last < 1 {
		last = 1
	}
	q.Set("page", strconv.Itoa(last))
	links = append(links, "<"+base+path+"?"+q.Encode()+">; rel=\"last\"")
	return strings.Join(links, ", ")
}

// ownedRepo looks up a repo the token owns; wrong owner or missing repo is 404.
func ownedRepo(w http.ResponseWriter, owner, name string) *Repo {
	if owner != Viewer.Login {
		errJSON(w, 404, "Not Found")
		return nil
	}
	repo := GetRepo(owner, name)
	if repo == nil {
		errJSON(w, 404, "Not Found")
		return nil
	}
	return repo
}

// ---------------------------------------------------------------- dispatch

func newServer() *http.Server {
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleRequest(w, r)
	})
	return &http.Server{
		Handler:      mux,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  0,
	}
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	pathname, query := splitPathQuery(r.URL.RequestURI())
	method := r.Method
	base := "http://" + hostOf(r)

	defer func() {
		if rec := recover(); rec != nil {
			if !headerSent(w) {
				errJSON(w, 500, fmt.Sprint(rec))
			}
		}
	}()

	// Health probe (no auth).
	if method == "GET" && pathname == "/healthz" {
		sendJSON(w, 200, map[string]any{"ok": true}, nil)
		return
	}

	// GraphQL.
	if pathname == "/api/graphql" {
		if method != "POST" {
			errJSON(w, 405, "Method Not Allowed")
			return
		}
		if !isAuthed(r) {
			errJSON(w, 401, "Requires authentication")
			return
		}
		raw, _ := readBody(r)
		val, ok := parseJSONValue(raw)
		if !ok {
			errJSON(w, 400, "Invalid JSON")
			return
		}
		if arr, isArr := val.([]any); isArr {
			var ops []GraphQLOp
			for _, o := range arr {
				m, _ := o.(map[string]any)
				if m == nil {
					m = map[string]any{}
				}
				ops = append(ops, GraphQLOp{
					Query:         strVal(m["query"]),
					Variables:     mapVal(m["variables"]),
					OperationName: strVal(m["operationName"]),
				})
			}
			sendJSON(w, 200, ExecuteGraphQLBatch(ops), nil)
			return
		}
		body, _ := val.(map[string]any)
		if body == nil {
			body = map[string]any{}
		}
		sendJSON(w, 200, ExecuteGraphQL(strVal(body["query"]), mapVal(body["variables"]), strVal(body["operationName"])), nil)
		return
	}

	// MCP.
	if pathname == "/mcp" {
		if method == "DELETE" {
			sendJSON(w, 200, map[string]any{}, nil)
			return
		}
		if method != "POST" {
			errJSON(w, 405, "Method Not Allowed")
			return
		}
		if !isAuthed(r) {
			h := w.Header()
			h.Set("Content-Type", jsonType)
			h.Set("X-GitHub-Request-Id", nextRequestID())
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Unauthorized"}}` + "\n"))
			return
		}
		raw, _ := readBody(r)
		out := HandleMCP(string(raw))
		sendRaw(w, out.Status, jsonType, []byte(out.Body+"\n"), nil)
		return
	}

	// REST v3 root.
	if pathname == "/api/v3" || pathname == "/api/v3/" {
		if method != "GET" {
			errJSON(w, 404, "Not Found")
			return
		}
		sendJSON(w, 200, map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}, map[string]string{"X-Oauth-Scopes": "repo, read:org"})
		return
	}
	if strings.HasPrefix(pathname, "/api/v3/") {
		handleRest(w, r, pathname, query, method, base)
		return
	}

	// Git smart HTTP.
	if strings.Contains(pathname, ".git/") {
		if gitHandle(w, r, pathname, query, method) {
			return
		}
	}

	errJSON(w, 404, "Not Found")
}

func hostOf(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	return "127.0.0.1"
}

func headerSent(w http.ResponseWriter) bool {
	// http.ResponseWriter gives no direct check; track via a wrapper is overkill:
	// handlers only call this from the recover path after a panic, where the
	// response is almost certainly unsent. Return false.
	return false
}

func gitHandle(w http.ResponseWriter, r *http.Request, pathname string, query url.Values, method string) bool {
	// ^/([^/]+)/([^/]+)\.git/(.*)$
	rest := strings.TrimPrefix(pathname, "/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	owner := decSeg(parts[0])
	repoName := decSeg(strings.TrimSuffix(parts[1], ".git"))
	suffix := parts[2]
	if !isAuthed(r) {
		errJSON(w, 401, "Requires authentication")
		return true
	}
	if method == "GET" && suffix == "info/refs" {
		service := query.Get("service")
		out := InfoRefs(owner, repoName, service, r.Header.Get("Git-Protocol"))
		sendRaw(w, out.Status, out.ContentType, out.Body, nil)
		return true
	}
	if method == "POST" && suffix == "git-upload-pack" {
		body, _ := readBody(r)
		out := UploadPack(owner, repoName, body, r.Header.Get("Git-Protocol"))
		sendRaw(w, out.Status, out.ContentType, out.Body, nil)
		return true
	}
	if method == "POST" && suffix == "git-receive-pack" {
		body, _ := readBody(r)
		out := ReceivePack(owner, repoName, body)
		sendRaw(w, out.Status, out.ContentType, out.Body, nil)
		return true
	}
	errJSON(w, 404, "Not Found")
	return true
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func mapVal(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func numVal(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

// ---------------------------------------------------------------- REST

func handleRest(w http.ResponseWriter, r *http.Request, pathname string, query url.Values, method, base string) {
	rest := pathname[len("/api/v3"):]
	segs := splitSegs(rest)

	// Public meta (no auth).
	if method == "GET" && rest == "/meta" {
		sendJSON(w, 200, map[string]any{
			"verifiable_password_authentication": false,
			"installed_version":                  "1.0.0",
			"github_services_sha":                "0000000000000000000000000000000000000000",
			"hooks":                              []any{}, "git": []any{}, "pages": []any{}, "importer": []any{}, "packages": []any{},
		}, nil)
		return
	}

	if !isAuthed(r) {
		errJSON(w, 401, "Requires authentication")
		return
	}

	var rawBody []byte
	if method == "POST" || method == "PATCH" || method == "PUT" || method == "DELETE" {
		rawBody, _ = readBody(r)
	}
	body := map[string]any{}
	if len(rawBody) > 0 {
		parsed, ok := parseJSONBody(rawBody)
		if !ok {
			errJSON(w, 400, "Invalid JSON")
			return
		}
		body = parsed
	}

	// GET /user
	if method == "GET" && len(segs) == 1 && segs[0] == "user" {
		sendJSON(w, 200, map[string]any{"login": Viewer.Login, "id": Viewer.ID, "name": Viewer.Name, "email": Viewer.Email, "type": "User", "html_url": "https://example.invalid/" + Viewer.Login}, nil)
		return
	}

	// GET /rate_limit
	if method == "GET" && len(segs) == 1 && segs[0] == "rate_limit" {
		sendJSON(w, 200, map[string]any{
			"resources": map[string]any{"core": map[string]any{"limit": 5000, "remaining": 5000, "reset": 1700000000, "used": 0}},
			"rate":      map[string]any{"limit": 5000, "remaining": 5000, "reset": 1700000000, "used": 0},
		}, nil)
		return
	}

	// GET /user/repos / POST /user/repos
	if len(segs) == 2 && segs[0] == "user" && segs[1] == "repos" {
		if method == "GET" {
			repos := ViewerRepos()
			pageNum, perPage, start := paginateCount(len(repos), query)
			end := start + perPage
			if end > len(repos) {
				end = len(repos)
			}
			var page []*Repo
			if start < len(repos) {
				page = repos[start:end]
			}
			hasNext := start+perPage < len(repos)
			totalPages := (len(repos) + perPage - 1) / perPage
			if totalPages < 1 {
				totalPages = 1
			}
			link := linkHeader(base, pathname, query, pageNum, perPage, hasNext, totalPages)
			out := make([]repoJSONOut, 0, len(page))
			for _, repo := range page {
				repo.mu.RLock()
				out = append(out, repoJSON(repo))
				repo.mu.RUnlock()
			}
			var extra map[string]string
			if link != "" {
				extra = map[string]string{"Link": link}
			}
			sendJSON(w, 200, out, extra)
			return
		}
		if method == "POST" {
			name := strVal(body["name"])
			if name == "" {
				errJSON(w, 422, "Repository name is required")
				return
			}
			desc := strVal(body["description"])
			priv, _ := body["private"].(bool)
			created, err := CreateRepo(name, desc, priv)
			if err != nil {
				errJSON(w, 422, err.Error())
				return
			}
			created.mu.RLock()
			out := repoJSON(created)
			created.mu.RUnlock()
			sendJSON(w, 201, out, nil)
			return
		}
	}

	// /repos/{owner}/{repo}[...]
	if len(segs) >= 3 && segs[0] == "repos" {
		owner := segs[1]
		name := segs[2]
		tail := segs[3:]

		if len(tail) == 0 {
			if method == "GET" {
				repo := ownedRepo(w, owner, name)
				if repo == nil {
					return
				}
				repo.mu.RLock()
				out := repoJSON(repo)
				repo.mu.RUnlock()
				sendJSON(w, 200, out, nil)
				return
			}
			if method == "PATCH" {
				repo := ownedRepo(w, owner, name)
				if repo == nil {
					return
				}
				repo.mu.Lock()
				if d, ok := body["description"].(string); ok {
					repo.Description = d
				}
				if p, ok := body["private"].(bool); ok {
					repo.Private = p
				}
				out := repoJSON(repo)
				repo.mu.Unlock()
				sendJSON(w, 200, out, nil)
				return
			}
			if method == "DELETE" {
				repo := ownedRepo(w, owner, name)
				if repo == nil {
					return
				}
				_ = repo
				DeleteRepo(owner, name)
				sendJSON(w, 204, nil, nil)
				return
			}
			errJSON(w, 404, "Not Found")
			return
		}

		repo := ownedRepo(w, owner, name)
		if repo == nil {
			return
		}

		// /branches
		if tail[0] == "branches" {
			if method == "GET" && len(tail) == 1 {
				repo.mu.RLock()
				var branches []string
				for ref := range repo.Refs {
					if strings.HasPrefix(ref, "refs/heads/") {
						branches = append(branches, ref[len("refs/heads/"):])
					}
				}
				sort.Strings(branches)
				out := make([]map[string]any, 0, len(branches))
				for _, b := range branches {
					out = append(out, map[string]any{"name": b, "commit": map[string]any{"sha": repo.Refs["refs/heads/"+b]}, "protected": false})
				}
				repo.mu.RUnlock()
				sendJSON(w, 200, out, nil)
				return
			}
			if method == "GET" && len(tail) == 2 {
				repo.mu.RLock()
				sha, ok := repo.Refs["refs/heads/"+tail[1]]
				repo.mu.RUnlock()
				if !ok {
					errJSON(w, 404, "Not Found")
					return
				}
				sendJSON(w, 200, map[string]any{"name": tail[1], "commit": map[string]any{"sha": sha}, "protected": false}, nil)
				return
			}
			errJSON(w, 404, "Not Found")
			return
		}

		// /commits
		if tail[0] == "commits" {
			if method == "GET" && len(tail) == 1 {
				ref := query.Get("sha")
				if ref == "" {
					ref = repo.DefaultBranch
				}
				_, perPage, _ := paginateCount(0, query)
				repo.mu.RLock()
				commits := ListCommits(repo, ref, perPage)
				repo.mu.RUnlock()
				out := make([]map[string]any, 0, len(commits))
				for _, c := range commits {
					out = append(out, map[string]any{
						"sha": c.Sha,
						"commit": map[string]any{"message": c.Message,
							"author":    map[string]any{"name": Viewer.Name, "email": Viewer.Email},
							"committer": map[string]any{"name": Viewer.Name, "email": Viewer.Email}},
						"author":  map[string]any{"login": Viewer.Login, "id": Viewer.ID},
						"parents": []any{},
					})
				}
				sendJSON(w, 200, out, nil)
				return
			}
			if method == "GET" && len(tail) == 2 {
				sha := tail[1]
				repo.mu.RLock()
				obj, ok := repo.Objects[sha]
				var parents []string
				var tree, msg string
				if ok && obj.Type == ObjCommit {
					c := ParseCommit(obj.Data)
					parents, tree, msg = c.Parents, c.Tree, strings.TrimSuffix(c.Message, "\n")
				}
				repo.mu.RUnlock()
				if !ok || obj.Type != ObjCommit {
					errJSON(w, 404, "Not Found")
					return
				}
				plist := make([]map[string]any, 0, len(parents))
				for _, p := range parents {
					plist = append(plist, map[string]any{"sha": p})
				}
				sendJSON(w, 200, map[string]any{
					"sha": sha,
					"commit": map[string]any{"message": msg,
						"tree":      map[string]any{"sha": tree},
						"author":    map[string]any{"name": Viewer.Name, "email": Viewer.Email},
						"committer": map[string]any{"name": Viewer.Name, "email": Viewer.Email}},
					"parents": plist,
				}, nil)
				return
			}
			errJSON(w, 404, "Not Found")
			return
		}

		// /git/refs
		if tail[0] == "git" && len(tail) > 1 && tail[1] == "refs" {
			if method == "POST" && len(tail) == 2 {
				ref := strVal(body["ref"])
				sha := strVal(body["sha"])
				repo.mu.Lock()
				_, hasObj := repo.Objects[sha]
				_, exists := repo.Refs[ref]
				if !strings.HasPrefix(ref, "refs/") || !hasObj {
					repo.mu.Unlock()
					errJSON(w, 422, "Invalid ref or sha")
					return
				}
				if exists {
					repo.mu.Unlock()
					errJSON(w, 422, "Reference "+ref+" already exists")
					return
				}
				repo.Refs[ref] = sha
				repo.mu.Unlock()
				sendJSON(w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}, "url": "/api/v3/repos/" + owner + "/" + name + "/git/refs/" + ref}, nil)
				return
			}
			if method == "GET" && len(tail) == 2 {
				repo.mu.RLock()
				refs := make([]map[string]any, 0, len(repo.Refs))
				for ref, sha := range repo.Refs {
					refs = append(refs, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}})
				}
				repo.mu.RUnlock()
				sendJSON(w, 200, refs, nil)
				return
			}
			if method == "GET" && len(tail) >= 3 {
				ref := strings.Join(tail[2:], "/")
				repo.mu.RLock()
				sha, ok := repo.Refs[ref]
				repo.mu.RUnlock()
				if !ok {
					errJSON(w, 404, "Not Found")
					return
				}
				sendJSON(w, 200, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
				return
			}
			if method == "PATCH" && len(tail) >= 3 {
				ref := strings.Join(tail[2:], "/")
				sha := strVal(body["sha"])
				repo.mu.Lock()
				_, hasRef := repo.Refs[ref]
				_, hasObj := repo.Objects[sha]
				if !hasRef || !hasObj {
					repo.mu.Unlock()
					errJSON(w, 422, "Invalid ref or sha")
					return
				}
				repo.Refs[ref] = sha
				repo.mu.Unlock()
				sendJSON(w, 200, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
				return
			}
			errJSON(w, 404, "Not Found")
			return
		}

		// /contents/{path...}
		if tail[0] == "contents" {
			handleContents(w, r, repo, owner, name, tail[1:], method, query, body)
			return
		}

		// /labels
		if tail[0] == "labels" {
			if method == "POST" && len(tail) == 1 {
				lname := strVal(body["name"])
				if lname == "" {
					errJSON(w, 422, "Label name is required")
					return
				}
				color := strVal(body["color"])
				if color == "" {
					color = "ededed"
				}
				repo.mu.Lock()
				label := CreateLabel(repo, lname, color)
				repo.mu.Unlock()
				sendJSON(w, 201, labelJSONOut{ID: label.ID, Name: label.Name, Color: label.Color, Description: ""}, nil)
				return
			}
			if method == "GET" && len(tail) == 1 {
				repo.mu.RLock()
				out := make([]labelJSONOut, 0, len(repo.Labels))
				for _, l := range repo.Labels {
					out = append(out, labelJSONOut{ID: l.ID, Name: l.Name, Color: l.Color, Description: ""})
				}
				repo.mu.RUnlock()
				sendJSON(w, 200, out, nil)
				return
			}
			errJSON(w, 404, "Not Found")
			return
		}

		// /issues
		if tail[0] == "issues" {
			handleIssues(w, r, repo, tail[1:], method, query, body)
			return
		}

		// /pulls
		if tail[0] == "pulls" {
			handlePulls(w, r, repo, tail[1:], method, query, body)
			return
		}

		errJSON(w, 404, "Not Found")
		return
	}

	errJSON(w, 404, "Not Found")
}

func handleContents(w http.ResponseWriter, r *http.Request, repo *Repo, owner, name string, tail []string, method string, query url.Values, body map[string]any) {
	filePath := strings.Join(tail, "/")
	if method == "PUT" {
		if filePath == "" {
			errJSON(w, 404, "Not Found")
			return
		}
		content := strVal(body["content"])
		message := strVal(body["message"])
		if message == "" {
			message = "update"
		}
		branch, hasBranch := body["branch"].(string)
		sha, hasSha := body["sha"].(string)
		repo.mu.Lock()
		result, cerr := PutContent(repo, filePath, content, message, branch, sha, hasBranch, hasSha)
		repo.mu.Unlock()
		if cerr != nil {
			errJSON(w, cerr.Status, cerr.Msg)
			return
		}
		size := 0
		if raw, err := base64.StdEncoding.DecodeString(stripB64Whitespace(content)); err == nil {
			size = len(raw)
		}
		status := 201
		if hasSha {
			status = 200
		}
		sendJSON(w, status, map[string]any{
			"content": map[string]any{"name": lastSeg(filePath), "path": filePath, "sha": result.ContentSha, "size": size, "type": "file", "encoding": "base64"},
			"commit":  map[string]any{"sha": result.CommitSha, "message": message},
		}, nil)
		return
	}
	if method == "DELETE" {
		if filePath == "" {
			errJSON(w, 404, "Not Found")
			return
		}
		message := strVal(body["message"])
		if message == "" {
			message = "delete"
		}
		branch, hasBranch := body["branch"].(string)
		sha, hasSha := body["sha"].(string)
		repo.mu.Lock()
		commitSha, cerr := DeleteContent(repo, filePath, message, branch, sha, hasBranch, hasSha)
		repo.mu.Unlock()
		if cerr != nil {
			errJSON(w, cerr.Status, cerr.Msg)
			return
		}
		sendJSON(w, 200, map[string]any{"commit": map[string]any{"sha": commitSha, "message": message}}, nil)
		return
	}
	if method == "GET" {
		ref := query.Get("ref")
		if ref == "" {
			ref = repo.DefaultBranch
		}
		repo.mu.RLock()
		tip, ok := ResolveRef(repo, ref)
		var treeOID string
		var tipObj *GitObject
		if ok {
			if o, oOk := repo.Objects[tip]; oOk && o.Type == ObjCommit {
				tipObj = &o
				treeOID = ParseCommit(o.Data).Tree
			}
		}
		if !ok || tipObj == nil {
			repo.mu.RUnlock()
			errJSON(w, 404, "Not Found")
			return
		}
		if filePath == "" {
			treeData := []byte{}
			if o, oOk := repo.Objects[treeOID]; oOk {
				treeData = o.Data
			}
			entries, _ := ParseTree(treeData)
			out := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				typ := "file"
				if e.Mode == "40000" {
					typ = "dir"
				}
				out = append(out, map[string]any{"type": typ, "name": e.Name, "path": e.Name, "sha": e.OID, "size": 0})
			}
			repo.mu.RUnlock()
			sendJSON(w, 200, out, nil)
			return
		}
		files := FlattenTree(treeOID, repo.lookup)
		entry, isFile := files[filePath]
		if isFile {
			blob := repo.Objects[entry.OID]
			data := blob.Data
			repo.mu.RUnlock()
			sendJSON(w, 200, map[string]any{
				"type": "file", "encoding": "base64", "size": len(data),
				"name": lastSeg(filePath), "path": filePath,
				"content": base64.StdEncoding.EncodeToString(data), "sha": entry.OID,
			}, nil)
			return
		}
		// Directory listing.
		prefix := filePath + "/"
		children := map[string]struct {
			typ string
			sha string
		}{}
		for p, e := range files {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := p[len(prefix):]
			if slash := strings.IndexByte(rest, '/'); slash < 0 {
				children[rest] = struct {
					typ string
					sha string
				}{"file", e.OID}
			} else {
				children[rest[:slash]] = struct {
					typ string
					sha string
				}{"dir", ""}
			}
		}
		if len(children) == 0 {
			repo.mu.RUnlock()
			errJSON(w, 404, "Not Found")
			return
		}
		var cur []byte
		if o, oOk := repo.Objects[treeOID]; oOk {
			cur = o.Data
		}
		for _, seg := range strings.Split(filePath, "/") {
			var found *TreeEntry
			if cur != nil {
				entries, _ := ParseTree(cur)
				for i, e := range entries {
					if e.Name == seg {
						found = &entries[i]
						break
					}
				}
			}
			if found == nil {
				cur = nil
				break
			}
			if o, oOk := repo.Objects[found.OID]; oOk {
				cur = o.Data
			} else {
				cur = nil
			}
		}
		dirMap := map[string]string{}
		if cur != nil {
			entries, _ := ParseTree(cur)
			for _, e := range entries {
				dirMap[e.Name] = e.OID
			}
		}
		names := make([]string, 0, len(children))
		for n := range children {
			names = append(names, n)
		}
		sort.Strings(names)
		out := make([]map[string]any, 0, len(names))
		for _, n := range names {
			info := children[n]
			sha := info.sha
			if info.typ == "dir" {
				sha = dirMap[n]
			}
			out = append(out, map[string]any{"type": info.typ, "name": n, "path": filePath + "/" + n, "sha": sha, "size": 0})
		}
		repo.mu.RUnlock()
		sendJSON(w, 200, out, nil)
		return
	}
	errJSON(w, 404, "Not Found")
}

func lastSeg(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func handleIssues(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string, method string, query url.Values, body map[string]any) {
	if len(tail) == 0 {
		if method == "POST" {
			title := strVal(body["title"])
			if title == "" {
				errJSON(w, 422, "Issue title is required")
				return
			}
			var labelNames []string
			if arr, ok := body["labels"].([]any); ok {
				for _, l := range arr {
					switch v := l.(type) {
					case string:
						labelNames = append(labelNames, v)
					case map[string]any:
						if s, ok := v["name"].(string); ok {
							labelNames = append(labelNames, s)
						}
					}
				}
			}
			repo.mu.Lock()
			created, rerr := CreateIssue(repo, title, strVal(body["body"]), labelNames)
			if rerr != nil {
				repo.mu.Unlock()
				errJSON(w, 422, rerr.Msg)
				return
			}
			buf := issueBody(repo, created)
			repo.mu.Unlock()
			sendBuf(w, 201, buf)
			return
		}
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			sortDir := query.Get("direction")
			if sortDir == "" {
				sortDir = "desc"
			}
			sortBy := query.Get("sort")
			if sortBy == "" {
				sortBy = "created"
			}
			repo.mu.RLock()
			var list []*Issue
			for _, is := range repo.Issues {
				if state != "all" && is.State != state {
					continue
				}
				list = append(list, is)
			}
			if sortBy == "created" {
				if sortDir == "asc" {
					sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
				} else {
					sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
				}
			}
			_, perPage, start := paginateCount(len(list), query)
			end := start + perPage
			if end > len(list) {
				end = len(list)
			}
			var page []*Issue
			if start < len(list) {
				page = list[start:end]
			}
			var sb strings.Builder
			sb.WriteByte('[')
			for i, is := range page {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(strings.TrimSuffix(issueBodyText(repo, is), "\n"))
			}
			sb.WriteString("]\n")
			text := sb.String()
			repo.mu.RUnlock()
			h := w.Header()
			h.Set("Content-Type", jsonType)
			h.Set("X-GitHub-Request-Id", nextRequestID())
			h.Set("Content-Length", strconv.Itoa(len(text)))
			w.WriteHeader(200)
			_, _ = w.Write([]byte(text))
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(tail) == 1 {
		repo.mu.RLock()
		issue := repo.IssuesByNum[num]
		repo.mu.RUnlock()
		if issue == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		if method == "GET" {
			repo.mu.RLock()
			buf := issueBody(repo, issue)
			repo.mu.RUnlock()
			sendBuf(w, 200, buf)
			return
		}
		if method == "PATCH" {
			repo.mu.Lock()
			if s, ok := body["state"].(string); ok && (s == "open" || s == "closed") {
				issue.State = s
			}
			if t, ok := body["title"].(string); ok {
				issue.Title = t
			}
			if b, ok := body["body"].(string); ok {
				issue.Body = b
			}
			issue.touch()
			buf := issueBody(repo, issue)
			repo.mu.Unlock()
			sendBuf(w, 200, buf)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		repo.mu.RLock()
		issue := repo.IssuesByNum[num]
		repo.mu.RUnlock()
		if issue == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		if method == "POST" {
			repo.mu.Lock()
			comment := AddComment(repo, issue.Number, strVal(body["body"]))
			repo.mu.Unlock()
			if comment == nil {
				errJSON(w, 404, "Not Found")
				return
			}
			sendJSON(w, 201, map[string]any{"id": comment.ID, "body": comment.Body, "user": map[string]any{"login": Viewer.Login, "id": Viewer.ID}, "created_at": isoTs(comment.CreatedTs)}, nil)
			return
		}
		if method == "GET" {
			repo.mu.RLock()
			out := make([]map[string]any, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				out = append(out, map[string]any{"id": c.ID, "body": c.Body, "user": map[string]any{"login": Viewer.Login, "id": Viewer.ID}, "created_at": isoTs(c.CreatedTs)})
			}
			repo.mu.RUnlock()
			sendJSON(w, 200, out, nil)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	errJSON(w, 404, "Not Found")
}

func handlePulls(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string, method string, query url.Values, body map[string]any) {
	if len(tail) == 0 {
		if method == "POST" {
			title := strVal(body["title"])
			head := strVal(body["head"])
			base := strVal(body["base"])
			if base == "" {
				base = repo.DefaultBranch
			}
			if title == "" || head == "" {
				errJSON(w, 422, "Pull request title and head are required")
				return
			}
			repo.mu.Lock()
			created, rerr := CreatePull(repo, title, strVal(body["body"]), head, base)
			if rerr != nil {
				repo.mu.Unlock()
				errJSON(w, 422, rerr.Msg)
				return
			}
			out := pullJSON(repo, created)
			repo.mu.Unlock()
			sendJSON(w, 201, out, nil)
			return
		}
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			repo.mu.RLock()
			var list []*Pull
			for _, p := range repo.Pulls {
				if state != "all" && p.State != state {
					continue
				}
				list = append(list, p)
			}
			_, perPage, start := paginateCount(len(list), query)
			end := start + perPage
			if end > len(list) {
				end = len(list)
			}
			var page []*Pull
			if start < len(list) {
				page = list[start:end]
			}
			out := make([]pullJSONOut, 0, len(page))
			for _, p := range page {
				out = append(out, pullJSON(repo, p))
			}
			repo.mu.RUnlock()
			sendJSON(w, 200, out, nil)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	repo.mu.RLock()
	pull := repo.PullsByNum[num]
	repo.mu.RUnlock()
	if len(tail) == 1 {
		if pull == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		if method == "GET" {
			repo.mu.RLock()
			out := pullJSON(repo, pull)
			repo.mu.RUnlock()
			sendJSON(w, 200, out, nil)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	if len(tail) == 2 && tail[1] == "files" {
		if pull == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		if method == "GET" {
			repo.mu.RLock()
			diff := DiffFiles(repo, pull.BaseSha, pull.HeadSha)
			repo.mu.RUnlock()
			out := make([]map[string]any, 0, len(diff))
			for _, f := range diff {
				out = append(out, map[string]any{
					"filename": f.Filename, "status": f.Status, "additions": f.Additions,
					"deletions": f.Deletions, "changes": f.Changes,
					"sha": "", "blob_url": "", "raw_url": "", "contents_url": "",
				})
			}
			sendJSON(w, 200, out, nil)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	if len(tail) == 2 && tail[1] == "merge" {
		if pull == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		if method == "PUT" {
			repo.mu.Lock()
			sha, cerr := MergePull(repo, pull)
			repo.mu.Unlock()
			if cerr != nil {
				errJSON(w, cerr.Status, cerr.Msg)
				return
			}
			sendJSON(w, 200, map[string]any{"sha": sha, "merged": true, "message": pull.Title + " (#" + strconv.Itoa(pull.Number) + ")"}, nil)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	errJSON(w, 404, "Not Found")
}
