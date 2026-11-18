// rest.go — the GitHub REST v3 surface: repos, issues, pulls, labels,
// comments, commits, contents and git refs. Responses are JSON with an
// X-GitHub-Request-Id on every one; shapes carry the fields the conformance
// client reads, plus the usual GitHub extras (extra fields are allowed).
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

var requestCounter int64

// requestID renders "req_%08x" without fmt.Sprintf (~160 ns -> ~20 ns; called
// once per response, so ~2,200x per suite run).
func requestID() string {
	n := atomic.AddInt64(&requestCounter, 1)
	var b [12]byte
	b[0], b[1], b[2], b[3] = 'r', 'e', 'q', '_'
	const hexd = "0123456789abcdef"
	for i := 11; i >= 4; i-- {
		b[i] = hexd[n&0xf]
		n >>= 4
	}
	return string(b[:])
}

// marshalJSON serializes a value the way JSON.stringify does: no HTML
// escaping of <, > and &, no trailing newline.
func marshalJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

func sendJSON(status int, value interface{}, extra map[string]string) *Response {
	body := marshalJSON(value)
	if extra == nil {
		extra = map[string]string{}
	}
	extra["X-GitHub-Request-Id"] = requestID()
	return &Response{Status: status, ContentType: "application/json", Body: body, Extra: extra}
}

func sendError(status int, message string) *Response {
	return sendJSON(status, map[string]interface{}{"message": message, "documentation_url": "https://docs.github.com/rest"}, nil)
}

func sendNoContent(status int) *Response {
	return &Response{Status: status, NoBody: true, Extra: map[string]string{"X-GitHub-Request-Id": requestID()}}
}

func b64(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

func userShape(login string) map[string]interface{} {
	return map[string]interface{}{
		"login": login, "id": 1, "node_id": b64("User:" + login),
		"avatar_url": "", "type": "User", "site_admin": false,
	}
}

func repoShape(store *Store, repo *Repo) map[string]interface{} {
	owner := store.repoOwner(repo)
	openIssues := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			openIssues++
		}
	}
	return map[string]interface{}{
		"id": repo.ID, "node_id": b64("Repository:" + itoa(repo.ID)),
		"name": repo.Name, "full_name": owner + "/" + repo.Name,
		"owner":         userShape(owner),
		"private":       repo.Private,
		"description":   repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":    tsFor(repo.CreatedSeq),
		"updated_at":    tsFor(repo.CreatedSeq),
		"pushed_at":     tsFor(repo.CreatedSeq),
		"html_url":      "https://github.com/" + owner + "/" + repo.Name,
		"url":           "/repos/" + owner + "/" + repo.Name,
		"clone_url":     "https://github.com/" + owner + "/" + repo.Name + ".git",
		"git_url":       "git://github.com/" + owner + "/" + repo.Name + ".git",
		"ssh_url":       "git@github.com:" + owner + "/" + repo.Name + ".git",
		"homepage":      nil,
		"language":      nil,
		"topics":        []interface{}{},
		"fork":          false,
		"forks_count":   0,
		"stargazers_count": 0,
		"watchers_count":   0,
		"open_issues_count": openIssues,
		"size":      0,
		"archived":  false,
		"disabled":  false,
		"visibility": func() string {
			if repo.Private {
				return "private"
			}
			return "public"
		}(),
		"license": nil,
	}
}

func labelShape(label *Label) map[string]interface{} {
	return map[string]interface{}{
		"id": label.ID, "node_id": b64("Label:" + itoa(label.ID)),
		"name": label.Name, "color": label.Color,
		"description": label.Description, "default": false,
	}
}

func issueShape(store *Store, repo *Repo, issue *Issue) map[string]interface{} {
	owner := store.repoOwner(repo)
	user := issue.User
	if user == "" {
		user = owner
	}
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, labelShape(l))
	}
	var closedAt interface{}
	if issue.State == "closed" {
		closedAt = tsFor(issue.UpdatedSeq)
	}
	return map[string]interface{}{
		"id": issue.ID, "node_id": b64("Issue:" + itoa(issue.ID)),
		"number": issue.Number, "title": issue.Title, "body": issue.Body,
		"state": issue.State, "locked": false,
		"labels": labels,
		"user":   userShape(user),
		"assignee": nil, "assignees": []interface{}{}, "milestone": nil,
		"comments":   issue.CommentsCount(),
		"created_at": tsFor(issue.CreatedSeq),
		"updated_at": tsFor(issue.UpdatedSeq),
		"closed_at":  closedAt,
		"html_url":   "https://github.com/" + owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		"url":        "/repos/" + owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		"repository_url":     "/repos/" + owner + "/" + repo.Name,
		"author_association": "OWNER",
	}
}

func (i *Issue) CommentsCount() int { return len(i.Comments) }

// --- Struct-based shapes (REST hot path) ---
//
// The map-based shapes above (userShape/labelShape/issueShape) are kept for
// MCP (mcpOKResult needs a map for structuredContent) and general use. The
// structs below are byte-identical twins used only by the REST handlers on
// the timed path (issue create / single-issue GET / PATCH / list): encoding
// a struct is ~3x faster than encoding a map (no key sorting, no per-value
// reflection), and the issue marshal is ~43% of the write dispatch.
//
// Field order is ALPHABETICAL in every struct: encoding/json emits struct
// fields in declaration order, and the map encoder emits keys sorted, so
// alphabetical declaration == the map output, byte for byte. Verified by
// TestIssueShapeByteIdentical (bench_test.go, temporary). Do NOT reorder
// fields, do NOT add `omitempty` (nil/empty fields must render as null/[]).

