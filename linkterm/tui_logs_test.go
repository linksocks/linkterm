package linkterm

import "testing"

func TestWrapLogLine(t *testing.T) {
	rows := wrapLogLine("2026-08-21T12:15:19+08:00 INF hello world", 0, 20)
	if len(rows) < 2 {
		t.Fatalf("expected wrapped rows, got %d", len(rows))
	}
	if rows[0].level != "INF" || rows[0].tsEnd < 0 || rows[0].levelEnd <= rows[0].tsEnd {
		t.Fatalf("unexpected zerolog prefix metadata: %+v", rows[0])
	}
	for _, row := range rows {
		width := 0
		for _, r := range []rune(row.text) {
			width += runeWidth(r)
		}
		if width > 20 {
			t.Fatalf("wrapped row exceeds width: %d", width)
		}
	}
}

func TestTextColumns(t *testing.T) {
	if got := textColumns("ab中文d", 2, 5); got != "中文" {
		t.Fatalf("textColumns() = %q, want %q", got, "中文")
	}
	if got := textColumns("ab中文d", 0, 1); got != "ab" {
		t.Fatalf("textColumns() = %q, want %q", got, "ab")
	}
}
