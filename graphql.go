// graphql.go — a hand-rolled GraphQL executor for the forge: a tokenizer and
// recursive-descent parser for the query/mutation subset the conformance
// client sends, an executor over the store, and the introspection schema
// (Query.__type / __schema) that gh's feature detection probes.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// tokenizer
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokName tokenKind = iota
	tokString
	tokInt
	tokFloat
	tokPunct
	tokSpread
	tokEOF
)

type token struct {
	kind  tokenKind
	value string
}

func isNameStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}
func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

func tokenize(source string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(source) {
		c := source[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.' {
			tokens = append(tokens, token{tokSpread, "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}():!$[]=|&", c) >= 0 {
			tokens = append(tokens, token{tokPunct, string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var sb strings.Builder
			for j < len(source) && source[j] != '"' {
				if source[j] == '\\' && j+1 < len(source) {
					esc := source[j+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(esc)
					}
					j += 2
				} else {
					sb.WriteByte(source[j])
					j++
				}
			}
			tokens = append(tokens, token{tokString, sb.String()})
			i = j + 1
			continue
		}
		if c >= '0' && c <= '9' || c == '-' {
			j := i
			for j < len(source) && (source[j] >= '0' && source[j] <= '9' || source[j] == '.' || source[j] == 'e' || source[j] == 'E' || source[j] == '+' || source[j] == '-') {
				j++
			}
			text := source[i:j]
			kind := tokInt
			if strings.ContainsAny(text, ".eE") {
				kind = tokFloat
			}
			tokens = append(tokens, token{kind, text})
			i = j
			continue
		}
		if isNameStart(c) {
			j := i
			for j < len(source) && isNameChar(source[j]) {
				j++
			}
			tokens = append(tokens, token{tokName, source[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %q at %d", string(c), i)
	}
	tokens = append(tokens, token{tokEOF, ""})
	return tokens, nil
}

// ---------------------------------------------------------------------------
// parser (AST)
// ---------------------------------------------------------------------------

type valueNode struct {
	kind   string // "variable","string","int","float","boolean","null","enum","list","object"
	name   string
	str    string
	num    float64
	boolean bool
	values []valueNode
	fields []struct {
		name  string
		value valueNode
	}
}

type fieldNode struct {
	alias     string
	hasAlias  bool
	name      string
	args      []struct {
		name  string
		value valueNode
	}
	selection []selectionNode
}

type selectionNode struct {
	kind          string // "field" | "fragment"
	field         *fieldNode
	typeCondition string
	selection     []selectionNode
}

type operationNode struct {
	operation string // "query" | "mutation"
	name      string
	variables []struct {
		name         string
		typ          string
		defaultValue *valueNode
	}
	selection []selectionNode
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token { return p.tokens[p.pos] }
func (p *parser) next() token {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *parser) expectPunct(value string) error {
	t := p.next()
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("expected '%s' but got %d %q", value, t.kind, t.value)
	}
	return nil
}

func (p *parser) expectName(value ...string) (string, error) {
	t := p.next()
	if t.kind != tokName {
		return "", fmt.Errorf("expected name but got %d %q", t.kind, t.value)
	}
	if len(value) > 0 && t.value != value[0] {
		return "", fmt.Errorf("expected name %s but got %q", value[0], t.value)
	}
	return t.value, nil
}

func (p *parser) parseDocument() ([]*operationNode, error) {
	var operations []*operationNode
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind == tokPunct && t.value == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			operations = append(operations, &operationNode{operation: "query", selection: sel})
			continue
		}
		op, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if op != "query" && op != "mutation" && op != "subscription" {
			return nil, fmt.Errorf("unsupported operation %s", op)
		}
		var name string
		if p.peek().kind == tokName {
			name = p.next().value
		}
		var variables []struct {
			name         string
			typ          string
			defaultValue *valueNode
		}
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			p.next()
			for !(p.peek().kind == tokPunct && p.peek().value == ")") {
				if err := p.expectPunct("$"); err != nil {
					return nil, err
				}
				varName, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				typ, err := p.parseType()
				if err != nil {
					return nil, err
				}
				var defaultValue *valueNode
				if p.peek().kind == tokPunct && p.peek().value == "=" {
					p.next()
					dv, err := p.parseValue()
					if err != nil {
						return nil, err
					}
					defaultValue = &dv
				}
				variables = append(variables, struct {
					name         string
					typ          string
					defaultValue *valueNode
				}{varName, typ, defaultValue})
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		operations = append(operations, &operationNode{operation: op, name: name, variables: variables, selection: sel})
	}
	return operations, nil
}

func (p *parser) parseType() (string, error) {
	var typ string
	if p.peek().kind == tokPunct && p.peek().value == "[" {
		p.next()
		inner, err := p.parseType()
		if err != nil {
			return "", err
		}
		typ = "[" + inner + "]"
	} else {
		name, err := p.expectName()
		if err != nil {
			return "", err
		}
		typ = name
	}
	if p.peek().kind == tokPunct && p.peek().value == "!" {
		p.next()
		typ += "!"
	}
	return typ, nil
}

func (p *parser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []selectionNode
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		if p.peek().kind == tokSpread {
			p.next()
			var typeCondition string
			if p.peek().kind == tokName && p.peek().value == "on" {
				p.next()
				tc, err := p.expectName()
				if err != nil {
					return nil, err
				}
				typeCondition = tc
			} else if p.peek().kind == tokName {
				return nil, fmt.Errorf("named fragment spreads are not supported")
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			selections = append(selections, selectionNode{kind: "fragment", typeCondition: typeCondition, selection: sel})
			continue
		}
		first, err := p.expectName()
		if err != nil {
			return nil, err
		}
		alias, name := "", first
		hasAlias := false
		if p.peek().kind == tokPunct && p.peek().value == ":" {
			p.next()
			alias = first
			hasAlias = true
			name, err = p.expectName()
			if err != nil {
				return nil, err
			}
		}
		var args []struct {
			name  string
			value valueNode
		}
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			p.next()
			for !(p.peek().kind == tokPunct && p.peek().value == ")") {
				argName, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				args = append(args, struct {
					name  string
					value valueNode
				}{argName, v})
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
		}
		var selection []selectionNode
		if p.peek().kind == tokPunct && p.peek().value == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			selection = sel
		}
		selections = append(selections, selectionNode{kind: "field", field: &fieldNode{alias: alias, hasAlias: hasAlias, name: name, args: args, selection: selection}})
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *parser) parseValue() (valueNode, error) {
	t := p.next()
	switch {
	case t.kind == tokPunct && t.value == "$":
		name, err := p.expectName()
		if err != nil {
			return valueNode{}, err
		}
		return valueNode{kind: "variable", name: name}, nil
	case t.kind == tokString:
		return valueNode{kind: "string", str: t.value}, nil
	case t.kind == tokInt:
		n, _ := strconv.ParseFloat(t.value, 64)
		return valueNode{kind: "int", num: n}, nil
	case t.kind == tokFloat:
		n, _ := strconv.ParseFloat(t.value, 64)
		return valueNode{kind: "float", num: n}, nil
	case t.kind == tokPunct && t.value == "[":
		var values []valueNode
		for !(p.peek().kind == tokPunct && p.peek().value == "]") {
			v, err := p.parseValue()
			if err != nil {
				return valueNode{}, err
			}
			values = append(values, v)
		}
		if err := p.expectPunct("]"); err != nil {
			return valueNode{}, err
		}
		return valueNode{kind: "list", values: values}, nil
	case t.kind == tokPunct && t.value == "{":
		var fields []struct {
			name  string
			value valueNode
		}
		for !(p.peek().kind == tokPunct && p.peek().value == "}") {
			name, err := p.expectName()
			if err != nil {
				return valueNode{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return valueNode{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return valueNode{}, err
			}
			fields = append(fields, struct {
				name  string
				value valueNode
			}{name, v})
		}
		if err := p.expectPunct("}"); err != nil {
			return valueNode{}, err
		}
		return valueNode{kind: "object", fields: fields}, nil
	case t.kind == tokName:
		switch t.value {
		case "true":
			return valueNode{kind: "boolean", boolean: true}, nil
		case "false":
			return valueNode{kind: "boolean", boolean: false}, nil
		case "null":
			return valueNode{kind: "null"}, nil
		default:
			return valueNode{kind: "enum", str: t.value}, nil
		}
	}
	return valueNode{}, fmt.Errorf("unexpected value token %d %q", t.kind, t.value)
}

// ---------------------------------------------------------------------------
// the introspection schema
// ---------------------------------------------------------------------------

type fieldDef struct {
	name string
	typ  string
	args []fieldDef
}

type typeDef struct {
	name        string
	kind        string // OBJECT, ENUM, INPUT_OBJECT, SCALAR, INTERFACE, UNION
	fields      []fieldDef
	enumValues  []string
	inputFields []fieldDef
	description string
}

func f(name, typ string, args ...fieldDef) fieldDef { return fieldDef{name: name, typ: typ, args: args} }

var gqlSchema = []typeDef{
	{name: "Query", kind: "OBJECT", fields: []fieldDef{
		f("viewer", "User"),
		f("repository", "Repository", f("owner", "String!"), f("name", "String!")),
		f("node", "Node", f("id", "ID!")),
		f("__schema", "__Schema!"),
		f("__type", "__Type", f("name", "String!")),
	}},
	{name: "Mutation", kind: "OBJECT", fields: []fieldDef{
		f("createIssue", "CreateIssuePayload", f("input", "CreateIssueInput!")),
		f("closeIssue", "CloseIssuePayload", f("input", "CloseIssueInput!")),
		f("createPullRequest", "CreatePullRequestPayload", f("input", "CreatePullRequestInput!")),
		f("mergePullRequest", "MergePullRequestPayload", f("input", "MergePullRequestInput!")),
		f("addComment", "AddCommentPayload", f("input", "AddCommentInput!")),
	}},
	{name: "Repository", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("databaseId", "Int"), f("name", "String!"), f("nameWithOwner", "String!"),
		f("description", "String"), f("isPrivate", "Boolean!"), f("isFork", "Boolean!"),
		f("owner", "RepositoryOwner!"), f("defaultBranchRef", "Ref"), f("viewerPermission", "RepositoryPermission"),
		f("hasIssuesEnabled", "Boolean!"), f("hasProjectsEnabled", "Boolean!"), f("hasWikiEnabled", "Boolean!"),
		f("issues", "IssueConnection!", f("first", "Int"), f("after", "String"), f("states", "[IssueState!]"), f("orderBy", "IssueOrder")),
		f("issue", "Issue", f("number", "Int!")),
		f("pullRequests", "PullRequestConnection!", f("first", "Int"), f("after", "String"), f("states", "[PullRequestState!]"), f("orderBy", "IssueOrder")),
		f("pullRequest", "PullRequest", f("number", "Int!")),
		f("labels", "LabelConnection", f("first", "Int"), f("after", "String")),
		f("createdAt", "DateTime!"), f("updatedAt", "DateTime!"), f("pushedAt", "DateTime"),
		f("url", "URI!"), f("sshUrl", "URI!"), f("forkCount", "Int!"), f("stargazerCount", "Int!"),
		f("watchers", "UserConnection!"), f("primaryLanguage", "Language"), f("licenseInfo", "License"), f("diskUsage", "Int"),
	}},
	{name: "Issue", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("databaseId", "Int"), f("number", "Int!"), f("title", "String!"),
		f("body", "String"), f("bodyText", "String!"), f("state", "IssueState!"), f("stateReason", "IssueStateReason"),
		f("author", "Actor"),
		f("labels", "LabelConnection", f("first", "Int"), f("after", "String")),
		f("comments", "IssueCommentConnection!", f("first", "Int"), f("after", "String")),
		f("createdAt", "DateTime!"), f("updatedAt", "DateTime!"), f("closedAt", "DateTime"),
		f("url", "URI!"), f("repository", "Repository!"), f("locked", "Boolean!"), f("milestone", "Milestone"),
		f("assignees", "UserConnection!"), f("participants", "UserConnection!"), f("timelineItems", "IssueTimelineItemsConnection!"),
	}},
	{name: "PullRequest", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("databaseId", "Int"), f("number", "Int!"), f("title", "String!"),
		f("body", "String"), f("state", "PullRequestState!"), f("merged", "Boolean!"),
		f("mergeable", "MergeableState!"), f("mergeStateStatus", "MergeStateStatus"),
		f("headRefName", "String!"), f("baseRefName", "String!"), f("headRef", "Ref"), f("baseRef", "Ref"),
		f("author", "Actor"),
		f("labels", "LabelConnection", f("first", "Int"), f("after", "String")),
		f("files", "PullRequestFileConnection", f("first", "Int"), f("after", "String")),
		f("commits", "PullRequestCommitConnection!"), f("reviews", "PullRequestReviewConnection"),
		f("createdAt", "DateTime!"), f("updatedAt", "DateTime!"), f("closedAt", "DateTime"), f("mergedAt", "DateTime"),
		f("mergeCommit", "Commit"), f("url", "URI!"), f("repository", "Repository!"), f("isDraft", "Boolean!"),
		f("additions", "Int!"), f("deletions", "Int!"), f("changedFiles", "Int!"),
	}},
	{name: "User", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("databaseId", "Int"), f("login", "String!"), f("name", "String"),
		f("avatarUrl", "URI!"), f("url", "URI!"), f("email", "String"),
	}},
	{name: "Ref", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("name", "String!"), f("prefix", "String!"), f("target", "GitObject"), f("repository", "Repository!"),
	}},
	{name: "Commit", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("oid", "GitObjectID!"), f("message", "String!"), f("messageHeadline", "String!"),
		f("author", "GitActor"), f("committer", "GitActor"), f("tree", "Tree!"),
	}},
	{name: "GitActor", kind: "OBJECT", fields: []fieldDef{
		f("name", "String"), f("email", "String"), f("date", "DateTime"), f("user", "User"),
	}},
	{name: "Tree", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("oid", "GitObjectID!"), f("entries", "[TreeEntry!]!"),
	}},
	{name: "TreeEntry", kind: "OBJECT", fields: []fieldDef{
		f("name", "String!"), f("type", "String!"), f("oid", "GitObjectID!"), f("mode", "Int!"),
	}},
	{name: "Label", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("name", "String!"), f("color", "String!"), f("description", "String"),
		f("isDefault", "Boolean!"), f("url", "URI!"),
	}},
	{name: "IssueComment", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("databaseId", "Int"), f("body", "String!"), f("bodyText", "String!"),
		f("author", "Actor"), f("createdAt", "DateTime!"), f("updatedAt", "DateTime!"),
		f("url", "URI!"), f("issue", "Issue!"),
	}},
	{name: "PullRequestFile", kind: "OBJECT", fields: []fieldDef{
		f("path", "String!"), f("additions", "Int!"), f("deletions", "Int!"), f("changeType", "PatchStatus!"),
	}},
	{name: "IssueConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[Issue]"), f("edges", "[IssueEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "IssueEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "Issue"), f("cursor", "String!"),
	}},
	{name: "PullRequestConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[PullRequest]"), f("edges", "[PullRequestEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "PullRequestEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "PullRequest"), f("cursor", "String!"),
	}},
	{name: "LabelConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[Label]"), f("edges", "[LabelEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "LabelEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "Label"), f("cursor", "String!"),
	}},
	{name: "IssueCommentConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[IssueComment]"), f("edges", "[IssueCommentEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "IssueCommentEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "IssueComment"), f("cursor", "String!"),
	}},
	{name: "PullRequestFileConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[PullRequestFile]"), f("edges", "[PullRequestFileEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "PullRequestFileEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "PullRequestFile"), f("cursor", "String!"),
	}},
	{name: "PullRequestCommitConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[PullRequestCommit]"), f("edges", "[PullRequestCommitEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "PullRequestCommit", kind: "OBJECT", fields: []fieldDef{
		f("commit", "Commit!"),
	}},
	{name: "PullRequestCommitEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "PullRequestCommit"), f("cursor", "String!"),
	}},
	{name: "PullRequestReviewConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[PullRequestReview]"), f("edges", "[PullRequestReviewEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "PullRequestReview", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("state", "PullRequestReviewState!"), f("author", "Actor"), f("body", "String!"),
	}},
	{name: "PullRequestReviewEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "PullRequestReview"), f("cursor", "String!"),
	}},
	{name: "UserConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[User]"), f("edges", "[UserEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "UserEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "User"), f("cursor", "String!"),
	}},
	{name: "IssueTimelineItemsConnection", kind: "OBJECT", fields: []fieldDef{
		f("totalCount", "Int!"), f("nodes", "[IssueTimelineItems]"), f("edges", "[IssueTimelineItemsEdge]"), f("pageInfo", "PageInfo!"),
	}},
	{name: "IssueTimelineItemsEdge", kind: "OBJECT", fields: []fieldDef{
		f("node", "IssueTimelineItems"), f("cursor", "String!"),
	}},
	{name: "PageInfo", kind: "OBJECT", fields: []fieldDef{
		f("hasNextPage", "Boolean!"), f("hasPreviousPage", "Boolean!"), f("startCursor", "String"), f("endCursor", "String"),
	}},
	{name: "CreateIssuePayload", kind: "OBJECT", fields: []fieldDef{
		f("issue", "Issue"), f("clientMutationId", "String"),
	}},
	{name: "CloseIssuePayload", kind: "OBJECT", fields: []fieldDef{
		f("issue", "Issue"), f("clientMutationId", "String"),
	}},
	{name: "CreatePullRequestPayload", kind: "OBJECT", fields: []fieldDef{
		f("pullRequest", "PullRequest"), f("clientMutationId", "String"),
	}},
	{name: "MergePullRequestPayload", kind: "OBJECT", fields: []fieldDef{
		f("pullRequest", "PullRequest"), f("actor", "Actor"), f("clientMutationId", "String"),
	}},
	{name: "AddCommentPayload", kind: "OBJECT", fields: []fieldDef{
		f("commentEdge", "IssueCommentEdge"), f("subject", "Node"), f("clientMutationId", "String"),
	}},
	{name: "Milestone", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("number", "Int!"), f("title", "String!"),
	}},
	{name: "Language", kind: "OBJECT", fields: []fieldDef{
		f("id", "ID!"), f("name", "String!"),
	}},
	{name: "License", kind: "OBJECT", fields: []fieldDef{
		f("key", "String!"), f("name", "String!"), f("spdxId", "String"),
	}},
	{name: "Node", kind: "INTERFACE", fields: []fieldDef{f("id", "ID!")}},
	{name: "Actor", kind: "INTERFACE", fields: []fieldDef{
		f("id", "ID!"), f("login", "String!"), f("avatarUrl", "URI!"), f("url", "URI!"),
	}},
	{name: "RepositoryOwner", kind: "INTERFACE", fields: []fieldDef{
		f("id", "ID!"), f("login", "String!"), f("avatarUrl", "URI!"), f("url", "URI!"),
	}},
	{name: "GitObject", kind: "INTERFACE", fields: []fieldDef{
		f("id", "ID!"), f("oid", "GitObjectID!"),
	}},
	{name: "IssueTimelineItems", kind: "UNION"},
	{name: "IssueState", kind: "ENUM", enumValues: []string{"OPEN", "CLOSED"}},
	{name: "IssueStateReason", kind: "ENUM", enumValues: []string{"COMPLETED", "NOT_PLANNED", "REOPENED"}},
	{name: "PullRequestState", kind: "ENUM", enumValues: []string{"OPEN", "CLOSED", "MERGED"}},
	{name: "MergeableState", kind: "ENUM", enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	{name: "MergeStateStatus", kind: "ENUM", enumValues: []string{"BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE"}},
	{name: "RepositoryPermission", kind: "ENUM", enumValues: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	{name: "PatchStatus", kind: "ENUM", enumValues: []string{"ADDED", "CHANGED", "COPIED", "DELETED", "MODIFIED", "REMOVED", "RENAMED"}},
	{name: "PullRequestReviewState", kind: "ENUM", enumValues: []string{"APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED", "PENDING"}},
	{name: "SearchType", kind: "ENUM", enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "ORGANIZATION", "MARKETPLACE", "TOPIC"}},
	{name: "IssueOrderField", kind: "ENUM", enumValues: []string{"COMMENTS", "CREATED_AT", "UPDATED_AT"}},
	{name: "OrderDirection", kind: "ENUM", enumValues: []string{"ASC", "DESC"}},
	{name: "IssueOrder", kind: "INPUT_OBJECT", inputFields: []fieldDef{
		f("field", "IssueOrderField!"), f("direction", "OrderDirection!"),
	}},
	{name: "CreateIssueInput", kind: "INPUT_OBJECT", inputFields: []fieldDef{
		f("repositoryId", "ID!"), f("title", "String!"), f("body", "String"), f("clientMutationId", "String"),
	}},
	{name: "CloseIssueInput", kind: "INPUT_OBJECT", inputFields: []fieldDef{
		f("issueId", "ID!"), f("stateReason", "IssueStateReason"), f("clientMutationId", "String"),
	}},
	{name: "CreatePullRequestInput", kind: "INPUT_OBJECT", inputFields: []fieldDef{
		f("repositoryId", "ID!"), f("baseRefName", "String!"), f("headRefName", "String!"),
		f("title", "String!"), f("body", "String"), f("clientMutationId", "String"),
	}},
	{name: "MergePullRequestInput", kind: "INPUT_OBJECT", inputFields: []fieldDef{
		f("pullRequestId", "ID!"), f("mergeMethod", "PullRequestMergeMethod"), f("clientMutationId", "String"),
	}},
	{name: "AddCommentInput", kind: "INPUT_OBJECT", inputFields: []fieldDef{
		f("subjectId", "ID!"), f("body", "String!"), f("clientMutationId", "String"),
	}},
	{name: "PullRequestMergeMethod", kind: "ENUM", enumValues: []string{"MERGE", "SQUASH", "REBASE"}},
	{name: "__Schema", kind: "OBJECT", fields: []fieldDef{
		f("queryType", "__Type!"), f("mutationType", "__Type"), f("subscriptionType", "__Type"),
		f("types", "[__Type!]!"), f("directives", "[__Directive!]!"),
	}},
	{name: "__Type", kind: "OBJECT", fields: []fieldDef{
		f("name", "String"), f("kind", "__TypeKind!"), f("description", "String"),
		f("fields", "[__Field!]", f("includeDeprecated", "Boolean")),
		f("enumValues", "[__EnumValue!]", f("includeDeprecated", "Boolean")),
		f("inputFields", "[__InputValue!]"), f("interfaces", "[__Type!]"), f("possibleTypes", "[__Type!]"),
		f("ofType", "__Type"), f("specifiedByURL", "String"), f("isOneOf", "Boolean"),
	}},
	{name: "__Field", kind: "OBJECT", fields: []fieldDef{
		f("name", "String!"), f("description", "String"), f("args", "[__InputValue!]!"),
		f("type", "__Type!"), f("isDeprecated", "Boolean!"), f("deprecationReason", "String"),
	}},
	{name: "__InputValue", kind: "OBJECT", fields: []fieldDef{
		f("name", "String!"), f("description", "String"), f("type", "__Type!"), f("defaultValue", "String"),
	}},
	{name: "__EnumValue", kind: "OBJECT", fields: []fieldDef{
		f("name", "String!"), f("description", "String"), f("isDeprecated", "Boolean!"), f("deprecationReason", "String"),
	}},
	{name: "__Directive", kind: "OBJECT", fields: []fieldDef{
		f("name", "String!"), f("description", "String"), f("locations", "[__DirectiveLocation!]!"),
		f("args", "[__InputValue!]!"), f("isRepeatable", "Boolean!"),
	}},
	{name: "__TypeKind", kind: "ENUM", enumValues: []string{"SCALAR", "OBJECT", "INTERFACE", "UNION", "ENUM", "INPUT_OBJECT", "LIST", "NON_NULL"}},
	{name: "__DirectiveLocation", kind: "ENUM", enumValues: []string{"QUERY", "MUTATION", "SUBSCRIPTION", "FIELD", "FRAGMENT_DEFINITION", "FRAGMENT_SPREAD", "INLINE_FRAGMENT", "VARIABLE_DEFINITION", "SCHEMA", "SCALAR", "OBJECT", "FIELD_DEFINITION", "ARGUMENT_DEFINITION", "INTERFACE", "UNION", "ENUM", "ENUM_VALUE", "INPUT_OBJECT", "INPUT_FIELD_DEFINITION"}},
	{name: "String", kind: "SCALAR"},
	{name: "Int", kind: "SCALAR"},
	{name: "Float", kind: "SCALAR"},
	{name: "Boolean", kind: "SCALAR"},
	{name: "ID", kind: "SCALAR"},
	{name: "DateTime", kind: "SCALAR"},
	{name: "URI", kind: "SCALAR"},
	{name: "GitObjectID", kind: "SCALAR"},
}

