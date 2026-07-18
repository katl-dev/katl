package operatorconsole

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minimumWidth    = 40
	minimumHeight   = 12
	fieldWidth      = 14
	panelFieldWidth = 12

	styleReset = "\x1b[0m"
	styleTitle = "\x1b[1;36m"
	styleGood  = "\x1b[1;32m"
	styleWarn  = "\x1b[1;33m"
	styleBad   = "\x1b[1;31m"
	styleDim   = "\x1b[2m"
)

const clearScreen = "\x1b[H\x1b[2J"

// Journal writes its newest lines into the bounded dashboard journal region.
// Implementations may retain a lock while writing.
type Journal interface {
	WriteTail(*JournalWriter)
}

// RenderCapacity returns enough capacity for a complete render without growth.
// UTFMax accounts for every terminal cell containing a four-byte UTF-8 rune.
func RenderCapacity(width, height int) int {
	width, height = renderDimensions(width, height)
	return height * (width*utf8.UTFMax + 32)
}

// RenderTarget owns fixed storage and its terminal geometry. Writers start at
// the top-left and may reset the cursor, but they never grow the storage.
type RenderTarget struct {
	storage  []byte
	width    int
	rows     int
	position int
}

// NewRenderTarget binds fixed storage to a terminal-sized region.
func NewRenderTarget(storage []byte, width, rows int) RenderTarget {
	return RenderTarget{storage: storage, width: width, rows: rows}
}

func (target *RenderTarget) reset() {
	target.position = 0
}

func (target *RenderTarget) bytes() []byte {
	return target.storage[:target.position]
}

func (target *RenderTarget) writeByte(value byte) {
	if target.position >= len(target.storage) {
		panic("operatorconsole: render capacity exhausted")
	}
	target.storage[target.position] = value
	target.position++
}

func (target *RenderTarget) writeString(value string) {
	if len(value) > len(target.storage)-target.position {
		panic("operatorconsole: render capacity exhausted")
	}
	target.position += copy(target.storage[target.position:], value)
}

func (target *RenderTarget) writeRune(value rune) {
	if value < utf8.RuneSelf {
		target.writeByte(byte(value))
		return
	}
	if utf8.RuneLen(value) > len(target.storage)-target.position {
		panic("operatorconsole: render capacity exhausted")
	}
	target.position += utf8.EncodeRune(target.storage[target.position:], value)
}

func (target *RenderTarget) truncate(position int) {
	target.position = position
}

func (target *RenderTarget) tail(rows int) RenderTarget {
	return RenderTarget{
		storage: target.storage[target.position:],
		width:   target.width,
		rows:    rows,
	}
}

// Renderer owns fixed output storage, terminal geometry, cursor state and line
// writers. Render always starts at the top-left of that storage and never grows
// it; callers replace the renderer when the terminal dimensions change.
type Renderer struct {
	output       RenderTarget
	contentRows  int
	row          int
	color        bool
	lineState    lineWriter
	wrappedState wrappedWriter
	journalState JournalWriter
}

// NewRenderer binds a renderer to a fixed render target.
func NewRenderer(target RenderTarget, color bool) Renderer {
	target.width, target.rows = renderDimensions(target.width, target.rows)
	required := RenderCapacity(target.width, target.rows)
	if len(target.storage) < required {
		panic("operatorconsole: renderer storage is smaller than RenderCapacity")
	}
	target.storage = target.storage[:required]
	return Renderer{
		output:      target,
		contentRows: target.rows - 1,
		color:       color,
	}
}

// MatchesDimensions reports whether the renderer owns storage for the given
// terminal geometry.
func (render *Renderer) MatchesDimensions(width, height int) bool {
	width, height = renderDimensions(width, height)
	return render.output.width == width && render.output.rows == height && len(render.output.storage) >= RenderCapacity(width, height)
}

