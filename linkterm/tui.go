package linkterm

// tui.go - a tmux-like TUI for the linkterm client.
//
// Layout (top to bottom):
//
//	+---------------------------------------------+
//	|  content area: the remote terminal (vt.go)  |
//	|  or the fullscreen log panel (F2 toggles)   |
//	+---------------------------------------------+
//	|  status:  ● host | Latency 12ms | F2 Logs F3 Quit |
//	+---------------------------------------------+
//
// Keys:
//
//	F2             toggle the fullscreen log panel
//	F3, Ctrl+Q     quit
//	wheel          rewinds the content scrollback / scrolls the log panel
// Esc/g (in rewind) jump back to the live screen
//
// Ordinary keys are forwarded to the remote terminal.

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/gorilla/websocket"
)

// logRing is a thread-safe bounded list of log lines shown in the log panel.
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLogRing(max int) *logRing {
	if max <= 0 {
		max = 3000
	}
	return &logRing{max: max}
}

func (r *logRing) Write(p []byte) (int, error) { // io.Writer for zerolog
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		r.lines = append(r.lines, line)
	}
	if len(r.lines) > r.max {
		r.lines = append(r.lines[:0], r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

func (r *logRing) Logf(format string, args ...interface{}) {
	r.Write([]byte(fmt.Sprintf(format, args...)))
}

func (r *logRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

type tui struct {
	url    string
	dialer *websocket.Dialer
	rttFn  func() time.Duration // reports link RTT (linksocks GetRTT), nil = direct

	screen tcell.Screen
	vt     *vt
	conn   *websocket.Conn
	ring   *logRing
	done   chan struct{}

	mu        sync.Mutex
	connected bool
	status    string
	latency   time.Duration
	fatalErr  error

	showLogs  bool
	logScroll int
}

func newTUI(target string, dialer *websocket.Dialer, rttFn func() time.Duration) *tui {
	return &tui{url: target, dialer: dialer, rttFn: rttFn, ring: newLogRing(3000), done: make(chan struct{})}
}

// setFatal records a fatal connection error; the TUI then renders an error
// screen (details in the F2 log panel) instead of trying to dial again.
func (t *tui) setFatal(err error) {
	t.mu.Lock()
	t.fatalErr = err
	t.status = "Connection failed"
	t.mu.Unlock()
	t.ring.Logf("connection failed: %v", err)
}

// Run runs the TUI; it owns stdin/stdout via tcell until it exits.
func (t *tui) Run() error {
	s, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err := s.Init(); err != nil {
		return err
	}
	defer s.Fini()
	t.screen = s
	s.EnableMouse(tcell.MouseButtonEvents)
	s.Clear()

	// Skip dial when a fatal error was already recorded (e.g. link relay
	// unreachable). The TUI will render the error screen instead.
	if t.fatalErr == nil {
		if err := t.dial(); err != nil {
			t.setFatal(err)
		}
	}

	stopTick := make(chan struct{})
	defer close(stopTick)
	go func() {
		tk := time.NewTicker(200 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopTick:
				return
			case <-tk.C:
				_ = s.PostEvent(tcell.NewEventInterrupt(make(chan struct{})))
			}
		}
	}()

	for {
		select {
		case <-t.done:
			return nil
		default:
		}
		ev := s.PollEvent()
		if ev == nil {
			continue
		}
		switch e := ev.(type) {
		case *tcell.EventResize:
			t.handleResize()
		case *tcell.EventKey:
			t.handleKey(e)
		case *tcell.EventMouse:
			t.handleMouse(e)
		}
		t.draw()
		select {
		case <-t.done:
			return nil
		default:
		}
	}
}

// --- connection -------------------------------------------------------

func (t *tui) dial() error {
	d := t.dialer
	if d == nil {
		d = websocket.DefaultDialer
	}
	d.HandshakeTimeout = 12 * time.Second
	hdr := map[string][]string{
		"User-Agent": {fmt.Sprintf("LinkTerm/%s tui", Version)},
	}
	conn, resp, err := d.Dial(t.url, hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("connect %s: HTTP %d %v", t.url, resp.StatusCode, err)
		}
		return fmt.Errorf("connect %s: %w", t.url, err)
	}
	t.conn = conn
	t.setStatus("Connected to " + t.host())

	// On a direct connection (no linksocks) we measure latency with WS pings.
	// When a link RTT source is available (rttFn) the ping/pong result is
	// ignored and the linksocks RTT is preferred instead.
	conn.SetPongHandler(func(appData string) error {
		if t.rttFn == nil {
			if ts, err := strconv.ParseInt(appData, 10, 64); err == nil {
				t.setLatency(time.Since(time.UnixMilli(ts)))
			} else {
				t.setLatency(0)
			}
		}
		t.wake()
		return nil
	})

	w, h := t.screen.Size()
	t.vt = newVt(w, h-1)
	t.sendResize(w, h-1)
	t.ring.Logf("connected to %s", t.host())
	go t.readLoop()
	go t.pingLoop()
	return nil
}

func (t *tui) sendResize(cols, rows int) {
	if t.conn == nil {
		return
	}
	_ = t.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("resize:%d:%d", cols, rows)))
}