var gqlTypeMap = func() map[string]*typeDef {
	m := make(map[string]*typeDef, len(gqlSchema))
	for i := range gqlSchema {
		m[gqlSchema[i].name] = &gqlSchema[i]
	}
	return m
}()

// typeRef renders a type reference string (e.g. '[Issue!]!') as an
// introspection __Type value.
func typeRef(typ string) map[string]interface{} {
	if strings.HasSuffix(typ, "!") {
		return map[string]interface{}{"kind": "NON_NULL", "name": nil, "ofType": typeRef(typ[:len(typ)-1])}
	}
	if strings.HasPrefix(typ, "[") && strings.HasSuffix(typ, "]") {
		return map[string]interface{}{"kind": "LIST", "name": nil, "ofType": typeRef(typ[1 : len(typ)-1])}
	}
	def := gqlTypeMap[typ]
	kind := "SCALAR"
	if def != nil {
		kind = def.kind
	}
	return map[string]interface{}{"kind": kind, "name": typ, "ofType": nil}
}

func globalID(kind string, id interface{}) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%v", kind, id)))
}

func decodeGlobalID(id string) (kind, value string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	text := string(raw)
	colon := strings.Index(text, ":")
	if colon < 0 {
		return "", "", false
	}
	return text[:colon], text[colon+1:], true
}

