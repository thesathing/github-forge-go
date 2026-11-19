// Git smart HTTP: info/refs (v0 + v2), upload-pack (v2 + v0), receive-pack (v0).
// Handlers take the raw request body and return {status, contentType, body}.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	uploadAd    = "application/x-git-upload-pack-advertisement"
	receiveAd   = "application/x-git-receive-pack-advertisement"
	uploadRes   = "application/x-git-upload-pack-result"
	receiveRes  = "application/x-git-receive-pack-result"
	jsonTypeGit = "application/json; charset=utf-8"
)

type GitResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

func gitNotFound() GitResponse {
	return GitResponse{Status: 404, ContentType: jsonTypeGit, Body: []byte(`{"message":"Not Found"}` + "\n")}
}

func gitBadRequest(msg string) GitResponse {
	return GitResponse{Status: 400, ContentType: jsonTypeGit, Body: []byte(`{"message":` + jsonString(msg) + `}` + "\n")}
}

func sortedRefs(repo *Repo) []struct {
	Name string
	OID  string
} {
	var refs []struct {
		Name string
		OID  string
	}
	for name, oid := range repo.Refs {
		refs = append(refs, struct {
			Name string
			OID  string
		}{name, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

func headOID(repo *Repo) (string, bool) {
	oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
	return oid, ok
}

func peeledOID(repo *Repo, oid string) (string, bool) {
	obj, ok := repo.Objects[oid]
	if !ok || obj.Type != ObjTag {
		return "", false
	}
	target := ParseTag(obj.Data).Object
	for guard := 0; guard < 10; guard++ {
		t, ok := repo.Objects[target]
		if !ok || t.Type != ObjTag {
			break
		}
		target = ParseTag(t.Data).Object
	}
	return target, true
}

// ---------------------------------------------------------------- info/refs

func v0Capabilities(repo *Repo, service string) string {
	symref := "symref=HEAD:refs/heads/" + repo.DefaultBranch
	if service == "git-upload-pack" {
		return "side-band-64k thin-pack ofs-delta shallow multi_ack " + symref + " agent=forge/1.0"
	}
	return "report-status side-band-64k delete-refs ofs-delta " + symref + " agent=forge/1.0"
}

func v0Advertisement(repo *Repo, service string) []byte {
	caps := v0Capabilities(repo, service)
	var parts [][]byte
	parts = append(parts, PktLineStr("# service="+service+"\n"), pktFlush)
	refs := sortedRefs(repo)
	head, hasHead := headOID(repo)
	if len(refs) == 0 {
		parts = append(parts, PktLineStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		var headLine string
		if hasHead {
			headLine = head + " HEAD\x00" + caps + "\n"
		} else {
			headLine = ZeroOID + " HEAD\x00" + caps + "\n"
		}
		parts = append(parts, PktLineStr(headLine))
		for _, ref := range refs {
			parts = append(parts, PktLineStr(ref.OID+" "+ref.Name+"\n"))
			if peeled, ok := peeledOID(repo, ref.OID); ok {
				parts = append(parts, PktLineStr(peeled+" "+ref.Name+"^{}\n"))
			}
		}
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func v2Advertisement() []byte {
	lines := []string{
		"version 2\n",
		"agent=forge/1.0\n",
		"ls-refs=unborn\n",
		"fetch=shallow filter\n",
		"object-format=sha1\n",
		"server-option\n",
	}
	var parts [][]byte
	for _, l := range lines {
		parts = append(parts, PktLineStr(l))
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func InfoRefs(owner, repoName, service, gitProtocol string) GitResponse {
	repo := GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	if service == "git-receive-pack" {
		return GitResponse{Status: 200, ContentType: receiveAd, Body: v0Advertisement(repo, "git-receive-pack")}
	}
	if service != "git-upload-pack" {
		return gitNotFound()
	}
	if gitProtocol == "version=2" {
		return GitResponse{Status: 200, ContentType: uploadAd, Body: v2Advertisement()}
	}
	return GitResponse{Status: 200, ContentType: uploadAd, Body: v0Advertisement(repo, "git-upload-pack")}
}

// ---------------------------------------------------------------- upload-pack v2

type v2FetchArgs struct {
	wants     []string
	haves     []string
	shallow   []string
	depth     int
	hasDepth  bool
	deepenNot []string
	filter    string
}

func parseV2Request(body []byte) (command string, args map[string][]string, err error) {
	packets, err := DecodePktLines(body)
	if err != nil {
		return "", nil, err
	}
	args = map[string][]string{}
	inArgs := false
	for _, p := range packets {
		if p.Kind == 'e' {
			inArgs = true
			continue
		}
		if p.Kind != 'd' {
			continue
		}
		text := string(p.Payload)
		if !inArgs {
			trimmed := strings.TrimSpace(text)
			if strings.HasPrefix(trimmed, "command=") {
				command = trimmed[len("command="):]
			}
			continue
		}
		trimmed := strings.TrimSuffix(text, "\n")
		key, val := trimmed, ""
		if sp := strings.Index(trimmed, " "); sp >= 0 {
			key, val = trimmed[:sp], trimmed[sp+1:]
		}
		args[key] = append(args[key], val)
	}
	return command, args, nil
}

// sidebandChunk frames a pack as side-band-64k band-1 chunks, one alloc per chunk.
func sidebandChunk(pack []byte) []byte {
	const chunk = 32768
	n := (len(pack) + chunk - 1) / chunk
	if n < 1 {
		n = 1
	}
	parts := make([][]byte, n)
	for i := 0; i < n; i++ {
		off := i * chunk
		dataLen := chunk
		if off+dataLen > len(pack) {
			dataLen = len(pack) - off
		}
		buf := make([]byte, 4+1+dataLen)
		copy(buf, fmt.Sprintf("%04x", dataLen+5))
		buf[4] = 1
		copy(buf[5:], pack[off:off+dataLen])
		parts[i] = buf
	}
	return bytes.Join(parts, nil)
}

func fetchResponseV2(repo *Repo, args v2FetchArgs) []byte {
	plan := PlanFetch(repo, args.wants, args.haves, args.shallow, args.depth, args.hasDepth)
	pack := WritePack(plan.Objects)
	var parts [][]byte
	parts = append(parts, PktLineStr("acknowledgments\n"))
	parts = append(parts, PktLineStr("NAK\n"))
	parts = append(parts, PktLineStr("ready\n"))
	parts = append(parts, pktDelim)
	if args.hasDepth || len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		parts = append(parts, PktLineStr("shallow-info\n"))
		for _, s := range plan.Shallow {
			parts = append(parts, PktLineStr("shallow "+s+"\n"))
		}
		for _, u := range plan.Unshallow {
			parts = append(parts, PktLineStr("unshallow "+u+"\n"))
		}
		parts = append(parts, pktDelim)
	}
	parts = append(parts, PktLineStr("packfile\n"))
	parts = append(parts, sidebandChunk(pack))
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func lsRefsResponseV2(repo *Repo, prefixes []string) []byte {
	var parts [][]byte
	head, hasHead := headOID(repo)
	refs := sortedRefs(repo)
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
	if !hasHead && keep("HEAD") {
		parts = append(parts, PktLineStr("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
	} else if hasHead && keep("HEAD") {
		parts = append(parts, PktLineStr(head+" HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
	}
	for _, ref := range refs {
		if !keep(ref.Name) {
			continue
		}
		if peeled, ok := peeledOID(repo, ref.OID); ok {
			parts = append(parts, PktLineStr(ref.OID+" "+ref.Name+" peeled:"+peeled+"\n"))
		} else {
			parts = append(parts, PktLineStr(ref.OID+" "+ref.Name+"\n"))
		}
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func UploadPack(owner, repoName string, body []byte, gitProtocol string) GitResponse {
	repo := GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	if gitProtocol == "version=2" {
		command, args, err := parseV2Request(body)
		if err != nil {
			return gitBadRequest(err.Error())
		}
		switch command {
		case "ls-refs":
			return GitResponse{Status: 200, ContentType: uploadRes, Body: lsRefsResponseV2(repo, args["ref-prefix"])}
		case "fetch":
			fa := v2FetchArgs{
				wants:     args["want"],
				haves:     args["have"],
				shallow:   args["shallow"],
				deepenNot: args["deepen-not"],
			}
			if v, ok := args["filter"]; ok && len(v) > 0 {
				fa.filter = v[0]
			}
			if v, ok := args["deepen"]; ok && len(v) > 0 && v[0] != "" {
				d, err := strconv.Atoi(v[0])
				if err != nil {
					return gitBadRequest("bad deepen")
				}
				fa.depth = d
				fa.hasDepth = true
			}
			for _, w := range fa.wants {
				if !IsObjectID(w) {
					return gitBadRequest("bad want oid")
				}
			}
			return GitResponse{Status: 200, ContentType: uploadRes, Body: fetchResponseV2(repo, fa)}
		default:
			return gitBadRequest(fmt.Sprintf("unknown v2 command %q", command))
		}
	}
	// v0 fetch
	resp, err := fetchResponseV0(repo, body)
	if err != nil {
		return gitBadRequest(err.Error())
	}
	return resp
}

func fetchResponseV0(repo *Repo, body []byte) (GitResponse, error) {
	packets, err := DecodePktLines(body)
	if err != nil {
		return GitResponse{}, err
	}
	var wants, haves, shallow []string
	var depth int
	hasDepth := false
	section := 0
	for _, p := range packets {
		if p.Kind == 'f' {
			section++
			continue
		}
		if p.Kind != 'd' {
			continue
		}
		text := strings.TrimSuffix(string(p.Payload), "\n")
		if section == 0 {
			if strings.HasPrefix(text, "want ") {
				rest := text[5:]
				if sp := strings.Index(rest, " "); sp < 0 {
					wants = append(wants, rest)
				} else {
					wants = append(wants, rest[:sp])
				}
			} else if strings.HasPrefix(text, "shallow ") {
				shallow = append(shallow, text[8:])
			} else if strings.HasPrefix(text, "deepen ") {
				d, err := strconv.Atoi(text[7:])
				if err != nil {
					return GitResponse{}, err
				}
				depth = d
				hasDepth = true
			}
		} else {
			if strings.HasPrefix(text, "have ") {
				haves = append(haves, text[5:])
			}
		}
	}
	plan := PlanFetch(repo, wants, haves, shallow, depth, hasDepth)
	pack := WritePack(plan.Objects)
	var parts [][]byte
	parts = append(parts, PktLineStr("NAK\n"))
	for _, s := range plan.Shallow {
		parts = append(parts, PktLineStr("shallow "+s+"\n"))
	}
	for _, u := range plan.Unshallow {
		parts = append(parts, PktLineStr("unshallow "+u+"\n"))
	}
	parts = append(parts, sidebandChunk(pack))
	parts = append(parts, pktFlush)
	return GitResponse{Status: 200, ContentType: uploadRes, Body: bytes.Join(parts, nil)}, nil
}

// ---------------------------------------------------------------- receive-pack v0

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func ReceivePack(owner, repoName string, body []byte) GitResponse {
	repo := GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()

	report := func(unpackOK bool, unpackDetail string, refs []struct {
		ref    string
		ok     bool
		reason string
	}) []byte {
		var inner [][]byte
		if unpackOK {
			inner = append(inner, PktLineStr("unpack ok\n"))
		} else {
			inner = append(inner, PktLineStr("unpack "+unpackDetail+"\n"))
		}
		for _, r := range refs {
			if r.ok {
				inner = append(inner, PktLineStr("ok "+r.ref+"\n"))
			} else {
				inner = append(inner, PktLineStr("ng "+r.ref+" "+r.reason+"\n"))
			}
		}
		innerBuf := bytes.Join(inner, nil)
		band := append([]byte{1}, innerBuf...)
		return bytes.Join([][]byte{PktLine(band), pktFlush}, nil)
	}
	failReport := func(detail string, commands []pushCommand) GitResponse {
		var refs []struct {
			ref    string
			ok     bool
			reason string
		}
		for _, c := range commands {
			refs = append(refs, struct {
				ref    string
				ok     bool
				reason string
			}{c.ref, false, detail})
		}
		return GitResponse{Status: 200, ContentType: receiveRes, Body: report(false, detail, refs)}
	}

	// Split the pkt-line command list (up to the first flush) from the raw pack.
	off := 0
	var commands []pushCommand
	packStart := len(body)
	for off+4 <= len(body) {
		head := string(body[off : off+4])
		ln, err := strconv.ParseInt(head, 16, 32)
		if err != nil {
			return gitBadRequest("bad pkt-line in command list")
		}
		if ln == 0 {
			packStart = off + 4
			break
		}
		if ln < 4 || off+int(ln) > len(body) {
			return gitBadRequest("truncated command")
		}
		line := string(body[off+4 : off+int(ln)])
		nul := strings.IndexByte(line, 0)
		text := line
		if nul >= 0 {
			text = line[:nul]
		}
		text = strings.TrimSuffix(text, "\n")
		parts := strings.Split(text, " ")
		if len(parts) != 3 {
			return gitBadRequest("bad push command")
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		off += int(ln)
	}
	if len(commands) == 0 {
		return gitBadRequest("no push commands")
	}
	hasUpdates := false
	for _, c := range commands {
		if c.newOID != ZeroOID {
			hasUpdates = true
			break
		}
	}
	packBytes := body[packStart:]
	if hasUpdates {
		if len(packBytes) == 0 {
			return failReport("unpacker error", commands)
		}
		parsed, err := ReadPack(packBytes, func(oid string) *GitObject {
			if o, ok := repo.Objects[oid]; ok {
				return &o
			}
			return nil
		})
		if err != nil {
			return failReport("unpacker error", commands)
		}
		for _, c := range commands {
			if c.newOID == ZeroOID {
				continue
			}
			var obj *GitObject
			if o, ok := parsed.Objects[c.newOID]; ok {
				obj = &o
			} else if o, ok := repo.Objects[c.newOID]; ok {
				obj = &o
			}
			if obj == nil || (obj.Type != ObjCommit && obj.Type != ObjTag) {
				return failReport("unpacker error", commands)
			}
		}
		for oid, obj := range parsed.Objects {
			if _, ok := repo.Objects[oid]; !ok {
				repo.Objects[oid] = obj
			}
		}
	}
	var results []struct {
		ref    string
		ok     bool
		reason string
	}
	for _, c := range commands {
		if !FastForwardOK(repo, c.ref, c.oldOID, c.newOID) {
			results = append(results, struct {
				ref    string
				ok     bool
				reason string
			}{c.ref, false, "non-fast-forward"})
			continue
		}
		if c.newOID == ZeroOID {
			delete(repo.Refs, c.ref)
		} else {
			repo.Refs[c.ref] = c.newOID
		}
		results = append(results, struct {
			ref    string
			ok     bool
			reason string
		}{c.ref, true, ""})
	}
	return GitResponse{Status: 200, ContentType: receiveRes, Body: report(true, "ok", results)}
}