type userJSON struct {
	AvatarURL string `json:"avatar_url"`
	ID        int    `json:"id"`
	Login     string `json:"login"`
	NodeID    string `json:"node_id"`
	SiteAdmin bool   `json:"site_admin"`
	Type      string `json:"type"`
}

type labelJSON struct {
	Color       string `json:"color"`
	Default     bool   `json:"default"`
	Description string `json:"description"`
	ID          int    `json:"id"`
	Name        string `json:"name"`
	NodeID      string `json:"node_id"`
}

type issueJSON struct {
	Assignee          interface{} `json:"assignee"`
	Assignees         []string    `json:"assignees"`
	AuthorAssociation string      `json:"author_association"`
	Body              string      `json:"body"`
	ClosedAt          interface{} `json:"closed_at"`
	Comments          int         `json:"comments"`
	CreatedAt         string      `json:"created_at"`
	HTMLURL           string      `json:"html_url"`
	ID                int         `json:"id"`
	Labels            []labelJSON `json:"labels"`
	Locked            bool        `json:"locked"`
	Milestone         interface{} `json:"milestone"`
	NodeID            string      `json:"node_id"`
	Number            int         `json:"number"`
	RepositoryURL     string      `json:"repository_url"`
	State             string      `json:"state"`
	Title             string      `json:"title"`
	UpdatedAt         string      `json:"updated_at"`
	URL               string      `json:"url"`
	User              userJSON    `json:"user"`
}

func userShapeStruct(login string) userJSON {
	return userJSON{
		AvatarURL: "", ID: 1, Login: login, NodeID: b64("User:" + login),
		SiteAdmin: false, Type: "User",
	}
}

func labelShapeStruct(label *Label) labelJSON {
	return labelJSON{
		Color: label.Color, Default: false, Description: label.Description,
		ID: label.ID, Name: label.Name, NodeID: b64("Label:" + itoa(label.ID)),
	}
}

func issueShapeStruct(store *Store, repo *Repo, issue *Issue) issueJSON {
	owner := store.repoOwner(repo)
	user := issue.User
	if user == "" {
		user = owner
	}
	labels := make([]labelJSON, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, labelShapeStruct(l))
	}
	var closedAt interface{}
	if issue.State == "closed" {
		closedAt = tsFor(issue.UpdatedSeq)
	}
	return issueJSON{
		Assignee: nil, Assignees: []string{}, AuthorAssociation: "OWNER",
		Body: issue.Body, ClosedAt: closedAt, Comments: issue.CommentsCount(),
		CreatedAt: tsFor(issue.CreatedSeq), UpdatedAt: tsFor(issue.UpdatedSeq),
		HTMLURL: "https://github.com/" + owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		ID: issue.ID, Labels: labels, Locked: false, Milestone: nil,
		NodeID: b64("Issue:" + itoa(issue.ID)), Number: issue.Number,
		RepositoryURL: "/repos/" + owner + "/" + repo.Name,
		State: issue.State, Title: issue.Title,
		URL: "/repos/" + owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		User: userShapeStruct(user),
	}
}

func pullShape(store *Store, repo *Repo, pull *Pull) map[string]interface{} {
	owner := store.repoOwner(repo)
	headRepo := repoShape(store, repo)
	user := pull.User
	if user == "" {
		user = owner
	}
	mergeable := store.mergeable(pull, repo)
	mergeableState := "dirty"
	if pull.Merged || mergeable {
		mergeableState = "clean"
	}
	var closedAt interface{}
	if pull.State == "closed" {
		closedAt = tsFor(pull.UpdatedSeq)
	}
	var mergedAt interface{}
	if pull.MergedAt != "" {
		mergedAt = pull.MergedAt
	}
	var mergeCommitSHA interface{}
	if pull.MergeCommitSHA != "" {
		mergeCommitSHA = pull.MergeCommitSHA
	}
	return map[string]interface{}{
		"id": pull.ID, "node_id": b64("PullRequest:" + itoa(pull.ID)),
		"number": pull.Number, "title": pull.Title, "body": pull.Body,
		"state": pull.State, "merged": pull.Merged,
		"merged_at": mergedAt, "merge_commit_sha": mergeCommitSHA,
		"mergeable": mergeable, "mergeable_state": mergeableState,
		"draft": false,
		"head": map[string]interface{}{
			"label": owner + ":" + pull.HeadRef, "ref": pull.HeadRef,
			"sha": pull.HeadSha, "user": userShape(owner), "repo": headRepo,
		},
		"base": map[string]interface{}{
			"label": owner + ":" + pull.BaseRef, "ref": pull.BaseRef,
			"sha": pull.BaseSha, "user": userShape(owner), "repo": headRepo,
		},
		"user":       userShape(user),
		"created_at": tsFor(pull.CreatedSeq),
		"updated_at": tsFor(pull.UpdatedSeq),
		"closed_at":  closedAt,
		"html_url":   "https://github.com/" + owner + "/" + repo.Name + "/pull/" + itoa(pull.Number),
		"url":        "/repos/" + owner + "/" + repo.Name + "/pulls/" + itoa(pull.Number),
		"commits_url":        "/repos/" + owner + "/" + repo.Name + "/pulls/" + itoa(pull.Number) + "/commits",
		"comments_url":       "/repos/" + owner + "/" + repo.Name + "/issues/" + itoa(pull.Number) + "/comments",
		"review_comments_url": "/repos/" + owner + "/" + repo.Name + "/pulls/" + itoa(pull.Number) + "/comments",
		"requested_reviewers": []interface{}{},
		"labels":              []interface{}{},
		"milestone":           nil,
		"author_association":  "OWNER",
	}
}

func parsePage(query url.Values) (page, perPage int) {
	page = 1
	if v := query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
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
	return page, perPage
}