func cursorFor(index int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", index)))
}

func indexFromCursor(cursor string) int {
	if cursor == "" {
		return -1
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return -1
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return -1
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return -1
	}
	return n
}

// ---------------------------------------------------------------------------
// values the executor hands around
// ---------------------------------------------------------------------------

// gqlValue is one resolved object in the executor. The __-prefixed keys carry
// the store objects the field resolvers need; __typename names the GraphQL type.
type gqlValue struct {
	typename string
	v        map[string]interface{}
}

func newGqlValue(typename string) *gqlValue {
	return &gqlValue{typename: typename, v: map[string]interface{}{}}
}

func (g *gqlValue) set(key string, value interface{}) *gqlValue {
	g.v[key] = value
	return g
}

func (g *gqlValue) get(key string) interface{} { return g.v[key] }

type gqlExecutor struct {
	store     *Store
	variables map[string]interface{}
	errors    []map[string]interface{}
}

func evalValue(node valueNode, variables map[string]interface{}) interface{} {
	switch node.kind {
	case "variable":
		return variables[node.name]
	case "string":
		return node.str
	case "int", "float":
		return node.num
	case "boolean":
		return node.boolean
	case "null":
		return nil
	case "enum":
		return node.str
	case "list":
		out := make([]interface{}, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, evalValue(v, variables))
		}
		return out
	case "object":
		out := map[string]interface{}{}
		for _, f := range node.fields {
			out[f.name] = evalValue(f.value, variables)
		}
		return out
	}
	return nil
}

