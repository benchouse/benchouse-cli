package cube

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
)

// Check requests: the deployment, then pages of thread ids (200 a page,
// without turns) to count the threads in the window, up to countCap.

const (
	deploymentsPath = "/api/v1/deployments?first=100"
	countPage       = 200 // Cube's maximum without turns
	countCap        = 10000
)

type deploymentList struct {
	Items    []deployment `json:"items"`
	PageInfo struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
	Pagination *struct {
		Total int64 `json:"total"`
	} `json:"pagination"`
}

func listDeployments(ctx context.Context, cl *client) (deploymentList, error) {
	var l deploymentList
	if err := cl.get(ctx, deploymentsPath, &l); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return l, fmt.Errorf("%s is not a Cube Cloud API (404 on /api/v1/deployments): give your tenant's URL, e.g. https://acme.cubecloud.dev", cl.base)
		}
		return l, explain(err, "the deployments list")
	}
	return l, nil
}

// verifyToken checks the API key with one call, for its field.
func verifyToken(ctx context.Context, in agent.Inputs) (string, error) {
	cl, err := newClient(in)
	if err != nil {
		return "", err
	}
	l, err := listDeployments(ctx, cl)
	if err != nil {
		return "", err
	}
	n := int64(len(l.Items))
	if l.Pagination != nil && l.Pagination.Total > n {
		n = l.Pagination.Total
	}
	return fmt.Sprintf("API key works: %s visible at %s", plural(n, "deployment", "deployments"), cl.base), nil
}

// lookupDeployments lists the deployments the key sees, for a pick list.
func lookupDeployments(ctx context.Context, in agent.Inputs) ([][2]string, error) {
	cl, err := newClient(in)
	if err != nil {
		return nil, err
	}
	l, err := listDeployments(ctx, cl)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, d := range l.Items {
		if d.ID == "" {
			continue
		}
		label := d.Name
		if label == "" {
			label = string(d.ID)
		}
		out = append(out, [2]string{string(d.ID), label})
	}
	return out, nil
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return agent.Thousands(n) + " " + many
}

type idPage struct {
	Items []struct {
		ChatID    string `json:"chatId"`
		UpdatedAt string `json:"updatedAt"`
	} `json:"items"`
	PageInfo struct {
		EndCursor   *string `json:"endCursor"`
		HasNextPage bool    `json:"hasNextPage"`
	} `json:"pageInfo"`
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	cl, err := newClient(in)
	if err != nil {
		return res, err
	}
	dep := in.Get("deployment-id")
	d, err := readDeployment(ctx, cl, dep)
	if err != nil {
		return res, err
	}
	name := d.Name
	if name == "" {
		name = "id " + dep
	} else {
		name += " (id " + dep + ")"
	}
	res.Facts = append(res.Facts, "Connected to Cube Cloud at "+cl.base+", deployment "+name)

	base := threadsPath(dep) + "?" + listQuery(opts, countPage, false)
	var count int64
	newest := ""
	cursor := ""
	more := false
	for n := 0; ; n++ {
		p := base
		if n > 0 {
			p += "&after=" + url.QueryEscape(cursor)
		}
		var pg idPage
		if err := cl.get(ctx, p, &pg); err != nil {
			if n == 0 {
				return res, listErr(err)
			}
			res.Warnings = append(res.Warnings, "stopped counting threads: "+agent.Clip(listErr(err)))
			break
		}
		if n == 0 && len(pg.Items) > 0 {
			newest = pg.Items[0].UpdatedAt
		}
		count += int64(len(pg.Items))
		if !pg.PageInfo.HasNextPage || pg.PageInfo.EndCursor == nil || *pg.PageInfo.EndCursor == "" || len(pg.Items) == 0 ||
			*pg.PageInfo.EndCursor == cursor {
			break
		}
		if count >= countCap {
			more = true
			break
		}
		cursor = *pg.PageInfo.EndCursor
	}
	res.Facts = append(res.Facts, "chat history export is enabled")
	if count == 0 {
		res.Warnings = append(res.Warnings, "no chat threads updated "+agent.Window(opts)+": widen --since, or check --deployment-id is the deployment your users chat with")
		return res, nil
	}
	fact := agent.Conversations(count, "chat thread", "chat threads", opts)
	if more {
		fact = "more than " + fact
	}
	if newest != "" {
		fact += " (newest updated " + newest + ")"
	}
	res.Facts = append(res.Facts, fact)
	res.Conversations = &count
	return res, nil
}

