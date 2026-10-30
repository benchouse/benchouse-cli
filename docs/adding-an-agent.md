# Adding an agent

An agent is one package under `internal/agents/<id>/` that calls
`agent.Register` from `init`, plus one import line in
`internal/agents/agents.go`. Nothing else in the CLI changes.

## What to register

```go
agent.Register(agent.Agent{
    ID:      "lightdash",            // lowercase, dashes; the --agent value
    Name:    "Lightdash AI",
    Summary: "one line for `benchouse agents`",
    Status:  agent.Experimental,     // Supported only when the statements are ones Benchouse runs in production
    Setup:   "what access is needed, and the least-privilege grant",
    Fields:  []agent.Field{...},     // connection inputs → flags, env vars and prompts
    Context: &agent.Context{...},    // the agent's semantic layer / context folder (with its own Check), or nil
    Plan:    plan,                   // the exact statements or API calls, for review before connecting
    Check:   check,                  // verifies the inputs cheaply, right after the user gives them
    Collect: collect,                // runs them, emitting each record as it is read
})
```

`Collect` has the signature
`func(ctx, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error)`.
It calls `emit` once per record as rows arrive and keeps none of them: the
CLI pseudonymises each record and appends it to a temp file before the
next is read, so a collection's size never reaches memory. An error from
`emit` stops the collection; return it. Tests use `agent.CollectAll` to get
the records back as a slice.

## Rules

1. **Read only.** SELECTs and GETs. Postgres connections set
   `default_transaction_read_only=on`; sqlite opens `mode=ro`.
2. **Plan is the truth.** Every statement or request `Collect` makes must
   appear in `Plan` and in `Result.Queries`, verbatim, with row counts.
3. **Choose the record form.**
   - The agent has a dedicated Benchouse connector (nao, Cortex, Genie): emit
     its native record kinds with the rows verbatim.
   - Otherwise: map each question/answer to a `turns.Turn` and emit
     `turns.Record` (contract: [turns-v1.md](turns-v1.md)). Declare the
     columns the agent actually records with `turns.NewColumns(...)`; a column
     it never records is left out, not filled with nulls.
4. **No business data by default.** Rows returned by the agent's queries are
   dropped unless `opts.IncludeResults`; keep columns and row counts. Add a
   line to `Result.Redactions` saying what was removed.
5. **No people by default.** User directories (names, emails) only with
   `opts.IncludeUsers`. A user id on a turn is fine.
6. **Optional sources degrade.** A table an older version lacks, or a view the
   role cannot read, becomes a `Result.Notes` entry, not a failure. Fail only
   when nothing conversational could be read.
7. **Honest status.** `Experimental` means written from the vendor's published
   schema, source or API docs and tested against fixtures only. Say in the
   package doc which schema version / source file the reader follows.
8. **Bounded.** Collect at most `opts.MaxConversations` conversations (0 =
   no cap), the most recent in the `Since` window, whichever limit is hit
   first. Choose the conversation ids first (`ORDER BY <last activity> DESC
   LIMIT n`), then fetch their children `agent.BatchSize` (200) ids at a
   time with bound parameters (`IN (?, ?, ...)`), never one giant join or
   an unpaged read of the customer's database. Rows not tied to a
   conversation (usage, logs) are read with a window and a LIMIT
   proportional to the cap, with a note when it is hit. Stream rows with an
   iterator (`pgread.Each`, `snowflake.Client.EachJSON`), not a slice. Set
   `Result.Sampled`, `Result.Conversations` and, when a count is cheap,
   `Result.InWindow`: they become the manifest's `sample`. In `Plan` and
   `Result.Queries` a batched statement appears once with its id list as
   `<up to 200 ... ids>`; record each run with `Result.Tally`, which sums
   rows and counts batches. A run with no cap must stay bounded too: keep
   the chosen ids in an `agent.IDSpool` (spills to a temp file past 10,000)
   or page through the conversations by keyset (`(last, id) < (?, ?)`,
   `agent.BatchSize` at a time), fetching each page's children before the
   next. When children must be joined into one record (a turn), hold one
   batch at a time, never the whole collection.
9. **Tests** build a fixture shaped like the real storage (a sqlite file, an
   `httptest` server) and check counts, redaction, the `Since` window, and
   that `Plan` matches what ran.
10. **Check before collecting.** `Check` connects with the inputs and proves
   every required source is readable, cheaply: Collect's statement under
   `LIMIT 0` (`pgread.LimitZero`), a `count(*)` bounded by the `Since`
   window, or one API page with a tiny limit. A required source that fails
   is an error naming the grant, table or flag to fix (reuse `Setup`); an
   optional one is a `Warning` saying what will be missing. `Facts` are short
   and concrete: what it connected to (engine, version, host, user), the
   entities it found (project, space, role, warehouse), and how much the
   window holds when that is cheap (`agent.Conversations` adds "the 2,000
   most recent will be sent" when the count is over the cap). Never put a secret in a fact, warning or
   error. Share the connection code with `Collect`. A `Context` gets a
   `Check` too, saying what the folder holds and failing when it does not
   look like the agent's context; `files.Scan` counts what the keep rule
   would include without reading a file. The dbt folder has
   `files.CheckDBT`.
