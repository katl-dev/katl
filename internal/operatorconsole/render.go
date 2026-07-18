package operatorconsole

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	minimumWidth    = 40
	minimumHeight   = 12
	maximumWidth    = 512
	maximumHeight   = 256
	wideLayoutWidth = 72
	fieldWidth      = 14

	styleReset Style = "\x1b[0m"
	styleTitle Style = "\x1b[1;36m"
	styleGood  Style = "\x1b[1;32m"
	styleWarn  Style = "\x1b[1;33m"
	styleBad   Style = "\x1b[1;31m"
	styleDim   Style = "\x1b[2m"
)

const clearScreen = "\x1b[H\x1b[2J"

// Journal writes its newest lines into the bounded dashboard journal region.
// Implementations may retain a lock while writing.
type Journal interface {
	WriteTail(*JournalWriter)
}

// RenderCapacity returns a useful initial serialization buffer size. Layout is
// bounded by a cell Frame, so unusually long grapheme encodings may grow this
// byte buffer without escaping the fixed terminal geometry.
func RenderCapacity(width, height int) int {
	width, height = renderDimensions(width, height)
	return height * (width*utf8.UTFMax + 32)
}

// RenderTarget supplies reusable serialization storage and physical geometry.
type RenderTarget struct {
	storage []byte
	width   int
	rows    int
}

// NewRenderTarget binds storage to a terminal-sized region.
func NewRenderTarget(storage []byte, width, rows int) RenderTarget {
	return RenderTarget{storage: storage, width: width, rows: rows}
}

// Renderer paints a bounded terminal-cell frame and serializes it only after
// all content and defensive dividers have been placed.
type Renderer struct {
	frame  Frame
	output []byte
	color  bool
}

// NewRenderer binds a reusable frame and serialization buffer to a target.
func NewRenderer(target RenderTarget, color bool) Renderer {
	width, height := renderDimensions(target.width, target.rows)
	return Renderer{
		frame:  newFrame(width, height),
		output: target.storage[:0],
		color:  color,
	}
}

// MatchesDimensions reports whether the renderer owns a frame for the given
// physical terminal geometry.
func (render *Renderer) MatchesDimensions(width, height int) bool {
	width, height = renderDimensions(width, height)
	return render.frame.Width == width && render.frame.Height == height
}

// Render paints a complete dashboard. Colour output ends with a carriage
// return on the bottom row; plain snapshots retain their final newline.
func (render *Renderer) Render(snapshot *Snapshot, journal Journal) []byte {
	render.frame.reset()
	if render.frame.Width < minimumWidth || render.frame.Height < minimumHeight {
		render.paintCompact(snapshot)
	} else {
		render.paintDashboard(snapshot, journal)
	}
	render.paintFooter(snapshot)
	render.output = serializeFrame(render.output[:0], &render.frame, render.frame.Height, render.color, true)
	return render.output
}

func (render *Renderer) paintCompact(snapshot *Snapshot) {
	content := NewViewport(&render.frame, Rect{Width: render.frame.Width, Height: max(render.frame.Height-1, 0)})
	content.Write("KatlOS", WrapOptions{Style: styleTitle, WordWrap: true})
	content.Write(stateLabel(snapshot.State), WrapOptions{Style: stateStyle(snapshot.State), WordWrap: true})
	if address := snapshot.ManagementAddress; address != "" {
		content.Write(address, WrapOptions{WordWrap: true})
	}
}

