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
	"github.com/rs/zerolog"
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
	pathFn func() string        // reports data path ("direct"/"relay"), nil = direct

	// connectFn sets up the link relay / proxy before the terminal dial and
	// fills dialer, rttFn, pathFn and relayClient on success. Nil means the
	// terminal is dialed directly. It runs asynchronously after the TUI
	// screen starts, so connection progress and failures stay visible.
	connectFn func() error

	screen           tcell.Screen
	vt               *vt
	conn             *websocket.Conn
	ring             *logRing
	logger           zerolog.Logger
	done             chan struct{}
	quitOnce         sync.Once
	mouseQuitPending bool

	mu         sync.Mutex
	connected  bool
	status     string
	latency    time.Duration
	connectErr error

	showLogs  bool
	logScroll int

	// log panel text selection (screen coordinates inside the panel)
	selActive                  bool
	selMoved                   bool
	selX0, selY0, selX1, selY1 int

	// terminal content selection (screen coordinates above the status bar)
	termSelActive                              bool
	termSelMoved                               bool
	termSelX0, termSelY0, termSelX1, termSelY1 int

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
	ring := newLogRing(3000)
	return &tui{
		url:    target,
		dialer: dialer,
		rttFn:  rttFn,
		ring:   ring,
		logger: zerolog.New(zerolog.ConsoleWriter{
			Out: ring, NoColor: true, TimeFormat: time.RFC3339,
		}).With().Timestamp().Logger(),
		done:        make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
		selX0:       -1,
		selY0:       -1,
		selX1:       -1,
		selY1:       -1,
		termSelX0:   -1,
		termSelY0:   -1,
		termSelX1:   -1,
		termSelY1:   -1,
	}
}

// setRelayStatus updates the relay connection state shown in the status bar.
func (t *tui) setRelayStatus(s string) {
	t.relayMu.Lock()
	t.relayStatus = s
	t.relayMu.Unlock()
}

// setPathFn records the data path reporter; called from the connect goroutine.
func (t *tui) setPathFn(fn func() string) {
	t.mu.Lock()
	t.pathFn = fn
	t.mu.Unlock()
}

func (t *tui) pathStr() string {
	t.mu.Lock()
	fn := t.pathFn
	t.mu.Unlock()
	if fn == nil {
		return ""
	}
	return fn()
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
	s.EnableMouse(tcell.MouseButtonEvents, tcell.MouseDragEvents)
	s.EnablePaste()
	s.Clear()

	// Show a connecting hint immediately; the relay setup and terminal dial
	// run asynchronously below so the screen never looks frozen.
	t.setStatus("Connecting to " + t.host() + "...")
	t.draw()

	// start background workers
	t.setRelayStatus("connected")
	go t.connectLoop()
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
			// return the startup connection error (if any) so the caller
			// can exit non-zero with a concise message; the screen is
			// already Fini'd by the defer, so stderr is safe again.
			t.mu.Lock()
			ce := t.connectErr
			t.mu.Unlock()
			return ce
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
		select {
		case <-t.done:
			t.mu.Lock()
			ce := t.connectErr
			t.mu.Unlock()
			return ce
		default:
		}
		t.draw()
		select {
		case <-t.done:
			t.mu.Lock()
			ce := t.connectErr
			t.mu.Unlock()
			return ce
		default:
		}
	}
}

// --- connection -------------------------------------------------------

// connectLoop sets up the link relay / proxy first, then dials the terminal.
// Runs once at startup, asynchronously while the TUI keeps drawing.
func (t *tui) connectLoop() {
	if t.connectFn != nil {
		t.logger.Info().Msg("setting up link connection...")
		if err := t.connectFn(); err != nil {
			t.setConnectError(err)
			t.logger.Error().Err(err).Msg("connection failed")
			return
		}
	}
	select {
	case <-t.done:
		return
	default:
	}
	if err := t.dial(); err != nil {
		t.setConnectError(err)
		t.logger.Error().Err(err).Msg("connection failed")
		return
	}
	t.setRelayStatus("connected")
	if t.relayClient != nil {
		go t.monitorRelay()
	}
}

