// Package web serves the htmx UI: full page, HTML fragments, and SSE streams.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/xifengjin/prb/internal/config"
	"github.com/xifengjin/prb/internal/diff"
	"github.com/xifengjin/prb/internal/github"
	"github.com/xifengjin/prb/internal/runner"
	"github.com/xifengjin/prb/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const aiPrefix = "🤖 *AI-generated suggestion (Claude):*\n\n"

var severities = []string{"Critical", "Important", "Suggestion", "Nit", "FYI"}

type Server struct {
	cfg   config.Config
	paths config.Paths
	st    *store.Store
	jobs  *runner.Manager
	tpl   *template.Template
	md    goldmark.Markdown

	prMu     sync.Mutex
	prCache  map[string]github.PR
	prOrder  []string
	prAt     time.Time
	prErr    error
	chromaLt string
	chromaDk string
}

func New(cfg config.Config, paths config.Paths, st *store.Store, jobs *runner.Manager) (*Server, error) {
	s := &Server{cfg: cfg, paths: paths, st: st, jobs: jobs, prCache: map[string]github.PR{},
		md: goldmark.New(goldmark.WithRendererOptions(gmhtml.WithHardWraps()))}
	funcs := template.FuncMap{
		"md":  s.markdown,
		"ago": ago,
		"short": func(s string, n int) string {
			if len(s) > n {
				return s[:n]
			}
			return s
		},
		"split":  func(key string) string { repo, n, _ := strings.Cut(key, "#"); return repo + "/" + n },
		"repoOf": func(full string) string { _, r, _ := strings.Cut(full, "/"); return r },
		"json":   func(v any) string { b, _ := json.Marshal(v); return string(b) },
		"add":    func(a, b int) int { return a + b },
		"has": func(list []string, v string) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
		"money": func(f float64) string { return fmt.Sprintf("$%.2f", f) },
		"rows": func(s string, min, max int) int {
			n := strings.Count(s, "\n") + 2
			if n < min {
				n = min
			}
			if n > max {
				n = max
			}
			return n
		},
		"ts": func(t int64) string {
			if t == 0 {
				return ""
			}
			return time.Unix(t, 0).Format("15:04:05")
		},
		"sevs":       func() []string { return severities },
		"lower":      strings.ToLower,
		"attr":       func(s string) template.HTMLAttr { return template.HTMLAttr(s) },
		"q":          url.QueryEscape,
		"list":       func(v ...string) []string { return v },
		"list_empty": func() []store.Comment { return nil },
		"cv": func(key string, c store.Comment, editing, inDiff bool) commentView {
			return commentView{Key: key, C: c, Editing: editing, Diff: inDiff}
		},
		"srow": func(d detailData) prRow { rv := d.Review; return prRow{Review: &rv, Stale: d.Stale} },
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tpl = tpl
	s.chromaLt = diff.CSS("github", ".hl")
	s.chromaDk = diff.CSS("github-dark", ".hl")
	return s, nil
}

func (s *Server) markdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := s.md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String()) // goldmark escapes raw HTML by default
}

func ago(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ---------- routing ----------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("GET /static/chroma.css", s.chromaCSS)
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /prs", s.prList)
	mux.HandleFunc("POST /run", s.runSelected)
	mux.HandleFunc("GET /events", s.events)

	pr := "/pr/{owner}/{repo}/{number}"
	mux.HandleFunc("GET "+pr, s.detail)
	mux.HandleFunc("GET "+pr+"/tab/{tab}", s.tab)
	mux.HandleFunc("POST "+pr+"/run", s.runOne)
	mux.HandleFunc("POST "+pr+"/continue", s.continueOne)
	mux.HandleFunc("POST "+pr+"/cancel", s.cancel)
	mux.HandleFunc("POST "+pr+"/reset", s.reset)
	mux.HandleFunc("GET "+pr+"/log", s.logStream)
	mux.HandleFunc("POST "+pr+"/result", s.patchResult)
	mux.HandleFunc("POST "+pr+"/comments", s.addComment)
	mux.HandleFunc("GET "+pr+"/comments/new", s.newCommentForm)
	mux.HandleFunc("POST "+pr+"/comments/{id}", s.updateComment)
	mux.HandleFunc("DELETE "+pr+"/comments/{id}", s.deleteComment)
	mux.HandleFunc("GET "+pr+"/comments/{id}/edit", s.editComment)
	mux.HandleFunc("GET "+pr+"/comments/{id}", s.showComment)
	mux.HandleFunc("GET "+pr+"/preflight", s.preflight)
	mux.HandleFunc("POST "+pr+"/post", s.post)
	mux.HandleFunc("GET "+pr+"/chat", s.chatPanel)
	mux.HandleFunc("POST "+pr+"/chat", s.chatSend)
	mux.HandleFunc("DELETE "+pr+"/chat", s.chatClear)
	mux.HandleFunc("POST "+pr+"/chat/cancel", s.chatCancel)
	mux.HandleFunc("GET "+pr+"/chat/stream", s.chatStream)
	mux.HandleFunc("GET "+pr+"/chat/prefill", s.chatPrefill)
	return mux
}

