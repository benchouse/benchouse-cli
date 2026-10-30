# Using benchouse

The full reference: every agent, every flag, and running it from a coding agent.
The short version is the [README](../README.md).

Find out where your analytics agent gets answers wrong, without giving anyone
access to your warehouse.

`benchouse` reads your agent's conversation history **on your machine**,
packages it with your dbt project and the agent's context, shows you exactly
what it collected, and uploads it to [Benchouse](https://benchouse.ai) only
when you say so. Within a few days André emails you the top issues: questions it got
wrong or refused, and the joins, columns and definitions that would fix them.

- **No credentials leave your machine.** They are used to read locally and
  are never written to the bundle.
- **Read-only.** Every query is a `SELECT` (or a `GET`), and you can print
  them all before anything connects: `benchouse plan --agent nao`.
- **Bounded.** By default it reads the 2,000 most recent conversations of the
  last 90 days, whichever limit is hit first, fetching their messages 200
  conversations per query, and streams them to disk instead of holding them
  in memory.
- **No business data by default.** The rows your agent's queries returned are
  removed; the question, the SQL, the answer and the row counts are kept.
- **No people by default.** User ids, names, emails and IP addresses become
  stable pseudonyms (`person_<hash>`), keyed by a secret that never leaves your
  machine; questions and answers are left as they are. User directories are
  left out entirely unless you pass `--include-users`.
- **Inspect before sending.** The bundle is a plain `.tar.gz` with a
  `manifest.json` listing every query that ran, every file included, and
  everything removed. See [docs/bundle-format.md](bundle-format.md).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/benchouse/benchouse-cli/main/install.sh | sh
```

It verifies the archive's checksum, and the checksum file's Sigstore signature
when `cosign` is installed. Every release also carries GitHub build
provenance: `gh attestation verify benchouse_<os>_<arch>.tar.gz --repo benchouse/benchouse-cli`.

or with Go 1.26+:

```sh
go install github.com/benchouse/benchouse-cli/cmd/benchouse@latest
```

or download a binary from [Releases](https://github.com/benchouse/benchouse-cli/releases).

## Use

Guided, about five minutes:

```sh
benchouse
```

It asks which agent you use, then three steps: where its conversations are
stored, your dbt project folder (optional), and the agent's context folder
(optional). Each step is checked as soon as you finish it. Then it shows what
it collected and asks before uploading. In a terminal this is a full-screen
run; `--plain` (or `BENCHOUSE_PLAIN=1`) asks line by line instead, as it does
when output is piped or `TERM=dumb`.

Collect without uploading, look, then send:

```sh
benchouse collect --agent nao --storage sqlite --sqlite-path ~/nao/db.sqlite --context ~/nao
benchouse inspect benchouse-nao-*.tar.gz
benchouse send benchouse-nao-*.tar.gz --email you@company.com   # shows the summary, asks first (--yes skips)
```

## Supported agents

<!-- agents:start -->
| `--agent` | Agent | Reads |
|---|---|---|
| `nao` | nao | its Postgres or sqlite database |
| `snowflake-cortex` | Snowflake Cortex Analyst and Agents | request history and traces in your account |
| `snowflake-intelligence` | Snowflake Intelligence | its Cortex Agents traces |
| `databricks-genie` | Databricks AI/BI Genie | audit and query history system tables, Genie messages API |
| `lightdash` | Lightdash AI agents (experimental) | the Lightdash Postgres database (threads, prompts, tool calls, query history) |
| `metabase` | Metabase Metabot (experimental) | the Metabase Postgres application database |
| `wren-ai` | Wren AI (experimental) | its sqlite or Postgres database (threads and responses) |
| `looker` | Looker Conversational Analytics (Gemini in Looker) (experimental) | System Activity via the Looker API; with `--per-user yes`, each user's conversations (admin impersonation) |
| `langfuse` | any custom agent traced with Langfuse (experimental) | traces via the Langfuse public API |
| `cube` | Cube (experimental) | the chat history export of the Cube Cloud API (admin; a Cube preview feature) |
| `thoughtspot` | ThoughtSpot Spotter (experimental) | the Spotter analytics model via REST API v2 searchdata; falls back to your own saved Spotter conversations |
| `sigma` | Sigma Assistant and agents (experimental) | the AI usage table (`AI_USAGE_<ORG>`) Sigma writes to your Snowflake, Databricks or BigQuery |
| `hex` | Hex agent threads (experimental) | threads and full message records via the Hex public API (Manager role or higher) |
| `omni` | Omni AI assistant (experimental) | AI conversations and their jobs' generated queries via the Omni API (results kept 14 days) |
<!-- agents:end -->

The first four read the same tables Benchouse's live connectors read in
production (under `--max-conversations`, the most recent conversations
first, then their rows in batches of 200). *Experimental* readers are written from the vendor's published
schema, source or API reference and tested against fixtures; if one trips
over your version, `benchouse collect` still writes nothing it shouldn't, and
an issue with the error helps us fix it fast.

`benchouse agents` lists them; `benchouse upload --agent <id> --help` shows an
agent's flags and the exact access it needs. Your agent isn't listed? Run
`benchouse` and pick **Other**: tell us which one and we'll add it, typically
within 72 hours.

### nao

```sh
# Postgres (DB_URI set)
BENCHOUSE_PASSWORD=... benchouse upload --agent nao \
  --host db.internal --database nao --user benchouse_reader \
  --dbt ~/analytics/dbt --context ~/nao-project --email you@company.com

# sqlite (default local install)
benchouse upload --agent nao --storage sqlite --sqlite-path ./db.sqlite --context . --email you@company.com
```

### Snowflake Cortex

```sh
BENCHOUSE_TOKEN=... benchouse upload --agent snowflake-cortex \
  --account myorg-myaccount --user BENCHOUSE_READER --role BENCHOUSE_READ --warehouse XS \
  --context ./semantic_models --email you@company.com
```

Key-pair instead of a token: `--auth keypair --private-key ./rsa_key.p8`.

## Flags every agent shares

| Flag | Default | |
|---|---|---|
| `--since` | `90d` | capture window: `30d`, `6m`, `2026-01-01`, `all` |
| `--max-conversations` | `2000` | collect at most this many conversations, the most recent in the window (whichever limit is hit first); `0` = no cap (warned: can be slow and large) |
| `--dbt` | | dbt project folder |
| `--context` | | the agent's context / semantic layer folder |
| `--email` | | where the findings go (or `$BENCHOUSE_EMAIL`) |
| `--out` | `benchouse-<agent>-<time>.tar.gz` | bundle path (never overwritten, never written through a symlink) |
| `--force` | off | overwrite an existing `--out` file |
| `--include-results` | off | keep the rows the agent's queries returned |
| `--include-users` | off | include the user directory |
| `--anonymize-users` | on | pseudonymise people; `--anonymize-users=false` keeps real identities |
| `--show-queries` | off | print every query before running it |
| `--yes` | off | upload without the confirmation prompt |
| `--no-input` | off when a terminal | never prompt |

Secrets can come from flags, from `BENCHOUSE_<FLAG>` environment variables
(`BENCHOUSE_PASSWORD`, `BENCHOUSE_TOKEN`, ...), from a file named by
`--<flag>-file` (`--password-file`, `--token-file`, or `BENCHOUSE_TOKEN_FILE`)
or from a hidden prompt. Prefer a file, the environment or the prompt over
flags, which end up in shell history and the process list.

Connections are encrypted by default. Postgres uses `--sslmode auto`:
`require` for any host but this machine (`prefer` for localhost), and
`verify-full` when `--sslrootcert` names a CA file; MySQL uses `--tls auto`
(`true`, a verified certificate). Weaker modes still work when asked for, with
a warning. API hosts (`--host`, `--endpoint`) must be `https://` unless they
are localhost, and redirects are never followed, so a token cannot be sent
to another host.

## Running it from a coding agent

`benchouse` is built to be driven by Claude Code, Cursor, Codex and friends:
everything has a flag, `--no-input` never blocks, `benchouse agents --json`
describes every agent's inputs, and output for humans goes to stderr while
stdout carries only the result (the bundle path or the upload reference). See
[AGENTS.md](../AGENTS.md) for the rules an agent should follow.

## What gets read

Folders: only the files an agent's reader names (for dbt: `.sql`, `.yml`,
`.md`, `target/manifest.json`, `target/catalog.json`). `profiles.yml`, `.env*`,
key files, keystores, Terraform state and anything containing a private key are
never included. Secret values are replaced with `[redacted]` and counted in the
manifest: by key in YAML and JSON (passwords, tokens, keys, DSNs), and in every
file by form (cloud and API keys, JWTs, webhook URLs, credentials in URLs,
connection strings and comments). A file whose secret cannot be masked is left
out and listed as skipped. nao's sample-data files (`preview.md`,
`profiling.md`) come only with `--include-results`.

