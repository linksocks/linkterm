package linkterm

import "testing"

func TestTerminalSelectionRange(t *testing.T) {
	tui := &tui{
		termSelMoved: true,
		termSelX0:    8,
		termSelY0:    4,
		termSelX1:    2,
		termSelY1:    1,
	}
	y0, y1, x0, x1 := tui.terminalSelectionRange()
	if y0 != 1 || y1 != 4 || x0 != 2 || x1 != 8 {
		t.Fatalf("terminalSelectionRange() = %d,%d,%d,%d", y0, y1, x0, x1)
	}
}

func TestContentHeightKeepsStatusBarSeparate(t *testing.T) {
	tui := &tui{}
	if got := tui.contentHeight(26); got != 25 {
		t.Fatalf("contentHeight(26) = %d, want 25", got)
	}
	if got := tui.contentHeight(1); got != 1 {
		t.Fatalf("contentHeight(1) = %d, want 1", got)
	}
}

func TestSelectionRequiresDrag(t *testing.T) {
	tui := &tui{selX0: 2, selY0: 1, selX1: 2, selY1: 1}
	if tui.hasSelection() {
		t.Fatal("a click without movement must not create a selection")
	}
	tui.selMoved = true
	if !tui.hasSelection() {
		t.Fatal("a moved drag should create a selection")
	}
}
