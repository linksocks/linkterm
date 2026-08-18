package linkterm

// vt.go — a compact ANSI (xterm) terminal emulator used to render the
// remote terminal output inside the TUI content area. It keeps a bounded
// scrollback history so the TUI can rewind with the mouse wheel or keys.
//
// Supported: UTF-8 text, CR/LF/CRLF/backspace/tab, SGR (incl. 256 & true
// color), cursor moves & save/restore, erase ops, insert/delete lines and
// chars, scroll regions, alternate screen (?1049/?47), line-drawing
// charset (ESC(0). OSC/DCS sequences are skipped to their terminator.

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

const maxScrollback = 2000

type vtCell struct {
	r         rune
	fg, bg    tcell.Color
	bold      bool
	underline bool
	italic    bool
	reverse   bool
}

type vtRow struct {
	cells []vtCell
}

type cursorState struct {
	x, y                             int
	fg, bg                           tcell.Color
	bold, underline, italic, reverse bool
}

type vt struct {
	w, h int
	// rows grows from scrollback to the current screen; the current screen
	// is the last h rows of `rows`.
	rows []vtRow

	curX, curY  int
	curVisible  bool
	top, bottom int // scroll region (inclusive)

	fg, bg                           tcell.Color
	bold, underline, italic, reverse bool
	saved                            cursorState
	lineDraw                         bool

	altScreen bool
	altRows   []vtRow

	// ANSI parse state
	escState int // 0=idle 1=ESC 2=CSI 3=OSC/DCS
	escBuf   []byte
	utfBuf   []byte
	private  bool

	// history rewind offset (0 = live)
	viewOffset int
}

func newVt(cols, rows_ int) *vt {
	v := &vt{}
	v.resize(cols, rows_)
	v.reset()
	for i := 0; i < v.h; i++ {
		v.rows = append(v.rows, v.blankRow())
	}
	return v
}

func (v *vt) blankRow() vtRow {
	cells := make([]vtCell, v.w)
	for i := range cells {
		cells[i].r = ' '
	}
	return vtRow{cells: cells}
}

func (v *vt) reset() {
	v.curX, v.curY = 0, 0
	v.curVisible = true
	v.top, v.bottom = 0, v.h-1
	v.fg, v.bg = tcell.ColorDefault, tcell.ColorDefault
	v.bold, v.underline, v.italic, v.reverse = false, false, false, false
	v.viewOffset = 0
}

// --- geometry -----------------------------------------------------------

func (v *vt) base() int { return len(v.rows) - v.h }

func (v *vt) abs(y int) int { return v.base() + y }

func (v *vt) clampCursor() {
	if v.curX < 0 {
		v.curX = 0
	}
	if v.curX >= v.w {
		v.curX = v.w - 1
	}
	if v.curY < 0 {
		v.curY = 0
	}
	if v.curY >= v.h {
		v.curY = v.h - 1
	}
}

// --- feed / parse ---------------------------------------------------------

func (v *vt) Feed(data []byte) {
	for _, b := range data {
		v.advance(b)
	}
}

func (v *vt) advance(b byte) {
	switch v.escState {
	case 0:
		switch {
		case b == 0x1b:
			v.escState = 1
			v.lineDraw = false
		case b == 0x07:
		case b == 0x08:
			v.backspace()
		case b == 0x09:
			v.tab()
		case b == 0x0a || b == 0x0b || b == 0x0c:
			v.lineFeed()
		case b == 0x0d:
			v.carriageReturn()
		case b >= 0x20:
			v.putByte(b)
		default:
		}
	case 1: // after ESC
		if len(v.escBuf) > 0 && v.escBuf[0] == 0x28 { // '(' — awaiting charset code
			v.lineDraw = b == '0' // DEC Special Graphics
			v.escState = 0
			break
		}
		switch b {
		case '[':
			v.escState = 2
			v.escBuf = v.escBuf[:0]
			v.private = false
		case ']':
			v.escState = 3
			v.escBuf = v.escBuf[:0]
		case '(', ')', '*', '+':
			v.escState = 1 // charset select; next byte is the code
			v.escBuf = append(v.escBuf[:0], b)
		case '7':
			v.saveCursor()
			v.escState = 0
		case '8':
			v.restoreCursor()
			v.escState = 0
		case 'D':
			v.index()
			v.escState = 0
		case 'M':
			v.reverseIndex()
			v.escState = 0
		case 'E':
			v.lineFeed()
			v.carriageReturn()
			v.escState = 0
		case 'c':
			v.reset()
			v.escState = 0
		case '=', '>':
			v.escState = 0
		default:
			v.escState = 0
		}
	case 2: // in CSI
		switch {
		case b == 0x3f: // '?'
			v.private = true
		case b >= 0x40 && b <= 0x7e:
			v.dispatchCSI(string(v.escBuf), b)
			v.escState = 0
		default:
			v.escBuf = append(v.escBuf, b)
		}
	case 3: // in OSC/DCS: skip until BEL or ST
		if b == 0x07 {
			v.escState = 0
		} else if b == 0x1b {
			v.escState = 0
		}
	}
}