Conversations: see each agent's queries with `benchouse plan --agent <id>`.
The readers for nao, Snowflake Cortex / Intelligence and Databricks Genie
choose the conversations first (`ORDER BY` last activity `DESC LIMIT
--max-conversations`, within `--since`), then fetch only their messages,
parts, spans or audit events, 200 conversation ids per statement with bound
parameters. Rows not tied to a conversation (usage, MCP call logs, query
history Snowflake cannot join) are read with the window and a `LIMIT` of
five per conversation in the cap, with a note when it is reached. The
manifest's `sample` says how many were collected out of how many in the
window. The other readers still read their whole window and do not apply
`--max-conversations` yet; they say so in a note.

## Usage stats

benchouse sends anonymous usage stats to Benchouse (through PostHog), so we
can see which agents people pick, where runs get stuck and which errors
occur. They never contain credentials, field values, names, emails, file
paths, hosts, queries, questions or any of your data.

- **Who:** a random install id, created on first run in your config
  directory (`benchouse/install-id`, readable only by you). No person
  profile is created, and events ask PostHog not to record your IP address
  or look up your location.
- **How:** in the background, in batches. Sending never slows a run down
  (at most one second at exit) and a failure is never shown.
- **Error messages** are scrubbed before they are sent, and cut to 200
  characters: emails become `<email>`; URLs, hostnames and IP addresses
  `<host>`; file paths `<path>`; quoted strings and the names after "user",
  "role", "as", "database", "account", "warehouse" and the like `<name>`;
  UUIDs, hex ids and long numbers `<id>`; anything token-like `<secret>`.
  Every value you typed or passed (connection fields, email, folders) is
  removed as well, whether or not it looks like one of those.

