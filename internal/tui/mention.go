package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file implements coding-agent style @ file attachment:
// typing @ in the composer opens a fuzzy file picker over the
// usual places files live; choosing one turns it into an
// attachment chip instead of making the user type a path.

const maxIndexedFiles = 600

// gatherFiles indexes candidate attachment files under the working
// directory and the common XDG user folders. It is depth- and
// count-limited and skips hidden directories.
func gatherFiles() []string {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	roots := []string{cwd}
	for _, sub := range []string{"Downloads", "Documents", "Pictures", "Desktop"} {
		if home != "" {
			roots = append(roots, filepath.Join(home, sub))
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if len(out) >= maxIndexedFiles {
				return filepath.SkipAll
			}
			if e.IsDir() {
				if p != root && strings.HasPrefix(e.Name(), ".") {
					return filepath.SkipDir
				}
				if rel, rerr := filepath.Rel(root, p); rerr == nil && rel != "." && strings.Count(rel, string(os.PathSeparator)) >= 3 {
					return filepath.SkipDir
				}
				return nil
			}
			out = append(out, p)
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// fuzzyMatch reports whether query is a subsequence of target
// (case-insensitive), matching coding-agent file pickers.
func fuzzyMatch(query, target string) bool {
	if query == "" {
		return true
	}
	q := []rune(strings.ToLower(query))
	t := strings.ToLower(target)
	i := 0
	for _, r := range t {
		if i < len(q) && r == q[i] {
			i++
		}
	}
	return i == len(q)
}

// filterFiles returns up to limit paths matching the query, ranked
// with basename matches first.
func filterFiles(index []string, query string, limit int) []string {
	type hit struct {
		path string
		base bool
	}
	var hits []hit
	for _, p := range index {
		if fuzzyMatch(query, p) {
			hits = append(hits, hit{p, fuzzyMatch(query, filepath.Base(p))})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].base != hits[j].base {
			return hits[i].base
		}
		return len(hits[i].path) < len(hits[j].path)
	})
	out := make([]string, 0, limit)
	for _, h := range hits {
		if len(out) >= limit {
			break
		}
		out = append(out, h.path)
	}
	return out
}

// mentionQuery extracts the active @ token from the composer: an @
// at the start or after whitespace, with no whitespace after it.
// It returns the query text and whether a mention is active.
func mentionQuery(input string) (string, bool) {
	at := strings.LastIndex(input, "@")
	if at < 0 {
		return "", false
	}
	if at > 0 {
		prev := input[at-1]
		if prev != ' ' && prev != '\t' {
			return "", false
		}
	}
	rest := input[at+1:]
	if strings.ContainsAny(rest, " \t\n") {
		return "", false
	}
	return rest, true
}

// stripMention removes the active @ token from the composer text.
func stripMention(input string) string {
	at := strings.LastIndex(input, "@")
	if at < 0 {
		return input
	}
	return strings.TrimSpace(input[:at] + input[at+1+len(mentionQueryOrEmpty(input)):])
}

func mentionQueryOrEmpty(input string) string {
	q, _ := mentionQuery(input)
	return q
}

// addAttach registers a pending attachment (deduped).
func (m *model) addAttach(path string) {
	for _, p := range m.attachList {
		if p == path {
			return
		}
	}
	m.attachList = append(m.attachList, path)
}

// refreshSuggest recomputes the @ completion popup from the
// current composer content.
func (m *model) refreshSuggest() {
	query, active := mentionQuery(m.input.Value())
	if !active {
		m.completing = false
		m.suggest = nil
		return
	}
	if m.fileIndex == nil {
		m.fileIndex = gatherFiles()
	}
	m.completing = true
	m.suggest = filterFiles(m.fileIndex, query, 8)
	if m.suggestSel >= len(m.suggest) {
		m.suggestSel = 0
	}
}

// acceptSuggest turns the highlighted suggestion into an
// attachment chip and clears the @ token from the composer.
func (m *model) acceptSuggest() {
	if len(m.suggest) == 0 {
		m.completing = false
		return
	}
	if m.suggestSel < 0 || m.suggestSel >= len(m.suggest) {
		m.suggestSel = 0
	}
	path := m.suggest[m.suggestSel]
	m.addAttach(path)
	m.input.SetValue(stripMention(m.input.Value()))
	m.input.CursorEnd()
	m.completing = false
	m.suggest = nil
	m.suggestSel = 0
	m.status = "已添加附件: " + filepath.Base(path) + "（Enter 发送，Ctrl+X 清空附件）"
}