func paginateLen(total, page, perPage int) (start, end, totalPages int) {
	totalPages = (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	return start, end, totalPages
}

func linkHeader(req *Request, path string, query url.Values, page, perPage, totalPages int) string {
	host := req.Headers["host"]
	if host == "" {
		host = "127.0.0.1"
	}
	build := func(p int) string {
		q := url.Values{}
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("page", itoa(p))
		q.Set("per_page", itoa(perPage))
		return "http://" + host + path + "?" + q.Encode()
	}
	var links []string
	if page < totalPages {
		links = append(links, "<"+build(page+1)+">; rel=\"next\"")
		links = append(links, "<"+build(totalPages)+">; rel=\"last\"")
	}
	if page > 1 {
		links = append(links, "<"+build(1)+">; rel=\"first\"")
		links = append(links, "<"+build(page-1)+">; rel=\"prev\"")
	}
	return strings.Join(links, ", ")
}

func parseJSONBody(body []byte) (map[string]interface{}, bool) {
	if len(body) == 0 {
		return map[string]interface{}{}, true
	}
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	obj, ok := parsed.(map[string]interface{})
	if !ok {
		return nil, false
	}
	return obj, true
}

func asString(value interface{}) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func asStringArray(value interface{}) []string {
	if arr, ok := value.([]interface{}); ok {
		var out []string
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := value.(string); ok {
		return []string{s}
	}
	return nil
}

func handleRest(store *Store, req *Request) *Response {
	query, _ := url.ParseQuery(req.Query)
	method := req.Method
	path := req.Path

	// Auth: every /api/v3 route needs a non-empty Bearer token.
	auth := req.Headers["authorization"]
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(auth[7:]) == "" {
		return sendError(401, "Requires authentication")
	}

	parsed, ok := parseJSONBody(req.Body)
	if !ok && (method == "POST" || method == "PUT" || method == "PATCH") {
		return sendError(400, "Problems parsing JSON")
	}
	input := parsed
	if input == nil {
		input = map[string]interface{}{}
	}

	segments := splitPath(path)
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "v3" {
		return sendError(404, "Not Found")
	}
	rest := segments[2:]

	// ---- /user ----
	if len(rest) == 1 && rest[0] == "user" {
		if method != "GET" {
			return sendError(404, "Not Found")
		}
		if store.Viewer == "" {
			return sendError(404, "Not Found")
		}
		return sendJSON(200, map[string]interface{}{
			"login": store.Viewer, "id": 1, "node_id": b64("User:" + store.Viewer),
			"type": "User", "site_admin": false,
		}, nil)
	}

	// ---- POST /user/repos, GET /user/repos ----
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "POST" {
			name := asString(input["name"])
			if name == "" || len(name) > 100 || strings.ContainsAny(name, " \t\n\r") {
				return sendError(422, "Validation Failed")
			}
			if store.findRepoByName(name) != nil {
				return sendError(422, "Repository already exists")
			}
			repo := store.createRepo(name, asString(input["description"]), input["private"] == true)
			return sendJSON(201, repoShape(store, repo), nil)
		}
		if method == "GET" {
			page, perPage := parsePage(query)
			var owned []*Repo
			for _, r := range store.Repos {
				owner := r.Owner
				if owner == "" {
					owner = store.Viewer
				}
				if owner == store.Viewer {
					owned = append(owned, r)
				}
			}
			start, end, totalPages := paginateLen(len(owned), page, perPage)
			extra := map[string]string{}
			if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
				extra["Link"] = link
			}
			items := make([]interface{}, 0, end-start)
			for _, r := range owned[start:end] {
				items = append(items, repoShape(store, r))
			}
			return sendJSON(200, items, extra)
		}
		return sendError(404, "Not Found")
	}

	// ---- /repos/:owner/:repo/... ----
	if len(rest) >= 3 && rest[0] == "repos" {
		owner := rest[1]
		repoName := rest[2]
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			return sendError(404, "Not Found")
		}
		return handleRepoRoute(store, req, repo, rest[3:], query, method, input, path)
	}

	return sendError(404, "Not Found")
}

