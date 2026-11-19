[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/thesathing/github-forge-go/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/thesathing/github-forge-go/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# GitHub-compatible git forge (Go)

A standalone, single-process forge: git smart HTTP (upload-pack v2,
receive-pack v0), REST v3, GraphQL v4 and MCP, from one in-memory store.
Written in Go (standard library only) for speed; `main.ts` launches the
statically linked `forge-go` binary.

## Run

```sh
# standalone (local testing): prints FORGE_URL and serves until stopped
MAKEFUN_SHOP_TOKEN=test-token-123 ./forge-go

# under the trial harness: node main.ts
node main.ts
```

## Layout

- `main.go` — entry lifecycle: bind a port, register with the env, poll done.
- `rest.go` — HTTP server and the REST v3 routes.
- `store.go` — in-memory store: repos, issues, pulls, git objects.
- `gitcore.go` — git objects, packs, pkt-lines, reachability.
- `gitproto.go` — git smart HTTP.
- `graphql.go` — GraphQL v4 executor.
- `mcp.go` — MCP tools over streamable HTTP.

## Build

```sh
export PATH=$HOME/go-install/go/bin:$PATH
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o forge-go .
```
