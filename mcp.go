// MCP endpoint (streamable HTTP, revision 2026-07-28 + legacy initialize).
// Tools act on the same store as REST.

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const MCPServerName = "forge"
const MCPServerVersion = "1.0.0"

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18"}

type toolDef struct {
	name        string
	description string
	properties  map[string]toolProp
	required    []string
}

type toolProp struct {
	typ         string
	description string
}

var tools = []toolDef{
	{name: "create_repo", description: "Create a new repository owned by the authenticated user.", properties: map[string]toolProp{
		"name":        {typ: "string", description: "Repository name"},
		"description": {typ: "string", description: "Repository description"},
		"private":     {typ: "boolean", description: "Whether the repository is private"},
		"owner":       {typ: "string", description: "Owner login (defaults to the authenticated user)"},
	}, required: []string{"name"}},
	{name: "get_repo", description: "Get a repository by owner and name.", properties: map[string]toolProp{
		"owner": {typ: "string", description: "Owner login"},
		"repo":  {typ: "string", description: "Repository name"},
	}, required: []string{"owner", "repo"}},
	{name: "list_repos", description: "List repositories owned by the authenticated user.", properties: map[string]toolProp{
		"owner": {typ: "string", description: "Owner login"},
	}, required: []string{}},
	{name: "create_issue", description: "Create an issue in a repository.", properties: map[string]toolProp{
		"owner": {typ: "string", description: "Owner login"},
		"repo":  {typ: "string", description: "Repository name"},
		"title": {typ: "string", description: "Issue title"},
		"body":  {typ: "string", description: "Issue body"},
	}, required: []string{"owner", "repo", "title"}},
	{name: "get_issue", description: "Get one issue by number.", properties: map[string]toolProp{
		"owner":  {typ: "string", description: "Owner login"},
		"repo":   {typ: "string", description: "Repository name"},
		"number": {typ: "integer", description: "Issue number"},
	}, required: []string{"owner", "repo", "number"}},
	{name: "list_issues", description: "List issues in a repository, optionally filtered by state.", properties: map[string]toolProp{
		"owner": {typ: "string", description: "Owner login"},
		"repo":  {typ: "string", description: "Repository name"},
		"state": {typ: "string", description: "Issue state: open, closed or all"},
	}, required: []string{"owner", "repo"}},
	{name: "create_comment", description: "Add a comment to an issue.", properties: map[string]toolProp{
		"owner":  {typ: "string", description: "Owner login"},
		"repo":   {typ: "string", description: "Repository name"},
		"number": {typ: "integer", description: "Issue number"},
		"body":   {typ: "string", description: "Comment body"},
	}, required: []string{"owner", "repo", "number", "body"}},
	{name: "create_pull_request", description: "Open a pull request from a head branch into a base branch.", properties: map[string]toolProp{
		"owner": {typ: "string", description: "Owner login"},
		"repo":  {typ: "string", description: "Repository name"},
		"title": {typ: "string", description: "Pull request title"},
		"body":  {typ: "string", description: "Pull request body"},
		"head":  {typ: "string", description: "Head branch name"},
		"base":  {typ: "string", description: "Base branch name"},
	}, required: []string{"owner", "repo", "title", "head", "base"}},
	{name: "get_pull_request", description: "Get one pull request by number.", properties: map[string]toolProp{
		"owner":  {typ: "string", description: "Owner login"},
		"repo":   {typ: "string", description: "Repository name"},
		"number": {typ: "integer", description: "Pull request number"},
	}, required: []string{"owner", "repo", "number"}},
	{name: "list_pull_requests", description: "List pull requests in a repository.", properties: map[string]toolProp{
		"owner": {typ: "string", description: "Owner login"},
		"repo":  {typ: "string", description: "Repository name"},
		"state": {typ: "string", description: "Pull request state: open, closed or all"},
	}, required: []string{"owner", "repo"}},
	{name: "merge_pull_request", description: "Merge a pull request with a squash commit.", properties: map[string]toolProp{
		"owner":       {typ: "string", description: "Owner login"},
		"repo":        {typ: "string", description: "Repository name"},
		"pull_number": {typ: "integer", description: "Pull request number"},
	}, required: []string{"owner", "repo", "pull_number"}},
}