// Render writes a complete dashboard from the top-left of the owned storage.
// Colour renderers include the terminal cursor-home and clear sequence.
func (render *Renderer) Render(snapshot *Snapshot, journal Journal) []byte {
	render.output.reset()
	render.row = 0
	if render.color {
		render.output.writeString(clearScreen)
	}

	line := render.line()
	line.style(styleTitle)
	line.writeString("KatlOS")
	if snapshot.Mode == ModeInstaller {
		line.writeString(" Installer")
	}
	line.resetStyle()
	line.finish()

	line = render.line()
	line.style(styleDim)
	for range min(render.output.width, 72) {
		line.writeByte('=')
	}
	line.resetStyle()
	line.finish()

	if snapshot.Mode == ModeRuntime {
		render.writePaneHeading("Status")
		render.writeRuntimeStatusPane(snapshot)
		render.writeNetwork(snapshot.Network)
	} else {
		render.writeInstallerStatus(snapshot)
	}
	if snapshot.Mode == ModeInstaller && snapshot.State == "running" {
		field := render.wrappedField("Disk changes")
		if snapshot.DestructiveMutation {
			field.style(styleWarn)
			field.writeString("started - do not power off")
			field.resetStyle()
		} else {
			field.writeString("not started")
		}
		field.finish()
	}
	if snapshot.Handoff.URL != "" {
		render.wrappedField("Configure").writeString(snapshot.Handoff.URL).finish()
		field := render.wrappedField("Run")
		field.writeString("katlctl config init cluster.yaml --installer ")
		if address := firstIPv4(snapshot.Network); address != "" {
			field.writeString(address)
		} else {
			field.writeString(installerBaseURL(snapshot.Handoff.URL))
		}
		field.finish()
	}
	render.writeWrappedField("Error", snapshot.LastError, styleBad)
	render.writeWrappedField("Next action", snapshot.RetryHint, styleWarn)
	render.writeWrappedField("Status read", snapshot.StatusError, styleWarn)

	render.finishBlank()
	line = render.line()
	line.style(styleTitle)
	line.writeString("Journal")
	line.resetStyle()
	line.finish()
	if remaining := render.contentRows - render.row; journal != nil && remaining > 0 {
		render.journalState = NewJournalWriter(render.output.tail(remaining))
		render.journalState.color = render.color
		journal.WriteTail(&render.journalState)
		render.output.position += render.journalState.output.position
		render.row += render.journalState.rows
	}
	for render.row < render.contentRows {
		render.finishBlank()
	}

	footer := newLine(&render.output)
	footer.color = render.color
	footer.style(styleDim)
	footer.writeString("Ctrl+Alt+F2: console")
	if snapshot.SSHEnabled {
		if address := firstIPv4(snapshot.Network); address != "" {
			if visibleWidth("Ctrl+Alt+F2: console | SSH: katl@")+visibleWidth(address) <= render.output.width {
				footer.writeString(" | SSH: katl@")
				footer.writeString(address)
			} else {
				footer.writeString(" | SSH enabled")
			}
		} else {
			footer.writeString(" | SSH enabled")
		}
	} else if snapshot.Mode == ModeInstaller {
		footer.writeString(" | SSH disabled")
	}
	footer.resetStyle()
	footer.endFrame()
	return render.output.bytes()
}

type pane struct {
	title  string
	fields []paneField
}

type paneField struct {
	label string
	value string
	style string
}

type paneFieldState struct {
	field    paneField
	position int
	first    bool
	done     bool
}

func (render *Renderer) writePaneHeading(title string) {
	line := render.line()
	line.style(styleTitle).writeString(title).resetStyle()
	line.finish()
}

func (render *Renderer) writeRuntimeStatusPane(snapshot *Snapshot) {
	hostState, hostStyle := runtimeHostState(snapshot)
	kubernetesState, kubernetesStyle := runtimeKubernetesState(snapshot)
	leftFields := [...]paneField{
		{label: "State", value: hostState, style: hostStyle},
		{label: "Node", value: fallback(snapshot.Hostname, "Unknown")},
		{label: "KatlOS", value: fallback(snapshot.Version, "Unknown")},
		{label: "Generation", value: fallback(snapshot.Generation, "Unknown")},
		{label: "Next boot", value: fallback(snapshot.NextGeneration, "-")},
	}
	rightFields := [...]paneField{
		{label: "State", value: kubernetesState, style: kubernetesStyle},
		{label: "Version", value: fallback(snapshot.KubernetesVersion, "Not installed")},
	}
	render.writeSplitPanes(
		pane{title: "Host", fields: leftFields[:]},
		pane{title: "Kubernetes", fields: rightFields[:]},
	)
}