func argMap(field *fieldNode, variables map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, arg := range field.args {
		out[arg.name] = evalValue(arg.value, variables)
	}
	return out
}

func connectionValue(typename string, items []*gqlValue, first *int, after *string) *gqlValue {
	total := len(items)
	startIndex := 0
	if after != nil {
		startIndex = indexFromCursor(*after) + 1
	}
	pageSize := total
	if first != nil {
		pageSize = *first
	}
	if startIndex < 0 {
		startIndex = 0
	}
	end := startIndex + pageSize
	if end > total {
		end = total
	}
	var page []*gqlValue
	if startIndex < total {
		page = items[startIndex:end]
	}
	endIndex := startIndex + len(page) - 1
	g := newGqlValue(typename)
	g.set("__items", page)
	g.set("__total", total)
	g.set("__hasNext", endIndex+1 < total)
	g.set("__hasPrev", startIndex > 0)
	if len(page) > 0 {
		g.set("__startCursor", cursorFor(startIndex))
		g.set("__endCursor", cursorFor(endIndex))
	} else {
		g.set("__startCursor", nil)
		g.set("__endCursor", nil)
	}
	return g
}

func issueValue(repo *Repo, issue *Issue) *gqlValue {
	return newGqlValue("Issue").set("__repo", repo).set("__issue", issue)
}

func pullValue(repo *Repo, pull *Pull) *gqlValue {
	return newGqlValue("PullRequest").set("__repo", repo).set("__pull", pull)
}

func repoValue(repo *Repo) *gqlValue {
	return newGqlValue("Repository").set("__repo", repo)
}

func userValue(login string) *gqlValue {
	return newGqlValue("User").set("__login", login)
}

func orderIssues(issues []*Issue, orderBy interface{}) []*Issue {
	out := append([]*Issue{}, issues...)
	order, _ := orderBy.(map[string]interface{})
	field := "CREATED_AT"
	direction := "DESC"
	if order != nil {
		if f, ok := order["field"].(string); ok && f != "" {
			field = f
		}
		if d, ok := order["direction"].(string); ok && d != "" {
			direction = d
		}
	}
	key := func(i *Issue) int {
		switch field {
		case "UPDATED_AT":
			return i.UpdatedSeq
		case "COMMENTS":
			return len(i.Comments)
		default:
			return i.CreatedSeq
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if direction == "ASC" {
			return key(out[a]) < key(out[b])
		}
		return key(out[a]) > key(out[b])
	})
	return out
}

// ---------------------------------------------------------------------------
// field resolution
// ---------------------------------------------------------------------------

func firstArgInt(args map[string]interface{}, name string) *int {
	if v, ok := args[name].(float64); ok {
		n := int(v)
		return &n
	}
	return nil
}

func afterArgString(args map[string]interface{}, name string) *string {
	if v, ok := args[name].(string); ok {
		return &v
	}
	return nil
}

