package sigma

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/snowflake"
)

// warehouse is a fake AI usage table behind any of the three SQL APIs: it
// recognises the reader's statements and answers them from rows.
type warehouse struct {
	t      *testing.T
	table  string
	cols   []column
	rows   []map[string]any
	mu     sync.Mutex
	ran    []string
	args   [][]string
	tables []string // what the information schema lists; nil = [table]
}

// usageRows is a small AI usage table: session s1 with two traces (t1 has a
// tool span and an older edit of its root), a legacy row with no session
// (t3), and an older session s2.
func usageRows() []map[string]any {
	return []map[string]any{
		{"ID": "r1", "ROW_VERSION": 1, "trace_id": "t1", "span_id": "a", "parent_id": nil, "span_type": "generation",
			"session_id": "s1", "input": `{"question":"Orders last week?"}`, "output": `{"result":"OLD"}`,
			"trace_created_at_utc": "2026-10-02 12:00:00.000", "user_name": "ana@example.com"},
		{"ID": "r1", "ROW_VERSION": 2, "trace_id": "t1", "span_id": "a", "parent_id": nil, "span_type": "generation",
			"session_id": "s1", "input": `{"question":"Orders last week?"}`, "output": `{"result":"1,204 orders."}`,
			"model": "gpt-5", "ai_provider": "openai", "agent_name": "Sales agent", "tokens_input": 100, "tokens_output": 20,
			"tool_calls": `[{"name":"run_sql","input":{"sql":"SELECT count(*) FROM orders WHERE week = 39"},"output":[{"count":1204}],"status":"success"},` +
				`{"name":"search_docs","input":{"query":"what is an order"},"output":"docs"}]`,
			"metadata": `{"teams":["sales"]}`, "trace_created_at_utc": "2026-10-02 12:00:00.000", "user_name": "ana@example.com",
			"custom_col": "x"},
		{"ID": "r2", "ROW_VERSION": 1, "trace_id": "t1", "span_id": "b", "parent_id": "a", "span_type": "tool",
			"session_id": "s1", "input": `{"sql":"select id from orders limit 5"}`, "output": "5 rows", "tokens_input": 7,
			"trace_created_at_utc": "2026-10-02 12:00:03.000", "user_name": "ana@example.com"},
		{"ID": "r3", "ROW_VERSION": 1, "trace_id": "t2", "span_id": "c", "span_type": "generation", "session_id": "s1",
			"input": "And the week before?", "output": "1,100 orders.", "trace_created_at_utc": "2026-10-02 12:05:00.000",
			"user_name": "ana@example.com"},
		{"ID": "r4", "ROW_VERSION": 1, "trace_id": "t3", "span_id": "t3", "span_type": "generation", "session_id": nil,
			"input": `{"question":"Revenue?"}`, "output": `{"result":"$5"}`, "metadata": `[{"key":"Error","value":"timeout"}]`,
			"trace_created_at_utc": "2026-10-01 08:00:00.000", "user_name": "bo@example.com"},
		{"ID": "r5", "ROW_VERSION": 1, "trace_id": "t4", "span_id": "d", "span_type": "generation", "session_id": "s2",
			"input": `{"question":"Churn?"}`, "output": `{"result":"2%"}`, "trace_created_at_utc": "2026-09-01 08:00:00.000",
			"user_name": "bo@example.com"},
	}
}

func allColumns() []column {
	var cs []column
	for _, c := range append(append([]string{}, documented...), "custom_col") {
		typ := "TEXT"
		if c == cAt {
			typ = "TIMESTAMP_NTZ"
		}
		cs = append(cs, column{Name: c, Type: typ})
	}
	return append(cs, column{Name: "ID", Type: "TEXT"}, column{Name: "ROW_VERSION", Type: "NUMBER"})
}

func newWarehouse(t *testing.T) *warehouse {
	return &warehouse{t: t, table: "AI_USAGE_ACME", cols: allColumns(), rows: usageRows()}
}