func (v *vt) putByte(b byte) {
	if b < 0x80 {
		v.putRune(rune(b))
		return
	}
	v.utfBuf = append(v.utfBuf, b)
	if utf8.FullRune(v.utfBuf) {
		r, _ := utf8.DecodeRune(v.utfBuf)
		v.utfBuf = v.utfBuf[:0]
		v.putRune(r)
	}
}

// --- output primitives ------------------------------------------------------

func (v *vt) putRune(r rune) {
	if v.lineDraw {
		if g, ok := acsGlyphs[r]; ok {
			r = g
		}
	}
	if v.curX >= v.w {
		v.wrapNewline()
	}
	v.rows[v.abs(v.curY)].cells[v.curX] = v.makeCell(r)
	if v.curX < v.w-1 {
		v.curX++
	} else {
		// consumed the last column; next printable char wraps
		if v.curX == v.w-1 {
			v.curX = v.w // triggers wrap on next put
		}
	}
}

func (v *vt) wrapNewline() {
	if v.curY >= v.bottom {
		v.scrollUp()
	} else {
		v.curY++
	}
	v.curX = 0
}

func (v *vt) makeCell(r rune) vtCell {
	return vtCell{r: r, fg: v.fg, bg: v.bg, bold: v.bold, underline: v.underline, italic: v.italic, reverse: v.reverse}
}

func (v *vt) carriageReturn() { v.curX = 0 }

func (v *vt) lineFeed() {
	if v.curY >= v.bottom {
		v.scrollUp()
	} else {
		v.curY++
	}
}

func (v *vt) backspace() {
	if v.curX > 0 {
		v.curX--
	}
}

func (v *vt) tab() {
	n := 8 - v.curX%8
	if v.curX+n > v.w-1 {
		v.curX = v.w - 1
	} else {
		v.curX += n
	}
}

func (v *vt) index() { v.lineFeed() }
func (v *vt) reverseIndex() {
	if v.curY == v.top {
		v.scrollDown()
	} else if v.curY > 0 {
		v.curY--
	}
}

func (v *vt) cursorUp(n int) {
	if n < 1 {
		n = 1
	}
	v.curY -= n
	if v.curY < v.top {
		v.curY = v.top
	}
}

func (v *vt) cursorDown(n int) {
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		if v.curY >= v.bottom {
			v.scrollUp()
		} else {
			v.curY++
		}
	}
}

// --- scrolling ----------------------------------------------------------------

func (v *vt) scrollUp() {
	if v.top == 0 && v.bottom == v.h-1 {
		v.rows = append(v.rows, v.blankRow())
		if extra := len(v.rows) - (v.h + maxScrollback); extra > 0 {
			v.rows = append(v.rows[:0:0], v.rows[extra:]...)
		}
		return
	}
	for i := v.top; i < v.bottom; i++ {
		v.copyRow(i, i+1)
	}
	v.clearAbs(v.bottom)
}

func (v *vt) scrollDown() {
	if v.top == 0 && v.bottom == v.h-1 {
		if len(v.rows) > v.h {
			copy(v.rows[v.base()+1:], v.rows[v.base():len(v.rows)-1])
			v.clearAbs(0)
		}
		return
	}
	for i := v.bottom; i > v.top; i-- {
		v.copyRow(i, i-1)
	}
	v.clearAbs(v.top)
}

func (v *vt) copyRow(dst, src int) {
	if src >= 0 && src < len(v.rows) && dst >= 0 && dst < len(v.rows) {
		v.rows[dst] = v.rows[src]
	}
}