func resolveField(exec *gqlExecutor, parent *gqlValue, field *fieldNode) interface{} {
	typ := parent.typename
	args := argMap(field, exec.variables)
	store := exec.store

	if field.name == "__typename" {
		return typ
	}

	switch typ {
	case "Query":
		switch field.name {
		case "viewer":
			if store.Viewer == "" {
				return nil
			}
			return userValue(store.Viewer)
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			repo := store.findRepo(owner, name)
			if repo == nil {
				return nil
			}
			return repoValue(repo)
		case "node":
			id, _ := args["id"].(string)
			kind, value, ok := decodeGlobalID(id)
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				n, _ := strconv.Atoi(value)
				for _, repo := range store.Repos {
					if repo.ID == n {
						return repoValue(repo)
					}
				}
				return nil
			case "Issue":
				n, _ := strconv.Atoi(value)
				for _, repo := range store.Repos {
					for _, issue := range repo.Issues {
						if issue.ID == n {
							return issueValue(repo, issue)
						}
					}
				}
				return nil
			case "PullRequest":
				n, _ := strconv.Atoi(value)
				for _, repo := range store.Repos {
					for _, pull := range repo.Pulls {
						if pull.ID == n {
							return pullValue(repo, pull)
						}
					}
				}
				return nil
			}
			return nil
		case "__type":
			name, _ := args["name"].(string)
			def := gqlTypeMap[name]
			if def == nil {
				return nil
			}
			return newGqlValue("__Type").set("__typeDef", def)
		case "__schema":
			return newGqlValue("__Schema")
		}
	case "Mutation":
		switch field.name {
		case "createIssue":
			input, _ := args["input"].(map[string]interface{})
			kind, value, ok := decodeGlobalID(stringArg(input, "repositoryId"))
			if !ok || kind != "Repository" {
				return newGqlValue("CreateIssuePayload").set("__issue", nil)
			}
			n, _ := strconv.Atoi(value)
			var repo *Repo
			for _, r := range store.Repos {
				if r.ID == n {
					repo = r
				}
			}
			if repo == nil {
				return newGqlValue("CreateIssuePayload").set("__issue", nil)
			}
			issue := store.createIssue(repo, stringArg(input, "title"), stringArg(input, "body"), nil)
			return newGqlValue("CreateIssuePayload").set("__issue", issueValue(repo, issue))
		case "closeIssue":
			input, _ := args["input"].(map[string]interface{})
			kind, value, ok := decodeGlobalID(stringArg(input, "issueId"))
			if !ok || kind != "Issue" {
				return newGqlValue("CloseIssuePayload").set("__issue", nil)
			}
			n, _ := strconv.Atoi(value)
			for _, repo := range store.Repos {
				for _, issue := range repo.Issues {
					if issue.ID == n {
						issue.State = "closed"
						issue.UpdatedSeq = store.nextSeq()
						delete(repo.IssueJSON, issue.Number)
						return newGqlValue("CloseIssuePayload").set("__issue", issueValue(repo, issue))
					}
				}
			}
			return newGqlValue("CloseIssuePayload").set("__issue", nil)
		case "createPullRequest":
			input, _ := args["input"].(map[string]interface{})
			kind, value, ok := decodeGlobalID(stringArg(input, "repositoryId"))
			if !ok || kind != "Repository" {
				return newGqlValue("CreatePullRequestPayload").set("__pull", nil)
			}
			n, _ := strconv.Atoi(value)
			var repo *Repo
			for _, r := range store.Repos {
				if r.ID == n {
					repo = r
				}
			}
			if repo == nil {
				return newGqlValue("CreatePullRequestPayload").set("__pull", nil)
			}
			pull := store.createPull(repo, stringArg(input, "title"), stringArg(input, "body"), stringArg(input, "headRefName"), stringArg(input, "baseRefName"))
			if pull == nil {
				return newGqlValue("CreatePullRequestPayload").set("__pull", nil)
			}
			return newGqlValue("CreatePullRequestPayload").set("__pull", pullValue(repo, pull))
		case "mergePullRequest":
			input, _ := args["input"].(map[string]interface{})
			kind, value, ok := decodeGlobalID(stringArg(input, "pullRequestId"))
			if !ok || kind != "PullRequest" {
				return newGqlValue("MergePullRequestPayload").set("__pull", nil)
			}
			n, _ := strconv.Atoi(value)
			for _, repo := range store.Repos {
				for _, pull := range repo.Pulls {
					if pull.ID == n {
						sha := store.mergePull(repo, pull)
						if sha == "" {
							exec.errors = append(exec.errors, map[string]interface{}{"message": fmt.Sprintf("Pull request #%d cannot be merged", pull.Number)})
						}
						return newGqlValue("MergePullRequestPayload").set("__pull", pullValue(repo, pull))
					}
				}
			}
			return newGqlValue("MergePullRequestPayload").set("__pull", nil)
		case "addComment":
			input, _ := args["input"].(map[string]interface{})
			kind, value, ok := decodeGlobalID(stringArg(input, "subjectId"))
			body := stringArg(input, "body")
			if !ok {
				return newGqlValue("AddCommentPayload").set("__comment", nil).set("__subject", nil)
			}
			if kind == "Issue" {
				n, _ := strconv.Atoi(value)
				for _, repo := range store.Repos {
					for _, issue := range repo.Issues {
						if issue.ID == n {
							comment := &Comment{ID: store.NextCommentID, Body: body, User: store.Viewer, CreatedSeq: store.nextSeq()}
							store.NextCommentID++
							issue.Comments = append(issue.Comments, comment)
							issue.UpdatedSeq = store.Seq
							delete(repo.IssueJSON, issue.Number)
							cv := newGqlValue("IssueComment").set("__comment", comment).set("__issue", issue).set("__repo", repo)
							return newGqlValue("AddCommentPayload").set("__comment", cv).set("__subject", issueValue(repo, issue))
						}
					}
				}
			}
			if kind == "PullRequest" {
				n, _ := strconv.Atoi(value)
				for _, repo := range store.Repos {
					for _, pull := range repo.Pulls {
						if pull.ID == n {
							comment := &Comment{ID: store.NextCommentID, Body: body, User: store.Viewer, CreatedSeq: store.nextSeq()}
							store.NextCommentID++
							pull.Comments = append(pull.Comments, comment)
							pull.UpdatedSeq = store.Seq
							cv := newGqlValue("IssueComment").set("__comment", comment).set("__pull", pull).set("__repo", repo)
							return newGqlValue("AddCommentPayload").set("__comment", cv).set("__subject", pullValue(repo, pull))
						}
					}
				}
			}
			return newGqlValue("AddCommentPayload").set("__comment", nil).set("__subject", nil)
		}
	case "Repository":
		repo := parent.get("__repo").(*Repo)
		owner := store.repoOwner(repo)
		switch field.name {
		case "id":
			return globalID("Repository", repo.ID)
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return owner + "/" + repo.Name
		case "description":
			if repo.Description == "" {
				return nil
			}
			return repo.Description
		case "isPrivate":
			return repo.Private
		case "isFork":
			return false
		case "owner":
			return userValue(owner)
		case "defaultBranchRef":
			tip := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if tip == "" {
				return nil
			}
			return newGqlValue("Ref").set("__name", repo.DefaultBranch).set("__repo", repo)
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "hasProjectsEnabled":
			return false
		case "hasWikiEnabled":
			return false
		case "issues":
			items := append([]*Issue{}, repo.Issues...)
			if states, ok := args["states"].([]interface{}); ok && len(states) > 0 {
				wanted := map[string]bool{}
				for _, s := range states {
					if str, ok := s.(string); ok {
						wanted[strings.ToLower(str)] = true
					}
				}
				var filtered []*Issue
				for _, i := range items {
					if wanted[i.State] {
						filtered = append(filtered, i)
					}
				}
				items = filtered
			}
			items = orderIssues(items, args["orderBy"])
			vals := make([]*gqlValue, 0, len(items))
			for _, i := range items {
				vals = append(vals, issueValue(repo, i))
			}
			return connectionValue("IssueConnection", vals, firstArgInt(args, "first"), afterArgString(args, "after"))
		case "issue":
			number := intArg(args, "number")
			for _, issue := range repo.Issues {
				if issue.Number == number {
					return issueValue(repo, issue)
				}
			}
			return nil
		case "pullRequests":
			items := append([]*Pull{}, repo.Pulls...)
			if states, ok := args["states"].([]interface{}); ok && len(states) > 0 {
				wanted := map[string]bool{}
				for _, s := range states {
					if str, ok := s.(string); ok {
						wanted[strings.ToUpper(str)] = true
					}
				}
				var filtered []*Pull
				for _, p := range items {
					st := strings.ToUpper(p.State)
					if p.Merged {
						st = "MERGED"
					}
					if wanted[st] {
						filtered = append(filtered, p)
					}
				}
				items = filtered
			}
			sort.Slice(items, func(a, b int) bool { return items[a].CreatedSeq > items[b].CreatedSeq })
			vals := make([]*gqlValue, 0, len(items))
			for _, p := range items {
				vals = append(vals, pullValue(repo, p))
			}
			return connectionValue("PullRequestConnection", vals, firstArgInt(args, "first"), afterArgString(args, "after"))
		case "pullRequest":
			number := intArg(args, "number")
			for _, pull := range repo.Pulls {
				if pull.Number == number {
					return pullValue(repo, pull)
				}
			}
			return nil
		case "labels":
			vals := make([]*gqlValue, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				vals = append(vals, newGqlValue("Label").set("__label", l))
			}
			return connectionValue("LabelConnection", vals, firstArgInt(args, "first"), afterArgString(args, "after"))
		case "createdAt":
			return tsFor(repo.CreatedSeq)
		case "updatedAt":
			return tsFor(repo.CreatedSeq)
		case "pushedAt":
			return tsFor(repo.CreatedSeq)
		case "url":
			return "https://github.com/" + owner + "/" + repo.Name
		case "sshUrl":
			return "git@github.com:" + owner + "/" + repo.Name + ".git"
		case "forkCount":
			return 0
		case "stargazerCount":
			return 0
		case "watchers":
			return connectionValue("UserConnection", nil, nil, nil)
		case "primaryLanguage":
			return nil
		case "licenseInfo":
			return nil
		case "diskUsage":
			return 0
		}
	case "Issue":
		issue := parent.get("__issue").(*Issue)
		repo := parent.get("__repo").(*Repo)
		switch field.name {
		case "id":
			return globalID("Issue", issue.ID)
		case "databaseId":
			return issue.ID
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "bodyText":
			return issue.Body
		case "state":
			return strings.ToUpper(issue.State)
		case "stateReason":
			if issue.State == "closed" {
				return "COMPLETED"
			}
			return nil
		case "author":
			user := issue.User
			if user == "" {
				user = store.repoOwner(repo)
			}
			return userValue(user)
		case "labels":
			vals := make([]*gqlValue, 0, len(issue.Labels))
			for _, l := range issue.Labels {
				vals = append(vals, newGqlValue("Label").set("__label", l))
			}
			return connectionValue("LabelConnection", vals, firstArgInt(args, "first"), afterArgString(args, "after"))
		case "comments":
			vals := make([]*gqlValue, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				vals = append(vals, newGqlValue("IssueComment").set("__comment", c).set("__issue", issue).set("__repo", repo))
			}
			return connectionValue("IssueCommentConnection", vals, firstArgInt(args, "first"), afterArgString(args, "after"))
		case "createdAt":
			return tsFor(issue.CreatedSeq)
		case "updatedAt":
			return tsFor(issue.UpdatedSeq)
		case "closedAt":
			if issue.State == "closed" {
				return tsFor(issue.UpdatedSeq)
			}
			return nil
		case "url":
			return "https://github.com/" + store.repoOwner(repo) + "/" + repo.Name + "/issues/" + itoa(issue.Number)
		case "repository":
			return repoValue(repo)
		case "locked":
			return false
		case "milestone":
			return nil
		case "assignees":
			return connectionValue("UserConnection", nil, nil, nil)
		case "participants":
			return connectionValue("UserConnection", nil, nil, nil)
		case "timelineItems":
			return connectionValue("IssueTimelineItemsConnection", nil, nil, nil)
		}
	case "PullRequest":
		pull := parent.get("__pull").(*Pull)
		repo := parent.get("__repo").(*Repo)
		switch field.name {
		case "id":
			return globalID("PullRequest", pull.ID)
		case "databaseId":
			return pull.ID
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
			if store.mergeable(pull, repo) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "mergeStateStatus":
			if store.mergeable(pull, repo) {
				return "CLEAN"
			}
			return "DIRTY"
		case "headRefName":
			return pull.HeadRef
		case "baseRefName":
			return pull.BaseRef
		case "headRef":
			return newGqlValue("Ref").set("__name", pull.HeadRef).set("__repo", repo)
		case "baseRef":
			return newGqlValue("Ref").set("__name", pull.BaseRef).set("__repo", repo)
		case "author":
			user := pull.User
			if user == "" {
				user = store.repoOwner(repo)
			}
			return userValue(user)
		case "labels":
			return connectionValue("LabelConnection", nil, nil, nil)
		case "files":
			files := pullFiles(store, repo, pull)
			vals := make([]*gqlValue, 0, len(files))
			for _, f := range files {
				ct := "MODIFIED"
				if f.Status == "added" {
					ct = "ADDED"
				} else if f.Status == "removed" {
					ct = "REMOVED"
				}
				vals = append(vals, newGqlValue("PullRequestFile").set("__path", f.Filename).set("__additions", f.Additions).set("__deletions", f.Deletions).set("__changeType", ct))
			}
			return connectionValue("PullRequestFileConnection", vals, firstArgInt(args, "first"), afterArgString(args, "after"))
		case "commits":
			commits := store.commitsFrom(repo, pull.HeadSha)
			baseTip := store.resolveRef(repo, pull.BaseRef)
			baseSet := map[string]bool{}
			for _, c := range store.commitsFrom(repo, baseTip) {
				baseSet[c.OID] = true
			}
			var vals []*gqlValue
			for _, c := range commits {
				if baseSet[c.OID] {
					continue
				}
				vals = append(vals, newGqlValue("PullRequestCommit").set("__commitOid", c.OID).set("__repo", repo))
			}
			return connectionValue("PullRequestCommitConnection", vals, firstArgInt(args, "first"), afterArgString(args, "after"))
		case "reviews":
			return connectionValue("PullRequestReviewConnection", nil, nil, nil)
		case "createdAt":
			return tsFor(pull.CreatedSeq)
		case "updatedAt":
			return tsFor(pull.UpdatedSeq)
		case "closedAt":
			if pull.State == "closed" {
				return tsFor(pull.UpdatedSeq)
			}
			return nil
		case "mergedAt":
			if pull.MergedAt == "" {
				return nil
			}
			return pull.MergedAt
		case "mergeCommit":
			if pull.MergeCommitSHA == "" {
				return nil
			}
			return newGqlValue("Commit").set("__oid", pull.MergeCommitSHA).set("__repo", repo)
		case "url":
			return "https://github.com/" + store.repoOwner(repo) + "/" + repo.Name + "/pull/" + itoa(pull.Number)
		case "repository":
			return repoValue(repo)
		case "isDraft":
			return false
		case "additions":
			sum := 0
			for _, f := range pullFiles(store, repo, pull) {
				sum += f.Additions
			}
			return sum
		case "deletions":
			sum := 0
			for _, f := range pullFiles(store, repo, pull) {
				sum += f.Deletions
			}
			return sum
		case "changedFiles":
			return len(pullFiles(store, repo, pull))
		}
	case "User":
		login, _ := parent.get("__login").(string)
		switch field.name {
		case "id":
			return globalID("User", login)
		case "databaseId":
			return 1
		case "login":
			return login
		case "name":
			return login
		case "avatarUrl":
			return ""
		case "url":
			return "https://github.com/" + login
		case "email":
			return nil
		}
	case "Ref":
		switch field.name {
		case "id":
			repo := parent.get("__repo").(*Repo)
			name, _ := parent.get("__name").(string)
			return globalID("Ref", fmt.Sprintf("%d:%s", repo.ID, name))
		case "name":
			return parent.get("__name")
		case "prefix":
			return "refs/heads/"
		case "target":
			return nil
		case "repository":
			return repoValue(parent.get("__repo").(*Repo))
		}
	case "Commit":
		switch field.name {
		case "id":
			return globalID("Commit", parent.get("__oid"))
		case "oid":
			return parent.get("__oid")
		case "message":
			return ""
		case "messageHeadline":
			return ""
		case "author":
			return nil
		case "committer":
			return nil
		case "tree":
			return newGqlValue("Tree").set("__oid", "").set("__repo", parent.get("__repo"))
		}
	case "Tree":
		switch field.name {
		case "id":
			return globalID("Tree", parent.get("__oid"))
		case "oid":
			return parent.get("__oid")
		case "entries":
			return []interface{}{}
		}
	case "Label":
		label := parent.get("__label").(*Label)
		switch field.name {
		case "id":
			return globalID("Label", label.ID)
		case "name":
			return label.Name
		case "color":
			return label.Color
		case "description":
			if label.Description == "" {
				return nil
			}
			return label.Description
		case "isDefault":
			return false
		case "url":
			return ""
		}
	case "IssueComment":
		comment := parent.get("__comment").(*Comment)
		switch field.name {
		case "id":
			return globalID("IssueComment", comment.ID)
		case "databaseId":
			return comment.ID
		case "body":
			return comment.Body
		case "bodyText":
			return comment.Body
		case "author":
			user := comment.User
			if user == "" {
				user = store.Viewer
			}
			return userValue(user)
		case "createdAt":
			return tsFor(comment.CreatedSeq)
		case "updatedAt":
			return tsFor(comment.CreatedSeq)
		case "url":
			return ""
		case "issue":
			issue, ok := parent.get("__issue").(*Issue)
			if !ok || issue == nil {
				return nil
			}
			return issueValue(parent.get("__repo").(*Repo), issue)
		}
	case "PullRequestFile":
		switch field.name {
		case "path":
			return parent.get("__path")
		case "additions":
			return parent.get("__additions")
		case "deletions":
			return parent.get("__deletions")
		case "changeType":
			return parent.get("__changeType")
		}
	case "PullRequestCommit":
		if field.name == "commit" {
			return newGqlValue("Commit").set("__oid", parent.get("__commitOid")).set("__repo", parent.get("__repo"))
		}
	case "PullRequestReview":
		switch field.name {
		case "id":
			return ""
		case "state":
			return "COMMENTED"
		case "author":
			return nil
		case "body":
			return ""
		}
	case "Milestone":
		switch field.name {
		case "id":
			return ""
		case "number":
			return 0
		case "title":
			return ""
		}
	case "Language":
		switch field.name {
		case "id":
			return ""
		case "name":
			return ""
		}
	case "License":
		switch field.name {
		case "key":
			return ""
		case "name":
			return ""
		case "spdxId":
			return nil
		}
	case "GitActor":
		switch field.name {
		case "name", "email", "date", "user":
			return nil
		}
	}

	// Connection types: nodes / edges / pageInfo / totalCount.
	if strings.HasSuffix(typ, "Connection") {
		items, _ := parent.get("__items").([]*gqlValue)
		switch field.name {
		case "totalCount":
			total, _ := parent.get("__total").(int)
			return total
		case "nodes":
			out := make([]interface{}, 0, len(items))
			for _, it := range items {
				out = append(out, it)
			}
			return out
		case "edges":
			out := make([]interface{}, 0, len(items))
			for i, node := range items {
				edgeTyp := strings.TrimSuffix(typ, "Connection") + "Edge"
				out = append(out, newGqlValue(edgeTyp).set("__node", node).set("__cursor", cursorFor(i)))
			}
			return out
		case "pageInfo":
			return newGqlValue("PageInfo").
				set("__hasNext", parent.get("__hasNext")).
				set("__hasPrev", parent.get("__hasPrev")).
				set("__startCursor", parent.get("__startCursor")).
				set("__endCursor", parent.get("__endCursor"))
		}
	}
	if strings.HasSuffix(typ, "Edge") {
		if field.name == "node" {
			return parent.get("__node")
		}
		if field.name == "cursor" {
			return parent.get("__cursor")
		}
	}
	if typ == "PageInfo" {
		switch field.name {
		case "hasNextPage":
			return parent.get("__hasNext")
		case "hasPreviousPage":
			return parent.get("__hasPrev")
		case "startCursor":
			return parent.get("__startCursor")
		case "endCursor":
			return parent.get("__endCursor")
		}
	}

	// Payload types.
	if typ == "CreateIssuePayload" || typ == "CloseIssuePayload" {
		if field.name == "issue" {
			return parent.get("__issue")
		}
		if field.name == "clientMutationId" {
			return nil
		}
	}
	if typ == "CreatePullRequestPayload" || typ == "MergePullRequestPayload" {
		if field.name == "pullRequest" {
			return parent.get("__pull")
		}
		if field.name == "actor" {
			if store.Viewer == "" {
				return nil
			}
			return userValue(store.Viewer)
		}
		if field.name == "clientMutationId" {
			return nil
		}
	}
	if typ == "AddCommentPayload" {
		if field.name == "commentEdge" {
			comment, _ := parent.get("__comment").(*gqlValue)
			if comment == nil {
				return nil
			}
			return newGqlValue("IssueCommentEdge").set("__node", comment).set("__cursor", cursorFor(0))
		}
		if field.name == "subject" {
			return parent.get("__subject")
		}
		if field.name == "clientMutationId" {
			return nil
		}
	}

	// Introspection types.
	if typ == "__Type" {
		def, _ := parent.get("__typeDef").(*typeDef)
		if def == nil {
			// A __Type built from a typeRef map (only used via __Field.type).
			if ref, ok := parent.get("__typeRef").(map[string]interface{}); ok {
				switch field.name {
				case "name":
					return ref["name"]
				case "kind":
					return ref["kind"]
				case "ofType":
					if of, ok := ref["ofType"].(map[string]interface{}); ok && of != nil {
						return newGqlValue("__Type").set("__typeRef", of)
					}
					return nil
				case "description", "fields", "enumValues", "inputFields", "interfaces", "possibleTypes", "specifiedByURL":
					return nil
				case "isOneOf":
					return false
				}
			}
			exec.errors = append(exec.errors, map[string]interface{}{"message": fmt.Sprintf("Cannot query field %q on type %q.", field.name, typ)})
			return nil
		}
		switch field.name {
		case "name":
			return def.name
		case "kind":
			return def.kind
		case "description":
			if def.description == "" {
				return nil
			}
			return def.description
		case "fields":
			if def.fields == nil {
				return nil
			}
			out := make([]interface{}, 0, len(def.fields))
			for i := range def.fields {
				out = append(out, newGqlValue("__Field").set("__fieldDef", &def.fields[i]))
			}
			return out
		case "enumValues":
			if def.enumValues == nil {
				return nil
			}
			out := make([]interface{}, 0, len(def.enumValues))
			for _, name := range def.enumValues {
				out = append(out, newGqlValue("__EnumValue").set("__enumName", name))
			}
			return out
		case "inputFields":
			if def.inputFields == nil {
				return nil
			}
			out := make([]interface{}, 0, len(def.inputFields))
			for i := range def.inputFields {
				out = append(out, newGqlValue("__InputValue").set("__fieldDef", &def.inputFields[i]))
			}
			return out
		case "interfaces":
			return []interface{}{}
		case "possibleTypes":
			return []interface{}{}
		case "ofType":
			return nil
		case "specifiedByURL":
			return nil
		case "isOneOf":
			return false
		}
	}
	if typ == "__Field" {
		def, _ := parent.get("__fieldDef").(*fieldDef)
		switch field.name {
		case "name":
			return def.name
		case "description":
			return nil
		case "args":
			out := make([]interface{}, 0, len(def.args))
			for i := range def.args {
				out = append(out, newGqlValue("__InputValue").set("__fieldDef", &def.args[i]))
			}
			return out
		case "type":
			return newGqlValue("__Type").set("__typeRef", typeRef(def.typ))
		case "isDeprecated":
			return false
		case "deprecationReason":
			return nil
		}
	}
	if typ == "__InputValue" {
		def, _ := parent.get("__fieldDef").(*fieldDef)
		switch field.name {
		case "name":
			return def.name
		case "description":
			return nil
		case "type":
			return newGqlValue("__Type").set("__typeRef", typeRef(def.typ))
		case "defaultValue":
			return nil
		}
	}
	if typ == "__EnumValue" {
		switch field.name {
		case "name":
			return parent.get("__enumName")
		case "description":
			return nil
		case "isDeprecated":
			return false
		case "deprecationReason":
			return nil
		}
	}
	if typ == "__Schema" {
		switch field.name {
		case "queryType":
			return newGqlValue("__Type").set("__typeDef", gqlTypeMap["Query"])
		case "mutationType":
			return newGqlValue("__Type").set("__typeDef", gqlTypeMap["Mutation"])
		case "subscriptionType":
			return nil
		case "types":
			out := make([]interface{}, 0, len(gqlSchema))
			for i := range gqlSchema {
				out = append(out, newGqlValue("__Type").set("__typeDef", &gqlSchema[i]))
			}
			return out
		case "directives":
			return []interface{}{}
		}
	}
	if typ == "__Directive" {
		switch field.name {
		case "name":
			return ""
		case "description":
			return nil
		case "locations":
			return []interface{}{}
		case "args":
			return []interface{}{}
		case "isRepeatable":
			return false
		}
	}

	// Unknown field on a known type.
	exec.errors = append(exec.errors, map[string]interface{}{"message": fmt.Sprintf("Cannot query field %q on type %q.", field.name, typ)})
	return nil
}

