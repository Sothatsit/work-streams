# work-stream

`ws` is a shared log for people and agents. Entries have a type, a
short subject, an optional body, and optional key-value metadata.
`ws-server` keeps them in SQLite; every client talks to one server.

Types can have 64 characters, subjects 128, bodies 2048, metadata
keys 64, and metadata values 256. Each entry can have 16 metadata
pairs.

The server logs every write for recovery. Deleting an entry removes it
from the stream, not those logs.

## Build

```
scripts/build.sh
```

This produces `bin/ws` and `bin/ws-server`.

## Run

Give the server an absolute data directory:

```
bin/ws-server --data /path/to/work-stream
```

`WORK_STREAM_DATA` can supply the directory. The default port is 7139;
change it with `WORK_STREAM_PORT` or `--port`.

Authentication is optional. Generate one shared secret, then set it in
the server and every client environment:

```
bin/ws secret
export WORK_STREAM_SECRET='<printed value>'
```

`ws secret` does not replace a value already set in the environment.

The connection is plain HTTP. Keep the server behind a firewall, VPN,
or authenticated tunnel even when a secret is set.

## Use

```
ws add todo "Count the new ducklings" --project duck-pond --jira QUACK-1
ws recent
ws search ducklings --type todo --no-subject '*(Solved*'
ws search --jira 'QUACK-*'
ws edit e1 "Count the new ducklings (Solved 22/07/2026)"
ws add-meta e1 pr https://github.com/example/pond/pull/123
ws entry e1
ws status
```

`ws search TEXT` finds the literal text in subjects or bodies. Search
flags take full-string, ASCII-case-insensitive SQLite GLOB patterns:

```
--subject  --body  --content  --type  --key  --meta
--project  --jira  --github   --confluence
```

Prefix any flag with `--no-` to exclude it. `--content` matches the
subject or body. `--meta` takes `KEY=VALUE`; both patterns must match
the same metadata pair. Repeated filters AND together.

A pattern without wildcards is exact. `*` matches any text, `?` one
character, and brackets form character classes. Quote patterns so the
shell does not expand them. In a pattern, use `[*]`, `[?]`, and `[[]`
for literal `*`, `?`, and `[`.

Searches return 50 entries by default. `--limit` accepts 1 through 500;
use `--offset` for later pages.

Point a client at the server with `WORK_STREAM_ADDRESS` and
`WORK_STREAM_PORT`, or with global `--address` and `--port` flags.
`WORK_STREAM_TIMEOUT` or `--timeout` sets the client or server
deadline; the default is 5 seconds. `ws --version` and `ws secret`
work offline.

Run `ws help` for the full command reference.