func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func handleRepoRoute(store *Store, req *Request, repo *Repo, tail []string, query url.Values, method string, input map[string]interface{}, path string) *Response {
	owner := store.repoOwner(repo)

	// ---- the repo itself ----
	if len(tail) == 0 {
		if method == "GET" {
			return sendJSON(200, repoShape(store, repo), nil)
		}
		if method == "PATCH" {
			if v, ok := input["description"].(string); ok {
				repo.Description = v
			}
			if v, ok := input["private"].(bool); ok {
				repo.Private = v
			}
			return sendJSON(200, repoShape(store, repo), nil)
		}
		if method == "DELETE" {
			var kept []*Repo
			for _, r := range store.Repos {
				if r != repo {
					kept = append(kept, r)
				}
			}
			store.Repos = kept
			delete(store.ByName, repo.Name)
			return sendNoContent(204)
		}
		return sendError(404, "Not Found")
	}

	resource := tail[0]

	// ---- issues ----
	if resource == "issues" {
		if len(tail) == 1 {
			if method == "GET" {
				page, perPage := parsePage(query)
				state := query.Get("state")
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
				if labelFilter := query.Get("labels"); labelFilter != "" {
					var wanted []string
					for _, s := range strings.Split(labelFilter, ",") {
						s = strings.TrimSpace(s)
						if s != "" {
							wanted = append(wanted, s)
						}
					}
					var filtered []*Issue
					for _, i := range items {
						okAll := true
						for _, w := range wanted {
							found := false
							for _, l := range i.Labels {
								if l.Name == w {
									found = true
									break
								}
							}
							if !found {
								okAll = false
								break
							}
						}
						if okAll {
							filtered = append(filtered, i)
						}
					}
					items = filtered
				}
				sortParam := query.Get("sort")
				if sortParam == "" {
					sortParam = "created"
				}
				direction := query.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				key := func(i *Issue) int {
					if sortParam == "updated" {
						return i.UpdatedSeq
					}
					return i.CreatedSeq
				}
				sort.Slice(items, func(a, b int) bool {
					if direction == "asc" {
						return key(items[a]) < key(items[b])
					}
					return key(items[a]) > key(items[b])
				})
				start, end, totalPages := paginateLen(len(items), page, perPage)
				extra := map[string]string{}
				if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
					extra["Link"] = link
				}
				out := make([]interface{}, 0, end-start)
				for _, i := range items[start:end] {
					out = append(out, issueShapeStruct(store, repo, i))
				}
				return sendJSON(200, out, extra)
			}
			if method == "POST" {
				title := asString(input["title"])
				if title == "" {
					return sendError(422, "Validation Failed")
				}
				labelNames := asStringArray(input["labels"])
				var unknown []string
				for _, name := range labelNames {
					found := false
					for _, l := range repo.Labels {
						if l.Name == name {
							found = true
							break
						}
					}
					if !found {
						unknown = append(unknown, name)
					}
				}
				if len(unknown) > 0 {
					return sendError(422, "Validation Failed: label \""+unknown[0]+"\" does not exist")
				}
				issue := store.createIssue(repo, title, asString(input["body"]), labelNames)
				return sendJSON(201, issueShapeStruct(store, repo, issue), nil)
			}
			return sendError(404, "Not Found")
		}
		number, err := strconv.Atoi(tail[1])
		if err != nil {
			return sendError(404, "Not Found")
		}
		var issue *Issue
		for _, i := range repo.Issues {
			if i.Number == number {
				issue = i
				break
			}
		}
		if issue == nil {
			return sendError(404, "Not Found")
		}
		// /issues/:number/comments
		if len(tail) == 3 && tail[2] == "comments" {
			if method == "GET" {
				page, perPage := parsePage(query)
				start, end, totalPages := paginateLen(len(issue.Comments), page, perPage)
				extra := map[string]string{}
				if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
					extra["Link"] = link
				}
				out := make([]interface{}, 0, end-start)
				for _, c := range issue.Comments[start:end] {
					out = append(out, commentShape(store, repo, issue, c))
				}
				return sendJSON(200, out, extra)
			}
			if method == "POST" {
				commentBody := asString(input["body"])
				if commentBody == "" {
					return sendError(422, "Validation Failed")
				}
				comment := &Comment{ID: store.NextCommentID, Body: commentBody, User: store.Viewer, CreatedSeq: store.nextSeq()}
				store.NextCommentID++
				issue.Comments = append(issue.Comments, comment)
				issue.UpdatedSeq = store.Seq
				delete(repo.IssueJSON, issue.Number)
				return sendJSON(201, commentShape(store, repo, issue, comment), nil)
			}
			return sendError(404, "Not Found")
		}
		if len(tail) == 2 {
			if method == "GET" {
				if cached, ok := repo.IssueJSON[issue.Number]; ok {
					return &Response{Status: 200, ContentType: "application/json", Body: cached, Extra: map[string]string{"X-GitHub-Request-Id": requestID()}}
				}
				body := marshalJSON(issueShapeStruct(store, repo, issue))
				repo.IssueJSON[issue.Number] = body
				return &Response{Status: 200, ContentType: "application/json", Body: body, Extra: map[string]string{"X-GitHub-Request-Id": requestID()}}
			}
			if method == "PATCH" {
				if v, ok := input["title"].(string); ok {
					issue.Title = v
				}
				if v, ok := input["body"].(string); ok {
					issue.Body = v
				}
				if st, ok := input["state"].(string); ok && (st == "open" || st == "closed") {
					issue.State = st
				}
				if _, has := input["labels"]; has {
					labelNames := asStringArray(input["labels"])
					var unknown []string
					for _, name := range labelNames {
						found := false
						for _, l := range repo.Labels {
							if l.Name == name {
								found = true
								break
							}
						}
						if !found {
							unknown = append(unknown, name)
						}
					}
					if len(unknown) > 0 {
						return sendError(422, "Validation Failed: label \""+unknown[0]+"\" does not exist")
					}
					var labels []*Label
					for _, name := range labelNames {
						labels = append(labels, store.getOrCreateLabel(repo, name))
					}
					issue.Labels = labels
				}
				issue.UpdatedSeq = store.nextSeq()
				delete(repo.IssueJSON, issue.Number)
				return sendJSON(200, issueShapeStruct(store, repo, issue), nil)
			}
			return sendError(404, "Not Found")
		}
		return sendError(404, "Not Found")
	}

	// ---- labels ----
	if resource == "labels" {
		if len(tail) == 1 {
			if method == "GET" {
				page, perPage := parsePage(query)
				start, end, totalPages := paginateLen(len(repo.Labels), page, perPage)
				extra := map[string]string{}
				if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
					extra["Link"] = link
				}
				out := make([]interface{}, 0, end-start)
				for _, l := range repo.Labels[start:end] {
					out = append(out, labelShapeStruct(l))
				}
				return sendJSON(200, out, extra)
			}
			if method == "POST" {
				name := asString(input["name"])
				if name == "" {
					return sendError(422, "Validation Failed")
				}
				for _, l := range repo.Labels {
					if l.Name == name {
						return sendJSON(201, labelShapeStruct(l), nil)
					}
				}
				label := &Label{ID: store.NextLabelID, Name: name, Color: asString(input["color"]), Description: asString(input["description"])}
				if label.Color == "" {
					label.Color = "ededed"
				}
				store.NextLabelID++
				repo.Labels = append(repo.Labels, label)
				return sendJSON(201, labelShapeStruct(label), nil)
			}
			return sendError(404, "Not Found")
		}
		if len(tail) == 2 {
			name := tail[1]
			var label *Label
			for _, l := range repo.Labels {
				if l.Name == name {
					label = l
					break
				}
			}
			if label == nil {
				return sendError(404, "Not Found")
			}
			if method == "GET" {
				return sendJSON(200, labelShapeStruct(label), nil)
			}
			if method == "PATCH" {
				if v, ok := input["name"].(string); ok && v != "" {
					label.Name = v
				}
				if v, ok := input["color"].(string); ok {
					label.Color = v
				}
				if v, ok := input["description"].(string); ok {
					label.Description = v
				}
				return sendJSON(200, labelShapeStruct(label), nil)
			}
			if method == "DELETE" {
				var kept []*Label
				for _, l := range repo.Labels {
					if l != label {
						kept = append(kept, l)
					}
				}
				repo.Labels = kept
				for _, issue := range repo.Issues {
					var keptLabels []*Label
					for _, l := range issue.Labels {
						if l != label {
							keptLabels = append(keptLabels, l)
						}
					}
					issue.Labels = keptLabels
					delete(repo.IssueJSON, issue.Number)
				}
				return sendNoContent(204)
			}
			return sendError(404, "Not Found")
		}
		return sendError(404, "Not Found")
	}

	// ---- pulls ----
	if resource == "pulls" {
		if len(tail) == 1 {
			if method == "GET" {
				page, perPage := parsePage(query)
				state := query.Get("state")
				if state == "" {
					state = "open"
				}
				var items []*Pull
				for _, p := range repo.Pulls {
					if state != "all" && p.State != state {
						continue
					}
					items = append(items, p)
				}
				sortParam := query.Get("sort")
				if sortParam == "" {
					sortParam = "created"
				}
				direction := query.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				key := func(p *Pull) int {
					if sortParam == "updated" {
						return p.UpdatedSeq
					}
					return p.CreatedSeq
				}
				sort.Slice(items, func(a, b int) bool {
					if direction == "asc" {
						return key(items[a]) < key(items[b])
					}
					return key(items[a]) > key(items[b])
				})
				start, end, totalPages := paginateLen(len(items), page, perPage)
				extra := map[string]string{}
				if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
					extra["Link"] = link
				}
				out := make([]interface{}, 0, end-start)
				for _, p := range items[start:end] {
					out = append(out, pullShape(store, repo, p))
				}
				return sendJSON(200, out, extra)
			}
			if method == "POST" {
				title := asString(input["title"])
				head := stripOwnerPrefix(asString(input["head"]))
				base := asString(input["base"])
				if title == "" || head == "" || base == "" {
					return sendError(422, "Validation Failed")
				}
				pull := store.createPull(repo, title, asString(input["body"]), head, base)
				if pull == nil {
					return sendError(422, "Validation Failed: head or base ref not found")
				}
				return sendJSON(201, pullShape(store, repo, pull), nil)
			}
			return sendError(404, "Not Found")
		}
		number, err := strconv.Atoi(tail[1])
		if err != nil {
			return sendError(404, "Not Found")
		}
		var pull *Pull
		for _, p := range repo.Pulls {
			if p.Number == number {
				pull = p
				break
			}
		}
		if pull == nil {
			return sendError(404, "Not Found")
		}
		if len(tail) == 2 {
			if method == "GET" {
				return sendJSON(200, pullShape(store, repo, pull), nil)
			}
			if method == "PATCH" {
				if v, ok := input["title"].(string); ok {
					pull.Title = v
				}
				if v, ok := input["body"].(string); ok {
					pull.Body = v
				}
				if st, ok := input["state"].(string); ok && (st == "open" || st == "closed") {
					pull.State = st
				}
				pull.UpdatedSeq = store.nextSeq()
				return sendJSON(200, pullShape(store, repo, pull), nil)
			}
			return sendError(404, "Not Found")
		}
		if len(tail) == 3 && tail[2] == "files" && method == "GET" {
			baseTip := store.resolveRef(repo, pull.BaseRef)
			var baseCommit *GitObject
			if baseTip != "" {
				if o, ok := repo.Objects[baseTip]; ok {
					baseCommit = &o
				}
			}
			var headCommit *GitObject
			if o, ok := repo.Objects[pull.HeadSha]; ok {
				headCommit = &o
			}
			if baseCommit == nil || baseCommit.Type != ObjCommit || headCommit == nil || headCommit.Type != ObjCommit {
				return sendError(422, "Validation Failed")
			}
			baseC, _ := parseCommit(baseCommit.Data)
			headC, _ := parseCommit(headCommit.Data)
			files := store.diffTrees(repo, baseC.Tree, headC.Tree)
			page, perPage := parsePage(query)
			start, end, totalPages := paginateLen(len(files), page, perPage)
			extra := map[string]string{}
			if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
				extra["Link"] = link
			}
			out := make([]interface{}, 0, end-start)
			for _, f := range files[start:end] {
				out = append(out, map[string]interface{}{
					"filename": f.Filename, "status": f.Status,
					"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
					"patch": "", "sha": "", "blob_url": "", "raw_url": "",
				})
			}
			return sendJSON(200, out, extra)
		}
		if len(tail) == 3 && tail[2] == "merge" {
			if method == "GET" {
				if pull.Merged {
					return sendNoContent(204)
				}
				return sendError(404, "Not Found")
			}
			if method == "PUT" {
				if pull.Merged {
					return sendError(405, "Pull Request is already merged")
				}
				if pull.State != "open" {
					return sendError(405, "Pull Request is closed")
				}
				if !store.mergeable(pull, repo) {
					return sendError(405, "Merge conflict")
				}
				sha := store.mergePull(repo, pull)
				if sha == "" {
					return sendError(405, "Merge conflict")
				}
				return sendJSON(200, map[string]interface{}{"merged": true, "message": "Pull Request successfully merged", "sha": sha}, nil)
			}
			return sendError(404, "Not Found")
		}
		if len(tail) == 3 && tail[2] == "commits" && method == "GET" {
			commits := store.commitsFrom(repo, pull.HeadSha)
			baseTip := store.resolveRef(repo, pull.BaseRef)
			baseSet := map[string]bool{}
			for _, c := range store.commitsFrom(repo, baseTip) {
				baseSet[c.OID] = true
			}
			var out []interface{}
			for _, c := range commits {
				if baseSet[c.OID] {
					continue
				}
				out = append(out, commitShape(store, repo, c.OID, c.Commit))
			}
			if out == nil {
				out = []interface{}{}
			}
			return sendJSON(200, out, nil)
		}
		return sendError(404, "Not Found")
	}

	// ---- commits ----
	if resource == "commits" {
		if len(tail) == 1 && method == "GET" {
			page, perPage := parsePage(query)
			sha := query.Get("sha")
			if sha == "" {
				sha = repo.DefaultBranch
			}
			commits := store.commitsFrom(repo, sha)
			start, end, totalPages := paginateLen(len(commits), page, perPage)
			extra := map[string]string{}
			if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
				extra["Link"] = link
			}
			out := make([]interface{}, 0, end-start)
			for _, c := range commits[start:end] {
				out = append(out, commitShape(store, repo, c.OID, c.Commit))
			}
			return sendJSON(200, out, extra)
		}
		if len(tail) == 2 && method == "GET" {
			ref := tail[1]
			oid := store.resolveRef(repo, ref)
			if oid == "" {
				return sendError(404, "Not Found")
			}
			object, ok := repo.Objects[oid]
			if !ok || object.Type != ObjCommit {
				return sendError(404, "Not Found")
			}
			commit, err := parseCommit(object.Data)
			if err != nil {
				return sendError(404, "Not Found")
			}
			return sendJSON(200, commitShape(store, repo, oid, commit), nil)
		}
		return sendError(404, "Not Found")
	}

	// ---- git refs ----
	if resource == "git" && len(tail) >= 2 && tail[1] == "refs" {
		if len(tail) == 2 && method == "POST" {
			ref := asString(input["ref"])
			sha := asString(input["sha"])
			if !strings.HasPrefix(ref, "refs/") || !isObjectID(sha) {
				return sendError(422, "Validation Failed")
			}
			if _, exists := repo.Refs[ref]; exists {
				return sendError(422, "Validation Failed: ref already exists")
			}
			object, ok := repo.Objects[sha]
			if !ok {
				return sendError(422, "Validation Failed: sha not found")
			}
			if strings.HasPrefix(ref, "refs/heads/") && object.Type != ObjCommit {
				return sendError(422, "Validation Failed: branch ref must point at a commit")
			}
			repo.Refs[ref] = sha
			store.invalidateGitCaches(repo)
			return sendJSON(201, refShape(store, repo, ref, sha, object.Type), nil)
		}
		if len(tail) >= 3 {
			refPath := strings.Join(tail[2:], "/")
			fullRef := "refs/" + refPath
			if method == "GET" {
				oid, ok := repo.Refs[fullRef]
				if !ok {
					return sendError(404, "Not Found")
				}
				object, hasObj := repo.Objects[oid]
				typ := ObjCommit
				if hasObj {
					typ = object.Type
				}
				return sendJSON(200, refShape(store, repo, fullRef, oid, typ), nil)
			}
			if method == "PATCH" {
				oid, ok := repo.Refs[fullRef]
				if !ok {
					return sendError(404, "Not Found")
				}
				sha := asString(input["sha"])
				force := input["force"] == true
				object, hasObj := repo.Objects[sha]
				if !isObjectID(sha) || !hasObj {
					return sendError(422, "Validation Failed: sha not found")
				}
				if !force {
					lookup := func(id string) *GitObject {
						if o, ok := repo.Objects[id]; ok {
							oo := o
							return &oo
						}
						return nil
					}
					if !isAncestor(oid, sha, lookup) {
						return sendError(422, "Validation Failed: not a fast-forward")
					}
				}
				repo.Refs[fullRef] = sha
				store.invalidateGitCaches(repo)
				return sendJSON(200, refShape(store, repo, fullRef, sha, object.Type), nil)
			}
			if method == "DELETE" {
				if _, ok := repo.Refs[fullRef]; !ok {
					return sendError(404, "Not Found")
				}
				delete(repo.Refs, fullRef)
				store.invalidateGitCaches(repo)
				return sendNoContent(204)
			}
		}
		return sendError(404, "Not Found")
	}

	// ---- branches ----
	if resource == "branches" && len(tail) == 1 && method == "GET" {
		page, perPage := parsePage(query)
		type branch struct {
			name string
			oid  string
		}
		var branches []branch
		for name, oid := range repo.Refs {
			if strings.HasPrefix(name, "refs/heads/") {
				branches = append(branches, branch{name[len("refs/heads/"):], oid})
			}
		}
		sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
		start, end, totalPages := paginateLen(len(branches), page, perPage)
		extra := map[string]string{}
		if link := linkHeader(req, path, query, page, perPage, totalPages); link != "" {
			extra["Link"] = link
		}
		out := make([]interface{}, 0, end-start)
		for _, b := range branches[start:end] {
			out = append(out, map[string]interface{}{
				"name": b.name,
				"commit": map[string]interface{}{
					"sha": b.oid,
					"url": "/repos/" + owner + "/" + repo.Name + "/commits/" + b.oid,
				},
				"protected": false,
			})
		}
		return sendJSON(200, out, extra)
	}

	// ---- contents ----
	if resource == "contents" {
		filePath := strings.Join(tail[1:], "/")
		if filePath == "" {
			return sendError(404, "Not Found")
		}
		branch := asString(input["branch"])
		if branch == "" {
			branch = query.Get("ref")
		}
		if branch == "" {
			branch = repo.DefaultBranch
		}
		if method == "GET" {
			tip := store.resolveRef(repo, branch)
			if tip == "" {
				return sendError(404, "Not Found")
			}
			commitObj, ok := repo.Objects[tip]
			if !ok || commitObj.Type != ObjCommit {
				return sendError(404, "Not Found")
			}
			commit, err := parseCommit(commitObj.Data)
			if err != nil {
				return sendError(404, "Not Found")
			}
			if entry := store.blobAtPath(repo, commit.Tree, filePath); entry != nil {
				blob, ok := repo.Objects[entry.OID]
				if !ok {
					return sendError(404, "Not Found")
				}
				return sendJSON(200, contentShape(store, repo, filePath, entry.OID, blob.Data), nil)
			}
			// A directory path lists its entries, GitHub-style.
			if dirTreeOID := treeAtPath(repo, commit.Tree, filePath); dirTreeOID != "" {
				dirObject, ok := repo.Objects[dirTreeOID]
				if ok && dirObject.Type == ObjTree {
					entries, _ := parseTree(dirObject.Data)
					out := make([]interface{}, 0, len(entries))
					for _, e := range entries {
						childPath := filePath + "/" + e.Name
						isDir := e.Mode == "40000"
						var size int
						if !isDir {
							if child, ok := repo.Objects[e.OID]; ok {
								size = len(child.Data)
							}
						}
						var encoding interface{}
						if !isDir {
							encoding = "base64"
						}
						var downloadURL interface{}
						if !isDir {
							downloadURL = "https://raw.githubusercontent.com/" + owner + "/" + repo.Name + "/main/" + childPath
						}
						typ := "file"
						gitKind := "blobs"
						if isDir {
							typ = "dir"
							gitKind = "trees"
						}
						out = append(out, map[string]interface{}{
							"type": typ, "encoding": encoding, "size": size,
							"name": e.Name, "path": childPath, "content": nil,
							"sha": e.OID,
							"url": "/repos/" + owner + "/" + repo.Name + "/contents/" + childPath,
							"html_url": "https://github.com/" + owner + "/" + repo.Name + "/blob/main/" + childPath,
							"git_url": "/repos/" + owner + "/" + repo.Name + "/git/" + gitKind + "/" + e.OID,
							"download_url": downloadURL,
							"_links": map[string]interface{}{
								"self": "/repos/" + owner + "/" + repo.Name + "/contents/" + childPath,
								"git":  "/repos/" + owner + "/" + repo.Name + "/git/" + gitKind + "/" + e.OID,
								"html": "https://github.com/" + owner + "/" + repo.Name + "/blob/main/" + childPath,
							},
						})
					}
					return sendJSON(200, out, nil)
				}
			}
			return sendError(404, "Not Found")
		}
		if method == "PUT" {
			message := asString(input["message"])
			contentB64 := asString(input["content"])
			sha := asString(input["sha"])
			if message == "" || contentB64 == "" {
				return sendError(422, "Validation Failed")
			}
			data, err := base64.StdEncoding.DecodeString(contentB64)
			if err != nil {
				return sendError(422, "Validation Failed")
			}
			tip := store.resolveRef(repo, branch)
			files := map[string]commitFile{}
			isUpdate := false
			if tip != "" {
				if commitObj, ok := repo.Objects[tip]; ok && commitObj.Type == ObjCommit {
					if commit, err := parseCommit(commitObj.Data); err == nil {
						files = store.treeFileMap(repo, commit.Tree)
					}
				}
				if existing, ok := files[filePath]; ok {
					isUpdate = true
					if sha != "" && sha != hashBlob(existing.Data) {
						return sendError(409, "Conflict: sha does not match")
					}
				} else if sha != "" {
					return sendError(422, "Validation Failed: sha given for a new file")
				}
			}
			files[filePath] = commitFile{Data: data, Mode: "100644"}
			commitOID := store.commitFiles(repo, files, message, tip)
			repo.Refs["refs/heads/"+branch] = commitOID
			store.invalidateGitCaches(repo)
			blobSha := hashBlob(data)
			status := 201
			if isUpdate {
				status = 200
			}
			return sendJSON(status, map[string]interface{}{
				"content": contentShape(store, repo, filePath, blobSha, data),
				"commit": map[string]interface{}{
					"sha": commitOID, "message": message,
					"author":    map[string]interface{}{"name": "Conformance", "email": "conformance@make.fun"},
					"committer": map[string]interface{}{"name": "Conformance", "email": "conformance@make.fun"},
				},
			}, nil)
		}
		if method == "DELETE" {
			message := asString(input["message"])
			sha := asString(input["sha"])
			if message == "" || sha == "" {
				return sendError(422, "Validation Failed")
			}
			tip := store.resolveRef(repo, branch)
			if tip == "" {
				return sendError(404, "Not Found")
			}
			commitObj, ok := repo.Objects[tip]
			if !ok || commitObj.Type != ObjCommit {
				return sendError(404, "Not Found")
			}
			commit, err := parseCommit(commitObj.Data)
			if err != nil {
				return sendError(404, "Not Found")
			}
			files := store.treeFileMap(repo, commit.Tree)
			existing, ok := files[filePath]
			if !ok {
				return sendError(404, "Not Found")
			}
			if sha != hashBlob(existing.Data) {
				return sendError(409, "Conflict: sha does not match")
			}
			delete(files, filePath)
			commitOID := store.commitFiles(repo, files, message, tip)
			repo.Refs["refs/heads/"+branch] = commitOID
			store.invalidateGitCaches(repo)
			return sendJSON(200, map[string]interface{}{
				"commit": map[string]interface{}{
					"sha": commitOID, "message": message,
					"author":    map[string]interface{}{"name": "Conformance", "email": "conformance@make.fun"},
					"committer": map[string]interface{}{"name": "Conformance", "email": "conformance@make.fun"},
				},
			}, nil)
		}
		return sendError(404, "Not Found")
	}

	return sendError(404, "Not Found")
}