func (render *Renderer) writeInstallerStatus(snapshot *Snapshot) {
	field := render.wrappedField("State")
	field.style(stateStyle(snapshot.State))
	field.writeString(stateLabel(snapshot.State))
	field.resetStyle()
	field.finish()
	if snapshot.Hostname != "" {
		render.wrappedField("Node").writeString(snapshot.Hostname).finish()
	}
	render.writeNetwork(snapshot.Network)
	if snapshot.Version != "" {
		render.wrappedField("Media").writeString(snapshot.Version).finish()
	}
	if snapshot.State == "running" && snapshot.CurrentStep != "" {
		render.wrappedField("Progress").writeString(snapshot.CurrentStep).finish()
	}
	if snapshot.Generation == "" {
		return
	}
	field = render.wrappedField("Generation")
	field.writeString(snapshot.Generation)
	if health := healthLabel(snapshot.GenerationHealth); health != "" {
		field.writeString("  health=")
		field.style(healthStyle(health))
		field.writeString(health)
		field.resetStyle()
	}
	field.finish()
}

func (render *Renderer) writeSplitPanes(left, right pane) {
	if render.row >= render.contentRows {
		return
	}
	divider := (render.output.width - 1) / 2
	line := render.line()
	line.style(styleTitle)
	line.writeUntil(left.title, divider)
	line.resetStyle()
	line.padTo(divider)
	line.style(styleDim).writeRune('│').resetStyle()
	line.style(styleTitle)
	line.writeUntil(right.title, render.output.width)
	line.resetStyle()
	line.finish()

	count := max(len(left.fields), len(right.fields))
	for index := 0; index < count && render.row < render.contentRows; index++ {
		leftState := paneFieldState{first: true, done: index >= len(left.fields)}
		if !leftState.done {
			leftState.field = left.fields[index]
		}
		rightState := paneFieldState{first: true, done: index >= len(right.fields)}
		if !rightState.done {
			rightState.field = right.fields[index]
		}
		for (!leftState.done || !rightState.done) && render.row < render.contentRows {
			line = render.line()
			render.writePaneFieldSegment(line, &leftState, 0, divider)
			line.padTo(divider)
			line.style(styleDim).writeRune('│').resetStyle()
			render.writePaneFieldSegment(line, &rightState, divider+1, render.output.width)
			line.finish()
		}
	}
}

func (render *Renderer) writePaneFieldSegment(line *lineWriter, state *paneFieldState, start, end int) {
	if state.done || end <= start {
		return
	}
	line.padTo(start)
	labelWidth := min(panelFieldWidth, end-start-1)
	if labelWidth < 1 {
		state.done = true
		return
	}
	if state.first {
		line.writeUntil(state.field.label, start+labelWidth-1)
		if line.columns < start+labelWidth {
			line.writeByte(':')
		}
		state.first = false
	}
	line.padTo(start + labelWidth)
	if line.columns >= end {
		return
	}

	state.position = skipVisibleSpace(state.field.value, state.position)
	if state.position >= len(state.field.value) {
		state.done = true
		return
	}
	segmentEnd, next := paneSegment(state.field.value, state.position, end-line.columns)
	if state.field.style != "" {
		line.style(state.field.style)
	}
	line.writeString(state.field.value[state.position:segmentEnd])
	if state.field.style != "" {
		line.resetStyle()
	}
	state.position = next
	state.done = skipVisibleSpace(state.field.value, state.position) >= len(state.field.value)
}

func paneSegment(value string, position, available int) (int, int) {
	scan := position
	end := position
	lastSpace := -1
	visible := 0
	for scan < len(value) && visible < available {
		r, next, ok := nextVisibleString(value, scan)
		if !ok {
			return len(value), len(value)
		}
		if r == '\n' {
			return scan, next
		}
		if unicode.IsSpace(r) {
			lastSpace = scan
		}
		end = next
		scan = next
		visible++
	}
	if scan >= len(value) {
		return end, end
	}
	if lastSpace > position {
		return lastSpace, skipVisibleSpace(value, lastSpace)
	}
	return end, end
}

func skipVisibleSpace(value string, position int) int {
	for position < len(value) {
		r, next, ok := nextVisibleString(value, position)
		if !ok || (!unicode.IsSpace(r) && r != '\n') {
			return position
		}
		position = next
	}
	return position
}

func (line *lineWriter) padTo(column int) {
	for line.columns < column {
		line.writeByte(' ')
	}
}

func (line *lineWriter) writeUntil(value string, end int) {
	for position := 0; position < len(value) && line.columns < end; {
		r, next, ok := nextVisibleString(value, position)
		position = next
		if !ok || r == '\n' {
			return
		}
		line.writeRune(r)
	}
}

