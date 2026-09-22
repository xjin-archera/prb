// Package github wraps the gh CLI: PR search, detail, diff, and posting a review.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type GhError struct{ Msg string }

func (e *GhError) Error() string { return e.Msg }

func run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, &GhError{Msg: fmt.Sprintf("gh %s failed: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))}
	}
	return out.Bytes(), nil
}

var (
	loginOnce sync.Once
	login     string
	loginErr  error
)

// CurrentLogin returns the gh user, cached for the process lifetime.
func CurrentLogin(ctx context.Context) (string, error) {
	loginOnce.Do(func() {
		out, err := run(ctx, "", "api", "user", "-q", ".login")
		login, loginErr = strings.TrimSpace(string(out)), err
	})
	return login, loginErr
}

const searchQuery = `
query($q: String!) {
  search(query: $q, type: ISSUE, first: 100) {
    nodes {
      ... on PullRequest {
        number title url isDraft createdAt updatedAt
        additions deletions changedFiles
        headRefOid headRefName baseRefName reviewDecision
        author { login }
        repository { nameWithOwner }
        labels(first: 10) { nodes { name } }
        latestReviews(first: 20) { nodes { author { login } state } }
      }
    }
  }
}`

// PR is an open pull request that wants the user's attention.
type PR struct {
	Repo           string   `json:"repo"`
	Number         int      `json:"number"`
	Title          string   `json:"title"`
	URL            string   `json:"url"`
	Author         string   `json:"author"`
	IsBot          bool     `json:"is_bot"`
	IsDraft        bool     `json:"is_draft"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	Additions      int      `json:"additions"`
	Deletions      int      `json:"deletions"`
	ChangedFiles   int      `json:"changed_files"`
	HeadSHA        string   `json:"head_sha"`
	HeadRef        string   `json:"head_ref"`
	BaseRef        string   `json:"base_ref"`
	ReviewDecision string   `json:"review_decision"`
	Labels         []string `json:"labels"`
	Reasons        []string `json:"reasons"` // review-requested | mentioned
	MyReviewState  string   `json:"my_review_state"`
}

func (p PR) Key() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

type searchNode struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	IsDraft        bool   `json:"isDraft"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	ChangedFiles   int    `json:"changedFiles"`
	HeadRefOid     string `json:"headRefOid"`
	HeadRefName    string `json:"headRefName"`
	BaseRefName    string `json:"baseRefName"`
	ReviewDecision string `json:"reviewDecision"`
	Author         struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	LatestReviews struct {
		Nodes []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			State string `json:"state"`
		} `json:"nodes"`
	} `json:"latestReviews"`
}

func nodeToPR(n searchNode, login, reason string) PR {
	author := n.Author.Login
	if author == "" {
		author = "ghost"
	}
	pr := PR{
		Repo: n.Repository.NameWithOwner, Number: n.Number, Title: n.Title, URL: n.URL, Author: author,
		IsBot:   strings.HasSuffix(author, "[bot]") || author == "dependabot" || author == "renovate",
		IsDraft: n.IsDraft, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
		Additions: n.Additions, Deletions: n.Deletions, ChangedFiles: n.ChangedFiles,
		HeadSHA: n.HeadRefOid, HeadRef: n.HeadRefName, BaseRef: n.BaseRefName, ReviewDecision: n.ReviewDecision,
		Reasons: []string{reason},
	}
	for _, l := range n.Labels.Nodes {
		pr.Labels = append(pr.Labels, l.Name)
	}
	for _, r := range n.LatestReviews.Nodes {
		if r.Author.Login == login {
			pr.MyReviewState = r.State
		}
	}
	return pr
}