func (render *Renderer) paintDashboard(snapshot *Snapshot, journal Journal) {
	title := "KatlOS"
	if snapshot.Mode == ModeInstaller {
		title += " Installer"
	}
	titleViewport := NewViewport(&render.frame, Rect{Width: render.frame.Width, Height: 1})
	titleViewport.Write(title, WrapOptions{Style: styleTitle})
	divider := NewViewport(&render.frame, Rect{Y: 1, Width: min(render.frame.Width, 72), Height: 1})
	divider.Write(strings.Repeat("=", divider.bounds.Width), WrapOptions{Style: styleDim})

	content := NewViewport(&render.frame, Rect{Y: 2, Width: render.frame.Width, Height: render.frame.Height - 3})
	if snapshot.Mode == ModeRuntime {
		writeHeading(&content, "Status")
		render.writeRuntimeStatus(&content, snapshot)
		writeNetwork(&content, snapshot.DisplayInterfaces, snapshot.AdditionalInterfaces)
	} else {
		writeInstallerStatus(&content, snapshot)
	}
	if snapshot.Mode == ModeInstaller && snapshot.State == "running" {
		value, style := "not started", Style("")
		if snapshot.DestructiveMutation {
			value, style = "started - do not power off", styleWarn
		}
		writeField(&content, "Disk changes", value, style)
	}
	if snapshot.Handoff.URL != "" {
		writeField(&content, "Configure", snapshot.Handoff.URL, "")
		writeField(&content, "Run", "katlctl config init cluster.yaml --installer "+installerCommandEndpoint(snapshot), "")
	}
	writeOptionalField(&content, "Error", snapshot.LastError, styleBad)
	writeOptionalField(&content, "Next action", snapshot.RetryHint, styleWarn)
	writeOptionalField(&content, "Status read", snapshot.StatusError, styleWarn)

	if content.rowsRemaining() > 0 {
		content.advance(1)
	}
	if content.rowsRemaining() > 0 {
		writeHeading(&content, "Journal")
	}
	if journal != nil && content.rowsRemaining() > 0 {
		journalViewport := content.sub(Rect{Y: content.y, Width: content.bounds.Width, Height: content.rowsRemaining()})
		writer := newJournalWriter(journalViewport)
		journal.WriteTail(&writer)
		content.advance(writer.RowsWritten())
	}
}

func (render *Renderer) paintFooter(snapshot *Snapshot) {
	footerText := "Ctrl+Alt+F2: console"
	if render.frame.Width < minimumWidth || render.frame.Height < minimumHeight {
		footerText = "F2: console"
	} else if snapshot.SSHEnabled {
		if address := snapshot.ManagementAddress; address != "" && displayWidth(footerText+" | SSH: katl@"+address) <= render.frame.Width {
			footerText += " | SSH: katl@" + address
		} else {
			footerText += " | SSH enabled"
		}
	} else if snapshot.Mode == ModeInstaller {
		footerText += " | SSH disabled"
	}
	footer := NewViewport(&render.frame, Rect{Y: render.frame.Height - 1, Width: render.frame.Width, Height: 1})
	footer.Write(footerText, WrapOptions{Style: styleDim})
}

type paneField struct {
	label string
	value string
	style Style
}

func (render *Renderer) writeRuntimeStatus(content *Viewport, snapshot *Snapshot) {
	hostState, hostStyle := runtimeHostState(snapshot)
	kubernetesState, kubernetesStyle := runtimeKubernetesState(snapshot)
	host := []paneField{
		{label: "State", value: hostState, style: hostStyle},
		{label: "Node", value: fallback(snapshot.Hostname, "Unknown")},
		{label: "KatlOS", value: fallback(snapshot.Version, "Unknown")},
		{label: "Generation", value: fallback(snapshot.Generation, "Unknown")},
		{label: "Next boot", value: fallback(snapshot.NextGeneration, "-")},
	}
	kubernetes := []paneField{
		{label: "State", value: kubernetesState, style: kubernetesStyle},
		{label: "Version", value: fallback(snapshot.KubernetesVersion, "Not installed")},
	}
	if content.bounds.Width < wideLayoutWidth {
		writePane(content, "Host", host)
		writePane(content, "Kubernetes", kubernetes)
		return
	}

	start := content.y
	dividerX := (content.bounds.Width - 1) / 2
	left := content.sub(Rect{Y: start, Width: dividerX, Height: content.rowsRemaining()})
	right := content.sub(Rect{X: dividerX + 1, Y: start, Width: content.bounds.Width - dividerX - 1, Height: content.rowsRemaining()})
	writePane(&left, "Host", host)
	writePane(&right, "Kubernetes", kubernetes)
	used := max(left.rowsUsed(), right.rowsUsed())
	content.advance(used)
	// Decorations are painted after pane content. Even malformed input cannot
	// move them because each pane was clipped to its own viewport.
	for offset := range used {
		render.frame.setGlyph(content.bounds.X+dividerX, content.bounds.Y+start+offset, "│", 1, styleDim)
	}
}