func (t *tui) pingLoop() {
	for {
		select {
		case <-t.done:
			return
		case <-time.After(2 * time.Second):
			if t.conn == nil {
				return
			}
			if t.rttFn != nil {
				t.setLatency(t.rttFn())
			} else {
				// no link RTT source: measure with a WebSocket ping
				ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
				_ = t.conn.WriteControl(websocket.PingMessage, []byte(ts), time.Now().Add(2*time.Second))
			}
			t.wake()
		}
	}
}

func (t *tui) readLoop() {
	for {
		if t.conn == nil {
			return
		}
		mt, p, err := t.conn.ReadMessage()
		if err != nil {
			t.connected = false
			t.setStatus("Disconnected: " + err.Error())
			t.ring.Logf("connection lost: %v", err)
			t.wake()
			return
		}
		switch mt {
		case websocket.TextMessage, websocket.BinaryMessage:
			t.vt.Feed(p)
			t.wake()
		}
	}
}

func (t *tui) wake() {
	if t.screen != nil {
		_ = t.screen.PostEvent(tcell.NewEventInterrupt(make(chan struct{})))
	}
}

func (t *tui) quit() {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	if t.conn != nil {
		_ = t.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
		_ = t.conn.Close()
	}
}

// --- state helpers ------------------------------------------------------

func (t *tui) host() string {
	u, err := url.Parse(t.url)
	if err != nil {
		return t.url
	}
	h := u.Host
	if h == "" {
		h = t.url
	}
	return h
}

func (t *tui) setStatus(s string) {
	t.mu.Lock()
	t.status = s
	t.mu.Unlock()
}

func (t *tui) statusStr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *tui) setLatency(d time.Duration) {
	t.mu.Lock()
	t.latency = d
	t.mu.Unlock()
}

func (t *tui) latencyStr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.latency == 0 {
		return "--"
	}
	return fmt.Sprintf("%dms", t.latency.Milliseconds())
}

// --- input ----------------------------------------------------------------

func (t *tui) handleResize() {
	if t.showLogs {
		return
	}
	w, h := t.screen.Size()
	if t.vt != nil {
		t.vt.resize(w, h-1)
		t.sendResize(w, h-1)
	}
}

func (t *tui) handleKey(ev *tcell.EventKey) {
	// global shortcuts (handled before forwarding)
	switch ev.Key() {
	case tcell.KeyF3, tcell.KeyCtrlQ:
		t.quit()
		return
	case tcell.KeyF2:
		t.showLogs = !t.showLogs
		t.logScroll = 0
		return
	}

	// content rewind controls while browsing history
	if t.vt != nil && t.vt.viewOffset > 0 {
		switch ev.Key() {
		case tcell.KeyUp, tcell.KeyPgUp:
			step := 1
			if ev.Key() == tcell.KeyPgUp {
				step = 10
			}
			t.vt.viewOffset += step
			return
		case tcell.KeyDown, tcell.KeyPgDn:
			step := 1
			if ev.Key() == tcell.KeyPgDn {
				step = 10
			}
			t.vt.viewOffset -= step
			if t.vt.viewOffset < 0 {
				t.vt.viewOffset = 0
			}
			return
		case tcell.KeyEscape, tcell.KeyHome, tcell.KeyEnter:
			t.vt.viewOffset = 0
			return
		}
	}

	b := t.keyToBytes(ev)
	if len(b) > 0 {
		if t.conn != nil {
			_ = t.conn.WriteMessage(websocket.TextMessage, b)
		}
	}
}

