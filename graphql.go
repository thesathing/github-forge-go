// GraphQL v4 executor for the exact documents gh / github-mcp-server send.
// A small recursive-descent parser for the query language subset in use, plus
// resolvers over the store. Unknown fields become GraphQL errors naming the field.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- global ids

func gid(kind, key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + key))
}

func ungid(id string) (kind, key string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	text := string(raw)
	i := strings.IndexByte(text, ':')
	if i <= 0 {
		return "", "", false
	}
	return text[:i], text[i+1:], true
}

func repoGid(r *Repo) string { return gid("R", r.Owner+"/"+r.Name) }
func issueGid(r *Repo, is *Issue) string {
	return gid("I", fmt.Sprintf("%s/%s#%d", r.Owner, r.Name, is.Number))
}
func pullGid(r *Repo, p *Pull) string {
	return gid("P", fmt.Sprintf("%s/%s#%d", r.Owner, r.Name, p.Number))
}
func commentGid(r *Repo, issueNumber, commentID int) string {
	return gid("C", fmt.Sprintf("%s/%s#%d:%d", r.Owner, r.Name, issueNumber, commentID))
}

func repoFromGid(id string) *Repo {
	kind, key, ok := ungid(id)
	if !ok || kind != "R" {
		return nil
	}
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	return GetRepo(parts[0], parts[1])
}

