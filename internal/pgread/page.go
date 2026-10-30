package pgread

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Batched reads: an application's conversations are chosen newest first, a
// page of agent.BatchSize at a time (keyset paging, so no page re-reads the
// table and nothing but one page of ids is ever held), and each page's
// children are fetched with the page's ids as bound parameters.

// IDsToken marks where a batched statement's id list goes.
const IDsToken = "{ids}"

// Placeholders renders n numbered parameters starting at $first:
// "$2, $3, $4".
func Placeholders(first, n int) string {
	if n < 1 {
		n = 1
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("$%d", first+i)
	}
	return strings.Join(parts, ", ")
}

// Batched is stmt with its IDsToken replaced by n parameters from $first.
func Batched(stmt string, first, n int) string {
	return strings.Replace(stmt, IDsToken, Placeholders(first, n), 1)
}

// Planned is stmt as a plan and a manifest show it: the id list reads
// "<up to 200 <noun> ids>".
func Planned(stmt, noun string) string {
	return strings.Replace(stmt, IDsToken, fmt.Sprintf("<up to %d %s ids>", agent.BatchSize, noun), 1)
}

// FirstCursor is the keyset cursor of the first page: after every real
// activity time, so the id half is never compared.
var FirstCursor = []any{"infinity", ""}

// EachPage chooses conversations with stmt, which takes $1 the Since bound,
// $2 and $3 the keyset cursor (last activity, id as text) and $4 the page
// size, and returns columns id and last_at, newest first:
//
//	... WHERE (last_at < $2 OR (last_at = $2 AND id::text < $3))
//	ORDER BY last_at DESC, id::text DESC LIMIT $4
//
// fn gets each page's ids, at most agent.BatchSize, before the next page is
// read. It stops after max conversations (0 = no cap). page is called with
// each page's row count, for Result.Tally. It returns how many
// conversations were chosen.
func EachPage(ctx context.Context, q Querier, stmt string, since time.Time, max int,
	page func(rows int), fn func(ids []any) error) (int, error) {
	cursor := append([]any(nil), FirstCursor...)
	total := 0
	for {
		n := agent.BatchSize
		if max > 0 && max-total < n {
			n = max - total
		}
		if n <= 0 {
			return total, nil
		}
		var ids []any
		var last any
		err := q.Each(ctx, stmt, []any{since, cursor[0], cursor[1], n}, func(row json.RawMessage) error {
			var c struct {
				ID   any `json:"id"`
				Last any `json:"last_at"`
			}
			if err := Decode(row, &c); err != nil {
				return err
			}
			id := fmt.Sprint(c.ID)
			if c.ID == nil || id == "" {
				return nil
			}
			ids = append(ids, id)
			last = c.Last
			return nil
		})
		if err != nil {
			return total, err
		}
		page(len(ids))
		if len(ids) == 0 {
			return total, nil
		}
		total += len(ids)
		if err := fn(ids); err != nil {
			return total, err
		}
		if len(ids) < n {
			return total, nil
		}
		cursor = []any{last, ids[len(ids)-1]}
	}
}