func (t *tui) handleMouse(ev *tcell.EventMouse) {
	switch {
	case ev.Buttons()&tcell.WheelUp != 0:
		if t.showLogs {
			t.logScroll++
		} else if t.vt != nil {
			t.vt.viewOffset++
			if max := t.vt.numLines() - t.vt.h; t.vt.viewOffset > max {
				t.vt.viewOffset = max
			}
		}
	case ev.Buttons()&tcell.WheelDown != 0:
		if t.showLogs {
			if t.logScroll > 0 {
				t.logScroll--
			}
		} else if t.vt != nil && t.vt.viewOffset > 0 {
			t.vt.viewOffset--
		}
	}
}

// --- drawing ----------------------------------------------------------------

func (t *tui) draw() {
	w, h := t.screen.Size()
	t.screen.Fill(' ', tcell.StyleDefault)

	if t.fatalErr != nil {
		t.drawError(w, h-1)
	} else if t.showLogs {
		// fullscreen log panel above the status bar
		t.drawLogs(w, 0, h-1)
	} else {
		ch := h - 1
		// keep vt sized to the full content area
		if t.vt == nil {
			t.vt = newVt(w, ch)
			t.sendResize(w, ch)
		} else if t.vt.w != w || t.vt.h != ch {
			t.vt.resize(w, ch)
			t.sendResize(w, ch)
		}
		t.vt.render(t.screen, 0, 0)
	}
	t.drawStatus(w, h-1)
	t.screen.Show()
}

// drawError renders a centered error screen shown when the initial connection
// failed (link relay unreachable, etc.). Details are in the F2 log panel.
func (t *tui) drawError(w, h int) {
	if t.fatalErr == nil {
		return
	}
	title := "Connection failed"
	body := wrapText(t.fatalErr.Error(), w-6)
	hint := "Press F2 to view logs, F3 to quit"

	borderStyle := tcell.StyleDefault.Foreground(tcell.ColorRed)
	titleStyle := tcell.StyleDefault.Foreground(tcell.ColorRed).Bold(true)
	bodyStyle := tcell.StyleDefault.Foreground(tcell.ColorSilver)
	hintStyle := tcell.StyleDefault.Foreground(tcell.ColorSilver)

	// title at ~1/3 height, body below, hint at the bottom area
	titleY := h / 3
	if titleY < 1 {
		titleY = 1
	}
	for i, r := range title {
		if titleY >= 0 && titleY < h {
			t.screen.SetContent(w/2-len([]rune(title))/2+i, titleY, r, nil, titleStyle)
		}
	}
	// draw a line under the title
	if titleY+1 < h {
		for x := w / 4; x < 3*w/4; x++ {
			t.screen.SetContent(x, titleY+1, '─', nil, borderStyle)
		}
	}
	for i, ln := range body {
		y := titleY + 3 + i
		if y >= h {
			break
		}
		t.putLine(3, y, w-6, ln, bodyStyle)
	}
	hy := h - 2
	if hy >= 0 {
		t.putLine(3, hy, w-6, hint, hintStyle)
	}
}

func (t *tui) drawLogs(w, y, count int) {
	lines := t.ring.snapshot()
	style := tcell.StyleDefault.Foreground(tcell.ColorSilver)
	// render the last `count` lines; logScroll rewinds from the end
	end := len(lines) - t.logScroll
	start := end - count
	if start < 0 {
		start = 0
	}
	for i := 0; i < count && start+i < len(lines); i++ {
		t.putLine(0, y+i, w, lines[start+i], style)
	}
}

// wrapText wraps s (splitting on newlines and width) into rune-safe lines.
func wrapText(s string, width int) []string {
	if width <= 0 {
		width = 1
	}
	var out []string
	for _, seg := range strings.Split(s, "\n") {
		runes := []rune(seg)
		for len(runes) > width {
			out = append(out, string(runes[:width]))
			runes = runes[width:]
		}
		out = append(out, string(runes))
	}
	return out
}

