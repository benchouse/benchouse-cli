# Running benchouse as a coding agent

You are helping a user send their analytics agent's history to Benchouse for
review. Follow these rules.

1. **Install** with `go install github.com/benchouse/benchouse-cli/cmd/benchouse@latest`
   or the install script in the README. Check with `benchouse version`.
2. **Find the agent's inputs** with `benchouse agents --json`. Each field has a
   flag, an environment variable and whether it is secret.
3. **Never handle secrets yourself.** Do not ask the user to paste a password
   or token into the chat, and never put one in a command line. Ask them to
   export it in their own shell (`export BENCHOUSE_PASSWORD=...`), or let
   `benchouse` prompt for it in their terminal.
4. **Show the plan first.** Run `benchouse plan --agent <id> ...` and show the
   user the queries before anything connects.
5. **Collect, then show.** Run `benchouse collect --no-input ...`, then
   `benchouse inspect <bundle>` and show the user the summary: record counts,
   files included, files left out, and what was removed.
6. **Upload only with the user's explicit yes**, for this bundle:
   `benchouse send <bundle> --email <their email>`. Do not pass
   `--include-results` or `--include-users` unless the user asked for them.
   People are pseudonymised by default; pass `--anonymize-users=false` only if
   the user explicitly asks to keep real identities.
7. Report the upload reference printed on stdout.

Exit codes: 0 success, 1 error (message on stderr), 2 usage.