var (
	sinceLit = regexp.MustCompile(`'(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)(\+00)?'`)
	limitLit = regexp.MustCompile(` LIMIT (\d+)`)
)

func convOf(r map[string]any) string {
	if s, _ := r["session_id"].(string); s != "" {
		return s
	}
	return r["trace_id"].(string)
}

func atOf(r map[string]any) time.Time {
	t, _ := parseTime(r["trace_created_at_utc"])
	return t
}

// answer returns the JSON rows for one statement and its bound args.
func (w *warehouse) answer(stmt string, args []string) []string {
	w.mu.Lock()
	w.ran = append(w.ran, stmt)
	w.args = append(w.args, args)
	w.mu.Unlock()
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	rows := w.rows
	if m := sinceLit.FindStringSubmatch(stmt); m != nil {
		since, _ := time.Parse("2006-01-02 15:04:05", m[1])
		var keep []map[string]any
		for _, r := range rows {
			if !atOf(r).Before(since) {
				keep = append(keep, r)
			}
		}
		rows = keep
	}
	lower := strings.ToLower(stmt)
	var out []string
	switch {
	case strings.Contains(lower, "information_schema.tables"):
		names := w.tables
		if names == nil {
			names = []string{w.table}
		}
		for _, n := range names {
			if len(args) > 0 && strings.Contains(lower, "upper(table_name) = upper(") && !strings.EqualFold(args[len(args)-1], n) {
				continue
			}
			out = append(out, j(map[string]string{"schema": "SIGMA", "name": n}))
		}
	case strings.Contains(lower, "information_schema.columns"):
		for _, c := range w.cols {
			out = append(out, j(c))
		}
	case strings.Contains(stmt, "COUNT(DISTINCT"):
		convs := map[string]bool{}
		var last time.Time
		for _, r := range rows {
			convs[convOf(r)] = true
			if atOf(r).After(last) {
				last = atOf(r)
			}
		}
		out = append(out, j(map[string]any{"N": len(convs), "LAST_AT": last.Format("2006-01-02 15:04:05.000")}))
	case strings.Contains(stmt, "AS conversation_id"):
		lastBy := map[string]time.Time{}
		for _, r := range rows {
			if c := convOf(r); atOf(r).After(lastBy[c]) {
				lastBy[c] = atOf(r)
			}
		}
		var convs []string
		for c := range lastBy {
			convs = append(convs, c)
		}
		sort.Slice(convs, func(i, k int) bool { return lastBy[convs[i]].After(lastBy[convs[k]]) })
		if m := limitLit.FindStringSubmatch(stmt); m != nil {
			n, _ := strconv.Atoi(m[1])
			convs = convs[:min(n, len(convs))]
		}
		for _, c := range convs {
			out = append(out, j(map[string]any{"CONVERSATION_ID": c, "LAST_AT": lastBy[c].Format("2006-01-02 15:04:05")}))
		}
	case strings.Contains(stmt, " IN ("):
		want := map[string]bool{}
		for _, a := range args {
			want[a] = true
		}
		for _, r := range rows {
			if want[convOf(r)] {
				out = append(out, j(r))
			}
		}
	default:
		w.t.Errorf("unexpected statement %s", stmt)
	}
	return out
}

// snowflakeServer serves the Snowflake SQL API v2.
func (w *warehouse) snowflakeServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			Statement string
			Bindings  map[string]map[string]string
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Statement == snowflake.SessionStmt {
			v := []string{"ACME", "READER", "ANALYST", "WH", "9.30.1"}
			json.NewEncoder(rw).Encode(map[string]any{
				"resultSetMetaData": map[string]any{"rowType": []map[string]string{{"name": "ACCOUNT"}, {"name": "USR"}, {"name": "ROLE"}, {"name": "WAREHOUSE"}, {"name": "VERSION"}}},
				"data":              [][]*string{{&v[0], &v[1], &v[2], &v[3], &v[4]}},
			})
			return
		}
		args := make([]string, len(req.Bindings))
		for k, b := range req.Bindings {
			i, _ := strconv.Atoi(k)
			args[i-1] = b["value"]
		}
		rows := w.answer(req.Statement, args)
		data := make([][]*string, len(rows))
		for i := range rows {
			data[i] = []*string{&rows[i]}
		}
		json.NewEncoder(rw).Encode(map[string]any{
			"resultSetMetaData": map[string]any{"rowType": []map[string]string{{"name": "ROW_JSON"}}},
			"data":              data,
		})
	}))
}