func (v *vt) clearAbs(y int) {
	if y < 0 || y >= len(v.rows) {
		return
	}
	r := v.blankRow()
	v.rows[y] = r
}

// --- cursor save/restore -------------------------------------------------------

func (v *vt) saveCursor() {
	v.saved = cursorState{x: v.curX, y: v.curY, fg: v.fg, bg: v.bg,
		bold: v.bold, underline: v.underline, italic: v.italic, reverse: v.reverse}
}

func (v *vt) restoreCursor() {
	v.curX, v.curY = v.saved.x, v.saved.y
	v.fg, v.bg = v.saved.fg, v.saved.bg
	v.bold, v.underline, v.italic, v.reverse = v.saved.bold, v.saved.underline, v.saved.italic, v.saved.reverse
	v.clampCursor()
}

// --- SGR -----------------------------------------------------------------------------

func (v *vt) sgr(params []int) {
	if len(params) == 0 {
		params = []int{0}
	}
	for i := 0; i < len(params); i++ {
		p := params[i]
		switch {
		case p == 0:
			v.fg, v.bg = tcell.ColorDefault, tcell.ColorDefault
			v.bold, v.underline, v.italic, v.reverse = false, false, false, false
		case p == 1:
			v.bold = true
		case p == 3:
			v.italic = true
		case p == 4:
			v.underline = true
		case p == 7:
			v.reverse = true
		case p == 22:
			v.bold = false
		case p == 23:
			v.italic = false
		case p == 24:
			v.underline = false
		case p == 27:
			v.reverse = false
		case p == 39:
			v.fg = tcell.ColorDefault
		case p == 49:
			v.bg = tcell.ColorDefault
		case p >= 30 && p <= 37:
			v.fg = tcell.PaletteColor(p - 30)
		case p >= 40 && p <= 47:
			v.bg = tcell.PaletteColor(p - 40)
		case p >= 90 && p <= 97:
			v.fg = tcell.PaletteColor(p - 90 + 8)
		case p >= 100 && p <= 107:
			v.bg = tcell.PaletteColor(p - 100 + 8)
		case p == 38 || p == 48:
			if i+1 >= len(params) {
				continue
			}
			switch params[i+1] {
			case 5:
				if i+2 < len(params) {
					c := tcell.PaletteColor(params[i+2])
					if p == 38 {
						v.fg = c
					} else {
						v.bg = c
					}
					i += 2
				}
			case 2:
				if i+4 < len(params) {
					r, g, b := params[i+2], params[i+3], params[i+4]
					c := tcell.NewRGBColor(int32(r), int32(g), int32(b))
					if p == 38 {
						v.fg = c
					} else {
						v.bg = c
					}
					i += 4
				}
			}
		}
	}
}

// --- CSI -------------------------------------------------------------------------------

func (v *vt) dispatchCSI(params string, c byte) {
	for i := 0; i < 2; i++ {
	} // no-op
	vals := parseParams(params)
	if v.private {
		v.privateCSI(vals, c)
		return
	}
	switch c {
	case 'm':
		v.sgr(vals)
	case 'A':
		v.cursorUp(param(vals, 0, 1))
	case 'B', 'e':
		v.cursorDown(param(vals, 0, 1))
	case 'C', 'a':
		if v.curX < v.w-1 {
			v.curX += param(vals, 0, 1)
		}
		v.clampCursor()
	case 'D':
		if v.curX > 0 {
			v.curX -= param(vals, 0, 1)
		}
		v.clampCursor()
	case 'H', 'f':
		v.cup(vals)
	case 'G':
		v.curX = param(vals, 0, 1) - 1
		v.clampCursor()
	case 'd':
		v.curY = param(vals, 0, 1) - 1
		v.clampCursor()
	case 'J':
		v.ed(param(vals, 0, 0))
	case 'K':
		v.el(param(vals, 0, 0))
	case 'X':
		n := param(vals, 0, 1)
		row := &v.rows[v.abs(v.curY)]
		for i := v.curX; i < v.curX+n && i < v.w; i++ {
			row.cells[i] = v.makeCell(' ')
		}
	case 'L':
		v.insertLines(param(vals, 0, 1))
	case 'M':
		v.deleteLines(param(vals, 0, 1))
	case '@':
		v.insertChars(param(vals, 0, 1))
	case 'P':
		v.deleteChars(param(vals, 0, 1))
	case 'S':
		for i := 0; i < param(vals, 0, 1); i++ {
			v.scrollUp()
		}
	case 'T':
		for i := 0; i < param(vals, 0, 1); i++ {
			v.scrollDown()
		}
	case 'r':
		if len(vals) >= 2 && vals[0] > 0 && vals[1] > 0 {
			v.top = vals[0] - 1
			v.bottom = vals[1] - 1
			if v.bottom >= v.h {
				v.bottom = v.h - 1
			}
			if v.bottom < v.top {
				v.bottom = v.top
			}
		} else {
			v.top, v.bottom = 0, v.h-1
		}
		v.curX, v.curY = 0, v.top
	case 's':
		v.saveCursor()
	case 'u':
		v.restoreCursor()
	}
}

