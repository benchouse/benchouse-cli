package turns

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRecordWritesTheDeclaredColumnsOnly(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.FixedZone("x", 3600))
	cols := NewColumns(ColStatements, ColConversationID, ColModelName)
	r, err := Record(Turn{
		TurnID: "t1", AskedAt: at, Question: "Orders last week?", Answer: Ptr("1,204 orders."),
		ConversationID: "c1", UserEmail: "dropped@x.test",
		Statements: []Statement{{SQL: "select 1", Status: "succeeded"}, {SQL: "  "}},
	}, cols, "sql:x")
	if err != nil {
		t.Fatal(err)
	}
	if r.SourceKind != SourceKind || r.RecordKind != RecordKind || r.NativeIDs["conversation_id"] != "c1" {
		t.Errorf("record = %+v", r)
	}
	var p struct {
		Contract string
		Columns  []string
		Row      map[string]any
	}
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Contract != Contract || strings.Join(p.Columns, ",") != "turn_id,asked_at,question_text,answer_text,conversation_id,model_name,statements" {
		t.Errorf("columns = %v", p.Columns)
	}
	if p.Row["asked_at"] != "2026-10-02T11:00:00Z" {
		t.Errorf("asked_at = %v, want UTC", p.Row["asked_at"])
	}
	if _, ok := p.Row["user_email"]; ok {
		t.Error("an undeclared column was written")
	}
	if v, ok := p.Row["model_name"]; !ok || v != nil {
		t.Errorf("a declared column with no value must be null, got %v (present %v)", v, ok)
	}
	if st := p.Row["statements"].([]any); len(st) != 1 {
		t.Errorf("blank statements kept: %v", st)
	}
}

func TestEmptyArraysAreRecordedAsEmpty(t *testing.T) {
	r, _ := Record(Turn{TurnID: "t", AskedAt: time.Now(), Question: "q"}, NewColumns(ColStatements), "x")
	if !strings.Contains(string(r.Payload), `"statements":[]`) {
		t.Errorf("payload = %s", r.Payload)
	}
}

func TestRecordRefusesIncompleteTurns(t *testing.T) {
	for name, tr := range map[string]Turn{
		"no id":       {AskedAt: time.Now(), Question: "q"},
		"no time":     {TurnID: "t", Question: "q"},
		"no question": {TurnID: "t", AskedAt: time.Now()},
	} {
		if _, err := Record(tr, NewColumns(), "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
