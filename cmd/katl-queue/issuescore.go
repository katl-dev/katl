package main

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
)

const (
	issueMatchFloor  = 0.20
	issueStrongFloor = 0.40
)

type issueText struct {
	Title, Body string
}

type issueMatch struct {
	Number    int
	Score     float64
	Identical bool
}

func (m issueMatch) strong() bool { return m.Identical || m.Score >= issueStrongFloor }

type issueDocument struct {
	text   issueText
	terms  map[string]float64
	order  []string
	length float64
}

// issueScorer owns the feature weights, corpus and cosine used by both issue
// intake and the report. A draft uses the same corpus weights as a saved issue.
type issueScorer struct {
	documents map[int]issueDocument
	idf       map[string]float64
	unseen    float64
}

func newIssueScorer(corpus map[int]issueText) issueScorer {
	s := issueScorer{
		documents: make(map[int]issueDocument, len(corpus)),
		idf:       map[string]float64{},
		unseen:    1 + math.Log(float64(len(corpus)+1)),
	}
	for number, text := range corpus {
		doc := issueDocumentFor(text)
		for term := range doc.terms {
			s.idf[term]++
		}
		s.documents[number] = doc
	}
	for term, count := range s.idf {
		s.idf[term] = 1 + math.Log(float64(len(corpus)+1)/(count+1))
	}
	for number, doc := range s.documents {
		s.documents[number] = s.weight(doc)
	}
	return s
}

var (
	issueWords       = regexp.MustCompile(`[\pL\pN]+`)
	issueIdentifiers = regexp.MustCompile(`#[0-9]+|[\pL\pN_]+(?:[./_-][\pL\pN_]+)+|\b(?:Test|test_)[A-Za-z0-9_]+\b`)
)

const issueStopWords = " a an and are as at be been but by can could do does for from had has have how if in into is it its must of on or our should so than that the their them then there these they this those to use was we were what when where which will with would you acceptance criteria "

func issueDocumentFor(text issueText) issueDocument {
	doc := issueDocument{
		text:  issueText{strings.Join(strings.Fields(text.Title), " "), strings.Join(strings.Fields(text.Body), " ")},
		terms: map[string]float64{},
	}
	add := func(value string, weight float64, pattern *regexp.Regexp, prefix string) {
		for _, match := range pattern.FindAllString(value, -1) {
			term := strings.ToLower(match)
			if prefix == "" && (len(term) < 2 || strings.Contains(issueStopWords, " "+term+" ")) {
				continue
			}
			doc.terms[prefix+term] += weight
		}
	}
	add(text.Title, 2, issueWords, "")
	add(text.Body, 1, issueWords, "")
	add(text.Title, 2, issueIdentifiers, "id:")
	add(text.Body, 2, issueIdentifiers, "id:")
	for term := range doc.terms {
		doc.order = append(doc.order, term)
	}
	slices.Sort(doc.order)
	return doc
}

func (s issueScorer) weight(doc issueDocument) issueDocument {
	for _, term := range doc.order {
		idf := cmp.Or(s.idf[term], s.unseen)
		weight := (1 + math.Log(doc.terms[term])) * idf
		doc.terms[term] = weight
		doc.length += weight * weight
	}
	doc.length = math.Sqrt(doc.length)
	return doc
}

func (s issueScorer) rank(text issueText, self int) []issueMatch {
	query, saved := s.documents[self]
	if !saved {
		query = s.weight(issueDocumentFor(text))
	}
	matches := make([]issueMatch, 0, len(s.documents))
	for number, doc := range s.documents {
		if number == self {
			continue
		}
		match := issueMatch{Number: number, Identical: query.text == doc.text}
		if query.length > 0 && doc.length > 0 {
			for _, term := range query.order {
				match.Score += query.terms[term] * doc.terms[term]
			}
			match.Score = min(1, match.Score/(query.length*doc.length))
		}
		matches = append(matches, match)
	}
	slices.SortFunc(matches, func(a, b issueMatch) int {
		if a.Identical != b.Identical {
			if a.Identical {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Number, b.Number))
	})
	return matches
}