func (v *vt) privateCSI(vals []int, c byte) {
	switch c {
	case 'h', 'l':
		enable := c == 'h'
		for _, p := range vals {
			switch p {
			case 25:
				v.curVisible = enable
			case 1049, 1047, 47:
				if enable {
					v.enterAlt()
				} else {
					v.exitAlt()
				}
			}
		}
	case 'm':
		// xterm mouse reporting: ignored (TUI handles mouse itself)
	}
}

func (v *vt) cup(vals []int) {
	row, col := param(vals, 0, 1), 1
	if len(vals) >= 2 {
		col = vals[1]
	}
	if row < 1 {
		row = 1
	}
	if col < 1 {
		col = 1
	}
	v.curY = row - 1
	v.curX = col - 1
	v.clampCursor()
}

func (v *vt) ed(mode int) {
	switch mode {
	case 0:
		v.eraseToEnd()
	case 1:
		row := &v.rows[v.abs(v.curY)]
		for i := 0; i <= v.curX && i < v.w; i++ {
			row.cells[i] = v.makeCell(' ')
		}
	case 2, 3:
		for y := 0; y < v.h; y++ {
			v.clearAbs(v.abs(y))
		}
	}
}

func (v *vt) eraseToEnd() {
	row := &v.rows[v.abs(v.curY)]
	for i := v.curX; i < v.w; i++ {
		row.cells[i] = v.makeCell(' ')
	}
	for y := v.curY + 1; y < v.h; y++ {
		v.clearAbs(v.abs(y))
	}
}

func (v *vt) el(mode int) {
	row := &v.rows[v.abs(v.curY)]
	switch mode {
	case 0:
		for i := v.curX; i < v.w; i++ {
			row.cells[i] = v.makeCell(' ')
		}
	case 1:
		for i := 0; i <= v.curX && i < v.w; i++ {
			row.cells[i] = v.makeCell(' ')
		}
	case 2:
		for i := range row.cells {
			row.cells[i] = v.makeCell(' ')
		}
	}
}

func (v *vt) insertLines(n int) {
	if v.curY < v.top || v.curY > v.bottom {
		return
	}
	for i := v.bottom; i >= v.curY; i-- {
		if i >= v.curY+n {
			v.copyRow(i, i-n)
		} else {
			v.clearAbs(v.abs(i))
		}
	}
}

func (v *vt) deleteLines(n int) {
	if v.curY < v.top || v.curY > v.bottom {
		return
	}
	for i := v.curY; i <= v.bottom; i++ {
		if i+n <= v.bottom {
			v.copyRow(i, i+n)
		} else {
			v.clearAbs(v.abs(i))
		}
	}
}

func (v *vt) insertChars(n int) {
	row := &v.rows[v.abs(v.curY)]
	if v.curX < v.w {
		w := v.w
		if v.curX+n < w {
			copy(row.cells[v.curX+n:w], row.cells[v.curX:w-n])
		}
	}
	for i := v.curX; i < v.curX+n && i < v.w; i++ {
		row.cells[i] = v.makeCell(' ')
	}
}

func (v *vt) deleteChars(n int) {
	row := &v.rows[v.abs(v.curY)]
	if v.curX+n < v.w {
		copy(row.cells[v.curX:], row.cells[v.curX+n:])
	}
	for i := v.w - n; i < v.w; i++ {
		if i >= v.curX && i >= 0 {
			row.cells[i] = v.makeCell(' ')
		}
	}
}