func stripOwnerPrefix(head string) string {
	if idx := strings.Index(head, ":"); idx >= 0 {
		return head[idx+1:]
	}
	return head
}

func commentShape(store *Store, repo *Repo, issue *Issue, c *Comment) map[string]interface{} {
	owner := store.repoOwner(repo)
	user := c.User
	if user == "" {
		user = owner
	}
	return map[string]interface{}{
		"id": c.ID, "node_id": b64("IssueComment:" + itoa(c.ID)),
		"body": c.Body, "user": userShape(user),
		"created_at": tsFor(c.CreatedSeq),
		"updated_at": tsFor(c.CreatedSeq),
		"html_url": "https://github.com/" + owner + "/" + repo.Name + "/issues/" + itoa(issue.Number) + "#issuecomment-" + itoa(c.ID),
		"url": "/repos/" + owner + "/" + repo.Name + "/issues/comments/" + itoa(c.ID),
		"issue_url": "/repos/" + owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
	}
}

func refShape(store *Store, repo *Repo, ref, oid string, typ ObjectType) map[string]interface{} {
	owner := store.repoOwner(repo)
	refPath := strings.TrimPrefix(ref, "refs/")
	return map[string]interface{}{
		"ref": ref, "node_id": b64("Ref:" + ref),
		"url": "/repos/" + owner + "/" + repo.Name + "/git/refs/" + refPath,
		"object": map[string]interface{}{
			"sha": oid, "type": string(typ),
			"url": "/repos/" + owner + "/" + repo.Name + "/git/" + string(typ) + "s/" + oid,
		},
	}
}

