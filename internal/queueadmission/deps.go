package queueadmission

import (
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var dependsOnLine = regexp.MustCompile(`(?im)^\s*depends on\b(.*)$`)
var issueRef = regexp.MustCompile(`#(\d+)`)

// DeclaredDeps lists the numbers on "Depends on #N" lines of a pull request
// body.
func DeclaredDeps(body string) []int {
	var deps []int
	for _, line := range dependsOnLine.FindAllStringSubmatch(body, -1) {
		for _, ref := range issueRef.FindAllStringSubmatch(line[1], -1) {
			n, _ := strconv.Atoi(ref[1])
			deps = append(deps, n)
		}
	}
	return deps
}

var holdLine = regexp.MustCompile(`(?i)^\s*hold:(.*)$`)

// HoldLines returns the text of each "Hold:" line of a pull request body
// outside a code fence, in order. A line that names nothing gives an empty
// string.
func HoldLines(body string) []string {
	var holds []string
	for line := range UnfencedLines(body) {
		if m := holdLine.FindStringSubmatch(line); m != nil {
			holds = append(holds, strings.TrimSpace(m[1]))
		}
	}
	return holds
}

// UnfencedLines yields the lines of a Markdown body outside code fences.
func UnfencedLines(body string) iter.Seq[string] {
	return func(yield func(string) bool) {
		fenced := false
		for line := range strings.SplitSeq(body, "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				fenced = !fenced
				continue
			}
			if !fenced && !yield(line) {
				return
			}
		}
	}
}

// Node is a pull request in a dependency graph.
type Node struct {
	Number int
	Closes []int
	// Deps lists the issues and pull requests it declares itself blocked
	// by.
	Deps []int
}

// DepKind is how a dependency resolves.
type DepKind int

const (
	// DepPulls waits for the pull requests in the graph named in On: the
	// dependency itself, or the pull requests that close it.
	DepPulls DepKind = iota
	// DepQueued waits for the queued pull request named in On, which is
	// not in the graph.
	DepQueued
	// DepUnknown is in neither the graph, the queue nor the refs.
	DepUnknown
	// DepNotFound does not exist.
	DepNotFound
	// DepSatisfied merged, or is a closed issue.
	DepSatisfied
	// DepClosed is a pull request closed without merging.
	DepClosed
	// DepOpen is any other open issue or pull request.
	DepOpen
)

// Dep is one resolved dependency.
type Dep struct {
	Number int
	Kind   DepKind
	On     []int
}

type graph struct {
	nodes   map[int]Node
	closers map[int][]int
	queued  map[int]int
	refs    map[int]Ref
}

func newGraph(nodes []Node, queue []Entry, refs map[int]Ref) graph {
	g := graph{nodes: map[int]Node{}, closers: map[int][]int{}, queued: map[int]int{}, refs: refs}
	for _, n := range nodes {
		g.nodes[n.Number] = n
		for _, issue := range n.Closes {
			g.closers[issue] = append(g.closers[issue], n.Number)
		}
	}
	// queued maps the pull requests in the queue, and the issues they
	// close, to the queued pull request.
	for _, e := range queue {
		g.queued[e.Number] = e.Number
		for _, issue := range e.Closes {
			g.queued[issue] = e.Number
		}
	}
	return g
}

// resolve classifies each dependency of n, in order. A pull request's
// dependency on itself or on an issue it closes is ignored.
func (g graph) resolve(n Node) []Dep {
	var deps []Dep
	for _, dep := range n.Deps {
		if dep == n.Number || slices.Contains(n.Closes, dep) {
			continue
		}
		_, inGraph := g.nodes[dep]
		ref, known := g.refs[dep]
		d := Dep{Number: dep}
		switch {
		case inGraph:
			d.Kind, d.On = DepPulls, []int{dep}
		case len(g.closers[dep]) > 0:
			d.Kind = DepPulls
			for _, c := range g.closers[dep] {
				if c != n.Number {
					d.On = append(d.On, c)
				}
			}
		case g.queued[dep] != 0:
			d.Kind, d.On = DepQueued, []int{g.queued[dep]}
		case !known:
			d.Kind = DepUnknown
		case ref.State == "NOT_FOUND":
			d.Kind = DepNotFound
		case ref.Resolved():
			d.Kind = DepSatisfied
		case ref.State == "CLOSED":
			d.Kind = DepClosed
		default:
			d.Kind = DepOpen
		}
		deps = append(deps, d)
	}
	return deps
}

// ResolveDeps resolves the dependencies of every node. Dependencies on
// other nodes wait for them; the rest resolve through the queue and refs.
func ResolveDeps(nodes []Node, queue []Entry, refs map[int]Ref) map[int][]Dep {
	g := newGraph(nodes, queue, refs)
	resolved := map[int][]Dep{}
	for _, n := range nodes {
		resolved[n.Number] = g.resolve(n)
	}
	return resolved
}

// InCycle reports whether n can reach itself through on, which maps each
// pull request to the pull requests it waits for.
func InCycle(on map[int][]int, n int) bool {
	return reaches(func(m int) []int { return on[m] }, n, n)
}

// reaches reports whether to is reachable from from in one step or more.
func reaches(edges func(int) []int, from, to int) bool {
	seen := map[int]bool{}
	var visit func(n int) bool
	visit = func(n int) bool {
		for _, next := range edges(n) {
			if next == to {
				return true
			}
			if !seen[next] {
				seen[next] = true
				if visit(next) {
					return true
				}
			}
		}
		return false
	}
	return visit(from)
}
