# github-core-clone-go

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/tamadung/github-core-clone-go/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/tamadung/github-core-clone-go/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process GitHub-compatible git forge, written in Go (stdlib only).
One process serves git smart HTTP (upload-pack v2 + receive-pack v0),
REST v3, GraphQL and MCP from an in-memory store — no git binary, no
network, no UI, no database.

This is a Go rewrite of the Node entry
[`tamadung/github-core-clone`](https://github.com/tamadung/github-core-clone)
(server score 923: feature 600 + agent 200 + perf 123, rank #4049 — outside
Top Rewards). Local suite: **120/120 checks, perf 174–179, score 974–979**.

## Layout

| file | what it is |
|------|-----------|
| `main.go` | entrypoint: trial lifecycle (bind, register, poll `/done`) |
| `http.go` | raw TCP HTTP/1.1 server (no `net/http`) |
| `git.go` | git object model: SHA-1, trees/commits/tags, packfiles, pkt-line, sideband |
| `store.go` | in-memory store: repos, objects, refs, issues, pulls, labels, comments |
| `gitserver.go` | git smart HTTP: v0/v2 advertisement, upload-pack, receive-pack |
| `rest.go` | GitHub REST v3 |
| `graphql.go` | hand-rolled GraphQL executor + introspection schema |
| `mcp.go` | MCP: `server/discover`, `initialize`, `tools/list`, `tools/call` |
| `app.go` | routing |
| `main.ts` | thin launcher — the harness only runs `main.ts`/`main.py`, so this spawns `./forge` |
| `forge` | static Linux amd64 binary (6.5 MB, `< 8 MiB` cap) |

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o forge .
```

## Run

The trial harness starts the entry with `node /bot/main.ts` (see
`NOTES.md` for why the launcher exists). The Go binary binds an ephemeral
port, POSTs it to `$MAKEFUN_ENV_URL/register`, then polls `/done`.

Local validation (stub env on 8999, forge on 18080):

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:8999 FORGE_PORT=18080 ./forge
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:18080 --token "$TOKEN" --perf
```

See `NOTES.md` for the build log, bench numbers and the five weakest
points.

## License

MIT
