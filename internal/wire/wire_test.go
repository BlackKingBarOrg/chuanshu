package wire

import "testing"

func TestSanitizeNameRejectsTraversal(t *testing.T) {
	bad := []string{"", ".", "..", "../etc/passwd", "a/b", `a\b`, "/abs", "a//b"}
	for _, name := range bad {
		if _, err := SanitizeName(name); err == nil {
			t.Errorf("SanitizeName(%q) succeeded, want error", name)
		}
	}
	good := []string{"report.pdf", "截图 2026.png", ".hidden", "a..b.txt"}
	for _, name := range good {
		if got, err := SanitizeName(name); err != nil || got != name {
			t.Errorf("SanitizeName(%q) = %q, %v; want unchanged", name, got, err)
		}
	}
}

func TestSanitizeRelPath(t *testing.T) {
	if got, err := SanitizeRelPath("docs/2026/report.pdf"); err != nil || got != "docs/2026/report.pdf" {
		t.Errorf("nested path = %q, %v", got, err)
	}
	for _, rel := range []string{"../escape", "a/../../escape", "/abs/path", `a\b`} {
		if _, err := SanitizeRelPath(rel); err == nil {
			t.Errorf("SanitizeRelPath(%q) succeeded, want error", rel)
		}
	}
	if got, err := SanitizeRelPath(""); err != nil || got != "" {
		t.Errorf("empty rel = %q, %v; want empty", got, err)
	}
}
