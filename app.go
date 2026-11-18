// app.go — the forge's HTTP routing: health probe, API root and meta, git
// smart HTTP, REST v3, GraphQL and MCP. One process, one in-memory store.
package main

import (
	"net/url"
	"strings"
)

// appJSON sends a JSON response the way app.ts does: no X-GitHub-Request-Id,
// no request-id counter increment.
func appJSON(status int, value interface{}) *Response {
	return &Response{Status: status, ContentType: "application/json", Body: marshalJSON(value)}
}

// appJSONWithID sends a JSON response with an X-GitHub-Request-Id (healthz,
// API root, meta).
func appJSONWithID(status int, value interface{}, extra map[string]string) *Response {
	if extra == nil {
		extra = map[string]string{}
	}
	extra["X-GitHub-Request-Id"] = requestID()
	return &Response{Status: status, ContentType: "application/json", Body: marshalJSON(value), Extra: extra}
}

func dispatch(store *Store, req *Request) *Response {
	path := req.Path
	method := req.Method

	// ---- health probe (the env's boot check calls this twice) ----
	if method == "GET" && path == "/healthz" {
		return appJSONWithID(200, map[string]interface{}{"ok": true}, nil)
	}

	// ---- GitHub API root ----
	if method == "GET" && (path == "/api/v3" || path == "/api/v3/") {
		return appJSONWithID(200, map[string]interface{}{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		}, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
	}

	// ---- GitHub meta ----
	if method == "GET" && path == "/api/v3/meta" {
		return appJSONWithID(200, map[string]interface{}{
			"verifiable_password_authentication":   true,
			"github_enterprise_importer_enabled":   false,
			"installed_version":                    "1.0.0",
			"hooks":                                []interface{}{},
			"git":                                  []interface{}{},
			"pages":                                []interface{}{},
			"importer":                             []interface{}{},
			"actions":                              []interface{}{},
			"packages":                             []interface{}{},
			"domains":                              map[string]interface{}{},
		}, nil)
	}

	// ---- git smart HTTP ----
	if owner, repoName, service, isGit := parseGitRoute(path); isGit {
		query, _ := url.ParseQuery(req.Query)
		gitProtocol := req.Headers["git-protocol"]
		if service == "info/refs" && method == "GET" {
			return handleInfoRefs(store, owner, repoName, query.Get("service"), gitProtocol)
		}
		if service == "git-upload-pack" && method == "POST" {
			return handleUploadPack(store, owner, repoName, req.Body, gitProtocol)
		}
		if service == "git-receive-pack" && method == "POST" {
			return handleReceivePack(store, owner, repoName, req.Body)
		}
		return appJSON(404, map[string]interface{}{"message": "Not Found"})
	}

	// ---- GraphQL: POST /api/graphql (Bearer required) ----
	if path == "/api/graphql" && method == "POST" {
		auth := req.Headers["authorization"]
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(auth[7:]) == "" {
			return appJSON(401, map[string]interface{}{"message": "Requires authentication"})
		}
		status, payload := handleGraphqlBody(store, req.Body)
		return appJSON(status, payload)
	}

	// ---- MCP: POST /mcp ----
	if path == "/mcp" && method == "POST" {
		return handleMcp(store, req)
	}

	// ---- REST v3: /api/v3/... ----
	if strings.HasPrefix(path, "/api/v3/") {
		return handleRest(store, req)
	}

	return appJSON(404, map[string]interface{}{"message": "Not Found"})
}

// parseGitRoute matches /{owner}/{repo}.git/{info/refs|git-upload-pack|git-receive-pack}.
func parseGitRoute(path string) (owner, repoName, service string, ok bool) {
	segments := splitPath(path)
	if len(segments) < 3 {
		return "", "", "", false
	}
	if !strings.HasSuffix(segments[1], ".git") {
		return "", "", "", false
	}
	repo := segments[1][:len(segments[1])-4]
	if repo == "" {
		return "", "", "", false
	}
	if len(segments) == 4 && segments[2] == "info" && segments[3] == "refs" {
		return segments[0], repo, "info/refs", true
	}
	if len(segments) == 3 && segments[2] == "git-upload-pack" {
		return segments[0], repo, "git-upload-pack", true
	}
	if len(segments) == 3 && segments[2] == "git-receive-pack" {
		return segments[0], repo, "git-receive-pack", true
	}
	return "", "", "", false
}
