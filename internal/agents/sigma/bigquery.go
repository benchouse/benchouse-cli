package sigma

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

// BigQuery without the Google SDK: a service account's JSON key signs an
// RS256 JWT (as sfauth does for Snowflake), exchanged at Google's token
// endpoint for an access token with the bigquery.readonly scope (RFC 7523
// JWT bearer grant), then jobs.query and jobs.getQueryResults page through
// the result.

const bqScope = "https://www.googleapis.com/auth/bigquery.readonly"

// The endpoints; tests point them at fakes. The key file's own token_uri
// is never used: the signed assertion only goes to Google.
var (
	bqTokenURL = "https://oauth2.googleapis.com/token"
	bqBase     = "https://bigquery.googleapis.com"
)

// bqPollEvery is the wait between polls of an unfinished job; tests shorten it.
var bqPollEvery = 2 * time.Second

const (
	bqMaxResponse = 64 << 20
	bqMaxKey      = 64 << 10
	bqPageRows    = 1000
	bqMaxPages    = 100000
)

type saKey struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	key          *rsa.PrivateKey
}

// readKeyFile reads and checks a service account key file.
func readKeyFile(p string) (*saKey, error) {
	if p == "" {
		return nil, errors.New("no service account key file given")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, bqMaxKey+1))
	if err != nil {
		return nil, err
	}
	if len(b) > bqMaxKey {
		return nil, errors.New("over 64 KiB: not a service account key")
	}
	var k saKey
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, errors.New("not JSON: give the key file downloaded for the service account")
	}
	if k.Type != "service_account" {
		return nil, errors.New(`not a service account key (its "type" is not "service_account")`)
	}
	if k.ClientEmail == "" || k.PrivateKey == "" {
		return nil, errors.New("the key file has no client_email or private_key")
	}
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, errors.New("the key file's private_key is not PEM")
	}
	var parsed any
	if parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		if parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return nil, errors.New("the key file's private_key could not be read")
		}
	}
	rk, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the key file's private_key is not an RSA key")
	}
	k.key = rk
	return &k, nil
}

func validKeyFile(v string) error {
	_, err := readKeyFile(v)
	return err
}

// assertion is the signed JWT for the token exchange.
func (k *saKey) assertion(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if k.PrivateKeyID != "" {
		hdr["kid"] = k.PrivateKeyID
	}
	h, _ := json.Marshal(hdr)
	c, _ := json.Marshal(map[string]any{"iss": k.ClientEmail, "scope": bqScope, "aud": bqTokenURL,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the token request: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

type bqClient struct {
	key     *saKey
	project string
	http    *http.Client
	token   string
	expires time.Time
}

func newBQ(in agent.Inputs) (*bqClient, error) {
	k, err := readKeyFile(in.Get("key-file"))
	if err != nil {
		return nil, fmt.Errorf("--key-file: %w", err)
	}
	p := in.Get("project")
	if p == "" {
		p = k.ProjectID
	}
	if err := validProject(p); err != nil {
		return nil, fmt.Errorf("--project: %w", err)
	}
	return &bqClient{key: k, project: p, http: netsafe.Client(180 * time.Second)}, nil
}

func (c *bqClient) account() string { return c.project }

// accessToken exchanges the signed assertion for an access token, once per
// hour.
func (c *bqClient) accessToken(ctx context.Context) (string, error) {
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	a, err := c.key.assertion(time.Now())
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {a}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bqTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "benchouse-cli")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach Google's token endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		why := out.Description
		if why == "" {
			why = out.Error
		}
		if why == "" {
			why = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return "", fmt.Errorf("Google refused the service account key %s (%s): check the key is not deleted or disabled", c.key.ClientEmail, clipText(why))
	}
	life := time.Duration(out.ExpiresIn) * time.Second
	if life <= 0 {
		life = 30 * time.Minute
	}
	c.token, c.expires = out.AccessToken, time.Now().Add(life-time.Minute)
	return c.token, nil
}

func clipText(s string) string {
	if len(s) > 240 {
		return s[:240] + "..."
	}
	return s
}

// call sends one BigQuery REST request to a path under /bigquery/v2/.
func (c *bqClient) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, bqBase+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "benchouse-cli")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bqMaxResponse+1))
	if err != nil {
		return err
	}
	if len(raw) > bqMaxResponse {
		return fmt.Errorf("bigquery answered with more than %d bytes", bqMaxResponse)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := string(raw)
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return fmt.Errorf("bigquery http %d: %s", resp.StatusCode, clipText(msg))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode bigquery response: %w", err)
	}
	return nil
}