func (s *Server) chromaCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	fmt.Fprint(w, s.chromaLt)
	fmt.Fprint(w, "@media (prefers-color-scheme: dark) {\n"+s.chromaDk+"}\n")
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// fail renders an error toast fragment with an htmx retarget, so any request can surface an error.
func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("HX-Retarget", "#toast")
	w.Header().Set("HX-Reswap", "beforeend")
	w.WriteHeader(status)
	_ = s.tpl.ExecuteTemplate(w, "toast", map[string]any{"Msg": msg, "Err": true})
}

func (s *Server) toast(w http.ResponseWriter, msg string) {
	_ = s.tpl.ExecuteTemplate(w, "toast", map[string]any{"Msg": msg, "Err": false, "OOB": true})
}

type prKey struct {
	repo   string
	number int
	key    string
}

func parseKey(r *http.Request) (prKey, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return prKey{}, err
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	return prKey{repo: repo, number: n, key: fmt.Sprintf("%s#%d", repo, n)}, nil
}

// ---------- PR list ----------

func (s *Server) refreshPRs(ctx context.Context, force bool) ([]github.PR, error) {
	s.prMu.Lock()
	defer s.prMu.Unlock()
	if force || time.Since(s.prAt) > 2*time.Minute || len(s.prCache) == 0 {
		prs, err := github.SearchMyPRs(ctx, s.cfg.IncludeMentions)
		s.prErr = err
		if err == nil {
			s.prCache = map[string]github.PR{}
			s.prOrder = nil
			for _, p := range prs {
				s.prCache[p.Key()] = p
				s.prOrder = append(s.prOrder, p.Key())
			}
			s.prAt = time.Now()
		} else if len(s.prCache) == 0 {
			return nil, err
		}
	}
	out := make([]github.PR, 0, len(s.prOrder))
	for _, k := range s.prOrder {
		out = append(out, s.prCache[k])
	}
	return out, nil
}

func (s *Server) cachedPR(key string) (github.PR, bool) {
	s.prMu.Lock()
	defer s.prMu.Unlock()
	p, ok := s.prCache[key]
	return p, ok
}

type prRow struct {
	github.PR
	Key      string
	HasClone bool
	Review   *store.Review
	Stale    bool
	Running  bool
}

type listData struct {
	Rows      []prRow
	Authors   []authorCount
	Login     string
	FetchedAt string
	Warn      string
	Filter    filter
	Total     int
}

type authorCount struct {
	Name  string
	Count int
	On    bool
}

type filter struct {
	HideBots   bool
	HideDrafts bool
	Q          string
	Authors    []string
}

func parseFilter(r *http.Request) filter {
	q := r.URL.Query()
	has1 := func(vs []string) bool {
		for _, v := range vs {
			if v == "1" {
				return true
			}
		}
		return false
	}
	_, botsSet := q["hide_bots"]
	f := filter{HideBots: !botsSet || has1(q["hide_bots"]), HideDrafts: has1(q["hide_drafts"]), Q: strings.ToLower(strings.TrimSpace(q.Get("q")))}
	for _, a := range q["author"] {
		if a != "" {
			f.Authors = append(f.Authors, a)
		}
	}
	return f
}

