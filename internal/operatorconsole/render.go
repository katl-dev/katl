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
	return height * (width*utf8.UTFMax + 1)
}

// Renderer retains line state between calls so fluent line construction does
// not allocate.
type Renderer struct {
	dst         []byte
	width       int
	contentRows int
	rows        int
	lineState   lineWriter
}

// Append appends a dashboard to caller-owned memory.
func (render *Renderer) Append(dst []byte, snapshot *Snapshot, journal Journal, width, height int) []byte {
	width, height = renderDimensions(width, height)
	*render = Renderer{
		dst:         dst,
		width:       width,
		contentRows: height - 1,
	}

	line := render.line().appendString("KatlOS")
	if snapshot.Mode == ModeInstaller {
		line.appendString(" Installer")
	}
	if snapshot.Version != "" {
		line.appendString("  ")
		line.appendString(snapshot.Version)
	}
	line.finish()

	line = render.line()
	for range min(width, 72) {
		line.appendByte('=')
	}
	line.finish()

	render.fieldLine("State").appendString(stateLabel(snapshot.State)).finish()
	if snapshot.Hostname != "" {
		render.fieldLine("Node").appendString(snapshot.Hostname).finish()
	}
	appendNetwork(render, snapshot.Network)
	if snapshot.CurrentStep != "" {
		render.fieldLine("Current step").appendString(snapshot.CurrentStep).finish()
	}
	if snapshot.TargetDisk != "" {
		render.fieldLine("Target disk").appendString(snapshot.TargetDisk).finish()
	}
	if snapshot.Generation != "" {
		line = render.fieldLine("Generation")
		line.appendString(snapshot.Generation)
		if snapshot.GenerationBoot != "" || snapshot.GenerationHealth != "" {
			line.appendString("  boot=")
			line.appendString(fallback(snapshot.GenerationBoot, "unknown"))
			line.appendString(" health=")
			line.appendString(fallback(snapshot.GenerationHealth, "unknown"))
		}
		line.finish()
	}
	if snapshot.Mode == ModeInstaller && snapshot.State == "running" {
		line = render.fieldLine("Disk changes")
		if snapshot.DestructiveMutation {
			line.appendString("started - do not power off")
		} else {
			line.appendString("not started")
		}
		line.finish()
	}
	if snapshot.Handoff.URL != "" {
		render.fieldLine("Configure").appendString(snapshot.Handoff.URL).finish()
		line = render.fieldLine("Run")
		line.appendString("katlctl config init cluster.yaml --installer ")
		if address := firstIPv4(snapshot.Network); address != "" {
			line.appendString(address)
		} else {
			line.appendString(installerBaseURL(snapshot.Handoff.URL))
		}
		line.finish()
	}
	appendWrappedField(render, "Error", snapshot.LastError)
	appendWrappedField(render, "Next action", snapshot.RetryHint)
	appendWrappedField(render, "Status read", snapshot.StatusError)

	journalRows := height - statusRows(snapshot, width) - 3
	if journalRows < 2 {
		journalRows = 2
	}
	render.finishBlank()
	render.line().appendString("Journal (live)").finish()
	if remaining := render.contentRows - render.rows; journal != nil && remaining > 0 {
		journalRows = min(journalRows, remaining)
		var written int
		render.dst, written = journal.AppendTail(render.dst, journalRows, width)
		render.rows += written
	}
	for render.rows < render.contentRows {
		render.finishBlank()
	}

	footer := newLine(render.dst, width)
	footer.appendString("Ctrl+Alt+F2: local console")
	if snapshot.SSHEnabled {
		if address := firstIPv4(snapshot.Network); address != "" {
			footer.appendString(" | SSH: ssh root@")
			footer.appendString(address)
		} else {
			footer.appendString(" | SSH enabled")
		}
	} else if snapshot.Mode == ModeInstaller {
		footer.appendString(" | SSH disabled by installer config")
	}
	return footer.end()
}

// AppendJournalLine sanitizes, truncates, and appends one journal row.
func AppendJournalLine(dst, value []byte, width int) []byte {
	if width < minimumWidth {
		width = minimumWidth
	}
	line := newLine(dst, width)
	line.appendBytes(value)
	return line.end()
}

func (r *Renderer) line() *lineWriter {
	if r.rows >= r.contentRows {
		r.lineState = lineWriter{owner: r}
		return &r.lineState
	}
	r.lineState = newLine(r.dst, r.width)
	r.lineState.owner = r
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
}

