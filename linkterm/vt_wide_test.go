package linkterm

import "testing"

func TestRuneWidth(t *testing.T) {
	cases := map[rune]int{
		'a': 1,
		'1': 1,
		'-': 1,
		'中': 2,
		'文': 2,
		'你': 2,
		'（': 2,
		'，': 2,
		'：': 2,
		'ｆ': 2, // fullwidth form
		'한': 2, // hangul
		'あ': 2, // hiragana
	}
	for r, want := range cases {
		if got := runeWidth(r); got != want {
			t.Errorf("runeWidth(%q) = %d, want %d", r, got, want)
		}
	}
}

func TestPutCJKChar(t *testing.T) {
	v := newVt(10, 3)
	v.putRune('中')
	if v.curX != 2 {
		t.Fatalf("curX = %d, want 2", v.curX)
	}
	c0 := v.rows[v.abs(0)].cells[0]
	if c0.r != '中' {
		t.Errorf("cell[0] = %q, want 中", c0.r)
	}
	c1 := v.rows[v.abs(0)].cells[1]
	if c1.r != 0 {
		t.Errorf("cell[1] = %q, want continuation marker (r==0)", c1.r)
	}
	v.putRune('a')
	c2 := v.rows[v.abs(0)].cells[2]
	if c2.r != 'a' {
		t.Errorf("cell[2] = %q, want a", c2.r)
	}
}

func TestCJKBackspace(t *testing.T) {
	v := newVt(10, 3)
	v.putRune('中') // cursor now at 2
	v.backspace()  // BS is a raw single-column move; it may land on the
	// continuation cell (col 1). The remote program (readline) sends one
	// BS per column based on wcwidth, so wide-aware skipping would make
	// the cursor over-shoot and corrupt the prompt.
	if v.curX != 1 {
		t.Errorf("curX = %d, want 1", v.curX)
	}
	// a second BS (readline uses one per column for a wide char) lands on
	// the left half of the wide char
	v.backspace()
	if v.curX != 0 {
		t.Errorf("curX = %d, want 0", v.curX)
	}
}

func TestCJKWrapLastColumn(t *testing.T) {
	v := newVt(4, 3)
	for i := 0; i < 3; i++ {
		v.putRune('a')
	} // cursor at 3 (last col)
	v.putRune('中') // wide char at last col: wraps to next line
	if v.curX != 2 {
		t.Errorf("curX = %d, want 2 (after wrap)", v.curX)
	}
	if v.curY != 1 {
		t.Errorf("curY = %d, want 1 (wrapped to next line)", v.curY)
	}
	c0 := v.rows[v.abs(1)].cells[0]
	if c0.r != '中' {
		t.Errorf("cell[0] of row 1 = %q, want 中", c0.r)
	}
}

func TestCJKEraseLine(t *testing.T) {
	v := newVt(10, 3)
	v.putRune('中') // cols 0-1
	v.putRune('文') // cols 2-3
	v.curX = 2
	v.el(2) // erase whole line
	row := v.rows[v.abs(0)]
	for i := 0; i < 4; i++ {
		if row.cells[i].r != ' ' {
			t.Errorf("cell[%d] = %q after erase, want space", i, row.cells[i].r)
		}
	}
}