func runtimeHostState(snapshot *Snapshot) (string, string) {
	if snapshot.State == "runtime-failed-needs-repair" {
		return "Needs repair", styleBad
	}
	if health := healthLabel(snapshot.GenerationHealth); health != "" {
		return health, healthStyle(health)
	}
	if snapshot.State == "starting-runtime" {
		return "Starting", styleWarn
	}
	if snapshot.State == "runtime-booted-not-ready" {
		return "Not ready", styleWarn
	}
	return "Running", styleGood
}

func runtimeKubernetesState(snapshot *Snapshot) (string, string) {
	if snapshot.KubernetesVersion == "" {
		return "Not installed", styleWarn
	}
	if snapshot.State == "runtime-failed-needs-repair" {
		return "Unavailable", styleBad
	}
	if snapshot.KubernetesBootstrapped {
		return "Bootstrapped", styleGood
	}
	switch snapshot.State {
	case "kubeadm-ready", "waiting-for-cluster-bootstrap":
		return "Ready for bootstrap", styleGood
	default:
		return "Waiting for KatlOS", styleWarn
	}
}

// JournalWriter owns the bounded journal region within a dashboard render.
// Journal implementations write logical lines without access to output storage.
type JournalWriter struct {
	output RenderTarget
	rows   int
	color  bool
}

// NewJournalWriter binds a bounded journal writer to a fixed render target. It
// is primarily useful for testing journal sources independently.
func NewJournalWriter(target RenderTarget) JournalWriter {
	if target.width < minimumWidth {
		target.width = minimumWidth
	}
	target.rows = max(target.rows, 0)
	target.reset()
	return JournalWriter{output: target}
}

// Bytes returns the journal output written into owned storage.
func (writer *JournalWriter) Bytes() []byte {
	return writer.output.bytes()
}

// RowsWritten reports the number of physical terminal rows written.
func (writer *JournalWriter) RowsWritten() int {
	return writer.rows
}

// RowsRemaining reports how many physical terminal rows remain available.
func (writer *JournalWriter) RowsRemaining() int {
	return writer.output.rows - writer.rows
}

// LineRows reports how many physical terminal rows a logical line needs in
// this journal region.
func (writer *JournalWriter) LineRows(value []byte) int {
	return journalLineRows(value, writer.output.width)
}

// WriteLine sanitizes and wraps one logical journal line. It returns false
// when the journal pane has no room for another line.
func (writer *JournalWriter) WriteLine(value []byte) bool {
	if writer.RowsRemaining() <= 0 {
		return false
	}
	written := writer.writeLine(value)
	writer.rows += written
	return true
}

func (writer *JournalWriter) writeLine(value []byte) int {
	remaining := writer.RowsRemaining()
	line := newLine(&writer.output)
	line.color = writer.color
	rows := 0
	wrote := false
	for position := 0; position < len(value); {
		r, next, ok := nextVisibleBytes(value, position)
		position = next
		if !ok {
			break
		}
		if r == '\n' || line.columns == writer.output.width {
			line.end()
			rows++
			if rows == remaining {
				return rows
			}
			line = newLine(&writer.output)
			line.color = writer.color
			if r == '\n' {
				continue
			}
		}
		line.writeRune(r)
		wrote = true
	}
	if wrote && rows < remaining {
		line.end()
		rows++
	}
	return rows
}

func journalLineRows(value []byte, width int) int {
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
	if r.row >= r.contentRows {
		r.lineState = lineWriter{owner: r}
		return &r.lineState
	}
	r.lineState = newLine(&r.output)
	r.lineState.owner = r
	r.lineState.color = r.color
	return &r.lineState
}

func (r *Renderer) fieldLine(label string) *lineWriter {
	line := r.line()
	if !line.active {
		return line
	}
	line.writeString(label)
	if label != "" {
		line.writeByte(':')
	}
	for line.columns < fieldWidth {
		line.writeByte(' ')
	}
	return line
}

func (r *Renderer) continuationLine() *lineWriter {
	line := r.line()
	if !line.active {
		return line
	}
	for range fieldWidth {
		line.writeByte(' ')
	}
	return line
}

func (r *Renderer) finishLine(line *lineWriter) {
	if r.row < r.contentRows {
		line.end()
	}
	r.row++
}

func (r *Renderer) finishBlank() {
	r.line().finish()
}

type lineWriter struct {
	owner     *Renderer
	output    *RenderTarget
	lastRune  int
	columns   int
	truncated bool
	active    bool
	color     bool
}

