package pgread

import "testing"

func TestBatchedAndPlanned(t *testing.T) {
	stmt := `SELECT * FROM m WHERE created_at > $1 AND c IN ({ids})`
	if got := Batched(stmt, 2, 3); got != `SELECT * FROM m WHERE created_at > $1 AND c IN ($2, $3, $4)` {
		t.Errorf("Batched = %s", got)
	}
	if got := Planned(stmt, "thread"); got != `SELECT * FROM m WHERE created_at > $1 AND c IN (<up to 200 thread ids>)` {
		t.Errorf("Planned = %s", got)
	}
}
