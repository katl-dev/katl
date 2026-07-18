package operatorconsole

import (
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

const maxGlyphBytes = 64

// Style is a semantic terminal style serialized after layout is complete.
type Style = string

// Cell is one terminal cell in a rendered frame. Wide graphemes occupy this
// cell and one or more private continuation cells.
type Cell struct {
	Glyph string
	Style Style

	continuation bool
	span         int
}

// Frame is a fixed terminal-cell canvas. Content can only be painted through a
// bounded Viewport.
type Frame struct {
	Cells  []Cell
	Width  int
	Height int
}

// Rect describes a bounded region within a Frame.
type Rect struct {
	X, Y          int
	Width, Height int
}

// WrapOptions selects the shared text layout behavior for a viewport.
type WrapOptions struct {
	Style              Style
	WordWrap           bool
	FirstIndent        int
	ContinuationIndent int
}

// WriteResult reports the physical rows consumed and whether content exceeded
// the viewport's bottom edge.
type WriteResult struct {
	Rows      int
	Truncated bool
}

// Viewport is the only text-painting surface. Its cursor and every grapheme are
// clipped to bounds before the Frame is modified.
type Viewport struct {
	frame  *Frame
	bounds Rect
	x, y   int
	used   int
}

func newFrame(width, height int) Frame {
	width, height = renderDimensions(width, height)
	return Frame{Cells: make([]Cell, width*height), Width: width, Height: height}
}

func (frame *Frame) reset() {
	for index := range frame.Cells {
		frame.Cells[index] = Cell{}
	}
}

// NewViewport returns a viewport intersected with the frame. Invalid or empty
// rectangles become inert viewports.
func NewViewport(frame *Frame, bounds Rect) Viewport {
	if frame == nil {
		return Viewport{}
	}
	if bounds.X < 0 {
		bounds.Width += bounds.X
		bounds.X = 0
	}
	if bounds.Y < 0 {
		bounds.Height += bounds.Y
		bounds.Y = 0
	}
	bounds.Width = min(max(bounds.Width, 0), max(frame.Width-bounds.X, 0))
	bounds.Height = min(max(bounds.Height, 0), max(frame.Height-bounds.Y, 0))
	return Viewport{frame: frame, bounds: bounds}
}

func (viewport *Viewport) sub(bounds Rect) Viewport {
	bounds.X += viewport.bounds.X
	bounds.Y += viewport.bounds.Y
	bounds.Width = min(bounds.Width, viewport.bounds.Width-bounds.X+viewport.bounds.X)
	bounds.Height = min(bounds.Height, viewport.bounds.Height-bounds.Y+viewport.bounds.Y)
	return NewViewport(viewport.frame, bounds)
}

func (viewport *Viewport) rowsRemaining() int {
	return max(viewport.bounds.Height-viewport.y, 0)
}

func (viewport *Viewport) rowsUsed() int {
	return viewport.used
}

func (viewport *Viewport) advance(rows int) {
	viewport.y = min(viewport.y+max(rows, 0), viewport.bounds.Height)
	viewport.x = 0
	viewport.used = max(viewport.used, viewport.y)
}

// Write lays out sanitized display graphemes through the shared word/hard-wrap
// engine and paints only cells inside the viewport.
func (viewport *Viewport) Write(text string, options WrapOptions) WriteResult {
	start := viewport.y
	if viewport.frame == nil || viewport.bounds.Width == 0 || viewport.rowsRemaining() == 0 || text == "" {
		return WriteResult{}
	}
	options.FirstIndent = min(max(options.FirstIndent, 0), viewport.bounds.Width-1)
	options.ContinuationIndent = min(max(options.ContinuationIndent, 0), viewport.bounds.Width-1)
	viewport.x = options.FirstIndent
	wrote := false
	pendingSpaces := 0
	inWord := false
	iterator := displayIterator{text: text, state: -1}
	for {
		token, ok := iterator.next()
		if !ok {
			break
		}
		if token.newline {
			pendingSpaces = 0
			inWord = false
			if !viewport.nextLine(options.ContinuationIndent) {
				viewport.markTruncated(options.Style)
				return viewport.writeResult(start, true)
			}
			wrote = true
			continue
		}
		if token.space {
			pendingSpaces++
			inWord = false
			continue
		}

		indent := options.ContinuationIndent
		if viewport.y == start {
			indent = options.FirstIndent
		}
		if options.WordWrap && !inWord {
			wordWidth := token.width + iterator.nextWordWidth()
			spaces := pendingSpaces
			if viewport.x == indent {
				spaces = 0
			}
			lineWidth := viewport.bounds.Width - indent
			if viewport.x+spaces+wordWidth > viewport.bounds.Width && (wordWidth <= lineWidth || viewport.x > indent) {
				if !viewport.nextLine(options.ContinuationIndent) {
					viewport.markTruncated(options.Style)
					return viewport.writeResult(start, true)
				}
				pendingSpaces = 0
			}
		}
		for pendingSpaces > 0 && viewport.x > indent {
			if !viewport.paint(displayToken{glyph: " ", width: 1}, options.Style, options.ContinuationIndent) {
				viewport.markTruncated(options.Style)
				return viewport.writeResult(start, true)
			}
			pendingSpaces--
			wrote = true
		}
		pendingSpaces = 0
		if !viewport.paint(token, options.Style, options.ContinuationIndent) {
			viewport.markTruncated(options.Style)
			return viewport.writeResult(start, true)
		}
		wrote = true
		inWord = true
	}
	if wrote {
		viewport.y++
		viewport.x = 0
		viewport.used = max(viewport.used, viewport.y)
	}
	return viewport.writeResult(start, false)
}

func (viewport *Viewport) writeResult(start int, truncated bool) WriteResult {
	return WriteResult{Rows: max(viewport.used-start, 0), Truncated: truncated}
}

func (viewport *Viewport) paint(token displayToken, style Style, continuationIndent int) bool {
	if token.width <= 0 {
		viewport.appendZeroWidth(token.glyph)
		return true
	}
	if token.width > viewport.bounds.Width-continuationIndent {
		token = displayToken{glyph: "�", width: 1}
	}
	if viewport.x+token.width > viewport.bounds.Width {
		if !viewport.nextLine(continuationIndent) {
			return false
		}
	}
	if viewport.y >= viewport.bounds.Height {
		return false
	}
	viewport.frame.setGlyph(viewport.bounds.X+viewport.x, viewport.bounds.Y+viewport.y, token.glyph, token.width, style)
	viewport.x += token.width
	viewport.used = max(viewport.used, viewport.y+1)
	return true
}

func (viewport *Viewport) appendZeroWidth(glyph string) {
	if viewport.x == 0 || viewport.y >= viewport.bounds.Height || len(viewport.frame.Cells) < viewport.frame.Width*viewport.frame.Height {
		return
	}
	index := (viewport.bounds.Y+viewport.y)*viewport.frame.Width + viewport.bounds.X + viewport.x - 1
	for index >= 0 && viewport.frame.Cells[index].continuation {
		index--
	}
	if index < 0 || len(viewport.frame.Cells[index].Glyph)+len(glyph) > maxGlyphBytes {
		return
	}
	viewport.frame.Cells[index].Glyph += glyph
}

func (viewport *Viewport) nextLine(indent int) bool {
	if viewport.y+1 >= viewport.bounds.Height {
		viewport.y = viewport.bounds.Height - 1
		viewport.x = viewport.bounds.Width
		return false
	}
	viewport.y++
	viewport.x = min(max(indent, 0), viewport.bounds.Width-1)
	viewport.used = max(viewport.used, viewport.y+1)
	return true
}

func (viewport *Viewport) markTruncated(style Style) {
	if viewport.frame == nil || viewport.bounds.Width == 0 || viewport.bounds.Height == 0 {
		return
	}
	viewport.frame.setGlyph(viewport.bounds.X+viewport.bounds.Width-1, viewport.bounds.Y+viewport.bounds.Height-1, "…", 1, style)
}

func (frame *Frame) setGlyph(x, y int, glyph string, width int, style Style) {
	if x < 0 || y < 0 || y >= frame.Height || x+width > frame.Width || width < 1 || len(frame.Cells) < frame.Width*frame.Height {
		return
	}
	frame.clearGlyphAt(x, y)
	for offset := 1; offset < width; offset++ {
		frame.clearGlyphAt(x+offset, y)
	}
	index := y*frame.Width + x
	frame.Cells[index] = Cell{Glyph: glyph, Style: style, span: width}
	for offset := 1; offset < width; offset++ {
		frame.Cells[index+offset] = Cell{Style: style, continuation: true}
	}
}

func (frame *Frame) clearGlyphAt(x, y int) {
	index := y*frame.Width + x
	cell := frame.Cells[index]
	if cell.continuation {
		start := index
		for start > y*frame.Width && frame.Cells[start].continuation {
			start--
		}
		span := max(frame.Cells[start].span, 1)
		for offset := range span {
			frame.Cells[start+offset] = Cell{}
		}
		return
	}
	span := max(cell.span, 1)
	for offset := 0; offset < span && index+offset < (y+1)*frame.Width; offset++ {
		frame.Cells[index+offset] = Cell{}
	}
}

type displayToken struct {
	glyph   string
	width   int
	space   bool
	newline bool
}

type displayIterator struct {
	text  string
	state int
}

func (iterator *displayIterator) next() (displayToken, bool) {
	for len(iterator.text) > 0 {
		if iterator.text[0] == '\x1b' {
			iterator.text = iterator.text[skipTerminalSequence(iterator.text):]
			iterator.state = -1
			continue
		}
		cluster, rest, width, state := uniseg.FirstGraphemeClusterInString(iterator.text, iterator.state)
		iterator.text, iterator.state = rest, state
		value, size := utf8.DecodeRuneInString(cluster)
		if value == utf8.RuneError && size == 1 {
			return displayToken{glyph: "�", width: 1}, true
		}
		if value == '\n' || value == '\r' {
			return displayToken{newline: true}, true
		}
		if value == '\t' || unicode.IsSpace(value) {
			return displayToken{glyph: " ", width: 1, space: true}, true
		}
		if unicode.IsControl(value) {
			continue
		}
		if len(cluster) > maxGlyphBytes {
			return displayToken{glyph: "�", width: 1}, true
		}
		return displayToken{glyph: cluster, width: width}, true
	}
	return displayToken{}, false
}

func (iterator displayIterator) nextWordWidth() int {
	width := 0
	for {
		token, ok := iterator.next()
		if !ok || token.space || token.newline {
			return width
		}
		width += token.width
	}
}

func skipTerminalSequence(value string) int {
	if len(value) < 2 {
		return len(value)
	}
	switch value[1] {
	case '[':
		for index := 2; index < len(value); index++ {
			if value[index] >= 0x40 && value[index] <= 0x7e {
				return index + 1
			}
		}
		return len(value)
	case ']':
		for index := 2; index < len(value); index++ {
			if value[index] == '\a' {
				return index + 1
			}
			if value[index] == '\x1b' && index+1 < len(value) && value[index+1] == '\\' {
				return index + 2
			}
		}
		return len(value)
	default:
		_, size := utf8.DecodeRuneInString(value[1:])
		return min(1+size, len(value))
	}
}

func displayWidth(value string) int {
	width := 0
	iterator := displayIterator{text: value, state: -1}
	for {
		token, ok := iterator.next()
		if !ok {
			return width
		}
		if !token.newline {
			width += token.width
		}
	}
}