// useSnowflake points the reader's Snowflake client at srv.
func useSnowflake(t *testing.T, srv *httptest.Server) {
	newSnowflake = func(cfg snowflake.Config, secret string) *snowflake.Client {
		c := snowflake.New(cfg, secret)
		c.BaseURL = srv.URL
		return c
	}
	t.Cleanup(func() { newSnowflake = snowflake.New })
}

func sfInputs() map[string]string {
	return map[string]string{"platform": "snowflake", "account": "acme-x", "user": "reader", "auth": "pat", "token": "tok",
		"database": "ANALYTICS", "schema": "SIGMA", "table": "AI_USAGE_ACME"}
}

// databricksServer serves the SQL Statement Execution API, splitting each
// result into chunks of two rows, plus SCIM Me and the warehouse.
func (w *warehouse) databricksServer() *httptest.Server {
	var mu sync.Mutex
	chunks := map[string][]string{}
	seq := 0
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dapi-tok" {
			http.Error(rw, `{"message":"bad token"}`, http.StatusUnauthorized)
			return
		}
		page := func(rows []string, id string, at int) map[string]any {
			end := min(at+2, len(rows))
			data := [][]*string{}
			for i := at; i < end; i++ {
				data = append(data, []*string{&rows[i]})
			}
			res := map[string]any{"data_array": data}
			if end < len(rows) {
				res["next_chunk_internal_link"] = fmt.Sprintf("/api/2.0/sql/statements/%s/result/chunks/%d", id, end)
			}
			return map[string]any{"statement_id": id, "status": map[string]string{"state": "SUCCEEDED"}, "result": res}
		}
		switch {
		case r.URL.Path == dbxMePath:
			fmt.Fprint(rw, `{"userName":"ana@example.com"}`)
		case r.URL.Path == dbxWarehousePath+"wh1":
			fmt.Fprint(rw, `{"name":"Serverless","state":"RUNNING"}`)
		case r.URL.Path == dbxStatements && r.Method == http.MethodPost:
			var req struct {
				Statement  string
				Parameters []dbxParam
			}
			json.NewDecoder(r.Body).Decode(&req)
			args := make([]string, len(req.Parameters))
			for _, p := range req.Parameters {
				i, _ := strconv.Atoi(strings.TrimPrefix(p.Name, "p"))
				args[i-1] = p.Value
			}
			rows := w.answer(req.Statement, args)
			mu.Lock()
			seq++
			id := fmt.Sprintf("st%d", seq)
			chunks[id] = rows
			mu.Unlock()
			json.NewEncoder(rw).Encode(page(rows, id, 0))
		case strings.Contains(r.URL.Path, "/result/chunks/"):
			parts := strings.Split(r.URL.Path, "/")
			at, _ := strconv.Atoi(parts[len(parts)-1])
			mu.Lock()
			rows := chunks[parts[5]]
			mu.Unlock()
			json.NewEncoder(rw).Encode(page(rows, parts[5], at))
		default:
			http.NotFound(rw, r)
		}
	}))
}

// bigquery is a fake token endpoint and BigQuery REST API. Each query's
// job is first reported incomplete, then its rows come one per page.
type bigquery struct {
	*warehouse
	pub      *rsa.PublicKey
	srv      *httptest.Server
	tokens   int
	claims   map[string]any
	refuse   bool
	paged    int
	location string
}