// --- alt screen ----------------------------------------------------------------------

func (v *vt) enterAlt() {
	if v.altScreen {
		return
	}
	v.altScreen = true
	v.altRows = v.rows
	v.rows = nil
	for i := 0; i < v.h; i++ {
		v.rows = append(v.rows, v.blankRow())
	}
	v.curX, v.curY = 0, 0
}

func (v *vt) exitAlt() {
	if !v.altScreen {
		return
	}
	v.altScreen = false
	v.rows = v.altRows
	v.altRows = nil
	for len(v.rows) < v.h {
		v.rows = append(v.rows, v.blankRow())
	}
	v.curX, v.curY = 0, 0
}

// --- resize ----------------------------------------------------------------------------

func (v *vt) resize(cols, rows_ int) {
	if cols <= 0 {
		cols = 1
	}
	if rows_ <= 0 {
		rows_ = 1
	}
	if cols == v.w && rows_ == v.h && v.rows != nil {
		return
	}
	old := v.rows
	savedX, savedY := v.curX, v.curY
	v.w, v.h = cols, rows_
	nr := make([]vtRow, 0, len(old)+v.h)
	for _, r := range old {
		rr := v.blankRow()
		for i := 0; i < cols && i < len(r.cells); i++ {
			rr.cells[i] = r.cells[i]
		}
		nr = append(nr, rr)
	}
	for len(nr) < v.h {
		nr = append(nr, v.blankRow())
	}
	v.rows = nr
	if v.top >= v.h {
		v.top = 0
	}
	if v.bottom >= v.h {
		v.bottom = v.h - 1
	}
	v.curX, v.curY = savedX, savedY
	v.clampCursor()
}

// --- rendering -----------------------------------------------------------------------------

// render draws the current view (screen or scrollback rewind) into the tcell
// screen at the given origin.
func (v *vt) render(s tcell.Screen, x0, y0 int) {
	off := v.viewOffset
	maxOff := len(v.rows) - v.h
	if off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	base := len(v.rows) - v.h - off
	for y := 0; y < v.h; y++ {
		abs := base + y
		if abs < 0 || abs >= len(v.rows) {
			continue
		}
		row := v.rows[abs]
		for x := 0; x < v.w && x < len(row.cells); x++ {
			c := row.cells[x]
			st := v.styleFor(c)
			if y == v.curY && x == v.curX && v.curVisible && v.viewOffset == 0 {
				st = st.Reverse(true)
			}
			if c.r == 0 {
				c.r = ' '
			}
			s.SetContent(x0+x, y0+y, c.r, nil, st)
		}
	}
}

// numLines returns the total number of lines (history + screen), used by the
// TUI to bound mouse-wheel rewinds.
func (v *vt) numLines() int { return len(v.rows) }

func (v *vt) styleFor(c vtCell) tcell.Style {
	fg, bg := c.fg, c.bg
	if c.reverse {
		fg, bg = bg, fg
	}
	st := tcell.StyleDefault
	if fg != tcell.ColorDefault {
		st = st.Foreground(fg)
	}
	if bg != tcell.ColorDefault {
		st = st.Background(bg)
	}
	if c.bold {
		st = st.Bold(true)
	}
	if c.underline {
		st = st.Underline(true)
	}
	if c.italic {
		st = st.Italic(true)
	}
	return st
}

// --- parameter parsing ----------------------------------------------------------------------

func param(vals []int, idx, def int) int {
	if idx < len(vals) && vals[idx] > 0 {
		return vals[idx]
	}
	return def
}

func parseParams(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			out = append(out, 0)
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			out = append(out, 0)
		} else {
			out = append(out, n)
		}
	}
	return out
}

// acsGlyphs maps the DEC/VT Graphics (line-drawing) repertoire to Unicode.
var acsGlyphs = map[rune]rune{
	'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊', 'f': '°', 'g': '±',
	'h': '␤', 'i': '␋', 'j': '┘', 'k': '┐', 'l': '┌', 'm': '└', 'n': '┼',
	'o': '⎺', 'p': '⎻', 'q': '─', 'r': '⎼', 's': '⎽', 't': '┤', 'u': '┴',
	'v': '┬', 'w': '├', 'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠',
	'}': '£', '~': '·',
}