func (s *Server) buildList(ctx context.Context, f filter, force bool) (listData, error) {
	prs, err := s.refreshPRs(ctx, force)
	if err != nil {
		return listData{}, err
	}
	reviews, err := s.st.All(ctx)
	if err != nil {
		return listData{}, err
	}
	login, _ := github.CurrentLogin(ctx)
	d := listData{Login: login, FetchedAt: s.prAt.Format("15:04:05"), Filter: f, Total: len(prs)}
	if s.prErr != nil {
		d.Warn = "PR list is stale: " + s.prErr.Error()
	}
	counts := map[string]int{}
	on := map[string]bool{}
	for _, a := range f.Authors {
		on[a] = true
	}
	for _, p := range prs {
		if !(f.HideBots && p.IsBot) {
			counts[p.Author]++
		}
	}
	for name, n := range counts {
		d.Authors = append(d.Authors, authorCount{Name: name, Count: n, On: on[name]})
	}
	sort.Slice(d.Authors, func(i, j int) bool { return strings.ToLower(d.Authors[i].Name) < strings.ToLower(d.Authors[j].Name) })
	for _, p := range prs {
		if f.HideBots && p.IsBot || f.HideDrafts && p.IsDraft {
			continue
		}
		if len(on) > 0 && !on[p.Author] {
			continue
		}
		if f.Q != "" && !strings.Contains(strings.ToLower(fmt.Sprintf("%s %s %s %d %s", p.Title, p.Author, p.Repo, p.Number, p.HeadRef)), f.Q) {
			continue
		}
		row := prRow{PR: p, Key: p.Key(), HasClone: s.cfg.RepoPath(p.Repo) != "", Running: s.jobs.IsRunning(p.Key())}
		if rv, ok := reviews[p.Key()]; ok {
			rvc := rv
			row.Review = &rvc
			row.Stale = rv.Result != nil && (rv.Status == store.Done || rv.Status == store.PostedS) && rv.HeadSHA != p.HeadSHA
		}
		d.Rows = append(d.Rows, row)
	}
	return d, nil
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.render(w, "index", map[string]any{"Filter": parseFilter(r), "ClaudeMissing": !claudeAvailable()})
}

func claudeAvailable() bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if fi, err := os.Stat(filepath.Join(d, "claude")); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

func (s *Server) prList(w http.ResponseWriter, r *http.Request) {
	d, err := s.buildList(r.Context(), parseFilter(r), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.fail(w, 502, err.Error())
		return
	}
	s.render(w, "prlist", d)
}

func (s *Server) runSelected(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	_, _ = s.refreshPRs(r.Context(), false)
	n := 0
	for _, k := range r.Form["keys"] {
		if p, ok := s.cachedPR(k); ok {
			if err := s.jobs.Start(r.Context(), p, false); err == nil {
				n++
			}
		}
	}
	s.toast(w, fmt.Sprintf("queued %d", n))
	w.Header().Set("HX-Trigger", "prs-changed")
}

// ---------- detail ----------

type detailData struct {
	PR       github.PR
	Key      string
	HasClone bool
	Review   store.Review
	Running  bool
	Chatting bool
	Stale    bool
	Tab      string
	Body     template.HTML
	ChatOpen bool
	Focus    string
}

func (s *Server) load(r *http.Request) (detailData, error) {
	k, err := parseKey(r)
	if err != nil {
		return detailData{}, err
	}
	_, _ = s.refreshPRs(r.Context(), false)
	p, ok := s.cachedPR(k.key)
	if !ok {
		return detailData{}, fmt.Errorf("%s is not in the current PR list; refresh first", k.key)
	}
	rv, err := s.st.Get(r.Context(), k.repo, k.number)
	if err != nil {
		return detailData{}, err
	}
	d := detailData{PR: p, Key: k.key, HasClone: s.cfg.RepoPath(p.Repo) != "", Review: rv,
		Running: s.jobs.IsRunning(k.key), Chatting: s.jobs.IsChatting(k.key),
		Stale: rv.Result != nil && (rv.Status == store.Done || rv.Status == store.PostedS) && rv.HeadSHA != p.HeadSHA}
	return d, nil
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	d, err := s.load(r)
	if err != nil {
		s.fail(w, 404, err.Error())
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		if d.Running {
			tab = "log"
		} else {
			tab = "review"
		}
	}
	d.Tab = tab
	d.ChatOpen = r.URL.Query().Get("chat") == "1"
	body, err := s.tabBody(r.Context(), d, tab)
	if err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	d.Body = body
	w.Header().Set("HX-Push-Url", fmt.Sprintf("/?pr=%s&tab=%s", d.Key, tab))
	s.render(w, "detail", d)
}

