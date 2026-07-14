package operatorconsole

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minimumWidth  = 40
	minimumHeight = 12
	fieldWidth    = 14

	styleReset = "\x1b[0m"
	styleTitle = "\x1b[1;36m"
	styleGood  = "\x1b[1;32m"
	styleWarn  = "\x1b[1;33m"
	styleBad   = "\x1b[1;31m"
	styleDim   = "\x1b[2m"
)

// Journal appends its newest lines to dst. Implementations may retain a lock
// while appending because the caller has already reserved enough render memory.
type Journal interface {
	AppendTail(dst []byte, rows, width int) ([]byte, int)
}

// RenderCapacity returns enough capacity for a complete render without growth.
// UTFMax accounts for every terminal cell containing a four-byte UTF-8 rune.
func RenderCapacity(width, height int) int {
	width, height = renderDimensions(width, height)
	return height * (width*utf8.UTFMax + 32)
}

// Renderer retains line state between calls so fluent line construction does
// not allocate.
type Renderer struct {
	dst         []byte
	width       int
	contentRows int
	rows        int
	color       bool
	lineState   lineWriter
}

// Append appends a dashboard to caller-owned memory.
func (render *Renderer) Append(dst []byte, snapshot *Snapshot, journal Journal, width, height int) []byte {
	return render.append(dst, snapshot, journal, width, height, false)
}

// AppendColor appends a dashboard with ANSI colour suitable for an interactive
// terminal. Width accounting excludes the styling sequences.
func (render *Renderer) AppendColor(dst []byte, snapshot *Snapshot, journal Journal, width, height int) []byte {
	return render.append(dst, snapshot, journal, width, height, true)
}

func (render *Renderer) append(dst []byte, snapshot *Snapshot, journal Journal, width, height int, color bool) []byte {
	width, height = renderDimensions(width, height)
	*render = Renderer{
		dst:         dst,
		width:       width,
		contentRows: height - 1,
		color:       color,
	}

	line := render.line()
	line.style(styleTitle)
	line.appendString("KatlOS")
	if snapshot.Mode == ModeInstaller {
		line.appendString(" Installer")
	}
	if snapshot.Version != "" {
		line.appendString("  ")
		line.appendString(snapshot.Version)
	}
	line.resetStyle()
	line.finish()

	line = render.line()
	line.style(styleDim)
	for range min(width, 72) {
		line.appendByte('=')
	}
	line.resetStyle()
	line.finish()

	field := render.wrappedField("State")
	field.style(stateStyle(snapshot.State))
	field.appendString(stateLabel(snapshot.State))
	field.resetStyle()
	field.finish()
	if snapshot.Hostname != "" {
		render.wrappedField("Node").appendString(snapshot.Hostname).finish()
	}
	appendNetwork(render, snapshot.Network)
	if snapshot.State == "running" && snapshot.CurrentStep != "" {
		render.wrappedField("Progress").appendString(snapshot.CurrentStep).finish()
	}
	if snapshot.Generation != "" {
		field = render.wrappedField("Generation")
		field.appendString(snapshot.Generation)
		if health := healthLabel(snapshot.GenerationHealth); health != "" {
			field.appendString("  health=")
			field.style(healthStyle(health))
			field.appendString(health)
			field.resetStyle()
		}
		field.finish()
	}
	if snapshot.Mode == ModeInstaller && snapshot.State == "running" {
		field = render.wrappedField("Disk changes")
		if snapshot.DestructiveMutation {
			field.style(styleWarn)
			field.appendString("started - do not power off")
			field.resetStyle()
		} else {
			field.appendString("not started")
		}
		field.finish()
	}
	if snapshot.Handoff.URL != "" {
		render.wrappedField("Configure").appendString(snapshot.Handoff.URL).finish()
		field = render.wrappedField("Run")
		field.appendString("katlctl config init cluster.yaml --installer ")
		if address := firstIPv4(snapshot.Network); address != "" {
			field.appendString(address)
		} else {
			field.appendString(installerBaseURL(snapshot.Handoff.URL))
		}
		field.finish()
	}
	appendWrappedField(render, "Error", snapshot.LastError, styleBad)
	appendWrappedField(render, "Next action", snapshot.RetryHint, styleWarn)
	appendWrappedField(render, "Status read", snapshot.StatusError, styleWarn)

	render.finishBlank()
	line = render.line()
	line.style(styleTitle)
	line.appendString("Journal")
	line.resetStyle()
	line.finish()
	if remaining := render.contentRows - render.rows; journal != nil && remaining > 0 {
		var written int
		render.dst, written = journal.AppendTail(render.dst, remaining, width)
		render.rows += written
	}
	for render.rows < render.contentRows {
		render.finishBlank()
	}

	footer := newLine(render.dst, width)
	footer.color = color
	footer.style(styleDim)
	footer.appendString("Ctrl+Alt+F2: console")
	if snapshot.SSHEnabled {
		if address := firstIPv4(snapshot.Network); address != "" {
			ssh := " | SSH: root@" + address
			if visibleWidth("Ctrl+Alt+F2: console"+ssh) <= width {
				footer.appendString(ssh)
			} else {
				footer.appendString(" | SSH enabled")
			}
		} else {
			footer.appendString(" | SSH enabled")
		}
	} else if snapshot.Mode == ModeInstaller {
		footer.appendString(" | SSH disabled")
	}
	footer.resetStyle()
	return footer.end()
}