func (w *warehouse) bigqueryServer(t *testing.T, pub *rsa.PublicKey) *bigquery {
	bq := &bigquery{warehouse: w, pub: pub, location: "EU"}
	var mu sync.Mutex
	jobs := map[string][]string{}
	seq := 0
	bq.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
			}
			claims, err := verifyJWT(r.Form.Get("assertion"), pub)
			if err != nil || bq.refuse {
				rw.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(rw, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`)
				return
			}
			bq.claims = claims
			bq.tokens++
			fmt.Fprint(rw, `{"access_token":"ya29.fake","expires_in":3599,"token_type":"Bearer"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ya29.fake" {
			rw.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(rw, `{"error":{"message":"Request had invalid authentication credentials."}}`)
			return
		}
		jobPage := func(id string, at int) map[string]any {
			rows := jobs[id]
			res := map[string]any{"jobComplete": true, "jobReference": map[string]string{"jobId": id, "location": bq.location}}
			if at < len(rows) {
				res["rows"] = []map[string]any{{"f": []map[string]any{{"v": rows[at]}}}}
			}
			if at+1 < len(rows) {
				res["pageToken"] = strconv.Itoa(at + 1)
			}
			return res
		}
		switch {
		case r.URL.Path == "/bigquery/v2/projects/acme-proj/queries" && r.Method == http.MethodPost:
			var req struct {
				Query           string
				UseLegacySQL    bool `json:"useLegacySql"`
				ParameterMode   string
				QueryParameters []struct {
					ParameterValue struct{ Value string }
				}
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.UseLegacySQL || (len(req.QueryParameters) > 0 && req.ParameterMode != "POSITIONAL") {
				t.Errorf("request = %+v", req)
			}
			var args []string
			for _, p := range req.QueryParameters {
				args = append(args, p.ParameterValue.Value)
			}
			rows := w.answer(req.Query, args)
			mu.Lock()
			seq++
			id := fmt.Sprintf("job_%d", seq)
			jobs[id] = rows
			mu.Unlock()
			fmt.Fprintf(rw, `{"jobComplete":false,"jobReference":{"jobId":%q,"location":%q}}`, id, bq.location)
		case strings.HasPrefix(r.URL.Path, "/bigquery/v2/projects/acme-proj/queries/"):
			if r.URL.Query().Get("location") != bq.location {
				t.Errorf("getQueryResults without location: %s", r.URL)
			}
			id := strings.TrimPrefix(r.URL.Path, "/bigquery/v2/projects/acme-proj/queries/")
			at, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
			if r.URL.Query().Get("pageToken") != "" {
				bq.paged++
			}
			mu.Lock()
			res := jobPage(id, at)
			mu.Unlock()
			json.NewEncoder(rw).Encode(res)
		case r.URL.Path == "/bigquery/v2/projects/acme-proj/datasets":
			fmt.Fprint(rw, `{"datasets":[{"datasetReference":{"datasetId":"sigma_logs"},"location":"EU"}]}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	bqTokenURL, bqBase, bqPollEvery = bq.srv.URL+"/token", bq.srv.URL, time.Millisecond
	t.Cleanup(func() {
		bq.srv.Close()
		bqTokenURL, bqBase, bqPollEvery = "https://oauth2.googleapis.com/token", "https://bigquery.googleapis.com", 2*time.Second
	})
	return bq
}

// verifyJWT checks an RS256 JWT's signature and returns its claims.
func verifyJWT(tok string, pub *rsa.PublicKey) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, err
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	return claims, json.Unmarshal(b, &claims)
}

// keyFile writes a service account key with a freshly generated RSA key
// (never a committed one) and returns its path and public key.
func keyFile(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "acme-proj", "private_key_id": "kid1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "benchouse@acme-proj.iam.gserviceaccount.com",
		"token_uri":    "https://evil.example.com/token",
	})
	p := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, &k.PublicKey
}