func (s *Server) tab(w http.ResponseWriter, r *http.Request) {
	d, err := s.load(r)
	if err != nil {
		s.fail(w, 404, err.Error())
		return
	}
	d.Tab = r.PathValue("tab")
	d.Focus = r.URL.Query().Get("focus")
	body, err := s.tabBody(r.Context(), d, d.Tab)
	if err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	d.Body = body
	s.render(w, "tabs", d)
}

func (s *Server) tabBody(ctx context.Context, d detailData, tab string) (template.HTML, error) {
	var buf bytes.Buffer
	var err error
	switch tab {
	case "log":
		err = s.tpl.ExecuteTemplate(&buf, "log", d)
	case "diff":
		dd, derr := s.diffData(ctx, d)
		if derr != nil {
			return "", derr
		}
		err = s.tpl.ExecuteTemplate(&buf, "diff", dd)
	default:
		err = s.tpl.ExecuteTemplate(&buf, "review", d)
	}
	return template.HTML(buf.String()), err
}

// ---------- actions ----------

func (s *Server) runOne(w http.ResponseWriter, r *http.Request) {
	d, err := s.load(r)
	if err != nil {
		s.fail(w, 404, err.Error())
		return
	}
	if err := s.jobs.Start(r.Context(), d.PR, false); err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	s.redirectDetail(w, d.Key, "log")
}

func (s *Server) continueOne(w http.ResponseWriter, r *http.Request) {
	d, err := s.load(r)
	if err != nil {
		s.fail(w, 404, err.Error())
		return
	}
	if err := s.jobs.Start(r.Context(), d.PR, true); err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	s.redirectDetail(w, d.Key, "log")
}

// redirectDetail asks htmx to reload the detail panel on a given tab.
func (s *Server) redirectDetail(w http.ResponseWriter, key, tab string) {
	repo, n, _ := strings.Cut(key, "#")
	w.Header().Set("HX-Location", fmt.Sprintf(`{"path":"/pr/%s/%s?tab=%s","target":"#detail","swap":"innerHTML"}`, repo, n, tab))
	w.Header().Set("HX-Trigger", "prs-changed")
	w.WriteHeader(204)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		s.fail(w, 400, err.Error())
		return
	}
	s.jobs.Cancel(k.key)
	s.toast(w, "cancelling")
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		s.fail(w, 400, err.Error())
		return
	}
	if s.jobs.IsRunning(k.key) {
		s.fail(w, 409, "review is running")
		return
	}
	rv, err := s.st.Get(r.Context(), k.repo, k.number)
	if err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	if rv.Worktree != "" {
		if rp := s.cfg.RepoPath(k.repo); rp != "" {
			if _, err := os.Stat(rv.Worktree); err == nil {
				_ = removeWorktree(r.Context(), rp, rv.Worktree)
			}
		}
	}
	if err := s.st.Delete(r.Context(), k.key); err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	s.redirectDetail(w, k.key, "review")
}

// ---------- streams ----------

func sseSetup(w http.ResponseWriter) (http.Flusher, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fl.Flush()
	return fl, true
}

func sseWrite(w http.ResponseWriter, fl http.Flusher, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(strings.ReplaceAll(data, "\r", ""), "\n") {
		fmt.Fprintf(w, "data: %s\n", line) // the browser joins data lines with \n
	}
	fmt.Fprint(w, "\n")
	fl.Flush()
}

func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fl, ok := sseSetup(w)
	if !ok {
		return
	}
	ch, backlog, unsub := s.jobs.Logs.Subscribe(k.key)
	defer unsub()
	if len(backlog) == 0 {
		if b, err := os.ReadFile(s.jobs.LogPath(k.repo, k.number)); err == nil {
			lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if len(lines) > 500 {
				lines = lines[len(lines)-500:]
			}
			backlog = lines
		}
	}
	for _, l := range backlog {
		sseWrite(w, fl, "line", logLineHTML(l))
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-ch:
			sseWrite(w, fl, "line", logLineHTML(l))
		case <-ping.C:
			sseWrite(w, fl, "ping", "")
		}
	}
}

func logLineHTML(l string) string {
	return `<span class="l">` + template.HTMLEscapeString(l) + `</span>`
}

