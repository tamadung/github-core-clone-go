// mcp.go — the Model Context Protocol surface: server/discover (modern era),
// initialize (legacy handshake, kept as a safety net), tools/list and
// tools/call for the six tools the conformance client needs. Responses are
// plain JSON-RPC 2.0 over HTTP; the tools list is a fixed value so two calls
// are byte-identical.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

const mcpServerName = "forge"
const mcpServerVersion = "1.0.0"
const mcpProtocolVersion = "2026-07-28"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolDef struct {
	name        string
	description string
	properties  map[string]map[string]interface{}
	required    []string
}

func mcpProp(typ, desc string) map[string]interface{} {
	return map[string]interface{}{"type": typ, "description": desc}
}

var mcpTools = []mcpToolDef{
	{
		name: "create_repo", description: "Create a repository",
		properties: map[string]map[string]interface{}{
			"name":        mcpProp("string", "Repository name"),
			"description": mcpProp("string", "Repository description"),
			"private":     mcpProp("boolean", "Whether the repository is private"),
			"owner":       mcpProp("string", "Repository owner login"),
		},
		required: []string{"name"},
	},
	{
		name: "get_repo", description: "Get a repository",
		properties: map[string]map[string]interface{}{
			"owner": mcpProp("string", "Repository owner login"),
			"repo":  mcpProp("string", "Repository name"),
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_issue", description: "Create an issue",
		properties: map[string]map[string]interface{}{
			"owner": mcpProp("string", "Repository owner login"),
			"repo":  mcpProp("string", "Repository name"),
			"title": mcpProp("string", "Issue title"),
			"body":  mcpProp("string", "Issue body"),
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name: "list_issues", description: "List issues in a repository",
		properties: map[string]map[string]interface{}{
			"owner": mcpProp("string", "Repository owner login"),
			"repo":  mcpProp("string", "Repository name"),
			"state": mcpProp("string", "Issue state: open, closed, or all"),
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_pull_request", description: "Create a pull request",
		properties: map[string]map[string]interface{}{
			"owner": mcpProp("string", "Repository owner login"),
			"repo":  mcpProp("string", "Repository name"),
			"title": mcpProp("string", "Pull request title"),
			"body":  mcpProp("string", "Pull request body"),
			"head":  mcpProp("string", "Head branch name"),
			"base":  mcpProp("string", "Base branch name"),
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name: "merge_pull_request", description: "Merge a pull request",
		properties: map[string]map[string]interface{}{
			"owner":       mcpProp("string", "Repository owner login"),
			"repo":        mcpProp("string", "Repository name"),
			"pull_number": mcpProp("integer", "Pull request number"),
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpToolsJSON() []interface{} {
	out := make([]interface{}, 0, len(mcpTools))
	for _, t := range mcpTools {
		props := map[string]interface{}{}
		for k, v := range t.properties {
			props[k] = v
		}
		out = append(out, map[string]interface{}{
			"name":        t.name,
			"description": t.description,
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": props,
				"required":   t.required,
			},
		})
	}
	return out
}

type mcpToolResult struct {
	content           []map[string]interface{}
	structuredContent map[string]interface{}
	isError           bool
}

func mcpOKResult(payload map[string]interface{}) mcpToolResult {
	return mcpToolResult{
		content:           []map[string]interface{}{{"type": "text", "text": string(marshalJSON(payload))}},
		structuredContent: payload,
		isError:           false,
	}
}

func mcpErrResult(message string) mcpToolResult {
	return mcpToolResult{
		content:           []map[string]interface{}{{"type": "text", "text": message}},
		structuredContent: map[string]interface{}{"error": message},
		isError:           true,
	}
}

func mcpMissingArg(name string) mcpToolResult {
	return mcpErrResult("Missing required argument: " + name)
}

func mcpAsNumber(value interface{}) (int, bool) {
	if n, ok := value.(float64); ok {
		return int(n), true
	}
	if s, ok := value.(string); ok {
		isDigits := len(s) > 0
		for _, c := range s {
			if c < '0' || c > '9' {
				isDigits = false
			}
		}
		if isDigits {
			n, err := strconv.Atoi(s)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func mcpCallTool(store *Store, name string, args map[string]interface{}) mcpToolResult {
	switch name {
	case "create_repo":
		repoName := asString(args["name"])
		if repoName == "" {
			return mcpMissingArg("name")
		}
		owner := asString(args["owner"])
		if owner != "" {
			store.learnViewer(owner)
		}
		if store.findRepoByName(repoName) != nil {
			return mcpErrResult("Repository " + repoName + " already exists")
		}
		repo := store.createRepo(repoName, asString(args["description"]), args["private"] == true)
		return mcpOKResult(repoShape(store, repo))
	case "get_repo":
		owner := asString(args["owner"])
		repoName := asString(args["repo"])
		if owner == "" {
			return mcpMissingArg("owner")
		}
		if repoName == "" {
			return mcpMissingArg("repo")
		}
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			return mcpErrResult("Repository " + owner + "/" + repoName + " not found")
		}
		return mcpOKResult(repoShape(store, repo))
	case "create_issue":
		owner := asString(args["owner"])
		repoName := asString(args["repo"])
		title := asString(args["title"])
		if owner == "" {
			return mcpMissingArg("owner")
		}
		if repoName == "" {
			return mcpMissingArg("repo")
		}
		if title == "" {
			return mcpMissingArg("title")
		}
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			return mcpErrResult("Repository " + owner + "/" + repoName + " not found")
		}
		issue := store.createIssue(repo, title, asString(args["body"]), nil)
		return mcpOKResult(issueShape(store, repo, issue))
	case "list_issues":
		owner := asString(args["owner"])
		repoName := asString(args["repo"])
		if owner == "" {
			return mcpMissingArg("owner")
		}
		if repoName == "" {
			return mcpMissingArg("repo")
		}
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			return mcpErrResult("Repository " + owner + "/" + repoName + " not found")
		}
		state := asString(args["state"])
		if state == "" {
			state = "open"
		}
		var items []*Issue
		for _, i := range repo.Issues {
			if state != "all" && i.State != state {
				continue
			}
			items = append(items, i)
		}
		// sort by createdSeq desc
		for a := 0; a < len(items); a++ {
			for b := a + 1; b < len(items); b++ {
				if items[b].CreatedSeq > items[a].CreatedSeq {
					items[a], items[b] = items[b], items[a]
				}
			}
		}
		issues := make([]interface{}, 0, len(items))
		for _, i := range items {
			issues = append(issues, issueShape(store, repo, i))
		}
		return mcpOKResult(map[string]interface{}{"issues": issues})
	case "create_pull_request":
		owner := asString(args["owner"])
		repoName := asString(args["repo"])
		title := asString(args["title"])
		head := stripOwnerPrefix(asString(args["head"]))
		base := asString(args["base"])
		if owner == "" {
			return mcpMissingArg("owner")
		}
		if repoName == "" {
			return mcpMissingArg("repo")
		}
		if title == "" {
			return mcpMissingArg("title")
		}
		if head == "" {
			return mcpMissingArg("head")
		}
		if base == "" {
			return mcpMissingArg("base")
		}
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			return mcpErrResult("Repository " + owner + "/" + repoName + " not found")
		}
		pull := store.createPull(repo, title, asString(args["body"]), head, base)
		if pull == nil {
			return mcpErrResult("Head or base ref not found")
		}
		return mcpOKResult(pullShape(store, repo, pull))
	case "merge_pull_request":
		owner := asString(args["owner"])
		repoName := asString(args["repo"])
		number, hasNumber := mcpAsNumber(args["pull_number"])
		if owner == "" {
			return mcpMissingArg("owner")
		}
		if repoName == "" {
			return mcpMissingArg("repo")
		}
		if !hasNumber {
			return mcpMissingArg("pull_number")
		}
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			return mcpErrResult("Repository " + owner + "/" + repoName + " not found")
		}
		var pull *Pull
		for _, p := range repo.Pulls {
			if p.Number == number {
				pull = p
				break
			}
		}
		if pull == nil {
			return mcpErrResult("Pull request #" + itoa(number) + " not found")
		}
		if pull.Merged {
			return mcpErrResult("Pull request #" + itoa(number) + " is already merged")
		}
		if !store.mergeable(pull, repo) {
			return mcpErrResult("Merge conflict")
		}
		sha := store.mergePull(repo, pull)
		if sha == "" {
			return mcpErrResult("Merge conflict")
		}
		return mcpOKResult(pullShape(store, repo, pull))
	default:
		return mcpErrResult("Unknown tool: " + name)
	}
}

func mcpSendJSON(status int, value interface{}) *Response {
	return &Response{Status: status, ContentType: "application/json", Body: marshalJSON(value)}
}

func handleMcp(store *Store, req *Request) *Response {
	// Auth: every /mcp call needs a non-empty Bearer token.
	auth := req.Headers["authorization"]
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(auth[7:]) == "" {
		var id interface{}
		var parsed map[string]interface{}
		if err := json.Unmarshal(req.Body, &parsed); err == nil && parsed != nil {
			if v, ok := parsed["id"]; ok {
				id = v
			}
		}
		return mcpSendJSON(401, map[string]interface{}{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]interface{}{"code": -32001, "message": "Unauthorized"},
		})
	}

	var request interface{}
	if err := json.Unmarshal(req.Body, &request); err != nil {
		return mcpSendJSON(400, map[string]interface{}{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]interface{}{"code": -32700, "message": "Parse error"},
		})
	}
	rec, ok := request.(map[string]interface{})
	if !ok {
		return mcpSendJSON(400, map[string]interface{}{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]interface{}{"code": -32600, "message": "Invalid Request"},
		})
	}
	method, _ := rec["method"].(string)
	var id interface{}
	hasID := false
	if v, ok := rec["id"]; ok {
		id = v
		hasID = true
	}
	params := map[string]interface{}{}
	if p, ok := rec["params"].(map[string]interface{}); ok {
		params = p
	}

	// Notifications carry no id and get no JSON-RPC response.
	if !hasID {
		return &Response{Status: 202, NoBody: true}
	}

	okResp := func(result interface{}) *Response {
		return mcpSendJSON(200, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
	}
	errResp := func(code int, message string) *Response {
		return mcpSendJSON(200, map[string]interface{}{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]interface{}{"code": code, "message": message},
		})
	}

	switch method {
	case "server/discover":
		return okResp(map[string]interface{}{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":        map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion},
			"_meta": map[string]interface{}{
				"io.modelcontextprotocol/serverInfo": map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion},
			},
		})
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		version := mcpProtocolVersion
		if requested != "" {
			for _, v := range mcpSupportedVersions {
				if v == requested {
					version = requested
					break
				}
			}
		}
		return okResp(map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion},
		})
	case "ping":
		return okResp(map[string]interface{}{})
	case "tools/list":
		return okResp(map[string]interface{}{"tools": mcpToolsJSON()})
	case "tools/call":
		toolName, _ := params["name"].(string)
		toolArgs := map[string]interface{}{}
		if a, ok := params["arguments"].(map[string]interface{}); ok {
			toolArgs = a
		}
		known := false
		for _, t := range mcpTools {
			if t.name == toolName {
				known = true
				break
			}
		}
		if !known {
			return errResp(-32602, "Unknown tool: "+toolName)
		}
		result := mcpCallTool(store, toolName, toolArgs)
		content := make([]interface{}, 0, len(result.content))
		for _, c := range result.content {
			content = append(content, c)
		}
		return okResp(map[string]interface{}{
			"content":           content,
			"structuredContent": result.structuredContent,
			"isError":           result.isError,
		})
	default:
		return errResp(-32601, "Method not found: "+method)
	}
}