func issueFromGid(id string) (*Repo, *Issue) {
	kind, key, ok := ungid(id)
	if !ok || kind != "I" {
		return nil, nil
	}
	hash := strings.IndexByte(key, '#')
	if hash < 0 {
		return nil, nil
	}
	parts := strings.SplitN(key[:hash], "/", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	repo := GetRepo(parts[0], parts[1])
	if repo == nil {
		return nil, nil
	}
	num, err := strconv.Atoi(key[hash+1:])
	if err != nil {
		return nil, nil
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	issue := repo.IssuesByNum[num]
	if issue == nil {
		return nil, nil
	}
	return repo, issue
}

func pullFromGid(id string) (*Repo, *Pull) {
	kind, key, ok := ungid(id)
	if !ok || kind != "P" {
		return nil, nil
	}
	hash := strings.IndexByte(key, '#')
	if hash < 0 {
		return nil, nil
	}
	parts := strings.SplitN(key[:hash], "/", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	repo := GetRepo(parts[0], parts[1])
	if repo == nil {
		return nil, nil
	}
	num, err := strconv.Atoi(key[hash+1:])
	if err != nil {
		return nil, nil
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	pull := repo.PullsByNum[num]
	if pull == nil {
		return nil, nil
	}
	return repo, pull
}

// ---------------------------------------------------------------- parser

type VarRef struct{ Name string }

type Value = any

type FieldNode struct {
	Alias        string
	Name         string
	Args         map[string]Value
	Children     []*FieldNode
	FragmentType string
	IsFragment   bool
}

type VarDef struct {
	Type    string
	Default Value
	HasDef  bool
}

type Operation struct {
	Kind       string // "query" | "mutation"
	Name       string
	Variables  map[string]VarDef
	Selections []*FieldNode
}

func tokenize(src string) []string {
	var tokens []string
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(src[i:], "...") {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if strings.IndexByte("{}():[]!=$@", c) >= 0 {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					out.WriteByte(src[j+1])
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, strconv.Quote(out.String()))
			i = j + 1
			continue
		}
		if isIdentChar(c) {
			j := i
			for j < len(src) && isIdentChar(src[j]) {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		i++
	}
	return tokens
}

func isIdentChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

type parser struct {
	pos       int
	tokens    []string
	fragments map[string][]*FieldNode
}

func (p *parser) peek() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	return p.tokens[p.pos]
}

func (p *parser) hasMore() bool { return p.pos < len(p.tokens) }

func (p *parser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) expect(t string) error {
	got := p.next()
	if got != t {
		return fmt.Errorf("expected %s, got %s", t, got)
	}
	return nil
}

var numRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

func (p *parser) parseValue() (Value, error) {
	t := p.next()
	switch {
	case t == "$":
		return VarRef{Name: p.next()}, nil
	case t == "[":
		var arr []Value
		for p.peek() != "]" {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		return arr, nil
	case t == "{":
		obj := map[string]Value{}
		for p.peek() != "}" {
			k := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
		if err := p.expect("}"); err != nil {
			return nil, err
		}
		return obj, nil
	case t == "true":
		return true, nil
	case t == "false":
		return false, nil
	case t == "null":
		return nil, nil
	default:
		if strings.HasPrefix(t, "\"") {
			var s string
			if err := unquote(t, &s); err == nil {
				return s, nil
			}
			return t, nil
		}
		if numRe.MatchString(t) {
			if n, err := strconv.ParseFloat(t, 64); err == nil {
				return n, nil
			}
		}
		return t, nil
	}
}

func unquote(q string, out *string) error {
	s, err := strconv.Unquote(q)
	if err != nil {
		return err
	}
	*out = s
	return nil
}

func (p *parser) parseSelections() ([]*FieldNode, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []*FieldNode
	for p.peek() != "}" {
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseField() (*FieldNode, error) {
	if p.peek() == "..." {
		p.next()
		if p.peek() == "on" {
			p.next()
			typeName := p.next()
			children, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			return &FieldNode{Children: children, FragmentType: typeName, IsFragment: true}, nil
		}
		fragName := p.next()
		children := p.fragments[fragName]
		return &FieldNode{Children: children, IsFragment: true}, nil
	}
	name := p.next()
	alias := name
	if p.peek() == ":" {
		p.next()
		alias = name
		name = p.next()
	}
	args := map[string]Value{}
	if p.peek() == "(" {
		p.next()
		for p.peek() != ")" {
			k := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[k] = v
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	for p.peek() == "@" {
		p.next()
		p.next()
		if p.peek() == "(" {
			depth := 0
			for {
				t := p.next()
				if t == "(" {
					depth++
				}
				if t == ")" {
					depth--
				}
				if depth <= 0 {
					break
				}
			}
		}
	}
	var children []*FieldNode
	if p.peek() == "{" {
		var err error
		children, err = p.parseSelections()
		if err != nil {
			return nil, err
		}
	}
	return &FieldNode{Alias: alias, Name: name, Args: args, Children: children}, nil
}

func (p *parser) parseOperation() (*Operation, error) {
	kind := "query"
	var name string
	t := p.peek()
	if t == "query" || t == "mutation" {
		kind = p.next()
		if p.hasMore() && p.peek() != "{" && p.peek() != "(" {
			name = p.next()
		}
	}
	variables := map[string]VarDef{}
	if p.peek() == "(" {
		p.next()
		for p.peek() != ")" {
			if err := p.expect("$"); err != nil {
				return nil, err
			}
			vname := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			var parts []string
			for p.peek() != ")" && p.peek() != "=" && p.peek() != "$" {
				parts = append(parts, p.next())
			}
			def := VarDef{Type: strings.Join(parts, "")}
			if p.peek() == "=" {
				p.next()
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				def.Default = v
				def.HasDef = true
			}
			variables[vname] = def
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	selections, err := p.parseSelections()
	if err != nil {
		return nil, err
	}
	return &Operation{Kind: kind, Name: name, Variables: variables, Selections: selections}, nil
}

func parseDocument(src string) ([]*Operation, map[string][]*FieldNode, error) {
	tokens := tokenize(src)
	fragments := map[string][]*FieldNode{}
	p := &parser{tokens: tokens, fragments: fragments}
	var operations []*Operation
	for p.hasMore() {
		if p.peek() == "fragment" {
			p.next()
			name := p.next()
			if err := p.expect("on"); err != nil {
				return nil, nil, err
			}
			p.next() // type name
			sels, err := p.parseSelections()
			if err != nil {
				return nil, nil, err
			}
			fragments[name] = sels
		} else {
			op, err := p.parseOperation()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, op)
		}
	}
	return operations, fragments, nil
}

// ---------------------------------------------------------------- schema (introspection)

type TypeInfo struct {
	Fields     []string
	EnumValues []string
	Kind       string // "OBJECT" | "ENUM" | "SCALAR"
}

var schema = map[string]*TypeInfo{
	"Query":                            {Kind: "OBJECT", Fields: []string{"viewer", "repository", "node", "__type", "__schema"}},
	"Mutation":                         {Kind: "OBJECT", Fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}},
	"Repository":                       {Kind: "OBJECT", Fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "createdAt", "url", "isPrivate"}},
	"Issue":                            {Kind: "OBJECT", Fields: []string{"id", "number", "title", "body", "state", "author", "createdAt", "updatedAt", "url", "comments", "labels", "repository"}},
	"PullRequest":                      {Kind: "OBJECT", Fields: []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "files", "createdAt", "mergedAt", "mergeCommitSha", "author", "url"}},
	"User":                             {Kind: "OBJECT", Fields: []string{"id", "login", "name", "email", "url"}},
	"Viewer":                           {Kind: "OBJECT", Fields: []string{"id", "login", "name", "email", "url"}},
	"Ref":                              {Kind: "OBJECT", Fields: []string{"id", "name", "target"}},
	"IssueConnection":                  {Kind: "OBJECT", Fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"PullRequestConnection":            {Kind: "OBJECT", Fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"PullRequestChangedFileConnection": {Kind: "OBJECT", Fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"PullRequestChangedFile":           {Kind: "OBJECT", Fields: []string{"path", "additions", "deletions", "changeType"}},
	"PageInfo":                         {Kind: "OBJECT", Fields: []string{"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"}},
	"IssueComment":                     {Kind: "OBJECT", Fields: []string{"id", "body", "author", "createdAt"}},
	"IssueCommentEdge":                 {Kind: "OBJECT", Fields: []string{"node", "cursor"}},
	"CreateIssuePayload":               {Kind: "OBJECT", Fields: []string{"issue", "clientMutationId"}},
	"CloseIssuePayload":                {Kind: "OBJECT", Fields: []string{"issue", "clientMutationId"}},
	"CreatePullRequestPayload":         {Kind: "OBJECT", Fields: []string{"pullRequest", "clientMutationId"}},
	"MergePullRequestPayload":          {Kind: "OBJECT", Fields: []string{"pullRequest", "clientMutationId"}},
	"AddCommentPayload":                {Kind: "OBJECT", Fields: []string{"commentEdge", "clientMutationId"}},
	"IssueState":                       {Kind: "ENUM", EnumValues: []string{"OPEN", "CLOSED"}},
	"PullRequestState":                 {Kind: "ENUM", EnumValues: []string{"OPEN", "CLOSED", "MERGED"}},
	"MergeableState":                   {Kind: "ENUM", EnumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"PullRequestMergeMethod":           {Kind: "ENUM", EnumValues: []string{"MERGE", "SQUASH", "REBASE"}},
	"RepositoryPermission":             {Kind: "ENUM", EnumValues: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	"SearchType":                       {Kind: "ENUM", EnumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE", "WIKI"}},
	"IssueOrderField":                  {Kind: "ENUM", EnumValues: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	"OrderDirection":                   {Kind: "ENUM", EnumValues: []string{"ASC", "DESC"}},
	"__Type":                           {Kind: "OBJECT", Fields: []string{"name", "kind", "fields", "enumValues", "description"}},
	"__Field":                          {Kind: "OBJECT", Fields: []string{"name", "description", "args", "type"}},
	"__EnumValue":                      {Kind: "OBJECT", Fields: []string{"name", "description"}},
	"__Schema":                         {Kind: "OBJECT", Fields: []string{"types", "queryType", "mutationType", "description"}},
	"__InputValue":                     {Kind: "OBJECT", Fields: []string{"name", "description", "type"}},
}

// ---------------------------------------------------------------- execution

type gqlCtx struct {
	typ   string
	value map[string]any
	repo  *Repo
}

type execState struct {
	errors    []map[string]any
	variables map[string]any
}

func resolveVars(value Value, variables map[string]any) any {
	if ref, ok := value.(VarRef); ok {
		return variables[ref.Name]
	}
	if arr, ok := value.([]Value); ok {
		out := make([]any, len(arr))
		for i, v := range arr {
			out[i] = resolveVars(v, variables)
		}
		return out
	}
	if m, ok := value.(map[string]Value); ok {
		out := map[string]any{}
		for k, v := range m {
			out[k] = resolveVars(v, variables)
		}
		return out
	}
	return value
}

func argValue(field *FieldNode, name string, state *execState) any {
	raw, ok := field.Args[name]
	if !ok {
		return nil
	}
	return resolveVars(raw, state.variables)
}

func cursorFor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func numberFromCursor(c string) (int, bool) {
	if c == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func issueNode(repo *Repo, issue *Issue) map[string]any {
	return map[string]any{"__type": "Issue", "__repo": repo, "__issue": issue}
}

func pullNode(repo *Repo, pull *Pull) map[string]any {
	return map[string]any{"__type": "PullRequest", "__repo": repo, "__pull": pull}
}

var fieldTypes = map[string]map[string]string{
	"Query":                            {"viewer": "User", "repository": "Repository", "node": "Node", "__type": "__Type", "__schema": "__Schema"},
	"Mutation":                         {"createIssue": "CreateIssuePayload", "closeIssue": "CloseIssuePayload", "createPullRequest": "CreatePullRequestPayload", "mergePullRequest": "MergePullRequestPayload", "addComment": "AddCommentPayload"},
	"Repository":                       {"owner": "User", "defaultBranchRef": "Ref", "issue": "Issue", "issues": "IssueConnection", "pullRequests": "PullRequestConnection", "pullRequest": "PullRequest"},
	"Issue":                            {"author": "User", "comments": "IssueConnection"},
	"PullRequest":                      {"files": "PullRequestChangedFileConnection", "headRef": "Ref", "baseRef": "Ref", "author": "User"},
	"IssueConnection":                  {"nodes": "Issue", "pageInfo": "PageInfo"},
	"PullRequestConnection":            {"nodes": "PullRequest", "pageInfo": "PageInfo"},
	"PullRequestChangedFileConnection": {"nodes": "PullRequestChangedFile", "pageInfo": "PageInfo"},
	"CreateIssuePayload":               {"issue": "Issue"},
	"CloseIssuePayload":                {"issue": "Issue"},
	"CreatePullRequestPayload":         {"pullRequest": "PullRequest"},
	"MergePullRequestPayload":          {"pullRequest": "PullRequest"},
	"AddCommentPayload":                {"commentEdge": "IssueCommentEdge"},
	"IssueCommentEdge":                 {"node": "IssueComment"},
	"__Type":                           {"fields": "__Field", "enumValues": "__EnumValue"},
	"__Schema":                         {"types": "__Type", "queryType": "__Type", "mutationType": "__Type"},
}

func fieldTypeName(typ, fieldName string) string {
	if m, ok := fieldTypes[typ]; ok {
		if t, ok := m[fieldName]; ok {
			return t
		}
	}
	return "Scalar"
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func executeSelections(ctx gqlCtx, selections []*FieldNode, state *execState) map[string]any {
	out := map[string]any{}
	for _, field := range selections {
		if field.IsFragment {
			if field.FragmentType != "" && !matchesType(ctx, field.FragmentType) {
				continue
			}
			for k, v := range executeSelections(ctx, field.Children, state) {
				out[k] = v
			}
			continue
		}
		if field.Name == "__typename" {
			if ctx.typ == "Node" {
				t, _ := ctx.value["__type"].(string)
				if t == "" {
					t = "Repository"
				}
				out[field.Alias] = t
			} else {
				out[field.Alias] = ctx.typ
			}
			continue
		}
		effectiveType := ctx.typ
		if ctx.typ == "Node" {
			if t, ok := ctx.value["__type"].(string); ok && t != "" {
				effectiveType = t
			} else {
				effectiveType = "Repository"
			}
		}
		allowed := []string{}
		if info := schema[ctx.typ]; info != nil {
			allowed = info.Fields
		}
		allowedEff := []string{}
		if info := schema[effectiveType]; info != nil {
			allowedEff = info.Fields
		}
		if !containsStr(allowed, field.Name) && !containsStr(allowedEff, field.Name) && field.Name != "__type" && field.Name != "__schema" {
			state.errors = append(state.errors, map[string]any{"message": fmt.Sprintf("Cannot query field %q on type %q.", field.Name, effectiveType)})
			out[field.Alias] = nil
			continue
		}
		value := resolveField(gqlCtx{typ: effectiveType, value: ctx.value, repo: ctx.repo}, field, state)
		if len(field.Children) > 0 && value != nil {
			childType := fieldTypeName(effectiveType, field.Name)
			switch v := value.(type) {
			case []any:
				arr := make([]any, len(v))
				for i, item := range v {
					if m, ok := item.(map[string]any); ok {
						arr[i] = executeSelections(gqlCtx{typ: childType, value: m, repo: ctx.repo}, field.Children, state)
					} else {
						arr[i] = item
					}
				}
				out[field.Alias] = arr
			case map[string]any:
				out[field.Alias] = executeSelections(gqlCtx{typ: childType, value: v, repo: ctx.repo}, field.Children, state)
			default:
				out[field.Alias] = value
			}
		} else {
			if value == nil {
				out[field.Alias] = nil
			} else {
				out[field.Alias] = value
			}
		}
	}
	return out
}

func matchesType(ctx gqlCtx, typeName string) bool {
	if ctx.typ == "Node" {
		t, _ := ctx.value["__type"].(string)
		return t == typeName
	}
	return ctx.typ == typeName
}

func toInt(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func resolveField(ctx gqlCtx, field *FieldNode, state *execState) any {
	v := ctx.value
	switch ctx.typ {
	case "Query":
		switch field.Name {
		case "viewer":
			return map[string]any{"__type": "User", "login": Viewer.Login, "id": strconv.Itoa(Viewer.ID), "name": Viewer.Name, "email": Viewer.Email}
		case "repository":
			owner := toStr(argValue(field, "owner", state))
			name := toStr(argValue(field, "name", state))
			repo := GetRepo(owner, name)
			if repo == nil {
				return nil
			}
			return map[string]any{"__type": "Repository", "__repo": repo}
		case "node":
			id := toStr(argValue(field, "id", state))
			if repo := repoFromGid(id); repo != nil {
				return map[string]any{"__type": "Repository", "__repo": repo}
			}
			if repo, issue := issueFromGid(id); repo != nil && issue != nil {
				return issueNode(repo, issue)
			}
			if repo, pull := pullFromGid(id); repo != nil && pull != nil {
				return pullNode(repo, pull)
			}
			return nil
		case "__type":
			name := toStr(argValue(field, "name", state))
			info := schema[name]
			if info == nil {
				return nil
			}
			return map[string]any{"__type": "__Type", "__typeName": name, "__info": info}
		case "__schema":
			return map[string]any{"__type": "__Schema"}
		}
		return nil
	case "User", "Viewer":
		switch field.Name {
		case "login":
			if s, ok := v["login"].(string); ok {
				return s
			}
			return Viewer.Login
		case "id":
			if s, ok := v["id"].(string); ok {
				return s
			}
			return strconv.Itoa(Viewer.ID)
		case "name":
			if s, ok := v["name"].(string); ok {
				return s
			}
			return Viewer.Name
		case "email":
			if s, ok := v["email"].(string); ok {
				return s
			}
			return Viewer.Email
		case "url":
			login, _ := v["login"].(string)
			if login == "" {
				login = Viewer.Login
			}
			return "https://example.invalid/" + login
		}
		return nil
	case "Repository":
		repo := v["__repo"].(*Repo)
		switch field.Name {
		case "id":
			return repoGid(repo)
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name
		case "owner":
			return map[string]any{"__type": "User", "login": repo.Owner, "id": strconv.Itoa(Viewer.ID), "name": Viewer.Name, "email": Viewer.Email}
		case "defaultBranchRef":
			return map[string]any{"__type": "Ref", "name": repo.DefaultBranch}
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "description":
			return repo.Description
		case "private", "isPrivate":
			return repo.Private
		case "createdAt":
			return isoTs(1700000000)
		case "url":
			return "https://example.invalid/" + repo.Owner + "/" + repo.Name
		case "issue":
			number := toInt(argValue(field, "number", state))
			repo.mu.RLock()
			issue := repo.IssuesByNum[number]
			repo.mu.RUnlock()
			if issue == nil {
				return nil
			}
			return issueNode(repo, issue)
		case "issues":
			first := 100
			if fv := argValue(field, "first", state); fv != nil {
				first = toInt(fv)
			}
			after, _ := argValue(field, "after", state).(string)
			var states []any
			if sv := argValue(field, "states", state); sv != nil {
				if arr, ok := sv.([]any); ok {
					states = arr
				}
			}
			var orderBy map[string]any
			if ov := argValue(field, "orderBy", state); ov != nil {
				if m, ok := ov.(map[string]any); ok {
					orderBy = m
				}
			}
			repo.mu.RLock()
			list := make([]*Issue, len(repo.Issues))
			copy(list, repo.Issues)
			repo.mu.RUnlock()
			if states != nil {
				wanted := map[string]bool{}
				for _, s := range states {
					wanted[strings.ToLower(toStr(s))] = true
				}
				var filtered []*Issue
				for _, is := range list {
					if wanted[is.State] {
						filtered = append(filtered, is)
					}
				}
				list = filtered
			}
			dir := -1
			if orderBy != nil {
				if d, _ := orderBy["direction"].(string); d == "ASC" {
					dir = 1
				}
			}
			// Sort by number (created order), ties by number; direction applies.
			for i := 0; i < len(list); i++ {
				for j := i + 1; j < len(list); j++ {
					if dir*(list[i].Number-list[j].Number) > 0 {
						list[i], list[j] = list[j], list[i]
					}
				}
			}
			if after != "" {
				if afterNum, ok := numberFromCursor(after); ok {
					var filtered []*Issue
					for _, is := range list {
						if dir > 0 && is.Number > afterNum || dir < 0 && is.Number < afterNum {
							filtered = append(filtered, is)
						}
					}
					list = filtered
				}
			}
			page := list
			if len(page) > first {
				page = page[:first]
			}
			hasNext := len(list) > first
			var endCursor any
			if len(page) > 0 {
				endCursor = cursorFor(page[len(page)-1].Number)
			}
			nodes := make([]any, len(page))
			for i, is := range page {
				nodes[i] = issueNode(repo, is)
			}
			return map[string]any{"__type": "IssueConnection", "__nodes": nodes, "__hasNext": hasNext, "__endCursor": endCursor}
		case "pullRequests":
			first := 100
			if fv := argValue(field, "first", state); fv != nil {
				first = toInt(fv)
			}
			after, _ := argValue(field, "after", state).(string)
			var states []any
			if sv := argValue(field, "states", state); sv != nil {
				if arr, ok := sv.([]any); ok {
					states = arr
				}
			}
			repo.mu.RLock()
			list := make([]*Pull, len(repo.Pulls))
			copy(list, repo.Pulls)
			repo.mu.RUnlock()
			if states != nil {
				wanted := map[string]bool{}
				for _, s := range states {
					wanted[toStr(s)] = true
				}
				var filtered []*Pull
				for _, p := range list {
					st := p.State
					if p.State == "closed" && p.Merged {
						st = "MERGED"
					} else {
						st = strings.ToUpper(p.State)
					}
					if wanted[st] {
						filtered = append(filtered, p)
					}
				}
				list = filtered
			} else {
				var filtered []*Pull
				for _, p := range list {
					if p.State == "open" {
						filtered = append(filtered, p)
					}
				}
				list = filtered
			}
			if after != "" {
				if afterNum, ok := numberFromCursor(after); ok {
					var filtered []*Pull
					for _, p := range list {
						if p.Number > afterNum {
							filtered = append(filtered, p)
						}
					}
					list = filtered
				}
			}
			page := list
			if len(page) > first {
				page = page[:first]
			}
			var endCursor any
			if len(page) > 0 {
				endCursor = cursorFor(page[len(page)-1].Number)
			}
			nodes := make([]any, len(page))
			for i, p := range page {
				nodes[i] = pullNode(repo, p)
			}
			return map[string]any{"__type": "PullRequestConnection", "__nodes": nodes, "__hasNext": len(list) > first, "__endCursor": endCursor}
		case "pullRequest":
			number := toInt(argValue(field, "number", state))
			repo.mu.RLock()
			pull := repo.PullsByNum[number]
			repo.mu.RUnlock()
			if pull == nil {
				return nil
			}
			return pullNode(repo, pull)
		}
		return nil
	case "Issue":
		repo := v["__repo"].(*Repo)
		issue := v["__issue"].(*Issue)
		switch field.Name {
		case "id":
			return issueGid(repo, issue)
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "state":
			return strings.ToUpper(issue.State)
		case "author":
			return map[string]any{"__type": "User", "login": Viewer.Login, "id": strconv.Itoa(Viewer.ID), "name": Viewer.Name, "email": Viewer.Email}
		case "createdAt":
			return isoTs(issue.CreatedTs)
		case "updatedAt":
			return isoTs(issue.UpdatedTs)
		case "url":
			return "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number)
		case "repository":
			return map[string]any{"__type": "Repository", "__repo": repo}
		case "labels":
			repo.mu.RLock()
			defer repo.mu.RUnlock()
			out := make([]any, len(issue.Labels))
			for i, l := range issue.Labels {
				out[i] = map[string]any{"name": l.Name, "color": l.Color}
			}
			return out
		case "comments":
			repo.mu.RLock()
			defer repo.mu.RUnlock()
			nodes := make([]any, len(issue.Comments))
			for i, c := range issue.Comments {
				nodes[i] = map[string]any{"__type": "IssueComment", "id": commentGid(repo, issue.Number, c.ID), "body": c.Body}
			}
			return map[string]any{"__type": "IssueConnection", "__nodes": nodes, "__hasNext": false, "__endCursor": nil}
		}
		return nil
	case "PullRequest":
		repo := v["__repo"].(*Repo)
		pull := v["__pull"].(*Pull)
		switch field.Name {
		case "id":
			return pullGid(repo, pull)
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.Body
		case "state":
			if pull.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pull.State)
		case "merged":
			return pull.Merged
		case "mergeable":
			repo.mu.RLock()
			conflict := PullConflicts(repo, pull)
			repo.mu.RUnlock()
			if conflict {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		case "headRefName":
			return pull.HeadRef
		case "baseRefName":
			return pull.BaseRef
		case "headRef":
			return map[string]any{"__type": "Ref", "name": pull.HeadRef}
		case "baseRef":
			return map[string]any{"__type": "Ref", "name": pull.BaseRef}
		case "createdAt":
			return isoTs(pull.CreatedTs)
		case "mergedAt":
			if pull.MergedAt == "" {
				return nil
			}
			return pull.MergedAt
		case "mergeCommitSha":
			if pull.MergeCommitSha == "" {
				return nil
			}
			return pull.MergeCommitSha
		case "author":
			return map[string]any{"__type": "User", "login": Viewer.Login, "id": strconv.Itoa(Viewer.ID), "name": Viewer.Name, "email": Viewer.Email}
		case "url":
			return "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number)
		case "files":
			first := 100
			if fv := argValue(field, "first", state); fv != nil {
				first = toInt(fv)
			}
			repo.mu.RLock()
			files := DiffFiles(repo, pull.BaseSha, pull.HeadSha)
			repo.mu.RUnlock()
			if len(files) > first {
				files = files[:first]
			}
			nodes := make([]any, len(files))
			for i, f := range files {
				nodes[i] = map[string]any{"__type": "PullRequestChangedFile", "path": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changeType": f.Status}
			}
			return map[string]any{"__type": "PullRequestChangedFileConnection", "__nodes": nodes, "__hasNext": false, "__endCursor": nil}
		}
		return nil
	case "Ref":
		switch field.Name {
		case "name":
			if s, ok := v["name"].(string); ok {
				return s
			}
			return ""
		case "id":
			s, _ := v["name"].(string)
			return gid("Ref", s)
		case "target":
			return nil
		}
		return nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		switch field.Name {
		case "nodes":
			if n, ok := v["__nodes"].([]any); ok {
				return n
			}
			return []any{}
		case "edges":
			nodes, _ := v["__nodes"].([]any)
			edges := make([]any, len(nodes))
			for i, n := range nodes {
				edges[i] = map[string]any{"node": n, "cursor": ""}
			}
			return edges
		case "pageInfo":
			return map[string]any{"__type": "PageInfo", "__hasNext": v["__hasNext"], "__endCursor": v["__endCursor"]}
		case "totalCount":
			if n, ok := v["__nodes"].([]any); ok {
				return len(n)
			}
			return 0
		}
		return nil
	case "PageInfo":
		switch field.Name {
		case "hasNextPage":
			b, _ := v["__hasNext"].(bool)
			return b
		case "hasPreviousPage":
			return false
		case "endCursor":
			return v["__endCursor"]
		case "startCursor":
			return nil
		}
		return nil
	case "PullRequestChangedFile":
		switch field.Name {
		case "path":
			return v["path"]
		case "additions":
			return v["additions"]
		case "deletions":
			return v["deletions"]
		case "changeType":
			return v["changeType"]
		}
		return nil
	case "IssueComment":
		switch field.Name {
		case "id":
			return v["id"]
		case "body":
			return v["body"]
		case "author":
			return map[string]any{"__type": "User", "login": Viewer.Login, "id": strconv.Itoa(Viewer.ID), "name": Viewer.Name, "email": Viewer.Email}
		case "createdAt":
			return isoTs(1700000000)
		}
		return nil
	case "IssueCommentEdge":
		switch field.Name {
		case "node":
			return v["node"]
		case "cursor":
			return ""
		}
		return nil
	case "__Type":
		info := v["__info"].(*TypeInfo)
		name, _ := v["__typeName"].(string)
		switch field.Name {
		case "name":
			return name
		case "kind":
			return info.Kind
		case "description":
			return nil
		case "fields":
			if info.Fields == nil {
				return nil
			}
			out := make([]any, len(info.Fields))
			for i, f := range info.Fields {
				out[i] = map[string]any{"__type": "__Field", "name": f}
			}
			return out
		case "enumValues":
			if info.EnumValues == nil {
				return nil
			}
			out := make([]any, len(info.EnumValues))
			for i, e := range info.EnumValues {
				out[i] = map[string]any{"__type": "__EnumValue", "name": e}
			}
			return out
		}
		return nil
	case "__Field":
		switch field.Name {
		case "name":
			return v["name"]
		case "description":
			return nil
		case "args":
			return []any{}
		case "type":
			return map[string]any{"__type": "__Type", "__typeName": "String", "__info": &TypeInfo{Kind: "SCALAR"}}
		}
		return nil
	case "__EnumValue":
		switch field.Name {
		case "name":
			return v["name"]
		case "description":
			return nil
		}
		return nil
	case "__Schema":
		switch field.Name {
		case "types":
			names := make([]string, 0, len(schema))
			for n := range schema {
				names = append(names, n)
			}
			// Deterministic order for stable output.
			for i := 0; i < len(names); i++ {
				for j := i + 1; j < len(names); j++ {
					if names[j] < names[i] {
						names[i], names[j] = names[j], names[i]
					}
				}
			}
			out := make([]any, len(names))
			for i, n := range names {
				out[i] = map[string]any{"__type": "__Type", "__typeName": n, "__info": schema[n]}
			}
			return out
		case "queryType":
			return map[string]any{"__type": "__Type", "__typeName": "Query", "__info": schema["Query"]}
		case "mutationType":
			return map[string]any{"__type": "__Type", "__typeName": "Mutation", "__info": schema["Mutation"]}
		case "description":
			return nil
		}
		return nil
	case "Mutation":
		return executeMutation(field, state)
	case "CreateIssuePayload", "CloseIssuePayload":
		if field.Name == "issue" {
			return v["issue"]
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field.Name == "pullRequest" {
			return v["pullRequest"]
		}
		return nil
	case "AddCommentPayload":
		if field.Name == "commentEdge" {
			return v["commentEdge"]
		}
		return nil
	}
	return nil
}

func executeMutation(field *FieldNode, state *execState) any {
	input, _ := argValue(field, "input", state).(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch field.Name {
	case "createIssue":
		repo := repoFromGid(toStr(input["repositoryId"]))
		if repo == nil {
			return map[string]any{"issue": nil}
		}
		repo.mu.Lock()
		created, rerr := CreateIssue(repo, toStr(input["title"]), toStr(input["body"]), nil)
		repo.mu.Unlock()
		if rerr != nil {
			state.errors = append(state.errors, map[string]any{"message": rerr.Msg})
			return map[string]any{"issue": nil}
		}
		return map[string]any{"issue": issueNode(repo, created)}
	case "closeIssue":
		repo, issue := issueFromGid(toStr(input["issueId"]))
		if repo == nil || issue == nil {
			return map[string]any{"issue": nil}
		}
		repo.mu.Lock()
		issue.State = "closed"
		issue.touch()
		repo.mu.Unlock()
		return map[string]any{"issue": issueNode(repo, issue)}
	case "createPullRequest":
		repo := repoFromGid(toStr(input["repositoryId"]))
		if repo == nil {
			return map[string]any{"pullRequest": nil}
		}
		base := toStr(input["baseRefName"])
		if base == "" {
			base = repo.DefaultBranch
		}
		repo.mu.Lock()
		created, rerr := CreatePull(repo, toStr(input["title"]), toStr(input["body"]), toStr(input["headRefName"]), base)
		repo.mu.Unlock()
		if rerr != nil {
			state.errors = append(state.errors, map[string]any{"message": rerr.Msg})
			return map[string]any{"pullRequest": nil}
		}
		return map[string]any{"pullRequest": pullNode(repo, created)}
	case "mergePullRequest":
		repo, pull := pullFromGid(toStr(input["pullRequestId"]))
		if repo == nil || pull == nil {
			return map[string]any{"pullRequest": nil}
		}
		repo.mu.Lock()
		_, merr := MergePull(repo, pull)
		repo.mu.Unlock()
		if merr != nil {
			state.errors = append(state.errors, map[string]any{"message": merr.Msg})
			return map[string]any{"pullRequest": pullNode(repo, pull)}
		}
		return map[string]any{"pullRequest": pullNode(repo, pull)}
	case "addComment":
		repo, issue := issueFromGid(toStr(input["subjectId"]))
		if repo == nil || issue == nil {
			return map[string]any{"commentEdge": nil}
		}
		repo.mu.Lock()
		comment := AddComment(repo, issue.Number, toStr(input["body"]))
		repo.mu.Unlock()
		if comment == nil {
			return map[string]any{"commentEdge": nil}
		}
		return map[string]any{"commentEdge": map[string]any{"node": map[string]any{"__type": "IssueComment", "id": commentGid(repo, issue.Number, comment.ID), "body": comment.Body}}}
	}
	return nil
}

// ---------------------------------------------------------------- entry points

type GraphQLOp struct {
	Query         string
	Variables     map[string]any
	OperationName string
}

type GraphQLResult struct {
	Data   map[string]any   `json:"data"`
	Errors []map[string]any `json:"errors"`
}

func ExecuteGraphQL(query string, variables map[string]any, operationName string) GraphQLResult {
	operations, _, err := parseDocument(query)
	if err != nil {
		return GraphQLResult{Data: nil, Errors: []map[string]any{{"message": err.Error()}}}
	}
	if len(operations) == 0 {
		return GraphQLResult{Data: nil, Errors: []map[string]any{{"message": "No operation found in document."}}}
	}
	op := operations[0]
	if operationName != "" {
		for _, o := range operations {
			if o.Name == operationName {
				op = o
				break
			}
		}
	}
	state := &execState{errors: []map[string]any{}, variables: map[string]any{}}
	for k, v := range variables {
		state.variables[k] = v
	}
	for name, def := range op.Variables {
		if _, ok := state.variables[name]; !ok && def.HasDef {
			state.variables[name] = resolveVars(def.Default, variables)
		}
	}
	rootType := "Query"
	if op.Kind == "mutation" {
		rootType = "Mutation"
	}
	data := executeSelections(gqlCtx{typ: rootType, value: map[string]any{}}, op.Selections, state)
	if state.errors == nil {
		state.errors = []map[string]any{}
	}
	return GraphQLResult{Data: data, Errors: state.errors}
}

func ExecuteGraphQLBatch(ops []GraphQLOp) []GraphQLResult {
	results := make([]GraphQLResult, len(ops))
	for i, o := range ops {
		results[i] = ExecuteGraphQL(o.Query, o.Variables, o.OperationName)
	}
	return results
}