func writePane(viewport *Viewport, title string, fields []paneField) {
	writeHeading(viewport, title)
	for _, field := range fields {
		writeField(viewport, field.label, field.value, field.style)
	}
}

func writeInstallerStatus(content *Viewport, snapshot *Snapshot) {
	writeField(content, "State", stateLabel(snapshot.State), stateStyle(snapshot.State))
	if snapshot.Hostname != "" {
		writeField(content, "Node", snapshot.Hostname, "")
	}
	writeNetwork(content, snapshot.DisplayInterfaces, snapshot.AdditionalInterfaces)
	if snapshot.Version != "" {
		writeField(content, "Media", snapshot.Version, "")
	}
	if snapshot.State == "running" && snapshot.CurrentStep != "" {
		writeField(content, "Progress", snapshot.CurrentStep, "")
	}
	if snapshot.Generation != "" {
		value := snapshot.Generation
		if health := healthLabel(snapshot.GenerationHealth); health != "" {
			value += "  health=" + health
		}
		writeField(content, "Generation", value, healthStyle(healthLabel(snapshot.GenerationHealth)))
	}
}

func writeNetwork(content *Viewport, network []NetworkInterface, additional int) {
	if len(network) == 0 {
		writeField(content, "Network", "waiting for an active interface", "")
		return
	}
	for index, iface := range network {
		label := ""
		if index == 0 {
			label = "Network"
		}
		value := iface.Name + ": configuring"
		if len(iface.Addresses) > 0 {
			value = iface.Name + ": " + strings.Join(iface.Addresses, ", ")
		}
		if iface.AdditionalAddresses > 0 {
			value += "  + " + pluralCount(iface.AdditionalAddresses, "address", "addresses")
		}
		writeField(content, label, value, "")
	}
	if additional > 0 {
		writeField(content, "", "+ "+pluralCount(additional, "interface", "interfaces"), styleDim)
	}
}

func pluralCount(count int, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return strconv.Itoa(count) + " " + label
}

func writeHeading(viewport *Viewport, value string) {
	if viewport.rowsRemaining() == 0 {
		return
	}
	heading := viewport.sub(Rect{Y: viewport.y, Width: viewport.bounds.Width, Height: 1})
	heading.Write(value, WrapOptions{Style: styleTitle})
	viewport.advance(1)
}

func writeOptionalField(viewport *Viewport, label, value string, style Style) {
	if strings.TrimSpace(value) != "" {
		writeField(viewport, label, value, style)
	}
}

func writeField(viewport *Viewport, label, value string, style Style) {
	if viewport.rowsRemaining() == 0 {
		return
	}
	if viewport.bounds.Width < 28 && label != "" {
		labelView := viewport.sub(Rect{Y: viewport.y, Width: viewport.bounds.Width, Height: 1})
		labelView.Write(label, WrapOptions{Style: styleDim})
		viewport.advance(1)
		if viewport.rowsRemaining() == 0 {
			return
		}
		valueView := viewport.sub(Rect{X: 2, Y: viewport.y, Width: max(viewport.bounds.Width-2, 0), Height: viewport.rowsRemaining()})
		result := valueView.Write(value, WrapOptions{Style: style, WordWrap: true})
		viewport.advance(max(result.Rows, 1))
		return
	}

	labelWidth := 0
	if label != "" {
		labelWidth = min(fieldWidth, max(viewport.bounds.Width-1, 0))
		labelView := viewport.sub(Rect{Y: viewport.y, Width: labelWidth, Height: 1})
		labelView.Write(label+":", WrapOptions{})
	}
	valueView := viewport.sub(Rect{X: labelWidth, Y: viewport.y, Width: viewport.bounds.Width - labelWidth, Height: viewport.rowsRemaining()})
	result := valueView.Write(value, WrapOptions{Style: style, WordWrap: true})
	viewport.advance(max(result.Rows, 1))
}