// toolsListJSON returns the tools/list payload, built from ordered structs so
// two calls serialize byte for byte identically.
func toolsListJSON() []map[string]any {
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		props := map[string]any{}
		// Keep property order stable by building the map once per tool in
		// definition order: encoding/json sorts map keys, which is also
		// deterministic across calls.
		for name, p := range t.properties {
			props[name] = map[string]any{"type": p.typ, "description": p.description}
		}
		req := make([]any, len(t.required))
		for j, r := range t.required {
			req[j] = r
		}
		out[i] = map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": map[string]any{"type": "object", "properties": props, "required": req},
		}
	}
	return out
}

func mcpRepoJSON(repo *Repo) map[string]any {
	return map[string]any{
		"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name,
		"owner": map[string]any{"login": repo.Owner, "id": Viewer.ID}, "private": repo.Private,
		"description": repo.Description, "default_branch": repo.DefaultBranch,
		"html_url":  "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		"clone_url": "https://example.invalid/" + repo.Owner + "/" + repo.Name + ".git",
	}
}

func mcpIssueJSON(repo *Repo, issue *Issue) map[string]any {
	labels := make([]any, len(issue.Labels))
	for i, l := range issue.Labels {
		labels[i] = map[string]any{"name": l.Name, "color": l.Color}
	}
	return map[string]any{
		"id": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State,
		"labels":   labels,
		"user":     map[string]any{"login": Viewer.Login, "id": Viewer.ID},
		"html_url": "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

func mcpPullJSON(repo *Repo, pull *Pull) map[string]any {
	return map[string]any{
		"id": pull.ID, "number": pull.Number, "title": pull.Title, "body": pull.Body, "state": pull.State,
		"merged": pull.Merged, "merged_at": strOrNil(pull.MergedAt), "merge_commit_sha": strOrNil(pull.MergeCommitSha),
		"head":     map[string]any{"ref": pull.HeadRef, "sha": pull.HeadSha, "label": repo.Owner + ":" + pull.HeadRef},
		"base":     map[string]any{"ref": pull.BaseRef, "sha": pull.BaseSha, "label": repo.Owner + ":" + pull.BaseRef},
		"html_url": "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
	}
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type toolOutcome struct {
	ok   bool
	data any
	msg  string
}

func mcpFindRepo(owner, repo any) *Repo {
	o, ok1 := owner.(string)
	r, ok2 := repo.(string)
	if !ok1 || !ok2 {
		return nil
	}
	return GetRepo(o, r)
}

func mcpStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func mcpNum(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		if matched, _ := regexp.MatchString(`^\d+$`, n); matched {
			i, err := strconv.Atoi(n)
			return i, err == nil
		}
	}
	return 0, false
}

func callTool(name string, args map[string]any) toolOutcome {
	switch name {
	case "create_repo":
		repoName := mcpStr(args["name"])
		if repoName == "" {
			return toolOutcome{ok: false, msg: "name is required"}
		}
		priv, _ := args["private"].(bool)
		created, err := CreateRepo(repoName, mcpStr(args["description"]), priv)
		if err != nil {
			return toolOutcome{ok: false, msg: err.Error()}
		}
		created.mu.RLock()
		defer created.mu.RUnlock()
		return toolOutcome{ok: true, data: mcpRepoJSON(created)}
	case "get_repo":
		repo := mcpFindRepo(args["owner"], args["repo"])
		if repo == nil {
			return toolOutcome{ok: false, msg: fmt.Sprintf("Repository %v/%v not found", args["owner"], args["repo"])}
		}
		repo.mu.RLock()
		defer repo.mu.RUnlock()
		return toolOutcome{ok: true, data: mcpRepoJSON(repo)}
	case "list_repos":
		var list []any
		for _, r := range AllRepos() {
			if r.Owner != Viewer.Login {
				continue
			}
			r.mu.RLock()
			list = append(list, mcpRepoJSON(r))
			r.mu.RUnlock()
		}
		if list == nil {
			list = []any{}
		}
		return toolOutcome{ok: true, data: map[string]any{"repositories": list}}
	case "create_issue":
		repo := mcpFindRepo(args["owner"], args["repo"])
		if repo == nil {
			return toolOutcome{ok: false, msg: fmt.Sprintf("Repository %v/%v not found", args["owner"], args["repo"])}
		}
		repo.mu.Lock()
		created, rerr := CreateIssue(repo, mcpStr(args["title"]), mcpStr(args["body"]), nil)
		if rerr != nil {
			repo.mu.Unlock()
			return toolOutcome{ok: false, msg: rerr.Msg}
		}
		out := mcpIssueJSON(repo, created)
		repo.mu.Unlock()
		return toolOutcome{ok: true, data: out}
	case "get_issue":
		repo := mcpFindRepo(args["owner"], args["repo"])
		n, nOk := mcpNum(args["number"])
		var issue *Issue
		if repo != nil && nOk {
			repo.mu.RLock()
			issue = repo.IssuesByNum[n]
			repo.mu.RUnlock()
		}
		if repo == nil || issue == nil {
			return toolOutcome{ok: false, msg: "Issue not found"}
		}
		repo.mu.RLock()
		defer repo.mu.RUnlock()
		return toolOutcome{ok: true, data: mcpIssueJSON(repo, issue)}
	case "list_issues":
		repo := mcpFindRepo(args["owner"], args["repo"])
		if repo == nil {
			return toolOutcome{ok: false, msg: fmt.Sprintf("Repository %v/%v not found", args["owner"], args["repo"])}
		}
		state := mcpStr(args["state"])
		if state == "" {
			state = "open"
		}
		repo.mu.RLock()
		var list []any
		for _, is := range repo.Issues {
			if state != "all" && is.State != state {
				continue
			}
			list = append(list, mcpIssueJSON(repo, is))
		}
		repo.mu.RUnlock()
		if list == nil {
			list = []any{}
		}
		return toolOutcome{ok: true, data: map[string]any{"issues": list}}
	case "create_comment":
		repo := mcpFindRepo(args["owner"], args["repo"])
		n, nOk := mcpNum(args["number"])
		if repo == nil || !nOk {
			return toolOutcome{ok: false, msg: "Issue not found"}
		}
		repo.mu.Lock()
		comment := AddComment(repo, n, mcpStr(args["body"]))
		repo.mu.Unlock()
		if comment == nil {
			return toolOutcome{ok: false, msg: "Issue not found"}
		}
		return toolOutcome{ok: true, data: map[string]any{"id": comment.ID, "body": comment.Body}}
	case "create_pull_request":
		repo := mcpFindRepo(args["owner"], args["repo"])
		if repo == nil {
			return toolOutcome{ok: false, msg: fmt.Sprintf("Repository %v/%v not found", args["owner"], args["repo"])}
		}
		base := mcpStr(args["base"])
		if base == "" {
			base = repo.DefaultBranch
		}
		repo.mu.Lock()
		created, rerr := CreatePull(repo, mcpStr(args["title"]), mcpStr(args["body"]), mcpStr(args["head"]), base)
		if rerr != nil {
			repo.mu.Unlock()
			return toolOutcome{ok: false, msg: rerr.Msg}
		}
		out := mcpPullJSON(repo, created)
		repo.mu.Unlock()
		return toolOutcome{ok: true, data: out}
	case "get_pull_request":
		repo := mcpFindRepo(args["owner"], args["repo"])
		n, nOk := mcpNum(args["number"])
		var pull *Pull
		if repo != nil && nOk {
			repo.mu.RLock()
			pull = repo.PullsByNum[n]
			repo.mu.RUnlock()
		}
		if repo == nil || pull == nil {
			return toolOutcome{ok: false, msg: "Pull request not found"}
		}
		repo.mu.RLock()
		defer repo.mu.RUnlock()
		return toolOutcome{ok: true, data: mcpPullJSON(repo, pull)}
	case "list_pull_requests":
		repo := mcpFindRepo(args["owner"], args["repo"])
		if repo == nil {
			return toolOutcome{ok: false, msg: fmt.Sprintf("Repository %v/%v not found", args["owner"], args["repo"])}
		}
		state := mcpStr(args["state"])
		if state == "" {
			state = "open"
		}
		repo.mu.RLock()
		var list []any
		for _, p := range repo.Pulls {
			if state != "all" && p.State != state {
				continue
			}
			list = append(list, mcpPullJSON(repo, p))
		}
		repo.mu.RUnlock()
		if list == nil {
			list = []any{}
		}
		return toolOutcome{ok: true, data: map[string]any{"pull_requests": list}}
	case "merge_pull_request":
		repo := mcpFindRepo(args["owner"], args["repo"])
		n, nOk := mcpNum(args["pull_number"])
		if !nOk {
			n, nOk = mcpNum(args["number"])
		}
		var pull *Pull
		if repo != nil && nOk {
			repo.mu.RLock()
			pull = repo.PullsByNum[n]
			repo.mu.RUnlock()
		}
		if repo == nil || pull == nil {
			return toolOutcome{ok: false, msg: "Pull request not found"}
		}
		repo.mu.Lock()
		sha, merr := MergePull(repo, pull)
		repo.mu.Unlock()
		if merr != nil {
			return toolOutcome{ok: false, msg: merr.Msg}
		}
		return toolOutcome{ok: true, data: map[string]any{"merged": true, "sha": sha, "message": pull.Title + " (#" + strconv.Itoa(pull.Number) + ")", "number": pull.Number}}
	default:
		return toolOutcome{ok: false, msg: "Unknown tool: " + name}
	}
}

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

// normalizeToolName matches a tool name the way the suite does: a server
// prefix (github__create_issue) is matched as a suffix, case-insensitively.
func normalizeToolName(name string) string {
	lower := strings.ToLower(name)
	if i := strings.LastIndex(lower, "__"); i >= 0 {
		lower = lower[i+2:]
	}
	return lower
}

func findTool(name string) *toolDef {
	want := normalizeToolName(name)
	for i := range tools {
		if normalizeToolName(tools[i].name) == want {
			return &tools[i]
		}
	}
	return nil
}

type mcpOut struct {
	Status int
	Body   string
}

// HandleMCP handles one POST /mcp body (already read as text). Auth is checked
// by the caller (401 before this runs).
func HandleMCP(rawBody string) mcpOut {
	toJSON := func(obj any) string {
		b, _ := json.Marshal(obj)
		return string(b)
	}
	var parsed any
	dec := json.NewDecoder(strings.NewReader(rawBody))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return mcpOut{Status: 400, Body: toJSON(rpcError(nil, -32700, "Parse error"))}
	}
	if parsed == nil {
		return mcpOut{Status: 400, Body: toJSON(rpcError(nil, -32600, "Invalid Request"))}
	}
	req, ok := parsed.(map[string]any)
	if !ok {
		return mcpOut{Status: 400, Body: toJSON(rpcError(nil, -32600, "Invalid Request"))}
	}
	_, hasID := req["id"]
	id := req["id"]
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// Notifications (no id): acknowledge with 202 and no JSON-RPC body.
	if !hasID {
		return mcpOut{Status: 202, Body: "{}"}
	}

	switch method {
	case "server/discover":
		serverInfo := map[string]any{"name": MCPServerName, "version": MCPServerVersion}
		return mcpOut{Status: 200, Body: toJSON(rpcResult(id, map[string]any{
			"supportedVersions": supportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"instructions":      "A GitHub-compatible git forge: create repos, issues and pull requests.",
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}))}
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		negotiated := "2026-07-28"
		for _, v := range supportedVersions {
			if v == requested {
				negotiated = v
				break
			}
		}
		return mcpOut{Status: 200, Body: toJSON(rpcResult(id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": MCPServerName, "version": MCPServerVersion},
		}))}
	case "ping":
		return mcpOut{Status: 200, Body: toJSON(rpcResult(id, map[string]any{}))}
	case "tools/list":
		return mcpOut{Status: 200, Body: toJSON(rpcResult(id, map[string]any{"tools": toolsListJSON()}))}
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		// The suite maps each argument onto the schema's nearest property
		// name and fills a full_name property with owner/repo.
		if fullName, ok := args["full_name"].(string); ok {
			if _, hasOwner := args["owner"]; !hasOwner {
				if _, hasRepo := args["repo"]; !hasRepo {
					if parts := strings.SplitN(fullName, "/", 2); len(parts) == 2 {
						args["owner"] = parts[0]
						args["repo"] = parts[1]
					}
				}
			}
		}
		tool := findTool(name)
		if tool == nil {
			return mcpOut{Status: 200, Body: toJSON(rpcError(id, -32602, "Unknown tool: "+name))}
		}
		var missing []string
		for _, r := range tool.required {
			v := args[r]
			if v == nil || v == "" {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			return mcpOut{Status: 200, Body: toJSON(rpcError(id, -32602, "Missing required argument(s): "+strings.Join(missing, ", ")))}
		}
		outcome := callTool(tool.name, args)
		if !outcome.ok {
			return mcpOut{Status: 200, Body: toJSON(rpcResult(id, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": outcome.msg}}, "isError": true,
			}))}
		}
		text, _ := json.Marshal(outcome.data)
		return mcpOut{Status: 200, Body: toJSON(rpcResult(id, map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(text)}},
			"structuredContent": outcome.data, "isError": false,
		}))}
	default:
		return mcpOut{Status: 200, Body: toJSON(rpcError(id, -32601, "Method not found: "+method))}
	}
}