// events pushes a status change; the page reloads the list and the open detail on it.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := sseSetup(w)
	if !ok {
		return
	}
	ch, _, unsub := s.jobs.Events.Subscribe("*")
	defer unsub()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			sseWrite(w, fl, "status", ev)
		case <-ping.C:
			sseWrite(w, fl, "ping", "")
		}
	}
}

// ---------- result editing ----------

func (s *Server) loadResult(r *http.Request) (prKey, store.Review, error) {
	k, err := parseKey(r)
	if err != nil {
		return k, store.Review{}, err
	}
	if s.jobs.IsRunning(k.key) {
		return k, store.Review{}, errors.New("review is running")
	}
	rv, err := s.st.Get(r.Context(), k.repo, k.number)
	if err != nil {
		return k, rv, err
	}
	if rv.Result == nil {
		return k, rv, errors.New("no review result yet")
	}
	return k, rv, nil
}

func (s *Server) patchResult(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	_ = r.ParseForm()
	if v, ok := r.Form["verdict"]; ok {
		rv.Result.Verdict = v[0]
	}
	if v, ok := r.Form["verified_locally"]; ok {
		rv.Result.VerifiedLocally = v[0]
	}
	if v, ok := r.Form["summary_body"]; ok {
		rv.Result.SummaryBody = v[0]
	}
	if err := s.st.Save(r.Context(), rv); err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	_ = k
	s.toast(w, "saved")
}

func commentFromForm(r *http.Request, c *store.Comment) {
	f := r.Form
	if v, ok := f["path"]; ok {
		c.Path = strings.TrimSpace(v[0])
	}
	if v, ok := f["line"]; ok {
		if n, err := strconv.Atoi(v[0]); err == nil && n > 0 {
			c.Line = n
		}
	}
	if v, ok := f["side"]; ok && (v[0] == "LEFT" || v[0] == "RIGHT") {
		c.Side = v[0]
	}
	if v, ok := f["severity"]; ok {
		c.Severity = v[0]
	}
	if v, ok := f["body"]; ok {
		c.Body = v[0]
	}
	// checkboxes: present in the form means the field was rendered; value "1" means checked
	if _, ok := f["include_present"]; ok {
		c.Include = f.Get("include") == "1"
	}
	if _, ok := f["ai_present"]; ok {
		c.AIGenerated = f.Get("ai_generated") == "1"
	}
}

type commentView struct {
	Key     string
	C       store.Comment
	Editing bool
	Diff    bool // rendered inside the diff table
	Ask     bool
}

func (s *Server) renderComment(w http.ResponseWriter, key string, c store.Comment, editing, inDiff bool) {
	s.render(w, "comment", commentView{Key: key, C: c, Editing: editing, Diff: inDiff, Ask: true})
}

func (s *Server) findComment(rv store.Review, id string) (int, bool) {
	for i, c := range rv.Result.Comments {
		if c.ID == id {
			return i, true
		}
	}
	return 0, false
}

func (s *Server) showComment(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	i, ok := s.findComment(rv, r.PathValue("id"))
	if !ok {
		s.fail(w, 404, "comment not found")
		return
	}
	s.renderComment(w, k.key, rv.Result.Comments[i], false, r.URL.Query().Get("diff") == "1")
}

func (s *Server) editComment(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	i, ok := s.findComment(rv, r.PathValue("id"))
	if !ok {
		s.fail(w, 404, "comment not found")
		return
	}
	s.renderComment(w, k.key, rv.Result.Comments[i], true, r.URL.Query().Get("diff") == "1")
}

func (s *Server) updateComment(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	i, ok := s.findComment(rv, r.PathValue("id"))
	if !ok {
		s.fail(w, 404, "comment not found")
		return
	}
	_ = r.ParseForm()
	commentFromForm(r, &rv.Result.Comments[i])
	if err := s.st.Save(r.Context(), rv); err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "prs-changed")
	s.renderComment(w, k.key, rv.Result.Comments[i], false, r.Form.Get("diff") == "1")
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	_, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	i, ok := s.findComment(rv, r.PathValue("id"))
	if !ok {
		s.fail(w, 404, "comment not found")
		return
	}
	rv.Result.Comments = append(rv.Result.Comments[:i], rv.Result.Comments[i+1:]...)
	if err := s.st.Save(r.Context(), rv); err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "prs-changed")
	w.WriteHeader(200) // empty body: htmx removes the element (hx-swap="outerHTML" with delete)
}

