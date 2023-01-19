package aho_corasick

import (
	"strings"
	"sync"
	"unicode"
)

type AhoCorasick struct {
	ptr uintptr
	abi *ahoCorasickABI

	matchOnlyWholeWords bool
	patternCount        int
}

func (ac AhoCorasick) PatternCount() int {
	return ac.patternCount
}

// Iter gives an iterator over the built patterns
func (ac AhoCorasick) Iter(haystack string) Iter {
	// Haystack must stay alive throughout the iteration so we malloc it. Unfortunately this
	// really slows things down.
	ac.abi.startOperation(0)
	defer ac.abi.endOperation()

	cs := ac.abi.newOwnedCString(haystack)

	iterPtr := ac.abi.findIter(ac.ptr, cs)

	return &findIter{ptr: iterPtr, abi: ac.abi, matchOnlyWholeWords: ac.matchOnlyWholeWords, haystack: haystack, haystackPtr: cs.ptr}
}

// IterOverlapping gives an iterator over the built patterns with overlapping matches
func (ac AhoCorasick) IterOverlapping(haystack string) Iter {
	// Haystack must stay alive throughout the iteration so we malloc it. Unfortunately this
	// really slows things down.
	ac.abi.startOperation(0)
	defer ac.abi.endOperation()

	cs := ac.abi.newOwnedCString(haystack)

	iterPtr := ac.abi.overlappingIter(ac.ptr, cs)

	return &overlappingIter{ptr: iterPtr, abi: ac.abi, matchOnlyWholeWords: ac.matchOnlyWholeWords, haystack: haystack, haystackPtr: cs.ptr}
}

var pool = sync.Pool{
	New: func() interface{} {
		return &strings.Builder{}
	},
}

type Replacer struct {
	finder Finder
}

func NewReplacer(finder Finder) Replacer {
	return Replacer{finder: finder}
}

// ReplaceAllFunc replaces the matches found in the haystack according to the user provided function
// it gives fine grained control over what is replaced.
// A user can chose to stop the replacing process early by returning false in the lambda
// In that case, everything from that point will be kept as the original haystack
func (r Replacer) ReplaceAllFunc(haystack string, f func(match Match) (string, bool)) string {
	matches := r.finder.FindAll(haystack)

	if len(matches) == 0 {
		return haystack
	}

	replaceWith := make([]string, 0)

	for _, match := range matches {
		rw, ok := f(match)
		if !ok {
			break
		}
		replaceWith = append(replaceWith, rw)
	}

	str := pool.Get().(*strings.Builder)

	defer func() {
		str.Reset()
		pool.Put(str)
	}()

	start := 0

	for i, match := range matches {
		if i >= len(replaceWith) {
			str.WriteString(haystack[start:])
			return str.String()
		}
		str.WriteString(haystack[start:match.Start()])
		str.WriteString(replaceWith[i])
		start = match.End()
	}

	if start-1 < len(haystack) {
		str.WriteString(haystack[start:])
	}

	return str.String()
}

// ReplaceAll replaces the matches found in the haystack according to the user provided slice `replaceWith`
// It panics, if `replaceWith` has length different from the patterns that it was built with
func (r Replacer) ReplaceAll(haystack string, replaceWith []string) string {
	if len(replaceWith) != r.finder.PatternCount() {
		panic("replaceWith needs to have the same length as the pattern count")
	}

	return r.ReplaceAllFunc(haystack, func(match Match) (string, bool) {
		return replaceWith[match.pattern], true
	})
}

type Finder interface {
	FindAll(haystack string) []Match
	PatternCount() int
}

// FindAll returns the matches found in the haystack
func (ac AhoCorasick) FindAll(haystack string) []Match {
	iter := ac.Iter(haystack)
	matches := make([]Match, 0)

	for {
		next := iter.Next()
		if next == nil {
			break
		}

		matches = append(matches, *next)
	}

	return matches
}

// Opts defines a set of options applied before the patterns are built
type Opts struct {
	AsciiCaseInsensitive bool
	MatchOnlyWholeWords  bool
	MatchKind            matchKind
	DFA                  bool
}

// NewAhoCorasickBuilder creates a new AhoCorasickBuilder based on Opts
func NewAhoCorasickBuilder(o Opts) AhoCorasickBuilder {
	return AhoCorasickBuilder{
		asciiCaseInsensitive: o.AsciiCaseInsensitive,
		matchOnlyWholeWords:  o.MatchOnlyWholeWords,
		matchKind:            o.MatchKind,
		dfa:                  o.DFA,
	}
}

type AhoCorasickBuilder struct {
	asciiCaseInsensitive bool
	matchOnlyWholeWords  bool
	matchKind            matchKind
	dfa                  bool
}