func newLine(dst []byte, width int) lineWriter {
	return lineWriter{dst: dst, lastRune: len(dst), width: width, active: true}
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
	for len(value) > 0 && !l.truncated && l.active {
		r, size := utf8.DecodeRuneInString(value)
		encoded := value[:size]
		value = value[size:]
		if r == '\t' {
			l.appendByte(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		if l.columns == l.width {
			l.truncated = true
			return l
		}
		l.lastRune = len(l.dst)
		if r == utf8.RuneError && size == 1 {
			l.dst = utf8.AppendRune(l.dst, r)
		} else {
			l.dst = append(l.dst, encoded...)
		}
		l.columns++
	}
	return l
}

func (l *lineWriter) appendBytes(value []byte) *lineWriter {
	for len(value) > 0 && !l.truncated && l.active {
		r, size := utf8.DecodeRune(value)
		encoded := value[:size]
		value = value[size:]
		if r == '\t' {
			l.appendByte(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		if l.columns == l.width {
			l.truncated = true
			return l
		}
		l.lastRune = len(l.dst)
		if r == utf8.RuneError && size == 1 {
			l.dst = utf8.AppendRune(l.dst, r)
		} else {
			l.dst = append(l.dst, encoded...)
		}
		l.columns++
	}
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
	}
	return append(l.dst, '\n')
}

func appendNetwork(render *Renderer, network []NetworkInterface) {
	if len(network) == 0 {
		render.fieldLine("Network").appendString("waiting for an active interface").finish()
		return
	}
	for index, iface := range network {
		label := ""
		if index == 0 {
			label = "Network"
		}
		line := render.fieldLine(label)
		line.appendString(iface.Name)
		if len(iface.Addresses) == 0 {
			line.appendString(": configuring")
		} else {
			line.appendString(": ")
			for addressIndex, address := range iface.Addresses {
				if addressIndex > 0 {
					line.appendString(", ")
				}
				line.appendString(address)
			}
		}
		line.finish()
	}
}

func appendWrappedField(render *Renderer, label, value string) {
	if value == "" {
		return
	}
	available := max(render.width-fieldWidth, 10)
	line := render.fieldLine(label)
	position := 0
	currentWidth := 0
	wroteWord := false
	for {
		word, next, width, ok := nextWord(value, position)
		if !ok {
			break
		}
		position = next
		if wroteWord && currentWidth+1+width > available {
			line.finish()
			line = render.continuationLine()
			currentWidth = 0
			wroteWord = false
		}
		if wroteWord {
			line.appendByte(' ')
			currentWidth++
		}
		line.appendString(word)
		currentWidth += width
		wroteWord = true
	}
	line.finish()
}

func statusRows(snapshot *Snapshot, width int) int {
	rows := 3 // title, separator, state
	if snapshot.Hostname != "" {
		rows++
	}
	rows += max(len(snapshot.Network), 1)
	if snapshot.CurrentStep != "" {
		rows++
	}
	if snapshot.TargetDisk != "" {
		rows++
	}
	if snapshot.Generation != "" {
		rows++
	}
	if snapshot.Mode == ModeInstaller && snapshot.State == "running" {
		rows++
	}
	if snapshot.Handoff.URL != "" {
		rows += 2
	}
	if snapshot.LastError != "" {
		rows += wrappedRows(snapshot.LastError, width)
	}
	if snapshot.RetryHint != "" {
		rows += wrappedRows(snapshot.RetryHint, width)
	}
	if snapshot.StatusError != "" {
		rows += wrappedRows(snapshot.StatusError, width)
	}
	return rows
}

func wrappedRows(value string, width int) int {
	available := max(width-fieldWidth, 10)
	position := 0
	rows := 0
	currentWidth := 0
	for {
		_, next, wordWidth, ok := nextWord(value, position)
		if !ok {
			break
		}
		position = next
		if rows == 0 {
			rows = 1
			currentWidth = wordWidth
			continue
		}
		if currentWidth+1+wordWidth > available {
			rows++
			currentWidth = wordWidth
		} else {
			currentWidth += 1 + wordWidth
		}
	}
	return max(rows, 1)
}

func nextWord(value string, position int) (string, int, int, bool) {
	for position < len(value) {
		r, size := utf8.DecodeRuneInString(value[position:])
		if wordSeparator(r) {
			position += size
			continue
		}
		if !unicode.IsControl(r) {
			break
		}
		position += size
	}
	if position == len(value) {
		return "", position, 0, false
	}
	start := position
	width := 0
	for position < len(value) {
		r, size := utf8.DecodeRuneInString(value[position:])
		if wordSeparator(r) {
			break
		}
		position += size
		if !unicode.IsControl(r) {
			width++
		}
	}
	return value[start:position], position, width, true
}

func wordSeparator(r rune) bool {
	return r == '\t' || (!unicode.IsControl(r) && unicode.IsSpace(r))
}

func stateLabel(state string) string {
	switch state {
	case "starting-installer":
		return "Starting installer"
	case "starting-runtime":
		return "Starting installed system"
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
		return "Installed system booted; not ready"
	case "runtime-failed-needs-repair":
		return "Installed system needs repair"
	default:
		return fallback(state, "Unknown")
	}
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