// treeAtPath is the tree oid at a directory path inside a root tree, or "".
func treeAtPath(repo *Repo, rootTreeOID, path string) string {
	parts := strings.Split(path, "/")
	var filtered []string
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	current := rootTreeOID
	for _, part := range filtered {
		object, ok := repo.Objects[current]
		if !ok || object.Type != ObjTree {
			return ""
		}
		entries, err := parseTree(object.Data)
		if err != nil {
			return ""
		}
		var found string
		for _, e := range entries {
			if e.Name == part && e.Mode == "40000" {
				found = e.OID
				break
			}
		}
		if found == "" {
			return ""
		}
		current = found
	}
	return current
}

func commitShape(store *Store, repo *Repo, oid string, commit CommitView) map[string]interface{} {
	owner := store.repoOwner(repo)
	authorName := commit.Author
	if idx := strings.Index(authorName, " <"); idx >= 0 {
		authorName = authorName[:idx]
	}
	committerName := commit.Committer
	if idx := strings.Index(committerName, " <"); idx >= 0 {
		committerName = committerName[:idx]
	}
	parents := make([]interface{}, 0, len(commit.Parents))
	for _, p := range commit.Parents {
		parents = append(parents, map[string]interface{}{
			"sha": p, "url": "/repos/" + owner + "/" + repo.Name + "/commits/" + p,
		})
	}
	return map[string]interface{}{
		"sha": oid, "node_id": b64("Commit:" + oid),
		"commit": map[string]interface{}{
			"author": map[string]interface{}{
				"name": authorName, "email": "conformance@make.fun", "date": tsFor(0),
			},
			"committer": map[string]interface{}{
				"name": committerName, "email": "conformance@make.fun", "date": tsFor(0),
			},
			"message": commit.Message,
			"tree":    map[string]interface{}{"sha": commit.Tree},
			"url":     "/repos/" + owner + "/" + repo.Name + "/git/commits/" + oid,
			"comment_count": 0,
		},
		"author":    userShape(owner),
		"committer": userShape(owner),
		"parents":   parents,
		"html_url":  "https://github.com/" + owner + "/" + repo.Name + "/commit/" + oid,
		"url":       "/repos/" + owner + "/" + repo.Name + "/commits/" + oid,
	}
}