func stringArg(m map[string]interface{}, name string) string {
	if m == nil {
		return ""
	}
	s, _ := m[name].(string)
	return s
}

func intArg(args map[string]interface{}, name string) int {
	if v, ok := args[name].(float64); ok {
		return int(v)
	}
	return 0
}

func pullFiles(store *Store, repo *Repo, pull *Pull) []diffFile {
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
		return nil
	}
	baseC, err := parseCommit(baseCommit.Data)
	if err != nil {
		return nil
	}
	headC, err := parseCommit(headCommit.Data)
	if err != nil {
		return nil
	}
	return store.diffTrees(repo, baseC.Tree, headC.Tree)
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

func executeSelection(exec *gqlExecutor, parent *gqlValue, selections []selectionNode) map[string]interface{} {
	out := map[string]interface{}{}
	for _, selection := range selections {
		if selection.kind == "fragment" {
			if selection.typeCondition != "" && selection.typeCondition != parent.typename {
				continue
			}
			for k, v := range executeSelection(exec, parent, selection.selection) {
				out[k] = v
			}
			continue
		}
		field := selection.field
		key := field.name
		if field.hasAlias {
			key = field.alias
		}
		value := resolveField(exec, parent, field)
		if value == nil {
			out[key] = nil
			continue
		}
		if len(field.selection) > 0 {
			switch v := value.(type) {
			case *gqlValue:
				out[key] = executeSelection(exec, v, field.selection)
			case []interface{}:
				items := make([]interface{}, 0, len(v))
				for _, item := range v {
					if gv, ok := item.(*gqlValue); ok {
						items = append(items, executeSelection(exec, gv, field.selection))
					} else {
						items = append(items, item)
					}
				}
				out[key] = items
			default:
				out[key] = value
			}
		} else {
			out[key] = value
		}
	}
	return out
}