// setConnectError records a failed startup connection; the TUI keeps
// running so the user can read the log panel and quit with F3. A nil error
// clears any previous failure.
func (t *tui) setConnectError(err error) {
	t.mu.Lock()
	t.connectErr = err
	if err != nil {
		t.status = "Connection failed"
	}
	t.mu.Unlock()
	t.wake()
}

func (t *tui) connectErrStatus() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connectErr
}

// setConnected updates the connection flag (also used by readLoop).
func (t *tui) setConnected(v bool) {
	t.mu.Lock()
	t.connected = v
	t.mu.Unlock()
}

func (t *tui) connectedStatus() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connected
}

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
	t.setConn(conn)
	t.setConnectError(nil)
	t.setConnected(true)
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

	// the draw loop owns the vt (it exists / is resized there), so only
	// announce the new terminal size from here.
	w, h := t.screen.Size()
	t.sendResize(w, h-1)
	t.logger.Info().Msgf("connected to %s", t.host())
	go t.readLoop()
	go t.pingLoop()
	return nil
}

// --- conn access is goroutine-safe: dial (connect/reconnect loops) writes,
// main loop and workers read. ---

func (t *tui) setConn(c *websocket.Conn) {
	t.mu.Lock()
	t.conn = c
	t.mu.Unlock()
}

func (t *tui) connRef() *websocket.Conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn
}

func (t *tui) sendResize(cols, rows int) {
	c := t.connRef()
	if c == nil {
		return
	}
	_ = c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("resize:%d:%d", cols, rows)))
}

func (t *tui) pingLoop() {
	for {
		select {
		case <-t.done:
			return
		case <-time.After(2 * time.Second):
			c := t.connRef()
			if c == nil {
				return
			}
			if t.rttFn != nil {
				t.setLatency(t.rttFn())
			} else {
				// no link RTT source: measure with a WebSocket ping
				ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
				_ = c.WriteControl(websocket.PingMessage, []byte(ts), time.Now().Add(2*time.Second))
			}
			t.wake()
		}
	}
}