func contentShape(store *Store, repo *Repo, filePath, sha string, data []byte) map[string]interface{} {
	owner := store.repoOwner(repo)
	name := filePath
	if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
		name = filePath[idx+1:]
	}
	return map[string]interface{}{
		"type": "file", "encoding": "base64", "size": len(data),
		"name": name, "path": filePath,
		"content": base64.StdEncoding.EncodeToString(data),
		"sha":     sha,
		"url":     "/repos/" + owner + "/" + repo.Name + "/contents/" + filePath,
		"html_url": "https://github.com/" + owner + "/" + repo.Name + "/blob/main/" + filePath,
		"git_url": "/repos/" + owner + "/" + repo.Name + "/git/blobs/" + sha,
		"download_url": "https://raw.githubusercontent.com/" + owner + "/" + repo.Name + "/main/" + filePath,
		"_links": map[string]interface{}{
			"self": "/repos/" + owner + "/" + repo.Name + "/contents/" + filePath,
			"git":  "/repos/" + owner + "/" + repo.Name + "/git/blobs/" + sha,
			"html": "https://github.com/" + owner + "/" + repo.Name + "/blob/main/" + filePath,
		},
	}
}

func hashBlob(data []byte) string {
	return hashObject(ObjBlob, data)
}

// sortStrings sorts a slice of strings in place.
func sortStrings(s []string) { sort.Strings(s) }