// keepContext keeps a Cube data model's sources: YAML and JavaScript cube
// and view files, and docs. The project's own configuration (cube.js,
// cube.py, package files, docker-compose) holds connection settings, not
// the model, and is left out.
func keepContext(rel string) bool {
	l := strings.ToLower(rel)
	b := path.Base(l)
	switch b {
	case "cube.js", "cube.py", "package.json", "package-lock.json", "yarn.lock", "docker-compose.yml", "docker-compose.yaml":
		return false
	}
	if strings.HasPrefix(b, ".") {
		return false
	}
	switch path.Ext(b) {
	case ".yml", ".yaml", ".js", ".md", ".jinja":
		return true
	}
	return false
}

var (
	jsCube = regexp.MustCompile(`(?m)^\s*cube\s*\(`)
	jsView = regexp.MustCompile(`(?m)^\s*view\s*\(`)
)

// maxContextRead bounds what checkContext reads of one file.
const maxContextRead = 1 << 20

// checkContext confirms dir holds a Cube data model: at least one file
// declaring cubes or views (YAML "cubes:"/"views:", JavaScript cube(...)
// or view(...)).
func checkContext(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	sum, err := files.Scan(dir, keepContext)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}
	var cubes, views int
	for _, rel := range sum.Kept {
		ext := path.Ext(strings.ToLower(rel))
		if ext == ".md" {
			continue
		}
		b, err := files.ReadBounded(filepath.Join(dir, filepath.FromSlash(rel)), maxContextRead)
		if err != nil {
			continue
		}
		s := string(b)
		switch ext {
		case ".js":
			cubes += len(jsCube.FindAllStringIndex(s, -1))
			views += len(jsView.FindAllStringIndex(s, -1))
		default:
			c, v := yamlDefinitions(s)
			cubes += c
			views += v
		}
	}
	if cubes == 0 && views == 0 {
		return res, fmt.Errorf("no Cube data model in %s: expected *.yml/*.yaml files with cubes: or views:, or *.js files with cube(...) or view(...)", dir)
	}
	res.Facts = append(res.Facts, fmt.Sprintf("Cube data model: %d cubes and %d views", cubes, views),
		fmt.Sprintf("%d files (%s) will be included", len(sum.Kept), agent.Bytes(sum.KeptBytes)))
	if len(sum.TooLarge) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d files are too large and will be left out", len(sum.TooLarge)))
	}
	return res, nil
}

var (
	topKey   = regexp.MustCompile(`^([A-Za-z_]+)\s*:`)
	listName = regexp.MustCompile(`^(\s*)-\s+name\s*:`)
)

// yamlDefinitions counts the entries of the top-level cubes: and views:
// lists of a YAML model file: the "- name:" items at the list's own indent.
func yamlDefinitions(s string) (cubes, views int) {
	section, indent := "", -1
	for _, line := range strings.Split(s, "\n") {
		if m := topKey.FindStringSubmatch(line); m != nil {
			section, indent = m[1], -1
			continue
		}
		m := listName.FindStringSubmatch(line)
		if m == nil || (section != "cubes" && section != "views") {
			continue
		}
		if indent < 0 {
			indent = len(m[1])
		}
		if len(m[1]) != indent {
			continue
		}
		if section == "cubes" {
			cubes++
		} else {
			views++
		}
	}
	return cubes, views
}
