package anon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/bundle"
)

func rec(kind, payload string, ids map[string]string) bundle.Record {
	return bundle.Record{SourceKind: "x", RecordKind: kind, NativeIDs: ids, Payload: json.RawMessage(payload)}
}

func TestPeopleArePseudonymisedAndChatsAreNot(t *testing.T) {
	h := New([]byte("k"))
	cases := []bundle.Record{
		// nao chat: user_id is a person, title is chat content.
		rec("nao_chat", `{"id":"c1","user_id":"u-ann","title":"Ann asked about ann@shop.test orders"}`, map[string]string{"chat_id": "c1"}),
		// Cortex span: dotted user attributes.
		rec("span", `{"RECORD_ATTRIBUTES":{"snow.ai.observability.user.name":"ANN","snow.ai.observability.role.name":"ANALYST","snow.ai.observability.agent.response":"There are 42."}}`, nil),
		// Genie audit: a person object.
		rec("audit_event", `{"user_identity":{"email":"ann@shop.test","subject_name":"Ann"},"source_ip_address":"203.0.113.7","service_name":"aibiGenie"}`, nil),
		// turns/v1 row.
		rec("sql_turn", `{"contract":"turns/v1","columns":["turn_id","user_id","user_email","question_text"],"row":{"turn_id":"t1","user_id":7,"user_email":"Ann@Shop.test","question_text":"orders for ann@shop.test?"}}`, map[string]string{"turn_id": "t1"}),
		// Cube turn: the embedding app's id for the person, often an email.
		rec("sql_turn", `{"contract":"turns/v1","columns":["turn_id"],"row":{"turn_id":"t2","extra":{"external_user_id":"ann@shop.test"}}}`, map[string]string{"turn_id": "t2"}),
		// Snowflake usage: the person is in the native ids too.
		rec("analyst_billing", `{"USERNAME":"ANN","CREDITS":1.5}`, map[string]string{"username": "ANN", "start_time": "2026-10-01"}),
		// Genie statement history: executed_as is an email, executed_by_user_id an id.
		rec("statement_history", `{"executed_as":"ann@shop.test","executed_by_user_id":"9911","statement_text":"select 1"}`, nil),
	}
	var all string
	for _, r := range cases {
		out, err := h.Record(r)
		if err != nil {
			t.Fatal(err)
		}
		all += string(out.Payload)
		for k, v := range out.NativeIDs {
			all += k + "=" + v + ";"
		}
	}
	for _, gone := range []string{`"u-ann"`, `"ANN"`, `"Ann"`, `"ann@shop.test"`, `"Ann@Shop.test"`, `"user_id":7`, "username=ANN", "203.0.113.7", `"9911"`} {
		if strings.Contains(all, gone) {
			t.Errorf("%s survived:\n%s", gone, all)
		}
	}
	for _, kept := range []string{"Ann asked about ann@shop.test orders", "There are 42.", "ANALYST", "aibiGenie",
		"orders for ann@shop.test?", `"turn_id":"t1"`, "chat_id=c1", "select 1", "start_time=2026-10-01", `"CREDITS":1.5`} {
		if !strings.Contains(all, kept) {
			t.Errorf("lost %q:\n%s", kept, all)
		}
	}
	// One person, one pseudonym, whatever the case or field.
	p := h.Person("ann@shop.test")
	if strings.Count(all, p) != 4 || h.Person(" Ann@Shop.TEST ") != p {
		t.Errorf("pseudonym %s not stable across fields:\n%s", p, all)
	}
}

func TestDirectoryRowsArePseudonymisedWhole(t *testing.T) {
	h := New([]byte("k"))
	out, err := h.Record(rec("directory_user", `{"id":"u-ann","name":"Ann Lee","email":"ann@shop.test"}`, map[string]string{"user_id": "u-ann"}))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out.Payload); strings.Contains(s, "Ann") || strings.Contains(s, "ann@") {
		t.Errorf("directory leaked: %s", s)
	}
	// The directory id and a chat's user_id map to the same pseudonym.
	if out.NativeIDs["user_id"] != h.Person("u-ann") {
		t.Errorf("native id = %s", out.NativeIDs["user_id"])
	}
}

func TestKeyIsCreatedOnceAndPrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("APPDATA", filepath.Join(dir, "cfg")) // os.UserConfigDir on Windows
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if a.Person("x") != b.Person("x") {
		t.Error("a second run made a new key")
	}
	cfg, _ := os.UserConfigDir()
	st, err := os.Stat(filepath.Join(cfg, "benchouse", "anonymize.key"))
	if err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Errorf("key file: %v, %v", st, err)
	}
	if New([]byte("other")).Person("x") == a.Person("x") {
		t.Error("pseudonyms do not depend on the key")
	}
}

func TestAKeyOfTheWrongLengthIsAnErrorNotALoop(t *testing.T) {
	p := filepath.Join(t.TempDir(), "benchouse", "anonymize.key")
	os.MkdirAll(filepath.Dir(p), 0o700)
	for _, size := range []int{0, 31, 33} {
		os.WriteFile(p, make([]byte, size), 0o600)
		done := make(chan error, 1)
		go func() { _, err := loadFrom(p); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "delete it") {
				t.Errorf("%d bytes: err = %v", size, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%d bytes: Load did not return", size)
		}
	}
}

func TestKeyCreationLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "anonymize.key")
	a, err := loadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadFrom(p)
	if err != nil || a.Person("x") != b.Person("x") {
		t.Fatalf("second load: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("dir holds %d files, want only the key", len(ents))
	}
}