// AppendJournalLine sanitizes and wraps one logical journal line into at most
// maxRows physical terminal rows.
func AppendJournalLine(dst, value []byte, width, maxRows int) ([]byte, int) {
	if width < minimumWidth {
		width = minimumWidth
	}
	if maxRows <= 0 {
		return dst, 0
	}
	line := newLine(dst, width)
	rows := 0
	wrote := false
	for position := 0; position < len(value); {
		r, next, ok := nextVisibleBytes(value, position)
		position = next
		if !ok {
			break
		}
		if r == '\n' || line.columns == width {
			dst = line.end()
			rows++
			if rows == maxRows {
				return dst, rows
			}
			line = newLine(dst, width)
			if r == '\n' {
				continue
			}
		}
		line.appendRune(r)
		wrote = true
	}
	if wrote && rows < maxRows {
		dst = line.end()
		rows++
	}
	return dst, rows
}

// JournalLineRows returns the number of terminal rows needed for one logical
// journal line after sanitization and wrapping.
func JournalLineRows(value []byte, width int) int {
	if width < minimumWidth {
		width = minimumWidth
	}
	columns := 0
	rows := 0
	wrote := false
	for position := 0; position < len(value); {
		r, next, ok := nextVisibleBytes(value, position)
		position = next
		if !ok {
			break
		}
		if r == '\n' || columns == width {
			rows++
			columns = 0
			if r == '\n' {
				continue
			}
		}
		columns++
		wrote = true
	}
	if wrote {
		rows++
	}
	return rows
}

func (r *Renderer) line() *lineWriter {
	if r.rows >= r.contentRows {
		r.lineState = lineWriter{owner: r}
		return &r.lineState
	}
	r.lineState = newLine(r.dst, r.width)
	r.lineState.owner = r
	r.lineState.color = r.color
	return &r.lineState
}

func (r *Renderer) fieldLine(label string) *lineWriter {
	line := r.line()
	if !line.active {
		return line
	}
	line.appendString(label)
	if label != "" {
		line.appendByte(':')
	}
	for line.columns < fieldWidth {
		line.appendByte(' ')
	}
	return line
}

func (r *Renderer) continuationLine() *lineWriter {
	line := r.line()
	if !line.active {
		return line
	}
	for range fieldWidth {
		line.appendByte(' ')
	}
	return line
}

func (r *Renderer) finishLine(line *lineWriter) {
	if r.rows < r.contentRows {
		r.dst = line.end()
	}
	r.rows++
}

func (r *Renderer) finishBlank() {
	r.line().finish()
}

type lineWriter struct {
	owner     *Renderer
	dst       []byte
	lastRune  int
	width     int
	columns   int
	truncated bool
	active    bool
	color     bool
}

func newLine(dst []byte, width int) lineWriter {
	return lineWriter{dst: dst, lastRune: len(dst), width: width, active: true}
}

func (l *lineWriter) style(value string) *lineWriter {
	if l.active && l.color {
		l.dst = append(l.dst, value...)
	}
	return l
}

func (l *lineWriter) resetStyle() *lineWriter {
	return l.style(styleReset)
}

func (l *lineWriter) appendByte(value byte) *lineWriter {
	if !l.active || l.truncated {
		return l
	}
	if l.columns == l.width {
		l.truncated = true
		return l
	}
	l.lastRune = len(l.dst)
	l.dst = append(l.dst, value)
	l.columns++
	return l
}