// Build builds a (non)deterministic finite automata from the user provided patterns
func (a *AhoCorasickBuilder) Build(patterns []string) AhoCorasick {
	// Concatenate to NULL-terminated strings
	numBytes := 0
	for _, pattern := range patterns {
		numBytes += len(pattern) + 1
	}

	buf := make([]byte, numBytes)
	idx := 0
	for _, pattern := range patterns {
		copy(buf[idx:], pattern)
		idx += len(pattern) + 1
	}

	abi := newABI()
	abi.startOperation(numBytes)
	ptr := abi.newMatcher(buf, a.asciiCaseInsensitive, a.dfa, int(a.matchKind))
	abi.endOperation()

	return AhoCorasick{
		ptr:                 ptr,
		abi:                 abi,
		matchOnlyWholeWords: a.matchOnlyWholeWords,
		patternCount:        len(patterns),
	}
}

// Iter is an iterator over matches found on the current haystack
// it gives the user more granular control. You can chose how many and what kind of matches you need.
type Iter interface {
	Next() *Match
}

type findIter struct {
	ptr                 uintptr
	abi                 *ahoCorasickABI
	matchOnlyWholeWords bool
	haystack            string
	haystackPtr         uintptr
}

// Next gives a pointer to the next match yielded by the iterator or nil, if there is none
func (f *findIter) Next() *Match {
	if f.ptr == 0 {
		return nil
	}

	f.abi.startOperation(12)
	pattern, start, end, ok := f.abi.findIterNext(f.ptr)
	f.abi.endOperation()

	if !ok {
		f.abi.startOperation(0)
		f.abi.findIterDelete(f.ptr)
		f.abi.freeOwnedCStringPtr(f.haystackPtr)
		f.abi.endOperation()
		f.ptr = 0
		return nil
	}

	result := &Match{
		pattern: pattern,
		start:   start,
		end:     end,
	}

	if f.matchOnlyWholeWords {
		if result.Start()-1 >= 0 && (unicode.IsLetter(rune(f.haystack[result.Start()-1])) || unicode.IsDigit(rune(f.haystack[result.Start()-1]))) {
			return f.Next()
		}
		if result.end < len(f.haystack) && (unicode.IsLetter(rune(f.haystack[result.end])) || unicode.IsDigit(rune(f.haystack[result.end]))) {
			return f.Next()
		}
	}

	return result
}

// While currently it could be possible to reuse findIter, the implementation
// seems like it will change significantly in next aho-corasick release
type overlappingIter struct {
	ptr                 uintptr
	abi                 *ahoCorasickABI
	matchOnlyWholeWords bool
	haystack            string
	haystackPtr         uintptr
}

// Next gives a pointer to the next match yielded by the iterator or nil, if there is none
func (o *overlappingIter) Next() *Match {
	if o.ptr == 0 {
		return nil
	}

	o.abi.startOperation(12)
	pattern, start, end, ok := o.abi.overlappingIterNext(o.ptr)
	o.abi.endOperation()

	if !ok {
		o.abi.startOperation(0)
		o.abi.overlappingIterDelete(o.ptr)
		o.abi.freeOwnedCStringPtr(o.haystackPtr)
		o.abi.endOperation()
		o.ptr = 0
		return nil
	}

	result := &Match{
		pattern: pattern,
		start:   start,
		end:     end,
	}

	if o.matchOnlyWholeWords {
		if result.Start()-1 >= 0 && (unicode.IsLetter(rune(o.haystack[result.Start()-1])) || unicode.IsDigit(rune(o.haystack[result.Start()-1]))) {
			return o.Next()
		}
		if result.end < len(o.haystack) && (unicode.IsLetter(rune(o.haystack[result.end])) || unicode.IsDigit(rune(o.haystack[result.end]))) {
			return o.Next()
		}
	}

	return result
}

type matchKind int

const (
	// Use standard match semantics, which support overlapping matches. When
	// used with non-overlapping matches, matches are reported as they are seen.
	StandardMatch matchKind = iota
	// Use leftmost-first match semantics, which reports leftmost matches.
	// When there are multiple possible leftmost matches, the match
	// corresponding to the pattern that appeared earlier when constructing
	// the automaton is reported.
	// This does **not** support overlapping matches or stream searching
	LeftMostFirstMatch
	// Use leftmost-longest match semantics, which reports leftmost matches.
	// When there are multiple possible leftmost matches, the longest match is chosen.
	LeftMostLongestMatch
)

// A representation of a match reported by an Aho-Corasick automaton.
//
// A match has two essential pieces of information: the identifier of the
// pattern that matched, along with the start and end offsets of the match
// in the haystack.
type Match struct {
	pattern int
	start   int
	end     int
}

// Pattern returns the index of the pattern in the slice of the patterns provided by the user that
// was matched
func (m *Match) Pattern() int {
	return m.pattern
}

// End gives the index of the last character of this match inside the haystack
func (m *Match) End() int {
	return m.end
}

// Start gives the index of the first character of this match inside the haystack
func (m *Match) Start() int {
	return m.start
}
