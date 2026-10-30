package snowflake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// The account becomes the host the token is sent to, so only an account
// identifier is accepted.
func TestBaseValidatesTheAccount(t *testing.T) {
	for account, want := range map[string]string{
		"myorg-myaccount":                                 "https://myorg-myaccount.snowflakecomputing.com",
		" MyOrg-MyAccount.snowflakecomputing.com":         "https://myorg-myaccount.snowflakecomputing.com",
		"xy12345.eu-west-1.aws":                           "https://xy12345.eu-west-1.aws.snowflakecomputing.com",
		"my_org-acct":                                     "https://my_org-acct.snowflakecomputing.com",
		"https://myorg-myaccount.snowflakecomputing.com/": "https://myorg-myaccount.snowflakecomputing.com",
	} {
		c := New(Config{Account: account}, "pat")
		if got, err := c.base(); err != nil || got != want {
			t.Errorf("base(%q) = %q, %v", account, got, err)
		}
	}
	for _, account := range []string{"", "evil.tld/x?", "a@evil.tld#", "evil.tld:443", "a b", "..", "a..b", "x/", "https://evil.tld/x"} {
		c := New(Config{Account: account}, "pat")
		if got, err := c.base(); err == nil {
			t.Errorf("base(%q) = %q, want an error", account, got)
		}
	}
}

func TestRedirectsAndBadHandlesAreRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("token sent to another host: %q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.RedirectHandler(other.URL+"/api/v2/statements", http.StatusTemporaryRedirect))
	defer redirect.Close()
	c := New(Config{Account: "a"}, "pat")
	c.BaseURL = redirect.URL
	if _, err := c.Query(context.Background(), "SELECT 1"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect: err = %v", err)
	}

	var calls int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"statementHandle": "../../x?y"})
	}))
	defer bad.Close()
	c.BaseURL = bad.URL
	if _, err := c.Query(context.Background(), "SELECT 1"); err == nil || !strings.Contains(err.Error(), "handle") {
		t.Errorf("bad handle: err = %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d", calls)
	}
}

// A large result is read one partition at a time: 200,000 rows of ~300
// bytes (60 MB of JSON) over 40 partitions must never be held at once.
func TestEachJSONHoldsOnePartitionAtATime(t *testing.T) {
	const partitions, perPartition = 40, 5000
	pad := strings.Repeat("x", 256)
	page := func(p int) [][]*string {
		rows := make([][]*string, perPartition)
		for i := range rows {
			s := fmt.Sprintf(`{"ID":%d,"PAD":"%s"}`, p*perPartition+i, pad)
			rows[i] = []*string{&s}
		}
		return rows
	}
	var fetched []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := 0
		if v := r.URL.Query().Get("partition"); v != "" {
			fmt.Sscan(v, &p)
		}
		fetched = append(fetched, p)
		info := make([]map[string]int, partitions)
		for i := range info {
			info[i] = map[string]int{"rowCount": perPartition}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"statementHandle":   "01b2-c3",
			"resultSetMetaData": map[string]any{"rowType": []map[string]string{{"name": "ROW_JSON"}}, "partitionInfo": info},
			"data":              page(p),
		})
	}))
	defer srv.Close()
	c := New(Config{Account: "a"}, "pat")
	c.BaseURL = srv.URL

	var peak uint64
	var ms runtime.MemStats
	seen := 0
	n, err := c.EachJSON(context.Background(), "SELECT 1", func(row json.RawMessage) error {
		var v struct{ ID int }
		if err := json.Unmarshal(row, &v); err != nil || v.ID != seen {
			return fmt.Errorf("row %d out of order: %s", seen, row)
		}
		seen++
		if seen%perPartition == perPartition/2 { // mid-partition: one page live
			runtime.GC()
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak {
				peak = ms.HeapAlloc
			}
			// The next partition is not fetched before this one is done.
			if got := len(fetched); got != seen/perPartition+1 {
				return fmt.Errorf("%d partitions fetched while reading partition %d", got, seen/perPartition)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != partitions*perPartition {
		t.Fatalf("rows = %d", n)
	}
	// One partition is ~1.5 MB of JSON, ~3 MB decoded; all of them ~120 MB.
	if peak > 32<<20 {
		t.Errorf("heap peaked at %d MiB: rows are being accumulated", peak>>20)
	}
}