// newCommentForm returns an editor for a new comment, prefilled from query params (diff line click).
func (s *Server) newCommentForm(w http.ResponseWriter, r *http.Request) {
	k, _, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	q := r.URL.Query()
	line, _ := strconv.Atoi(q.Get("line"))
	if line < 1 {
		line = 1
	}
	side := q.Get("side")
	if side != "LEFT" {
		side = "RIGHT"
	}
	c := store.Comment{ID: "new", Path: q.Get("path"), Line: line, Side: side, Severity: "Suggestion", Include: true}
	name := "comment"
	if q.Get("diff") == "1" {
		name = "comment_row"
	}
	s.render(w, name, commentView{Key: k.key, C: c, Editing: true, Diff: q.Get("diff") == "1"})
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	_ = r.ParseForm()
	c := store.Comment{ID: store.NewID(), Side: "RIGHT", Severity: "Suggestion", Include: true, AIGenerated: false, Line: 1}
	commentFromForm(r, &c)
	if strings.TrimSpace(c.Body) == "" {
		s.fail(w, 400, "empty comment")
		return
	}
	rv.Result.Comments = append(rv.Result.Comments, c)
	if err := s.st.Save(r.Context(), rv); err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "prs-changed")
	s.renderComment(w, k.key, c, false, r.Form.Get("diff") == "1")
}

// ---------- diff tab ----------

type diffFileView struct {
	diff.File
	Comments int
}

type diffData struct {
	D        detailData
	Files    []diffFileView
	Source   string
	HeadSHA  string
	Adds     int
	Dels     int
	ByLine   map[string]map[string]map[int][]store.Comment // path -> side -> line -> comments
	Orphans  []store.Comment
	Editable bool
}

func (dd diffData) At(path, side string, line int) []store.Comment {
	return dd.ByLine[path][side][line]
}

func (s *Server) diffData(ctx context.Context, d detailData) (diffData, error) {
	dd := diffData{D: d, Editable: d.Review.Result != nil && !d.Running}
	var text string
	patch := filepath.Join(d.Review.Worktree, ".pr-review", "diff.patch")
	if b, err := os.ReadFile(patch); d.Review.Worktree != "" && err == nil {
		text, dd.Source, dd.HeadSHA = string(b), "worktree", d.Review.HeadSHA
	} else {
		t, err := github.PRDiff(ctx, d.PR.Repo, d.PR.Number)
		if err != nil {
			return dd, err
		}
		text, dd.Source = t, "github"
	}
	files := diff.Parse(text)
	diff.Highlight(files)
	dd.ByLine = map[string]map[string]map[int][]store.Comment{}
	seen := map[string]bool{}
	if d.Review.Result != nil {
		for _, c := range d.Review.Result.Comments {
			if dd.ByLine[c.Path] == nil {
				dd.ByLine[c.Path] = map[string]map[int][]store.Comment{"RIGHT": {}, "LEFT": {}}
			}
			dd.ByLine[c.Path][c.Side][c.Line] = append(dd.ByLine[c.Path][c.Side][c.Line], c)
		}
	}
	for _, f := range files {
		fv := diffFileView{File: f}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				for _, c := range dd.At(f.Path, l.Side, l.Line) {
					seen[c.ID] = true
					fv.Comments++
				}
				if l.Type == "ctx" {
					for _, c := range dd.At(f.Path, "LEFT", l.OldN) {
						seen[c.ID] = true
						fv.Comments++
					}
				}
			}
		}
		dd.Adds += f.Adds
		dd.Dels += f.Dels
		dd.Files = append(dd.Files, fv)
	}
	if d.Review.Result != nil {
		for _, c := range d.Review.Result.Comments {
			if !seen[c.ID] {
				dd.Orphans = append(dd.Orphans, c)
			}
		}
	}
	return dd, nil
}

// ---------- posting ----------

type payload struct {
	github.ReviewPayload
	Unanchored []store.Comment
	HeadMoved  bool
}

