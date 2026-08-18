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

	showLogs  bool
	logScroll int

	// relay connection monitoring
	relayClient interface {
		ConnectedChan() <-chan struct{}
		DisconnectedChan() <-chan struct{}
	}
	relayStatus string
	relayMu     sync.Mutex

	// reconnect
	reconnectCh chan struct{}

	// clickable status bar regions (x range, set during drawStatus)
	clickF2X, clickF3X [2]int
}

func newTUI(target string, dialer *websocket.Dialer, rttFn func() time.Duration) *tui {
	return &tui{
		url:         target,
		dialer:      dialer,
		rttFn:       rttFn,
		ring:        newLogRing(3000),
		done:        make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
	}
}

// setRelayStatus updates the relay connection state shown in the status bar.
func (t *tui) setRelayStatus(s string) {
	t.relayMu.Lock()
	t.relayStatus = s
	t.relayMu.Unlock()
}

func (t *tui) relayStatusStr() string {
	t.relayMu.Lock()
	defer t.relayMu.Unlock()
	return t.relayStatus
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

	if err := t.dial(); err != nil {
		return err
	}

	// start background workers
	t.setRelayStatus("connected")
	if t.relayClient != nil {
		go t.monitorRelay()
	}
	go t.reconnectLoop()

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
	t.connected = true
	t.setStatus("Connected to " + t.host())

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
	if t.vt == nil {
		t.vt = newVt(w, h-1)
	}
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
			// signal reconnect loop
			select {
			case t.reconnectCh <- struct{}{}:
			default:
			}
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

// reconnectLoop listens for disconnect signals and attempts to re-establish
// the terminal WebSocket connection with exponential backoff.
func (t *tui) reconnectLoop() {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	for {
		select {
		case <-t.done:
			return
		case <-t.reconnectCh:
			t.setStatus(fmt.Sprintf("Reconnecting in %v...", backoff))
			for {
				select {
				case <-t.done:
					return
				default:
				}
				t.ring.Logf("reconnecting in %v...", backoff)
				time.Sleep(backoff)
				if err := t.dial(); err != nil {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					continue
				}
				t.ring.Logf("reconnected successfully")
				backoff = 1 * time.Second
				break
			}
		}
	}
}

// monitorRelay watches the link relay connection state (via ConnectedChan /
// DisconnectedChan) and updates the status bar accordingly.
func (t *tui) monitorRelay() {
	for {
		select {
		case <-t.done:
			return
		default:
		}
		// block until the relay disconnects
		<-t.relayClient.DisconnectedChan()
		t.setRelayStatus("disconnected")
		t.ring.Logf("link relay disconnected")
		t.wake()
		// block until the relay reconnects
		<-t.relayClient.ConnectedChan()
		t.setRelayStatus("connected")
		t.ring.Logf("link relay reconnected")
		t.wake()
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

	// PgUp/PgDn in the log panel scroll by a full page
	if t.showLogs {
		_, h := t.screen.Size()
		page := h - 1
		if page < 1 {
			page = 1
		}
		switch ev.Key() {
		case tcell.KeyPgUp:
			t.logScroll += page
			return
		case tcell.KeyPgDn:
			t.logScroll -= page
			if t.logScroll < 0 {
				t.logScroll = 0
			}
			return
		}
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
	x, y := ev.Position()
	_, h := t.screen.Size()

	// left-click on the status bar regions
	if y == h-1 && ev.Buttons() == tcell.Button1 {
		if x >= t.clickF2X[0] && x < t.clickF2X[1] {
			t.showLogs = !t.showLogs
			t.logScroll = 0
			return
		}
		if x >= t.clickF3X[0] && x < t.clickF3X[1] {
			t.quit()
			return
		}
		return
	}

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

	if t.showLogs {
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

func (t *tui) drawLogs(w, y, count int) {
	lines := t.ring.snapshot()
	// render the last `count` lines; logScroll rewinds from the end
	end := len(lines) - t.logScroll
	start := end - count
	if start < 0 {
		start = 0
	}
	for i := 0; i < count && start+i < len(lines); i++ {
		line := lines[start+i]
		style := logLineStyle(line)
		t.putLine(0, y+i, w, line, style)
	}
}

// logLineStyle selects a colour based on the zerolog level marker in the line.
func logLineStyle(line string) tcell.Style {
	// ConsoleWriter format: "<time> <LEVEL> <message>"
	switch {
	case strings.Contains(line, " ERR "), strings.Contains(line, " FTAL "):
		return tcell.StyleDefault.Foreground(tcell.ColorRed)
	case strings.Contains(line, " WRN "):
		return tcell.StyleDefault.Foreground(tcell.ColorYellow)
	case strings.Contains(line, " DBG "), strings.Contains(line, " TRC "):
		return tcell.StyleDefault.Foreground(tcell.ColorGray)
	default:
		return tcell.StyleDefault.Foreground(tcell.ColorSilver)
	}
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
	// dark modern theme: dark background, light text, subtle accent
	barBg := tcell.NewRGBColor(0x23, 0x27, 0x2e)
	barFg := tcell.NewRGBColor(0xc8, 0xcc, 0xd4)
	accent := tcell.NewRGBColor(0x52, 0x9e, 0xff)
	style := tcell.StyleDefault.Background(barBg).Foreground(barFg)

	// clear line
	for i := 0; i < w; i++ {
		t.screen.SetContent(i, y, ' ', nil, style)
	}

	// left side: indicator + host + relay status
	indicator := "●"
	indicatorColor := tcell.NewRGBColor(0x3e, 0xb4, 0x6b) // green dot
	relay := t.relayStatusStr()
	if relay == "disconnected" || !t.connected {
		indicator = "○"
		indicatorColor = tcell.NewRGBColor(0xe0, 0x60, 0x60) // red dot
		relay = "disconnected"
	} else if relay == "reconnecting" {
		indicator = "○"
		indicatorColor = tcell.NewRGBColor(0xe0, 0xc0, 0x40) // amber dot
	}
	indStyle := tcell.StyleDefault.Background(barBg).Foreground(indicatorColor)
	t.screen.SetContent(0, y, []rune(indicator)[0], nil, indStyle)

	left := t.host()
	if relay != "" && relay != "connected" {
		left += " | " + relay
	}
	cx := 2
	for i, r := range left {
		t.screen.SetContent(cx+i, y, r, nil, style)
	}

	// middle: latency (hidden when disconnected)
	mid := ""
	if t.connected {
		mid = "Latency " + t.latencyStr()
	}
	if mid != "" {
		mx := w/2 - len([]rune(mid))/2
		if mx < 0 {
			mx = 0
		}
		for i, r := range mid {
			t.screen.SetContent(mx+i, y, r, nil, style)
		}
	}

	// right side: clickable hotkey hints
	right := "F2 Logs  F3 Quit"
	rx := w - len([]rune(right))
	if rx < 0 {
		rx = 0
	}
	// record clickable regions ("F2 Logs" and "F3 Quit", two spaces between)
	f2Len := len([]rune("F2 Logs"))
	f3Len := len([]rune("F3 Quit"))
	t.clickF2X = [2]int{rx, rx + f2Len}
	t.clickF3X = [2]int{rx + f2Len + 2, rx + f2Len + 2 + f3Len}

	hotkeyStyle := tcell.StyleDefault.Background(barBg).Foreground(accent)
	for i, r := range right {
		s := style
		if i < f2Len || (i >= f2Len+2 && i < f2Len+2+f3Len) {
			s = hotkeyStyle
		}
		t.screen.SetContent(rx+i, y, r, nil, s)
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