// putLine renders text into a single screen row with the given style.
func (t *tui) putLine(x, y, max int, text string, style tcell.Style) {
	if x < 0 {
		x = 0
	}
	for i, r := range []rune(text) {
		if i >= max {
			break
		}
		t.screen.SetContent(x+i, y, r, nil, style)
	}
}

func (t *tui) drawStatus(w, y int) {
	style := tcell.StyleDefault.Background(tcell.ColorNavy).Foreground(tcell.ColorWhite)
	// clear line
	for i := 0; i < w; i++ {
		t.screen.SetContent(i, y, ' ', nil, style)
	}
	left := "● " + t.statusStr()
	mid := "Latency " + t.latencyStr()
	right := "F2 Logs  F3 Quit"
	// draw with the status style
	for i, r := range left {
		t.screen.SetContent(i, y, r, nil, style)
	}
	cx := w/2 - len([]rune(mid))/2
	if cx < 0 {
		cx = 0
	}
	for i, r := range mid {
		t.screen.SetContent(cx+i, y, r, nil, style)
	}
	rx := w - len([]rune(right))
	if rx < 0 {
		rx = 0
	}
	for i, r := range right {
		t.screen.SetContent(rx+i, y, r, nil, style)
	}
}

// keyToBytes encodes a key event as bytes to forward to the remote terminal.
func (t *tui) keyToBytes(ev *tcell.EventKey) []byte {
	if ev.Rune() != 0 {
		r := ev.Rune()
		if ev.Modifiers()&tcell.ModCtrl != 0 && r >= 'a' && r <= 'z' {
			return []byte{byte(r - 'a' + 1)}
		}
		if ev.Modifiers()&tcell.ModCtrl != 0 && r >= 'A' && r <= 'Z' {
			return []byte{byte(r - 'A' + 1)}
		}
		return []byte(string(r))
	}
	switch ev.Key() {
	case tcell.KeyEnter:
		return []byte{'\r'}
	case tcell.KeyTab:
		return []byte{'\t'}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return []byte{0x7f}
	case tcell.KeyDelete:
		return []byte{0x1b, '[', '3', '~'}
	case tcell.KeyEscape:
		return []byte{0x1b}
	case tcell.KeyUp:
		return []byte{0x1b, '[', 'A'}
	case tcell.KeyDown:
		return []byte{0x1b, '[', 'B'}
	case tcell.KeyRight:
		return []byte{0x1b, '[', 'C'}
	case tcell.KeyLeft:
		return []byte{0x1b, '[', 'D'}
	case tcell.KeyHome:
		return []byte{0x1b, '[', '1', '~'}
	case tcell.KeyEnd:
		return []byte{0x1b, '[', '4', '~'}
	case tcell.KeyPgUp:
		return []byte{0x1b, '[', '5', '~'}
	case tcell.KeyPgDn:
		return []byte{0x1b, '[', '6', '~'}
	case tcell.KeyInsert:
		return []byte{0x1b, '[', '2', '~'}
	}

	// function keys (xterm sequences)
	fmap := map[int]string{
		int(tcell.KeyF1): "11", int(tcell.KeyF2): "12", int(tcell.KeyF3): "13",
		int(tcell.KeyF4): "14", int(tcell.KeyF5): "15", int(tcell.KeyF6): "17",
		int(tcell.KeyF7): "18", int(tcell.KeyF8): "19", int(tcell.KeyF9): "20",
		int(tcell.KeyF10): "21", int(tcell.KeyF11): "23", int(tcell.KeyF12): "24",
	}
	if code, ok := fmap[int(ev.Key())]; ok {
		b := []byte{0x1b, '['}
		b = append(b, []byte(code)...)
		b = append(b, '~')
		return b
	}

	switch ev.Key() {
	case tcell.KeyCtrlC:
		return []byte{0x03}
	case tcell.KeyCtrlD:
		return []byte{0x04}
	case tcell.KeyCtrlZ:
		return []byte{0x1a}
	}
	return nil
}