type graphqlResult struct {
	Data   map[string]interface{}   `json:"data"`
	Errors []map[string]interface{} `json:"errors,omitempty"`
}

func executeGraphql(store *Store, operation *operationNode, variables map[string]interface{}) (result map[string]interface{}) {
	exec := &gqlExecutor{store: store, variables: variables}
	rootType := "Query"
	if operation.operation == "mutation" {
		rootType = "Mutation"
	}
	root := newGqlValue(rootType)
	defer func() {
		if r := recover(); r != nil {
			result = map[string]interface{}{"data": nil, "errors": []map[string]interface{}{{"message": fmt.Sprintf("%v", r)}}}
		}
	}()
	data := executeSelection(exec, root, operation.selection)
	result = map[string]interface{}{"data": data}
	if len(exec.errors) > 0 {
		result["errors"] = exec.errors
	}
	return result
}

// handleGraphqlBody parses and executes a GraphQL request body (a single
// operation or a batch array). Returns the HTTP status and the payload.
func handleGraphqlBody(store *Store, body []byte) (int, interface{}) {
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 400, map[string]interface{}{"errors": []map[string]interface{}{{"message": "Problems parsing JSON"}}}
	}
	var requests []interface{}
	isBatch := false
	if arr, ok := parsed.([]interface{}); ok {
		requests = arr
		isBatch = true
	} else {
		requests = []interface{}{parsed}
	}
	responses := make([]interface{}, 0, len(requests))
	for _, request := range requests {
		rec, _ := request.(map[string]interface{})
		query, _ := rec["query"].(string)
		variables := map[string]interface{}{}
		if v, ok := rec["variables"].(map[string]interface{}); ok {
			variables = v
		}
		operationName, _ := rec["operationName"].(string)
		if query == "" {
			responses = append(responses, map[string]interface{}{"data": nil, "errors": []map[string]interface{}{{"message": "Must provide query string."}}})
			continue
		}
		tokens, err := tokenize(query)
		if err != nil {
			responses = append(responses, map[string]interface{}{"data": nil, "errors": []map[string]interface{}{{"message": err.Error()}}})
			continue
		}
		operations, err := (&parser{tokens: tokens}).parseDocument()
		if err != nil {
			responses = append(responses, map[string]interface{}{"data": nil, "errors": []map[string]interface{}{{"message": err.Error()}}})
			continue
		}
		var operation *operationNode
		if operationName != "" {
			for _, op := range operations {
				if op.name == operationName {
					operation = op
					break
				}
			}
			if operation == nil && len(operations) > 0 {
				operation = operations[0]
			}
		} else if len(operations) > 0 {
			operation = operations[0]
		}
		if operation == nil {
			responses = append(responses, map[string]interface{}{"data": nil, "errors": []map[string]interface{}{{"message": "Must provide an operation."}}})
			continue
		}
		// Fill variable defaults.
		vars := map[string]interface{}{}
		for k, v := range variables {
			vars[k] = v
		}
		for _, v := range operation.variables {
			if _, ok := vars[v.name]; !ok && v.defaultValue != nil {
				vars[v.name] = evalValue(*v.defaultValue, vars)
			}
		}
		responses = append(responses, executeGraphql(store, operation, vars))
	}
	if isBatch {
		return 200, responses
	}
	return 200, responses[0]
}