func runtimeHostState(snapshot *Snapshot) (string, Style) {
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

func runtimeKubernetesState(snapshot *Snapshot) (string, Style) {
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

// JournalWriter renders logical journal entries through the same bounded
// viewport and grapheme wrapper used by every dashboard field and pane.
type JournalWriter struct {
	frame      *Frame
	viewport   Viewport
	output     []byte
	standalone bool
}

// NewJournalWriter binds a journal writer to a standalone render target.
func NewJournalWriter(target RenderTarget) JournalWriter {
	width, rows := renderDimensions(target.width, target.rows)
	frame := newFrame(width, rows)
	return JournalWriter{
		frame:      &frame,
		viewport:   NewViewport(&frame, Rect{Width: width, Height: rows}),
		output:     target.storage[:0],
		standalone: true,
	}
}

func newJournalWriter(viewport Viewport) JournalWriter {
	return JournalWriter{frame: viewport.frame, viewport: viewport}
}

// Bytes returns standalone journal output.
func (writer *JournalWriter) Bytes() []byte {
	if !writer.standalone {
		return nil
	}
	writer.output = serializeFrame(writer.output[:0], writer.frame, writer.RowsWritten(), false, false)
	return writer.output
}

// RowsWritten reports the number of physical rows consumed.
func (writer *JournalWriter) RowsWritten() int {
	return writer.viewport.rowsUsed()
}

// RowsRemaining reports how many physical rows remain.
func (writer *JournalWriter) RowsRemaining() int {
	return writer.viewport.rowsRemaining()
}

// LineRows measures a journal entry through the shared hard-wrap engine.
func (writer *JournalWriter) LineRows(value []byte) int {
	frame := Frame{Width: writer.viewport.bounds.Width, Height: maximumHeight}
	measure := NewViewport(&frame, Rect{Width: frame.Width, Height: frame.Height})
	return measure.Write(string(value), WrapOptions{}).Rows
}

// WriteLine sanitizes and hard-wraps one logical journal line.
func (writer *JournalWriter) WriteLine(value []byte) bool {
	if writer.RowsRemaining() == 0 {
		return false
	}
	return writer.viewport.Write(string(value), WrapOptions{}).Rows > 0
}

func serializeFrame(output []byte, frame *Frame, rows int, color, clear bool) []byte {
	rows = min(max(rows, 0), frame.Height)
	if color && clear {
		output = append(output, clearScreen...)
	}
	for row := range rows {
		last := -1
		for column := frame.Width - 1; column >= 0; column-- {
			cell := frame.Cells[row*frame.Width+column]
			if cell.Glyph != "" || cell.continuation {
				last = column
				break
			}
		}
		activeStyle := Style("")
		for column := 0; column <= last; column++ {
			cell := frame.Cells[row*frame.Width+column]
			if cell.continuation {
				continue
			}
			if cell.Glyph == "" {
				if activeStyle != "" && color {
					output = append(output, string(styleReset)...)
					activeStyle = ""
				}
				output = append(output, ' ')
				continue
			}
			if color && cell.Style != activeStyle {
				if activeStyle != "" {
					output = append(output, string(styleReset)...)
				}
				if cell.Style != "" {
					output = append(output, string(cell.Style)...)
				}
				activeStyle = cell.Style
			}
			output = append(output, cell.Glyph...)
		}
		if activeStyle != "" && color {
			output = append(output, string(styleReset)...)
		}
		if color {
			output = append(output, '\r')
			if row < rows-1 {
				output = append(output, '\n')
			}
		} else {
			output = append(output, '\n')
		}
	}
	return output
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

func stateStyle(state string) Style {
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

func healthStyle(value string) Style {
	if value == "" {
		return ""
	}
	if value == "OK" {
		return styleGood
	}
	return styleBad
}

func visibleWidth(value string) int {
	return displayWidth(value)
}

func installerCommandEndpoint(snapshot *Snapshot) string {
	if address := snapshot.ManagementAddress; address != "" {
		return address
	}
	return installerBaseURL(snapshot.Handoff.URL)
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
	return min(max(width, 1), maximumWidth), min(max(height, 1), maximumHeight)
}