func (s *Server) buildPayload(ctx context.Context, rv store.Review, pr *github.PR, event string) (payload, error) {
	d, err := github.PRDiff(ctx, rv.Repo, rv.Number)
	if err != nil {
		return payload{}, err
	}
	anchors := github.DiffLineMap(d)
	p := payload{ReviewPayload: github.ReviewPayload{Event: event, CommitID: rv.HeadSHA, Comments: []github.InlineComment{}}}
	if pr != nil {
		p.CommitID = pr.HeadSHA
		p.HeadMoved = pr.HeadSHA != rv.HeadSHA
	}
	for _, c := range rv.Result.Comments {
		if !c.Include {
			continue
		}
		if anchors.Has(c.Path, c.Side, c.Line) {
			body := c.Body
			if c.AIGenerated {
				body = aiPrefix + body
			}
			p.Comments = append(p.Comments, github.InlineComment{Path: c.Path, Line: c.Line, Side: c.Side, Body: body})
		} else {
			p.Unanchored = append(p.Unanchored, c)
		}
	}
	body := strings.TrimSpace(rv.Result.SummaryBody)
	if len(p.Unanchored) > 0 {
		body += "\n\n**Not inline because the lines aren't in the diff:**\n"
		for _, c := range p.Unanchored {
			body += fmt.Sprintf("- `%s:%d` — %s\n", c.Path, c.Line, c.Body)
		}
	}
	p.Body = body
	return p, nil
}

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	_, _ = s.refreshPRs(r.Context(), false)
	var prp *github.PR
	if p, ok := s.cachedPR(k.key); ok {
		prp = &p
	}
	pl, err := s.buildPayload(r.Context(), rv, prp, "COMMENT")
	if err != nil {
		s.fail(w, 502, err.Error())
		return
	}
	un := map[string]bool{}
	for _, c := range pl.Unanchored {
		un[c.ID] = true
	}
	s.render(w, "preflight", map[string]any{"Key": k.key, "Inline": len(pl.Comments), "Unanchored": pl.Unanchored, "UnIDs": un,
		"HeadMoved": pl.HeadMoved, "Comments": rv.Result.Comments})
}

func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	_ = r.ParseForm()
	event := r.Form.Get("event")
	if event != "COMMENT" && event != "APPROVE" && event != "REQUEST_CHANGES" {
		s.fail(w, 400, "bad event")
		return
	}
	_, _ = s.refreshPRs(r.Context(), true)
	var prp *github.PR
	if p, ok := s.cachedPR(k.key); ok {
		prp = &p
	}
	pl, err := s.buildPayload(r.Context(), rv, prp, event)
	if err != nil {
		s.fail(w, 502, err.Error())
		return
	}
	htmlURL, err := github.PostReview(r.Context(), k.repo, k.number, pl.ReviewPayload)
	unanchored := len(pl.Unanchored)
	if err != nil {
		if strings.Contains(err.Error(), "422") && len(pl.Comments) > 0 {
			// Fall back: fold every inline comment into the body.
			fb := pl.ReviewPayload
			fb.Body += "\n\n**Inline comments (could not be anchored):**\n"
			for _, c := range pl.Comments {
				fb.Body += fmt.Sprintf("- `%s:%d` — %s\n", c.Path, c.Line, c.Body)
			}
			unanchored = len(pl.Comments)
			fb.Comments = []github.InlineComment{}
			pl.Comments = nil
			htmlURL, err = github.PostReview(r.Context(), k.repo, k.number, fb)
		}
		if err != nil {
			s.fail(w, 502, err.Error())
			return
		}
	}
	rv.Status = store.PostedS
	rv.Posted = &store.Posted{URL: htmlURL, Event: event, At: store.Now(), Inline: len(pl.Comments), Unanchored: unanchored}
	if s.cfg.CleanupAfterPost {
		rv.Posted.Cleanup = s.cleanupWorktree(r.Context(), &rv)
	}
	if err := s.st.Save(r.Context(), rv); err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	b, _ := json.Marshal(map[string]string{"key": k.key, "status": store.PostedS})
	s.jobs.Events.Publish("*", string(b))
	s.redirectDetail(w, k.key, "review")
}

func (s *Server) cleanupWorktree(ctx context.Context, rv *store.Review) string {
	if rv.Worktree == "" {
		return "no worktree"
	}
	rp := s.cfg.RepoPath(rv.Repo)
	if rp == "" {
		return "no local clone"
	}
	if _, err := os.Stat(rv.Worktree); err == nil {
		if err := removeWorktree(ctx, rp, rv.Worktree); err != nil {
			return "failed: " + err.Error()
		}
	}
	dropPRRef(ctx, rp, rv.Number)
	rv.Worktree = ""
	return "removed"
}