func newLine(output *RenderTarget) lineWriter {
	return lineWriter{output: output, lastRune: output.position, active: true}
}

func (l *lineWriter) style(value string) *lineWriter {
	if l.active && l.color {
		l.output.writeString(value)
	}
	return l
}

func (l *lineWriter) resetStyle() *lineWriter {
	return l.style(styleReset)
}

func (l *lineWriter) writeByte(value byte) *lineWriter {
	if !l.active || l.truncated {
		return l
	}
	if l.columns == l.output.width {
		l.truncated = true
		return l
	}
	l.lastRune = l.output.position
	l.output.writeByte(value)
	l.columns++
	return l
}

func (l *lineWriter) writeString(value string) *lineWriter {
	for position := 0; position < len(value) && !l.truncated && l.active; {
		r, next, ok := nextVisibleString(value, position)
		position = next
		if !ok {
			break
		}
		if l.columns == l.output.width {
			l.truncated = true
			return l
		}
		l.writeRune(r)
	}
	return l
}

func (l *lineWriter) writeRune(r rune) *lineWriter {
	if !l.active || l.truncated || l.columns == l.output.width {
		return l
	}
	l.lastRune = l.output.position
	l.output.writeRune(r)
	l.columns++
	return l
}

func (l *lineWriter) finish() {
	l.owner.finishLine(l)
}

func (l *lineWriter) end() {
	l.finishOutput()
	// A character in the terminal's final column leaves autowrap pending. A
	// carriage return clears that state before the line feed, so one rendered
	// row always consumes exactly one terminal row.
	if l.color {
		l.output.writeByte('\r')
	}
	l.output.writeByte('\n')
}

func (l *lineWriter) endFrame() {
	l.finishOutput()
	if l.color {
		// The footer occupies the terminal's bottom row. Leave the cursor on that
		// row and clear pending autowrap without emitting a line feed that would
		// scroll the completed frame.
		l.output.writeByte('\r')
		return
	}
	l.output.writeByte('\n')
}

func (l *lineWriter) finishOutput() {
	if !l.active {
		return
	}
	if l.truncated {
		l.output.truncate(l.lastRune)
		l.output.writeByte('~')
		if l.color {
			l.output.writeString(styleReset)
		}
	}
}

type wrappedWriter struct {
	render       *Renderer
	line         *lineWriter
	activeStyle  string
	pendingSpace int
}

func (r *Renderer) wrappedField(label string) *wrappedWriter {
	r.wrappedState = wrappedWriter{render: r, line: r.fieldLine(label)}
	return &r.wrappedState
}

func (w *wrappedWriter) writeString(value string) *wrappedWriter {
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
		w.writeWord(value[wordStart:wordEnd], wordWidth)
		position = wordEnd
	}
	return w
}

func (w *wrappedWriter) writeWord(word string, wordWidth int) {
	contentStart := fieldWidth
	if w.line.columns > contentStart && w.pendingSpace > 0 && wordWidth <= w.render.output.width-contentStart && w.line.columns+w.pendingSpace+wordWidth > w.render.output.width {
		w.continueLine()
	}
	if w.line.columns > contentStart {
		for range w.pendingSpace {
			if w.line.columns == w.render.output.width {
				w.continueLine()
				break
			}
			w.line.writeRune(' ')
		}
	}
	w.pendingSpace = 0
	for position := 0; position < len(word); {
		r, next, ok := nextVisibleString(word, position)
		position = next
		if !ok {
			break
		}
		if w.line.columns == w.render.output.width {
			w.continueLine()
		}
		w.line.writeRune(r)
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

func (render *Renderer) writeNetwork(network []NetworkInterface) {
	if len(network) == 0 {
		render.wrappedField("Network").writeString("waiting for an active interface").finish()
		return
	}
	for index, iface := range network {
		label := ""
		if index == 0 {
			label = "Network"
		}
		field := render.wrappedField(label)
		field.writeString(iface.Name)
		if len(iface.Addresses) == 0 {
			field.writeString(": configuring")
		} else {
			field.writeString(": ")
			for addressIndex, address := range iface.Addresses {
				if addressIndex > 0 {
					field.writeString(", ")
				}
				field.writeString(address)
			}
		}
		field.finish()
	}
}

func (render *Renderer) writeWrappedField(label, value, style string) {
	if value == "" {
		return
	}
	field := render.wrappedField(label)
	if style != "" {
		field.style(style)
	}
	field.writeString(value)
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
