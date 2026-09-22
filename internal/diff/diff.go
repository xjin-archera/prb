// Package diff parses a unified diff and highlights it for the viewer.
package diff

import (
	"bytes"
	"html/template"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

type Line struct {
	Type string // add | del | ctx
	Text string
	HTML template.HTML // highlighted Text
	OldN int           // 0 when absent
	NewN int
	// Anchor is what a review comment on this line uses.
	Side string // RIGHT for add/ctx, LEFT for del
	Line int
}

type Hunk struct {
	Header string
	Lines  []Line
}

type File struct {
	Index   int
	OldPath string
	Path    string
	Status  string // new | deleted | renamed | ""
	Binary  bool
	Adds    int
	Dels    int
	Hunks   []Hunk
}

var commentRe = regexp.MustCompile(`^/\*.*?\*/\s*`)

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// Parse splits a unified diff into files, hunks, and numbered lines.
func Parse(text string) []File {
	var files []File
	var f *File
	var h *Hunk
	oldN, newN := 0, 0
	for _, raw := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git"):
			files = append(files, File{Index: len(files)})
			f = &files[len(files)-1]
			h = nil
			continue
		case f == nil:
			continue
		case strings.HasPrefix(raw, "--- "):
			f.OldPath = strings.TrimPrefix(strings.TrimSpace(raw[4:]), "a/")
			continue
		case strings.HasPrefix(raw, "+++ "):
			p := strings.TrimSpace(raw[4:])
			if p == "/dev/null" {
				f.Path, f.Status = f.OldPath, "deleted"
			} else {
				f.Path = strings.TrimPrefix(p, "b/")
			}
			continue
		case strings.HasPrefix(raw, "new file"):
			f.Status = "new"
			continue
		case strings.HasPrefix(raw, "deleted file"):
			f.Status = "deleted"
			continue
		case strings.HasPrefix(raw, "rename from"):
			f.Status = "renamed"
			continue
		case strings.HasPrefix(raw, "Binary files"):
			f.Binary = true
			continue
		}
		if m := hunkRe.FindStringSubmatch(raw); m != nil {
			oldN, _ = strconv.Atoi(m[1])
			newN, _ = strconv.Atoi(m[3])
			f.Hunks = append(f.Hunks, Hunk{Header: raw})
			h = &f.Hunks[len(f.Hunks)-1]
			continue
		}
		if h == nil || strings.HasPrefix(raw, "\\") {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "+"):
			h.Lines = append(h.Lines, Line{Type: "add", Text: raw[1:], NewN: newN, Side: "RIGHT", Line: newN})
			newN++
			f.Adds++
		case strings.HasPrefix(raw, "-"):
			h.Lines = append(h.Lines, Line{Type: "del", Text: raw[1:], OldN: oldN, Side: "LEFT", Line: oldN})
			oldN++
			f.Dels++
		default:
			t := raw
			if strings.HasPrefix(t, " ") {
				t = t[1:]
			}
			h.Lines = append(h.Lines, Line{Type: "ctx", Text: t, OldN: oldN, NewN: newN, Side: "RIGHT", Line: newN})
			oldN++
			newN++
		}
	}
	for i := range files {
		if files[i].Path == "" {
			files[i].Path = files[i].OldPath
		}
	}
	return files
}

// Highlight fills Line.HTML for every line using a lexer chosen by the file name.
// Each side of a hunk is tokenised as one block so multi-line constructs keep their state.
func Highlight(files []File) {
	for fi := range files {
		f := &files[fi]
		lexer := lexers.Match(filepath.Base(f.Path))
		if lexer == nil {
			lexer = lexers.Fallback
		}
		lexer = chroma.Coalesce(lexer)
		for hi := range f.Hunks {
			h := &f.Hunks[hi]
			// old side = del+ctx, new side = add+ctx; ctx lines take the new-side rendering
			var newIdx, oldIdx []int
			var newText, oldText []string
			for i, l := range h.Lines {
				if l.Type != "del" {
					newIdx = append(newIdx, i)
					newText = append(newText, l.Text)
				}
				if l.Type == "del" {
					oldIdx = append(oldIdx, i)
					oldText = append(oldText, l.Text)
				}
			}
			apply(h, newIdx, highlightBlock(lexer, newText))
			apply(h, oldIdx, highlightBlock(lexer, oldText))
		}
	}
}

func apply(h *Hunk, idx []int, rendered []template.HTML) {
	for i, li := range idx {
		if i < len(rendered) {
			h.Lines[li].HTML = rendered[i]
		} else {
			h.Lines[li].HTML = template.HTML(template.HTMLEscapeString(h.Lines[li].Text))
		}
	}
}

// highlightBlock renders lines as one source block and splits the output back into lines.
func highlightBlock(lexer chroma.Lexer, lines []string) []template.HTML {
	if len(lines) == 0 {
		return nil
	}
	src := strings.Join(lines, "\n")
	it, err := lexer.Tokenise(nil, src)
	if err != nil {
		return escapeAll(lines)
	}
	var out []template.HTML
	var cur bytes.Buffer
	for tok := it(); tok != chroma.EOF; tok = it() {
		cls := tokenClass(tok.Type) // may be ""
		parts := strings.Split(tok.Value, "\n")
		for pi, part := range parts {
			if pi > 0 {
				out = append(out, template.HTML(cur.String()))
				cur.Reset()
			}
			if part == "" {
				continue
			}
			if cls != "" {
				cur.WriteString(`<span class="` + cls + `">`)
			}
			cur.WriteString(template.HTMLEscapeString(part))
			if cls != "" {
				cur.WriteString("</span>")
			}
		}
	}
	out = append(out, template.HTML(cur.String()))
	if len(out) != len(lines) {
		return escapeAll(lines)
	}
	return out
}

// tokenClass mirrors chroma's HTML formatter: the type's class, else its sub-category's, else its category's.
func tokenClass(t chroma.TokenType) string {
	for _, tt := range []chroma.TokenType{t, t.SubCategory(), t.Category()} {
		if c, ok := chroma.StandardTypes[tt]; ok && c != "" {
			return c
		}
	}
	return ""
}

func escapeAll(lines []string) []template.HTML {
	out := make([]template.HTML, len(lines))
	for i, l := range lines {
		out[i] = template.HTML(template.HTMLEscapeString(l))
	}
	return out
}

// CSS returns the chroma stylesheet for a style, scoped under a selector.
func CSS(styleName, scope string) string {
	st := styles.Get(styleName)
	if st == nil {
		st = styles.Fallback
	}
	var b bytes.Buffer
	f := html.New(html.WithClasses(true), html.PreventSurroundingPre(true))
	if err := f.WriteCSS(&b, st); err != nil {
		return ""
	}
	// chroma emits `/* Name */ .chroma .k { ... }`; keep token rules only and scope them under the selector
	var sb strings.Builder
	for _, line := range strings.Split(b.String(), "\n") {
		line = commentRe.ReplaceAllString(line, "")
		if !strings.HasPrefix(line, ".chroma .") {
			continue
		}
		sb.WriteString(scope + strings.TrimPrefix(line, ".chroma") + "\n")
	}
	return sb.String()
}