// ---------- chat ----------

type chatData struct {
	Key      string
	Review   store.Review
	Chatting bool
	Prefill  string
}

func (s *Server) chatPanel(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		s.fail(w, 400, err.Error())
		return
	}
	rv, err := s.st.Get(r.Context(), k.repo, k.number)
	if err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	s.render(w, "chat", chatData{Key: k.key, Review: rv, Chatting: s.jobs.IsChatting(k.key), Prefill: r.URL.Query().Get("prefill")})
}

func (s *Server) chatPrefill(w http.ResponseWriter, r *http.Request) {
	k, rv, err := s.loadResult(r)
	if err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	text := ""
	if i, ok := s.findComment(rv, r.URL.Query().Get("comment")); ok {
		c := rv.Result.Comments[i]
		var q []string
		for _, l := range strings.Split(c.Body, "\n") {
			q = append(q, "> "+l)
		}
		text = fmt.Sprintf("About your comment at `%s:%d`:\n%s\n\n", c.Path, c.Line, strings.Join(q, "\n"))
	}
	s.render(w, "chat", chatData{Key: k.key, Review: rv, Chatting: s.jobs.IsChatting(k.key), Prefill: text})
}

func (s *Server) chatSend(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		s.fail(w, 400, err.Error())
		return
	}
	_ = r.ParseForm()
	msg := strings.TrimSpace(r.Form.Get("message"))
	if msg == "" {
		s.fail(w, 400, "empty message")
		return
	}
	rv, err := s.st.Get(r.Context(), k.repo, k.number)
	if err != nil {
		s.fail(w, 500, err.Error())
		return
	}
	if rv.Result == nil {
		s.fail(w, 409, "no review result yet; run the review first")
		return
	}
	if err := s.jobs.Chat(r.Context(), rv, msg); err != nil {
		s.fail(w, 409, err.Error())
		return
	}
	rv, _ = s.st.Get(r.Context(), k.repo, k.number)
	s.render(w, "chat", chatData{Key: k.key, Review: rv, Chatting: true})
}

func (s *Server) chatClear(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		s.fail(w, 400, err.Error())
		return
	}
	if s.jobs.IsChatting(k.key) {
		s.fail(w, 409, "a chat turn is running")
		return
	}
	_ = s.st.ClearChat(r.Context(), k.key)
	rv, _ := s.st.Get(r.Context(), k.repo, k.number)
	s.render(w, "chat", chatData{Key: k.key, Review: rv})
}

func (s *Server) chatCancel(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		s.fail(w, 400, err.Error())
		return
	}
	s.jobs.CancelChat(k.key)
	s.toast(w, "cancelling")
}

// chatStream turns runner chat events into SSE events the chat template listens for:
// "text" appends rendered markdown to the live bubble; "tool" appends a tool line; "done" triggers a panel reload.
func (s *Server) chatStream(w http.ResponseWriter, r *http.Request) {
	k, err := parseKey(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fl, ok := sseSetup(w)
	if !ok {
		return
	}
	ch, _, unsub := s.jobs.ChatBus.Subscribe(k.key)
	defer unsub()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case raw := <-ch:
			var ev map[string]any
			_ = json.Unmarshal([]byte(raw), &ev)
			switch ev["type"] {
			case "start":
				sseWrite(w, fl, "start", "")
			case "text":
				t, _ := ev["text"].(string)
				sseWrite(w, fl, "text", `<div class="chunk">`+string(s.markdown(t))+`</div>`)
			case "tool":
				sseWrite(w, fl, "tool", fmt.Sprintf(`<div class="tool">⚙ %s: %s</div>`, template.HTMLEscapeString(fmt.Sprint(ev["name"])), template.HTMLEscapeString(fmt.Sprint(ev["brief"]))))
			case "done":
				sseWrite(w, fl, "done", raw)
			}
		case <-ping.C:
			sseWrite(w, fl, "ping", "")
		}
	}
}

// oneLine makes an HTML fragment safe for a single SSE data line.
func oneLine(html string) string {
	return strings.ReplaceAll(strings.ReplaceAll(html, "\r", ""), "\n", "&#10;")
}
