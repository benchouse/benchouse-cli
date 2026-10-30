# turns/v1

The common form for agents without a dedicated Benchouse connector: one row
per turn (a question and the agent's answer), with the agent's steps as JSON
arrays on the same row. It is the same contract Benchouse's live "custom
agent" connection reads, so an uploaded turn is analysed identically.

In a bundle a turn is a record with `source_kind: "custom_sql"`,
`record_kind: "sql_turn"` and this payload:

```json
{
  "contract": "turns/v1",
  "columns": ["turn_id", "asked_at", "question_text", "answer_text", "statements"],
  "row": {
    "turn_id": "101",
    "asked_at": "2026-10-02T12:00:00Z",
    "question_text": "Orders last week?",
    "answer_text": "1,204 orders.",
    "statements": [{"sql": "SELECT count(*) FROM orders WHERE order_week = 39", "status": "succeeded", "rows": 1}]
  }
}
```

`columns` lists what the agent records. A column it never records is absent;
a column it records but a turn lacks is `null` (or `[]` for an array).
Timestamps are ISO 8601 in UTC.

## Columns

| Column | Type | Required | Meaning |
|---|---|---|---|
| `turn_id` | text | yes | stable unique id |
| `asked_at` | timestamp | yes | when the question was asked |
| `question_text` | text | yes | the user's question |
| `answer_text` | text | yes, nullable | the answer the user saw; null after an error |
| `updated_at` | timestamp | | when the turn last changed |
| `completed_at` | timestamp | | when the answer finished (latency) |
| `outcome` | text | | `completed`/`success`/`ok`, `failed`/`error`, `running` |
| `error_type`, `error_message` | text | | if it failed |
| `conversation_id` | text | | groups turns into a conversation |
| `position` | integer | | position in the conversation, from 1 |
| `user_id`, `user_name`, `user_email`, `user_role` | text | | who asked |
| `client_interface` | text | | `web`, `slack`, `api`, `mcp`, ... |
| `session_id` | text | | client session |
| `model_name` | text | | the model that answered |
| `input_tokens`, `output_tokens` | integer | | token usage |
| `semantic_model_ref` | text | | the semantic model or layer used |
| `statements` | JSON array | | SQL generated or run |
| `reasoning` | JSON array | | reasoning steps |
| `messages` | JSON array | | anything else the agent said |
| `tool_calls` | JSON array | | other tools used |
| `extra` | JSON object | | `declined` (bool), `missing_concept` (text), anything else |

### Array elements

- `statements`: `sql` (required), `status` (`succeeded`, `failed`,
  `not_executed`), `error`, `rows`, `started_at`, `ended_at`, `is_answer`,
  `result` (a sample of returned rows; the CLI omits it unless
  `--include-results`).
- `reasoning`: `text` (required), `label` (`restatement`, `assumption`,
  `data_sourcing`, `plan`, `deliberation`, or other).
- `messages`: `text` (required), `kind` (`clarifying_question`,
  `suggested_questions`, `visualization`, `restatement`, or other).
- `tool_calls`: `name` (required), `input`, `output`, `status`, `started_at`,
  `ended_at`.

`extra.declined: true` marks a turn the agent declined to answer; Benchouse
then counts it against completeness instead of inferring a refusal from the
answer text.