func (l *lineWriter) appendString(value string) *lineWriter {
	for position := 0; position < len(value) && !l.truncated && l.active; {
		r, next, ok := nextVisibleString(value, position)
		position = next
		if !ok {
			break
		}
		if l.columns == l.width {
			l.truncated = true
			return l
		}
		l.appendRune(r)
	}
	return l
}

func (l *lineWriter) appendRune(r rune) *lineWriter {
	if !l.active || l.truncated || l.columns == l.width {
		return l
	}
	l.lastRune = len(l.dst)
	l.dst = utf8.AppendRune(l.dst, r)
	l.columns++
	return l
}

func (l *lineWriter) finish() {
	l.owner.finishLine(l)
}

func (l *lineWriter) end() []byte {
	if !l.active {
		return l.dst
	}
	if l.truncated {
		l.dst = l.dst[:l.lastRune]
		l.dst = append(l.dst, '~')
		if l.color {
			l.dst = append(l.dst, styleReset...)
		}
	}
	return append(l.dst, '\n')
}

type wrappedWriter struct {
	render       *Renderer
	line         *lineWriter
	activeStyle  string
	pendingSpace int
}

func (r *Renderer) wrappedField(label string) *wrappedWriter {
	return &wrappedWriter{render: r, line: r.fieldLine(label)}
}

func (w *wrappedWriter) appendString(value string) *wrappedWriter {
	for position := 0; position < len(value); {
		r, next, ok := nextVisibleString(value, position)
		if !ok {
			break
		}
		if r == '\n' {
			position = next
			w.pendingSpace = 0
			w.continueLine()
			continue
		}
		if unicode.IsSpace(r) {
			position = next
			w.pendingSpace++
			continue
		}

		wordStart := position
		wordEnd := position
		wordWidth := 0
		for scan := position; scan < len(value); {
			wordRune, wordNext, wordOK := nextVisibleString(value, scan)
			if !wordOK || wordRune == '\n' || unicode.IsSpace(wordRune) {
				break
			}
			wordWidth++
			wordEnd = wordNext
			scan = wordNext
		}
		if wordEnd == wordStart {
			position = next
			continue
		}
		w.appendWord(value[wordStart:wordEnd], wordWidth)
		position = wordEnd
	}
	return w
}

func (w *wrappedWriter) appendWord(word string, wordWidth int) {
	contentStart := fieldWidth
	if w.line.columns > contentStart && w.pendingSpace > 0 && wordWidth <= w.render.width-contentStart && w.line.columns+w.pendingSpace+wordWidth > w.render.width {
		w.continueLine()
	}
	if w.line.columns > contentStart {
		for range w.pendingSpace {
			if w.line.columns == w.render.width {
				w.continueLine()
				break
			}
			w.line.appendRune(' ')
		}
	}
	w.pendingSpace = 0
	for position := 0; position < len(word); {
		r, next, ok := nextVisibleString(word, position)
		position = next
		if !ok {
			break
		}
		if w.line.columns == w.render.width {
			w.continueLine()
		}
		w.line.appendRune(r)
	}
}

func (w *wrappedWriter) continueLine() {
	w.line.finish()
	w.line = w.render.continuationLine()
	if w.activeStyle != "" {
		w.line.style(w.activeStyle)
	}
}

func (w *wrappedWriter) style(value string) *wrappedWriter {
	w.activeStyle = value
	w.line.style(value)
	return w
}

func (w *wrappedWriter) resetStyle() *wrappedWriter {
	w.line.resetStyle()
	w.activeStyle = ""
	return w
}

func (w *wrappedWriter) finish() {
	w.line.finish()
}

func appendNetwork(render *Renderer, network []NetworkInterface) {
	if len(network) == 0 {
		render.wrappedField("Network").appendString("waiting for an active interface").finish()
		return
	}
	for index, iface := range network {
		label := ""
		if index == 0 {
			label = "Network"
		}
		field := render.wrappedField(label)
		field.appendString(iface.Name)
		if len(iface.Addresses) == 0 {
			field.appendString(": configuring")
		} else {
			field.appendString(": ")
			for addressIndex, address := range iface.Addresses {
				if addressIndex > 0 {
					field.appendString(", ")
				}
				field.appendString(address)
			}
		}
		field.finish()
	}
}