type bqResult struct {
	JobComplete  bool `json:"jobComplete"`
	JobReference struct {
		JobID    string `json:"jobId"`
		Location string `json:"location"`
	} `json:"jobReference"`
	Rows []struct {
		F []struct {
			V *string `json:"v"`
		} `json:"f"`
	} `json:"rows"`
	PageToken string `json:"pageToken"`
	Errors    []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

var bqRef = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,1000}$`)

func (c *bqClient) projectPath() string {
	return "/bigquery/v2/projects/" + url.PathEscape(c.project)
}

// each runs stmt (GoogleSQL, ? bound positionally as STRING) with
// jobs.query, then pages through jobs.getQueryResults, one page at a time.
func (c *bqClient) each(ctx context.Context, stmt string, args []string, fn func(map[string]any) error) (int, error) {
	req := map[string]any{"query": stmt, "useLegacySql": false, "timeoutMs": 50000, "maxResults": bqPageRows}
	if len(args) > 0 {
		ps := make([]map[string]any, len(args))
		for i, a := range args {
			ps[i] = map[string]any{"parameterType": map[string]string{"type": "STRING"}, "parameterValue": map[string]string{"value": a}}
		}
		req["parameterMode"] = "POSITIONAL"
		req["queryParameters"] = ps
	}
	var res bqResult
	if err := c.call(ctx, http.MethodPost, c.projectPath()+"/queries", req, &res); err != nil {
		return 0, err
	}
	job, loc := res.JobReference.JobID, res.JobReference.Location
	if !bqRef.MatchString(job) || (loc != "" && !bqRef.MatchString(loc)) {
		return 0, fmt.Errorf("bigquery returned a job reference that is not one: %.80q", job)
	}
	results := func(token string) error {
		q := url.Values{"timeoutMs": {"50000"}, "maxResults": {fmt.Sprint(bqPageRows)}}
		if loc != "" {
			q.Set("location", loc)
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		res = bqResult{}
		return c.call(ctx, http.MethodGet, c.projectPath()+"/queries/"+url.PathEscape(job)+"?"+q.Encode(), nil, &res)
	}
	started := time.Now()
	for !res.JobComplete {
		if time.Since(started) > 10*time.Minute {
			return 0, fmt.Errorf("bigquery job %s still running after 10 minutes", job)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(bqPollEvery):
		}
		if err := results(""); err != nil {
			return 0, err
		}
	}
	if len(res.Errors) > 0 {
		return 0, fmt.Errorf("bigquery job %s: %s", job, clipText(res.Errors[0].Message))
	}
	n := 0
	for page := 0; ; page++ {
		for _, r := range res.Rows {
			if len(r.F) == 0 || r.F[0].V == nil {
				continue
			}
			m, err := decodeRow([]byte(*r.F[0].V))
			if err != nil {
				return n, err
			}
			if err := fn(m); err != nil {
				return n, err
			}
			n++
		}
		if res.PageToken == "" {
			return n, nil
		}
		if page >= bqMaxPages {
			return n, fmt.Errorf("bigquery job %s: more than %d result pages", job, bqMaxPages)
		}
		token := res.PageToken
		if err := results(token); err != nil {
			return n, err
		}
		if res.PageToken == token {
			return n, fmt.Errorf("bigquery job %s repeated a page token", job)
		}
	}
}

func (c *bqClient) signIn(ctx context.Context) (string, error) {
	if _, err := c.accessToken(ctx); err != nil {
		return "", err
	}
	return "Signed in to BigQuery project " + c.project + " as " + c.key.ClientEmail, nil
}

func verifyKeyFile(ctx context.Context, in agent.Inputs) (string, error) {
	c, err := newBQ(in)
	if err != nil {
		return "", err
	}
	if _, err := c.accessToken(ctx); err != nil {
		return "", err
	}
	return "Key works: signed in as " + c.key.ClientEmail, nil
}

// bqDatasets lists the project's datasets, for the dataset prompt.
func bqDatasets(ctx context.Context, in agent.Inputs) ([][2]string, error) {
	c, err := newBQ(in)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	token := ""
	for range 20 {
		path := c.projectPath() + "/datasets?maxResults=1000"
		if token != "" {
			path += "&pageToken=" + url.QueryEscape(token)
		}
		var resp struct {
			Datasets []struct {
				DatasetReference struct {
					DatasetID string `json:"datasetId"`
				} `json:"datasetReference"`
				Location string `json:"location"`
			} `json:"datasets"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.call(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("list datasets: %s", agent.Clip(err))
		}
		for _, d := range resp.Datasets {
			id := d.DatasetReference.DatasetID
			if validDataset(id) != nil {
				continue
			}
			label := id
			if d.Location != "" {
				label += " (" + d.Location + ")"
			}
			out = append(out, [2]string{id, label})
		}
		if resp.NextPageToken == "" || resp.NextPageToken == token {
			break
		}
		token = resp.NextPageToken
	}
	return out, nil
}
