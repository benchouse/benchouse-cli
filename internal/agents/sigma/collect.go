package sigma

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

const sourceKind = "sigma"

var errEmit = errors.New("writing records")

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	res := agent.Result{Sampled: opts.MaxConversations > 0}
	b, err := open(in)
	if err != nil {
		return res, err
	}
	record := func(purpose, stmt string, rows int) {
		res.Queries = append(res.Queries, bundle.Query{Purpose: purpose, Statement: stmt, Rows: rows})
	}
	l, candidates, err := discover(ctx, b, in, record)
	if err != nil {
		return res, err
	}
	if len(candidates) > 1 {
		res.Notes = append(res.Notes, fmt.Sprintf("several AI usage tables (%s): read %s; give --table to choose", strings.Join(candidates, ", "), l.name))
	}
	if a := l.absent(); len(a) > 0 {
		res.Notes = append(res.Notes, "the AI usage table lacks documented columns, left empty: "+strings.Join(a, ", "))
	}
	if u := l.unmapped(); len(u) > 0 {
		res.Notes = append(res.Notes, "AI usage table columns not mapped (not collected): "+strings.Join(u, ", "))
	}

	if opts.MaxConversations > 0 {
		stmt := l.countStmt(opts)
		var n int64
		var counted bool
		_, err := b.each(ctx, stmt, nil, func(m map[string]any) error {
			if f, ok := number(m["n"]); ok {
				n, counted = int64(f), true
			}
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("count the conversations in %s: %s", l.ref(), agent.Clip(err))
		}
		record(purposeCount, stmt, 1)
		if counted {
			res.InWindow = &n
			if n > int64(opts.MaxConversations) {
				res.Notes = append(res.Notes, fmt.Sprintf("%s Sigma AI conversations in the window: the %s most recent were read",
					agent.Thousands(n), agent.Thousands(int64(opts.MaxConversations))))
			}
		}
	}

	var ids agent.IDSpool
	defer ids.Close()
	stmt := l.candidatesStmt(opts)
	n, err := b.each(ctx, stmt, nil, func(m map[string]any) error {
		if id := str(m["conversation_id"]); id != "" {
			return ids.Add(id)
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("choose the conversations in %s: %s", l.ref(), agent.Clip(err))
	}
	record(purposeCandidates, stmt, n)

	mp := &mapper{l: l, includeResults: opts.IncludeResults, includeUsers: opts.IncludeUsers}
	cols := mp.columns()
	fetch := "sql:" + l.ref()
	var skipped int
	if ids.Len() == 0 {
		record(purposeRows, l.plannedBatch(opts), 0)
	}
	err = ids.Batches(agent.BatchSize, func(batch []string) error {
		latest := map[string]span{}
		var keys []string
		rows, err := b.each(ctx, l.batchStmt(opts, len(batch)), batch, func(m map[string]any) error {
			s := newSpan(m)
			k := s.key()
			old, seen := latest[k]
			if !seen {
				keys = append(keys, k)
			}
			if !seen || newer(s, old) {
				latest[k] = s
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("read %s: %s", l.ref(), agent.Clip(err))
		}
		res.Tally(purposeRows, l.plannedBatch(opts), rows)
		byConv := map[string][]span{}
		for _, k := range keys {
			s := latest[k]
			byConv[s.conv] = append(byConv[s.conv], s)
		}
		for _, conv := range batch {
			spans := byConv[conv]
			if len(spans) == 0 {
				continue
			}
			ts := mp.conversation(conv, spans)
			if len(ts) == 0 {
				skipped++
				continue
			}
			res.Conversations++
			for _, t := range ts {
				rec, err := turns.Record(t, cols, fetch)
				if err != nil {
					continue
				}
				if err := emit(rec); err != nil {
					return fmt.Errorf("%w: %w", errEmit, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	if skipped > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%s conversations had no question text (input) in any span and were left out", agent.Thousands(int64(skipped))))
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, "tool call outputs and SQL results (tool_calls outputs, outputs of tool spans) were dropped; "+
			"tool names, inputs and the SQL were kept. Pass --include-results to keep them")
	}
	if l.has(cUserName) && !opts.IncludeUsers {
		res.Redactions = append(res.Redactions, "user_name is kept only as the user id (pseudonymised unless --anonymize-users=false); --include-users adds it as user_name")
	}
	res.Sources = append(res.Sources, bundle.Source{SourceKind: sourceKind, NativeID: l.name, Account: b.account(), Label: "Sigma AI usage " + l.ref()})
	return res, nil
}