Every event carries `cli_version`, `os`, `arch`, `mode` (`tui`, `plain` or
`scripted`), the install id as `distinct_id`, and `$process_person_profile:
false`, `$ip: null`, `$geoip_disable: true`. `agent` is always an agent id
from `benchouse agents` (or `other`, or `unknown` for a bundle of an unknown
agent). The events:

| Event | Properties |
|---|---|
| `cli_started` | `command`: `guided`, `upload`, `collect`, `send`, `inspect`, `plan` or `agents` |
| `cli_agent_selected` | `agent` (`other` for Other) |
| `cli_other_requested` | `agent_text_length`: the length of the name typed (the name itself goes only to the agent request) |
| `cli_other_email_given` | `email_given`: true or false |
| `cli_step_viewed` | `agent`, `step` (`conversations`, `dbt`, `context`, `review`, `summary`, `uploading`, `done`), `step_index`, `step_count` |
| `cli_field_failed` | `agent`, `field` (the field's flag name, e.g. `host`), `kind` (`validate` or `verify`), `category`, `message` (scrubbed) |
| `cli_field_verified` | `agent`, `field` |
| `cli_check_finished` | `agent`, `step` (`conversations`, `dbt`, `context`), `ok`, `category` (empty when ok), `message` (scrubbed, only when it failed), `warnings` (a count), `duration_ms` |
| `cli_collected` | `agent`, `conversations`, `records`, `sampled`, `files_dbt`, `files_context`, `size_bucket` (`<1MB`, `1-5MB`, `5-32MB`, `>32MB`), `duration_ms`, `include_results`, `include_users`, `anonymize` |
| `cli_collect_failed` | `agent`, `category`, `message` (scrubbed) |
| `cli_uploaded` | `agent`, `size_bucket` |
| `cli_upload_failed` | `agent`, `http_status` (0 when no answer came), `category` |
| `cli_saved` | `agent` |
| `cli_quit` | `last_step` (`start`, `agent`, `other` or a step above), `how` (`done`, `ctrl_c`, `error`, `signal`), `agent` (empty before one is picked) |

`category` is one of `auth_failed`, `permission_denied`, `not_found`,
`not_configured`, `network`, `tls`, `timeout`, `rate_limited`,
`invalid_input`, `server_error`, `canceled`, `unknown`.

`step_index` is the step's position in conversations, dbt, context (for
agents with a context folder), review, summary, uploading (for an upload),
done; `step_count` is the length of that list.

Development setting: `BENCHOUSE_TELEMETRY_ENDPOINT` sends the stats to
another URL (a local test server, say). Test binaries send nothing unless
it is set.

## Privacy

What Benchouse does with an upload: [benchouse.ai/privacy](https://benchouse.ai/privacy).
Questions: andre@benchouse.ai.

## Contributing

Adding an agent is one package: [docs/adding-an-agent.md](adding-an-agent.md).

```sh
go test ./... && go vet ./...
```

MIT licensed.