func (t *tui) readLoop() {
	for {
		c := t.connRef()
		if c == nil {
			return
		}
		mt, p, err := c.ReadMessage()
		if err != nil {
			t.setConnected(false)
			t.setStatus("Disconnected: " + err.Error())
			t.logger.Error().Err(err).Msg("connection lost")
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
				t.logger.Warn().Dur("backoff", backoff).Msg("reconnecting")
				time.Sleep(backoff)
				if err := t.dial(); err != nil {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					continue
				}
				t.logger.Info().Msg("reconnected successfully")
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
		t.logger.Error().Msg("link relay disconnected")
		t.wake()
		// block until the relay reconnects
		<-t.relayClient.ConnectedChan()
		t.setRelayStatus("connected")
		t.logger.Info().Msg("link relay reconnected")
		t.wake()
	}
}

func (t *tui) wake() {
	select {
	case <-t.done:
		return // screen may already be finalized
	default:
	}
	if t.screen != nil {
		_ = t.screen.PostEvent(tcell.NewEventInterrupt(make(chan struct{})))
	}
}

func (t *tui) quit() {
	t.closeConn()
	t.finishQuit()
}

func (t *tui) quitAfterMouseRelease() {
	t.closeConn()
	t.mouseQuitPending = true
	time.AfterFunc(500*time.Millisecond, t.finishQuit)
}

func (t *tui) finishQuit() {
	t.quitOnce.Do(func() {
		close(t.done)
		if t.screen != nil {
			_ = t.screen.PostEvent(tcell.NewEventInterrupt(make(chan struct{})))
		}
	})
}

func (t *tui) closeConn() {
	c := t.connRef()
	if c != nil {
		_ = c.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
		_ = c.Close()
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
	contentHeight := t.contentHeight(h)
	if t.vt != nil {
		t.vt.resize(w, contentHeight)
		t.sendResize(w, contentHeight)
	}
}

func (t *tui) contentHeight(screenHeight int) int {
	if screenHeight <= 1 {
		return 1
	}
	return screenHeight - 1
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
		t.clearSelection()
		t.clearTerminalSelection()
		return
	}

	// PgUp/PgDn in the log panel scroll by a full page
	if t.showLogs {
		if ev.Key() == tcell.KeyCtrlC {
			t.copySelection()
			return
		}
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
	if ev.Key() == tcell.KeyCtrlC && t.hasTerminalSelection() {
		t.copyTerminalSelection()
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
		if c := t.connRef(); c != nil {
			_ = c.WriteMessage(websocket.TextMessage, b)
		}
	}
}

func (t *tui) handleMouse(ev *tcell.EventMouse) {
	if t.mouseQuitPending {
		if ev.Buttons() == tcell.ButtonNone {
			t.finishQuit()
		}
		return
	}
	x, y := ev.Position()
	_, h := t.screen.Size()

	// left-click on the status bar regions
	if y == h-1 && ev.Buttons() == tcell.Button1 {
		if x >= t.clickF2X[0] && x < t.clickF2X[1] {
			t.showLogs = !t.showLogs
			t.logScroll = 0
			t.clearSelection()
			t.clearTerminalSelection()
			return
		}
		if x >= t.clickF3X[0] && x < t.clickF3X[1] {
			t.quitAfterMouseRelease()
			return
		}
		t.clearSelection()
		t.clearTerminalSelection()
		return
	}

	// drag-select text in the log panel (Button1 down / motion / up)
	if t.showLogs && y < h-1 {
		switch {
		case ev.Buttons() == tcell.Button1:
			if !t.selActive {
				t.clearSelection()
				t.clearTerminalSelection()
				t.selActive = true
				t.selX0, t.selY0 = x, y
				t.selX1, t.selY1 = x, y
				t.selMoved = false
			} else if x != t.selX0 || y != t.selY0 {
				t.selMoved = true
			}
			t.selX1, t.selY1 = x, y
			return
		case ev.Buttons() == tcell.ButtonNone && t.selActive:
			t.selActive = false
			if !t.selMoved {
				t.clearSelection()
			}
			return
		}
	}

	// drag-select rendered terminal text above the status bar
	if !t.showLogs && y < h-1 {
		switch {
		case ev.Buttons() == tcell.Button1:
			if !t.termSelActive {
				t.clearSelection()
				t.clearTerminalSelection()
				t.termSelActive = true
				t.termSelX0, t.termSelY0 = x, y
				t.termSelX1, t.termSelY1 = x, y
				t.termSelMoved = false
			} else if x != t.termSelX0 || y != t.termSelY0 {
				t.termSelMoved = true
			}
			t.termSelX1, t.termSelY1 = x, y
			return
		case ev.Buttons() == tcell.ButtonNone && t.termSelActive:
			t.termSelActive = false
			if !t.termSelMoved {
				t.clearTerminalSelection()
			}
			return
		}
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

// --- log panel selection ---------------------------------------------------

func (t *tui) clearSelection() {
	t.selActive = false
	t.selMoved = false
	t.selX0, t.selY0, t.selX1, t.selY1 = -1, -1, -1, -1
}

func (t *tui) clearTerminalSelection() {
	t.termSelActive = false
	t.termSelMoved = false
	t.termSelX0, t.termSelY0, t.termSelX1, t.termSelY1 = -1, -1, -1, -1
}

func (t *tui) hasSelection() bool {
	return t.selMoved && t.selX0 >= 0 && t.selY0 >= 0 && t.selX1 >= 0 && t.selY1 >= 0
}

func (t *tui) hasTerminalSelection() bool {
	return t.termSelMoved && t.termSelX0 >= 0 && t.termSelY0 >= 0 && t.termSelX1 >= 0 && t.termSelY1 >= 0
}

// selRange returns the normalized selection rectangle (top-left/bottom-right).
func (t *tui) selRange() (y0, y1, x0, x1 int) {
	if !t.hasSelection() {
		return -1, -1, -1, -1
	}
	y0, x0 = t.selY0, t.selX0
	y1, x1 = t.selY1, t.selX1
	if y0 > y1 || (y0 == y1 && x0 > x1) {
		y0, y1 = y1, y0
		x0, x1 = x1, x0
	}
	return
}

func (t *tui) terminalSelectionRange() (y0, y1, x0, x1 int) {
	if !t.hasTerminalSelection() {
		return -1, -1, -1, -1
	}
	y0, x0 = t.termSelY0, t.termSelX0
	y1, x1 = t.termSelY1, t.termSelX1
	if y0 > y1 || (y0 == y1 && x0 > x1) {
		y0, y1 = y1, y0
		x0, x1 = x1, x0
	}
	return
}

// copySelection copies the selected log text to the clipboard (OSC 52).
func (t *tui) copySelection() {
	if !t.hasSelection() {
		return
	}
	w, h := t.screen.Size()
	count := h - 1
	if count < 1 {
		count = 1
	}
	lines := t.ring.snapshot()
	rows := make([]logRow, 0, len(lines))
	for li, line := range lines {
		rows = append(rows, wrapLogLine(line, li, w)...)
	}
	end := len(rows) - t.logScroll
	start := end - count
	if start < 0 {
		start = 0
	}
	selY0, selY1, selX0, selX1 := t.selRange()
	if selY0 < 0 {
		return
	}
	var b strings.Builder
	for i := 0; i < count && start+i < len(rows); i++ {
		sy := i
		if sy < selY0 || sy > selY1 {
			continue
		}
		r := rows[start+i]
		c0, c1 := 0, w-1
		if sy == selY0 {
			c0 = selX0
		}
		if sy == selY1 {
			c1 = selX1
		}
		b.WriteString(textColumns(r.text, c0, c1))
		if sy < selY1 {
			b.WriteByte('\n')
		}
	}
	if b.Len() > 0 {
		t.screen.SetClipboard([]byte(b.String()))
		t.logger.Info().Msg("copied selection to clipboard")
	}
}

func textColumns(text string, start, end int) string {
	if start > end {
		return ""
	}
	var b strings.Builder
	col := 0
	for _, r := range []rune(text) {
		rw := runeWidth(r)
		if col+rw > start && col <= end {
			b.WriteRune(r)
		}
		col += rw
		if col > end {
			break
		}
	}
	return b.String()
}

// --- drawing ----------------------------------------------------------------

func (t *tui) draw() {
	w, h := t.screen.Size()
	t.screen.Fill(' ', tcell.StyleDefault)
	contentHeight := t.contentHeight(h)

	if t.showLogs {
		// fullscreen log panel above the status bar
		t.drawLogs(w, 0, contentHeight)
	} else {
		ch := contentHeight
		// keep vt sized to the full content area
		if t.vt == nil {
			t.vt = newVt(w, ch)
			t.sendResize(w, ch)
		} else if t.vt.w != w || t.vt.h != ch {
			t.vt.resize(w, ch)
			t.sendResize(w, ch)
		}
		t.vt.render(t.screen, 0, 0)
		t.drawTerminalSelection(w, ch)
	}

	// overlay a failure notice in the content area so the error is visible
	// even while the log panel is closed (vt is empty before the first
	// successful dial, so both branches can show it).
	if !t.showLogs {
		err := t.connectErrStatus()
		if err != nil && !t.connectedStatus() {
			lines := strings.Split(err.Error(), "\n")
			msg := "Connection failed - press F2 for logs, F3 to quit"
			max := w - 4
			if max < 1 {
				max = 1
			}
			for i, l := range lines {
				if i >= contentHeight-1 {
					break
				}
				style := tcell.StyleDefault.Foreground(tcell.ColorRed)
				t.putLine(2, 2+i, max, l, style)
			}
			style := tcell.StyleDefault.Foreground(tcell.ColorYellow)
			t.putLine(2, 2+len(lines), max, msg, style)
		}
	}
	if h > 0 {
		t.drawStatus(w, h-1)
	}
	t.screen.Show()
}

func (t *tui) drawTerminalSelection(w, h int) {
	y0, y1, x0, x1 := t.terminalSelectionRange()
	if y0 < 0 {
		return
	}
	if y0 >= h || x0 >= w {
		return
	}
	if y1 >= h {
		y1 = h - 1
	}
	if x1 >= w {
		x1 = w - 1
	}
	selBg := tcell.NewRGBColor(0x26, 0x4f, 0x78)
	for y := y0; y <= y1; y++ {
		start, end := 0, w-1
		if y == y0 {
			start = x0
		}
		if y == y1 {
			end = x1
		}
		for x := start; x <= end; x++ {
			primary, combining, style, width := t.screen.GetContent(x, y)
			if width == 0 {
				continue
			}
			t.screen.SetContent(x, y, primary, combining, style.Background(selBg))
		}
	}
}

func (t *tui) copyTerminalSelection() {
	w, h := t.screen.Size()
	y0, y1, x0, x1 := t.terminalSelectionRange()
	if y0 < 0 || y0 >= h || x0 >= w {
		return
	}
	if y1 >= h {
		y1 = h - 1
	}
	if x1 >= w {
		x1 = w - 1
	}
	var b strings.Builder
	for y := y0; y <= y1; y++ {
		start, end := 0, w-1
		if y == y0 {
			start = x0
		}
		if y == y1 {
			end = x1
		}
		var line strings.Builder
		for x := start; x <= end; x++ {
			primary, _, _, width := t.screen.GetContent(x, y)
			if width > 0 && primary != 0 {
				line.WriteRune(primary)
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		if y < y1 {
			b.WriteByte('\n')
		}
	}
	text := strings.TrimRight(b.String(), "\n")
	if text == "" {
		return
	}
	t.screen.SetClipboard([]byte(text))
	t.logger.Info().Msg("copied terminal selection to clipboard")
}

// drawLogs renders the tail of the wrapped log; logScroll rewinds from the
// end. Selected cells (see selRange) get a highlight background.
func (t *tui) drawLogs(w, y, count int) {
	lines := t.ring.snapshot()
	rows := make([]logRow, 0, len(lines))
	for li, line := range lines {
		rows = append(rows, wrapLogLine(line, li, w)...)
	}
	// render the last `count` rows; logScroll rewinds from the end
	end := len(rows) - t.logScroll
	start := end - count
	if start < 0 {
		start = 0
	}
	selY0, selY1, selX0, selX1 := t.selRange()
	for i := 0; i < count && start+i < len(rows); i++ {
		r := rows[start+i]
		sy := y + i
		t.putLogRow(w, y+i, r, func(x int) bool {
			if sy < selY0 || sy > selY1 {
				return false
			}
			if sy == selY0 && sy == selY1 {
				return x >= selX0 && x <= selX1
			}
			if sy == selY0 {
				return x >= selX0
			}
			if sy == selY1 {
				return x <= selX1
			}
			return true
		})
	}
}

// logRow is one wrapped segment of a log line.
type logRow struct {
	level    string // "", "ERR", "WRN", ... extracted from the line
	text     string // this segment (may be narrower than the full line)
	lineIdx  int    // index of the original line in the ring
	colStart int    // rune offset of this segment within the original line
	tsEnd    int    // rune offset just past the timestamp (-1 if no level)
	levelEnd int    // rune offset just past the level marker (-1 if none)
}

// wrapLogLine splits a log line into width-limited segments so the panel
// wraps long lines. Each segment remembers its position in the original
// line (for selection/copy) and the prefix extent (for level colouring).
func wrapLogLine(line string, lineIdx, w int) []logRow {
	runes := []rune(line)
	if w < 1 {
		w = 1
	}
	// locate the " LEVEL " marker; everything before it is the timestamp
	level, levelStart := "", -1
	for _, lv := range []string{"FTAL", "ERR", "WRN", "INF", "DBG", "TRC"} {
		if i := strings.Index(line, " "+lv+" "); i >= 0 {
			level = lv
			levelStart = i + 1
			break
		}
	}
	tsEnd := -1
	levelEnd := -1
	if levelStart >= 0 {
		tsEnd = levelStart - 1
		levelEnd = levelStart + len(level)
	}
	var rows []logRow
	start := 0
	for start < len(runes) {
		width := 0
		end := start
		for end < len(runes) {
			rw := runeWidth(runes[end])
			if width+rw > w {
				break
			}
			width += rw
			end++
		}
		if end == start {
			end = start + 1
		}
		rows = append(rows, logRow{
			level:    level,
			text:     string(runes[start:end]),
			lineIdx:  lineIdx,
			colStart: start,
			tsEnd:    tsEnd,
			levelEnd: levelEnd,
		})
		start = end
	}
	if len(rows) == 0 {
		rows = []logRow{{level: level, text: "", lineIdx: lineIdx, tsEnd: tsEnd, levelEnd: levelEnd}}
	}
	return rows
}

// splitLogLevel splits "2026-08-21T12:15:19+08:00 ERR message" into the
// level and the remainder (message). Returns ("", line) when no marker.
func splitLogLevel(line string) (string, string) {
	for _, lv := range []string{"FTAL", "ERR", "WRN", "INF", "DBG", "TRC"} {
		marker := " " + lv + " "
		if i := strings.Index(line, marker); i >= 0 {
			return lv, line[i+len(marker):]
		}
	}
	return "", line
}

// putLogRow renders one wrapped segment. Colouring is positional: the
// timestamp part is dim, the level marker uses the level colour, the
// message stays light. Selected cells get a highlight background.
// Wide (CJK) runes occupy two cells; the right half is a continuation
// marker that tcell draws as part of the wide glyph.
func (t *tui) putLogRow(w, y int, r logRow, selCell func(x int) bool) {
	msgCol := tcell.NewRGBColor(0xd8, 0xdc, 0xe2)
	tsCol := tcell.NewRGBColor(0x5c, 0x64, 0x70)
	selBg := tcell.NewRGBColor(0x26, 0x4f, 0x78)

	x := 0
	runes := []rune(r.text)
	for i := 0; i < len(runes) && x < w; i++ {
		c := runes[i]
		rw := runeWidth(c)
		pos := r.colStart + i
		st := tcell.StyleDefault.Foreground(msgCol)
		if r.level != "" && pos < r.levelEnd && r.tsEnd >= 0 {
			if pos <= r.tsEnd {
				st = tcell.StyleDefault.Foreground(tsCol)
			} else {
				st = tcell.StyleDefault.Foreground(logLevelColor(r.level))
			}
		}
		if selCell(x) {
			st = st.Background(selBg)
		}
		t.screen.SetContent(x, y, c, nil, st)
		x += rw
		if rw > 1 && x < w {
			// continuation cell: rendered as part of the wide rune
			st2 := st
			if selCell(x) {
				st2 = st2.Background(selBg)
			}
			t.screen.SetContent(x, y, 0, nil, st2)
			x++
		}
	}
}

// logLevelColor maps a zerolog level to a colour.
func logLevelColor(level string) tcell.Color {
	switch level {
	case "ERR", "FTAL":
		return tcell.ColorRed
	case "WRN":
		return tcell.ColorYellow
	case "DBG", "TRC":
		return tcell.ColorGray
	default:
		return tcell.NewRGBColor(0x52, 0x9e, 0xff)
	}
}

// logLineStyle selects a colour based on the zerolog level marker in the
// line (used for whole-line rendering, e.g. the failure overlay).
func logLineStyle(line string) tcell.Style {
	level, _ := splitLogLevel(line)
	return tcell.StyleDefault.Foreground(logLevelColor(level))
}

func (t *tui) putLine(x, y, max int, text string, style tcell.Style) {
	if x < 0 {
		x = 0
	}
	col := 0
	for _, r := range []rune(text) {
		rw := runeWidth(r)
		if col+rw > max {
			break
		}
		t.screen.SetContent(x+col, y, r, nil, style)
		if rw > 1 && col+1 < max {
			t.screen.SetContent(x+col+1, y, 0, nil, style)
		}
		col += rw
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

	// left side: indicator + status / host + relay status
	indicator := "●"
	indicatorColor := tcell.NewRGBColor(0x3e, 0xb4, 0x6b) // green dot
	relay := t.relayStatusStr()
	connected := t.connectedStatus()
	if !connected {
		// connecting or failed: amber while in progress, red on failure
		indicator = "○"
		connectErr := t.connectErrStatus()
		if connectErr != nil {
			indicatorColor = tcell.NewRGBColor(0xe0, 0x60, 0x60) // red dot
		} else {
			indicatorColor = tcell.NewRGBColor(0xe0, 0xc0, 0x40) // amber dot
		}
		relay = "disconnected"
	} else if relay == "disconnected" {
		indicator = "○"
		indicatorColor = tcell.NewRGBColor(0xe0, 0x60, 0x60) // red dot
	} else if relay == "reconnecting" {
		indicator = "○"
		indicatorColor = tcell.NewRGBColor(0xe0, 0xc0, 0x40) // amber dot
	}
	indStyle := tcell.StyleDefault.Background(barBg).Foreground(indicatorColor)
	t.screen.SetContent(0, y, []rune(indicator)[0], nil, indStyle)

	left := t.host()
	if !connected {
		if s := t.statusStr(); s != "" {
			left = s
		}
	} else if relay != "" && relay != "connected" {
		left += " | " + relay
	}
	cx := 2
	for i, r := range left {
		t.screen.SetContent(cx+i, y, r, nil, style)
	}

	// middle: latency (hidden when disconnected)
	mid := ""
	if connected {
		mid = "Latency " + t.latencyStr()
		// annotate the data path so the number is not mistaken for the
		// relay link when direct transport carries the traffic
		if t.pathStr() == "direct" {
			mid += " (direct)"
		}
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
	f2Label := "F2 Logs"
	f3Label := "F3 Quit"
	copyLabel := ""
	if t.showLogs {
		right = "F2 Back  Ctrl+C Copy  F3 Quit"
		f2Label = "F2 Back"
		copyLabel = "Ctrl+C Copy"
	} else if t.hasTerminalSelection() {
		right = "F2 Logs  Ctrl+C Copy  F3 Quit"
		copyLabel = "Ctrl+C Copy"
	}
	rx := w - len([]rune(right))
	if rx < 0 {
		rx = 0
	}
	// record clickable regions for the F2/F3 labels
	f2Start := strings.Index(right, f2Label)
	f3Start := strings.LastIndex(right, f3Label)
	t.clickF2X = [2]int{rx + f2Start, rx + f2Start + len([]rune(f2Label))}
	t.clickF3X = [2]int{rx + f3Start, rx + f3Start + len([]rune(f3Label))}
	copyStart := -1
	if copyLabel != "" {
		copyStart = strings.Index(right, copyLabel)
	}

	hotkeyStyle := tcell.StyleDefault.Background(barBg).Foreground(accent)
	for i, r := range right {
		s := style
		if (i >= f2Start && i < f2Start+len([]rune(f2Label))) ||
			(copyStart >= 0 && i >= copyStart && i < copyStart+len([]rune(copyLabel))) ||
			(i >= f3Start && i < f3Start+len([]rune(f3Label))) {
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
