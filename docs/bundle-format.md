# Bundle format (`benchouse-bundle/1`)

A bundle is a gzip-compressed tar archive. `benchouse collect` writes one to
disk; `benchouse upload` writes one and sends it. You can always inspect a
bundle before it leaves your machine:

```sh
benchouse collect --agent nao ... --out bundle.tar.gz
benchouse inspect bundle.tar.gz          # summary
tar -xzf bundle.tar.gz -C ./look         # or just unpack it
```

## Layout

| Entry | Always | What it is |
|---|---|---|
| `manifest.json` | yes, first entry | what is inside and how it was made |
| `records.jsonl` | yes (may be empty) | one captured conversation row per line |
| `dbt/...` | if a dbt folder was given | the dbt project, filtered |
| `context/...` | if a context folder was given | the agent's context or semantic layer, filtered |

No other top-level entries are allowed. Paths are relative, use `/`, and never
contain `..`.

## `manifest.json`

```json
{
  "format": "benchouse-bundle/1",
  "cli_version": "0.1.0",
  "created_at": "2026-10-30T09:00:00Z",
  "agent": "nao",
  "email": "you@company.com",
  "since": "2026-08-01T00:00:00Z",
  "sources": [ { "source_kind": "nao", "native_id": "<nao project id>", "account": "db.internal/nao" } ],
  "sample": {
    "max_conversations": 2000, "conversations": 2000, "in_window": 1000000,
    "since": "2026-08-01T00:00:00Z", "order": "most_recent"
  },
  "records": { "nao_chat": 2000, "nao_chat_message": 9480, "nao_message_part": 42210 },
  "queries": [
    { "purpose": "conversations", "statement": "SELECT * FROM chat WHERE ... ORDER BY updated_at DESC LIMIT ?", "rows": 2000 },
    { "purpose": "messages", "statement": "SELECT * FROM chat_message WHERE chat_id IN (<up to 200 ids>) AND ...", "rows": 9480, "batches": 10 }
  ],
  "files": [
    { "path": "dbt/models/orders.sql", "bytes": 812, "sha256": "…", "masked": 0 }
  ],
  "redactions": [
    "nao message_part.tool_output: query result rows removed; columns and row count kept"
  ],
  "skipped": [ { "path": "dbt/profiles.yml", "reason": "credentials file" } ]
}
```

- `sources` identifies the agents read, in the vendor's own terms (a nao
  project id, a Genie space id, a Snowflake account), so an upload can later
  be matched to a live connection for the same agent. Nothing secret.
- `sample` (optional) says which conversations were collected: the
  `conversations` most recent ones (`order: "most_recent"`) in the window
  starting at `since`, capped at `max_conversations` (0: no cap), whichever
  limit is hit first. `in_window` is how many the window held, when the
  reader counted them; it is absent when unknown, and may be slightly below
  `conversations` if rows arrived between the count and the read. It is
  written only by readers that apply the cap; a bundle without it holds
  everything in the window.
- `queries` lists every statement the CLI ran against your systems, verbatim.
  A statement run once per batch of up to 200 conversation ids appears once,
  its id list shown as `<up to 200 ids>`, with `batches` (how many times it
  ran) and `rows` totalled over every batch.
- `redactions` lists every kind of content the CLI removed or masked.
- `files[].sha256` is the SHA-256 of the file as stored in the bundle (after
  masking).

## `records.jsonl`

One JSON object per line:

```json
{"source_kind":"nao","record_kind":"nao_chat","native_ids":{"chat_id":"c1"},"fetch_method":"sql:chat","cursor_value":"2026-09-01T10:00:00","payload":{ "...": "the row as the source returned it" }}
```

| Field | Meaning |
|---|---|
| `source_kind` | the agent family: `nao`, `snowflake_cortex_analyst`, `snowflake_cortex_agents`, ... |
| `record_kind` | which table or view the row came from, e.g. `nao_chat`, `analyst_request`, `span` |
| `native_ids` | the source's own keys for the row |
| `fetch_method` | `sql:<table>` or `rest:<endpoint>` |
| `cursor_value` | the row's timestamp as the source rendered it, when it has one |
| `payload` | the row, every column, as the source returned it, minus redactions |

`source_kind` and `record_kind` use the same identifiers as a live Benchouse
connection, so an uploaded bundle is analysed exactly like captured telemetry.

## Upload

```
POST https://api.benchouse.ai/v1/uploads
Content-Type: application/gzip
X-Benchouse-Email: you@company.com
X-Benchouse-CLI: 0.1.0

<bundle bytes>
```

- No account or API key is needed. The email is where the findings go.
- Maximum size: 32 MiB. A redacted bundle of the default 2,000 most recent
  conversations is usually a few MB; lower `--max-conversations` or shorten
  `--since` if yours is larger.
- `201 Created` → `{"id": "upl_…", "bytes": 12345}`
- Errors → `{"error": "…"}`: 400 malformed bundle, 403 sent from a browser
  (bundles come from the CLI), 413 too large, 415 not gzip, 429 too many
  uploads from one address, 503 daily capacity reached (with `Retry-After`).
- Every file under `dbt/` and `context/` must be listed in `files` with its
  exact size and SHA-256. `dbt/` is the dbt project root.