func appendWrappedField(render *Renderer, label, value, style string) {
	if value == "" {
		return
	}
	field := render.wrappedField(label)
	if style != "" {
		field.style(style)
	}
	field.appendString(value)
	if style != "" {
		field.resetStyle()
	}
	field.finish()
}

func nextVisibleString(value string, position int) (rune, int, bool) {
	for position < len(value) {
		if value[position] == '\x1b' {
			position = skipANSIString(value, position)
			continue
		}
		r, size := utf8.DecodeRuneInString(value[position:])
		position += size
		if r == '\t' {
			return ' ', position, true
		}
		if r == '\n' || !unicode.IsControl(r) {
			return r, position, true
		}
	}
	return 0, position, false
}

func nextVisibleBytes(value []byte, position int) (rune, int, bool) {
	for position < len(value) {
		if value[position] == '\x1b' {
			position = skipANSIBytes(value, position)
			continue
		}
		r, size := utf8.DecodeRune(value[position:])
		position += size
		if r == '\t' {
			return ' ', position, true
		}
		if r == '\n' || !unicode.IsControl(r) {
			return r, position, true
		}
	}
	return 0, position, false
}

func skipANSIString(value string, position int) int {
	return skipANSI(value[position:], position)
}

func skipANSIBytes(value []byte, position int) int {
	if len(value)-position < 2 {
		return len(value)
	}
	if value[position+1] != '[' {
		return position + 2
	}
	for index := position + 2; index < len(value); index++ {
		if value[index] >= 0x40 && value[index] <= 0x7e {
			return index + 1
		}
	}
	return len(value)
}

func skipANSI(value string, offset int) int {
	if len(value) < 2 {
		return offset + len(value)
	}
	if value[1] != '[' {
		return offset + 2
	}
	for index := 2; index < len(value); index++ {
		if value[index] >= 0x40 && value[index] <= 0x7e {
			return offset + index + 1
		}
	}
	return offset + len(value)
}

func stateLabel(state string) string {
	switch state {
	case "starting-installer":
		return "Starting installer"
	case "starting-runtime":
		return "Starting KatlOS"
	case "running":
		return "Installing"
	case "debug-hold":
		return "Debug hold; installation disabled"
	case "waiting-for-config":
		return "Waiting for configuration"
	case "install-refused":
		return "Installation refused"
	case "failed-before-mutation":
		return "Installation failed; disk unchanged"
	case "failed-after-mutation":
		return "Installation failed; repair required"
	case "reboot-requested":
		return "Installation complete; rebooting"
	case "kubeadm-ready":
		return "Ready for Kubernetes bootstrap"
	case "waiting-for-cluster-bootstrap":
		return "Waiting for Kubernetes bootstrap"
	case "runtime-booted-not-ready":
		return "KatlOS booted; not ready"
	case "runtime-failed-needs-repair":
		return "KatlOS needs repair"
	default:
		return fallback(state, "Unknown")
	}
}

func stateStyle(state string) string {
	switch state {
	case "failed-before-mutation", "failed-after-mutation", "install-refused", "runtime-failed-needs-repair":
		return styleBad
	case "kubeadm-ready", "waiting-for-cluster-bootstrap":
		return styleGood
	default:
		return styleWarn
	}
}

func healthLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case "healthy", "good", "ok", "success":
		return "OK"
	case "unhealthy", "failed", "failure":
		return "FAILED"
	default:
		return strings.ToUpper(strings.TrimSpace(value))
	}
}

func healthStyle(value string) string {
	if value == "OK" {
		return styleGood
	}
	return styleBad
}

func visibleWidth(value string) int {
	width := 0
	for position := 0; position < len(value); {
		r, next, ok := nextVisibleString(value, position)
		position = next
		if !ok {
			break
		}
		if r != '\n' {
			width++
		}
	}
	return width
}

func firstIPv4(network []NetworkInterface) string {
	for _, iface := range network {
		for _, address := range iface.Addresses {
			if strings.IndexByte(address, '.') < 0 {
				continue
			}
			if slash := strings.IndexByte(address, '/'); slash >= 0 {
				return address[:slash]
			}
			return address
		}
	}
	return ""
}

func installerBaseURL(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, "/v1/"); index >= 0 {
		return value[:index]
	}
	return strings.TrimRight(value, "/")
}

func fallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func renderDimensions(width, height int) (int, int) {
	return max(width, minimumWidth), max(height, minimumHeight)
}