// SearchMyPRs lists open PRs that request the user's review, and optionally those that mention them.
func SearchMyPRs(ctx context.Context, includeMentions bool) ([]PR, error) {
	login, err := CurrentLogin(ctx)
	if err != nil {
		return nil, err
	}
	type q struct{ reason, query string }
	queries := []q{{"review-requested", "is:pr is:open review-requested:" + login}}
	if includeMentions {
		queries = append(queries, q{"mentioned", fmt.Sprintf("is:pr is:open mentions:%s -author:%s", login, login)})
	}
	type res struct {
		nodes []searchNode
		err   error
	}
	results := make([]res, len(queries))
	var wg sync.WaitGroup
	for i, qq := range queries {
		wg.Add(1)
		go func(i int, qq q) {
			defer wg.Done()
			out, err := run(ctx, "", "api", "graphql", "-f", "query="+searchQuery, "-f", "q="+qq.query)
			if err != nil {
				results[i].err = err
				return
			}
			var data struct {
				Data struct {
					Search struct {
						Nodes []searchNode `json:"nodes"`
					} `json:"search"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out, &data); err != nil {
				results[i].err = err
				return
			}
			results[i].nodes = data.Data.Search.Nodes
		}(i, qq)
	}
	wg.Wait()
	merged := map[string]*PR{}
	for i, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		for _, n := range r.nodes {
			if n.Number == 0 {
				continue
			}
			pr := nodeToPR(n, login, queries[i].reason)
			if ex, ok := merged[pr.Key()]; ok {
				ex.Reasons = append(ex.Reasons, queries[i].reason)
			} else {
				merged[pr.Key()] = &pr
			}
		}
	}
	out := make([]PR, 0, len(merged))
	for _, p := range merged {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// Detail is `gh pr view --json` output.
type Detail struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HeadRefOid  string `json:"headRefOid"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	Comments []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Body string `json:"body"`
	} `json:"comments"`
	Reviews []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		State string `json:"state"`
		Body  string `json:"body"`
	} `json:"reviews"`
}

func PRDetail(ctx context.Context, repo string, number int) (Detail, error) {
	out, err := run(ctx, "", "pr", "view", strconv.Itoa(number), "--repo", repo, "--json",
		"number,title,body,author,headRefOid,headRefName,baseRefName,reviews,comments")
	if err != nil {
		return Detail{}, err
	}
	var d Detail
	err = json.Unmarshal(out, &d)
	return d, err
}

func PRDiff(ctx context.Context, repo string, number int) (string, error) {
	out, err := run(ctx, "", "pr", "diff", strconv.Itoa(number), "--repo", repo)
	return string(out), err
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// LineMap is {path: {"RIGHT": lines, "LEFT": lines}} of lines GitHub accepts as review anchors.
type LineMap map[string]map[string]map[int]bool

func (m LineMap) Has(path, side string, line int) bool { return m[path][side][line] }

// DiffLineMap lists every line in a hunk (added, removed, or context) per file and side.
func DiffLineMap(diff string) LineMap {
	out := LineMap{}
	var path string
	left, right, inHunk := 0, 0, false
	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git"):
			path, inHunk = "", false
		case strings.HasPrefix(raw, "+++ "):
			p := strings.TrimSpace(raw[4:])
			if p == "/dev/null" {
				path = ""
			} else {
				path = strings.TrimPrefix(p, "b/")
				if _, ok := out[path]; !ok {
					out[path] = map[string]map[int]bool{"RIGHT": {}, "LEFT": {}}
				}
			}
		case strings.HasPrefix(raw, "--- "):
		default:
			if m := hunkRe.FindStringSubmatch(raw); m != nil {
				left, _ = strconv.Atoi(m[1])
				right, _ = strconv.Atoi(m[3])
				inHunk = true
				continue
			}
			if !inHunk || path == "" || strings.HasPrefix(raw, "\\") {
				continue
			}
			switch {
			case strings.HasPrefix(raw, "+"):
				out[path]["RIGHT"][right] = true
				right++
			case strings.HasPrefix(raw, "-"):
				out[path]["LEFT"][left] = true
				left++
			default:
				out[path]["RIGHT"][right] = true
				out[path]["LEFT"][left] = true
				right++
				left++
			}
		}
	}
	return out
}

// ReviewPayload is the body of POST /repos/{o}/{r}/pulls/{n}/reviews.
type ReviewPayload struct {
	CommitID string          `json:"commit_id,omitempty"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []InlineComment `json:"comments"`
}

type InlineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// PostReview publishes one review and returns its html_url.
func PostReview(ctx context.Context, repo string, number int, payload ReviewPayload) (string, error) {
	body, _ := json.Marshal(payload)
	out, err := run(ctx, string(body), "api", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), "-X", "POST", "--input", "-")
	if err != nil {
		return "", err
	}
	var res struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(out, &res)
	return res.HTMLURL, nil
}
