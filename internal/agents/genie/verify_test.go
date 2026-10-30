package genie

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

func TestWarehouseIDFromAPath(t *testing.T) {
	for in, want := range map[string]string{
		"a1b2c3d4e5f67890":                       "a1b2c3d4e5f67890",
		" /sql/1.0/warehouses/a1b2c3d4e5f67890 ": "a1b2c3d4e5f67890",
		"/sql/1.0/endpoints/abc123":              "abc123",
		"jdbc:databricks://x:443/default;transportMode=http;httpPath=/sql/1.0/warehouses/abc123;AuthMech=3": "abc123",
	} {
		if got := warehouseID(in); got != want {
			t.Errorf("warehouseID(%q) = %q, want %q", in, got, want)
		}
		if err := validWarehouse(in); err != nil {
			t.Errorf("validWarehouse(%q) = %v", in, err)
		}
	}
	for _, bad := range []string{"my warehouse", "a/b", "../x", "id?x=1"} {
		if validWarehouse(bad) == nil {
			t.Errorf("validWarehouse(%q) passed", bad)
		}
	}
}

// fakeVerify answers the field checks: a good token, a warehouse list of
// two pages, one stopped warehouse, one readable space.
func fakeVerify(t *testing.T) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer dapi-GOOD" {
			http.Error(w, `{"error_code":"401","message":"Invalid access token"}`, http.StatusUnauthorized)
			return
		}
		switch p := r.URL.Path; {
		case p == mePath:
			w.Write([]byte(`{"userName":"ann@shop.test"}`))
		case p == "/api/2.0/sql/warehouses" && r.URL.Query().Get("page_token") == "":
			w.Write([]byte(`{"warehouses":[{"id":"wh1","name":"Starter","state":"RUNNING","cluster_size":"Small"},{"id":"bad/id","name":"x"}],"next_page_token":"p2"}`))
		case p == "/api/2.0/sql/warehouses":
			w.Write([]byte(`{"warehouses":[{"id":"wh2","name":"Nightly","state":"STOPPED","cluster_size":"2X-Small"}]}`))
		case p == warehousePath+"wh2":
			w.Write([]byte(`{"id":"wh2","name":"Nightly","state":"STOPPED"}`))
		case p == spacePath+sp:
			w.Write([]byte(`{"space_id":"` + sp + `","title":"Orders"}`))
		default:
			http.Error(w, `{"error_code":"RESOURCE_DOES_NOT_EXIST"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestFieldVerifies(t *testing.T) {
	srv, _ := fakeVerify(t)
	ctx := context.Background()
	in := agent.Inputs{"host": srv.URL, "token": "dapi-GOOD"}

	fact, err := verifyToken(ctx, in)
	if err != nil || !strings.Contains(fact, "signed in to "+srv.URL+" as ann@shop.test") {
		t.Errorf("token: %q, %v", fact, err)
	}
	_, err = verifyToken(ctx, agent.Inputs{"host": srv.URL, "token": "dapi-WRONG"})
	if err == nil || !strings.Contains(err.Error(), "token is valid") || strings.Contains(err.Error(), "dapi-WRONG") {
		t.Errorf("bad token: %v", err)
	}

	list, err := Warehouses(ctx, in)
	if err != nil || len(list) != 2 || list[0] != [2]string{"wh1", "Starter (RUNNING, Small)"} || list[1][0] != "wh2" {
		t.Errorf("warehouses = %v, %v", list, err)
	}

	in["warehouse-id"] = "/sql/1.0/warehouses/wh2"
	fact, err = verifyWarehouse(ctx, in)
	if err != nil || !strings.HasPrefix(fact, "SQL warehouse Nightly (STOPPED): ") || !strings.Contains(fact, "starts it") {
		t.Errorf("warehouse: %q, %v", fact, err)
	}
	in["warehouse-id"] = "nope"
	if _, err := verifyWarehouse(ctx, in); err == nil || !strings.Contains(err.Error(), "CAN USE") {
		t.Errorf("missing warehouse: %v", err)
	}

	if fact, err := verifySpace(ctx, in); fact != "" || err != nil {
		t.Errorf("no space: %q, %v", fact, err)
	}
	in["space-id"] = sp
	if fact, err := verifySpace(ctx, in); err != nil || fact != `Genie space "Orders" (`+sp+`)` {
		t.Errorf("space: %q, %v", fact, err)
	}
	in["space-id"] = "0000"
	if _, err := verifySpace(ctx, in); err == nil || !strings.Contains(err.Error(), "CAN VIEW") {
		t.Errorf("unreadable space: %v", err)
	}
}

// The fields carry the checks, so the guided run uses them.
func TestGenieFieldsVerify(t *testing.T) {
	a, _ := agent.Get("databricks-genie")
	for _, f := range a.Fields {
		switch f.Key {
		case "token", "warehouse-id", "space-id":
			if f.Verify == nil {
				t.Errorf("%s has no Verify", f.Key)
			}
		}
		if f.Key == "warehouse-id" && f.Lookup == nil {
			t.Error("warehouse-id has no list")
		}
	}
}

// In the guided run the warehouses are listed only once the token has
// been verified, and the chosen warehouse is then checked.
func TestGuidedRunListsWarehousesAfterTheToken(t *testing.T) {
	srv, seen := fakeVerify(t)
	a, _ := agent.Get("databricks-genie")
	tm := teatest.NewTestModel(t, tui.New(context.Background(), tui.Config{Agents: []agent.Agent{a}, Agent: &a}),
		teatest.WithInitialTermSize(110, 45))
	wait := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
			teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
	}
	wait("Genie's history is split")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	wait("Databricks workspace URL")
	tm.Type(srv.URL)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	time.Sleep(50 * time.Millisecond)
	tm.Type("dapi-WRONG")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	wait("Check the workspace URL and that the token is valid")
	if len(*seen) != 1 || (*seen)[0] != mePath {
		t.Fatalf("requests after a refused token: %v", *seen)
	}
	for range len("WRONG") {
		tm.Send(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	tm.Type("GOOD")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	wait("Starter (RUNNING, Small)")
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	wait("Genie space")
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	want := []string{mePath, mePath, "/api/2.0/sql/warehouses", "/api/2.0/sql/warehouses", warehousePath + "wh2"}
	if got := *seen; len(got) < len(want) || strings.Join(got[:len(want)], " ") != strings.Join(want, " ") {
		t.Errorf("requests = %v, want %v first", got, want)
	}
}
