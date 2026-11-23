package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A word whose brace lists multiply past braceLimit is kept whole, at once, rather than
// expanded into a billion words before placement.
func TestBraceExpansionIsBounded(t *testing.T) {
	if got := braceExpand("/a/{b,c}/d"); !slices.Equal(got, []string{"/a/b/d", "/a/c/d"}) {
		t.Errorf("braceExpand(/a/{b,c}/d) = %q", got)
	}
	w := strings.Repeat("{a,b}", 30)
	done := make(chan []string, 1)
	go func() { done <- braceExpand(w) }()
	select {
	case got := <-done:
		if !slices.Equal(got, []string{w}) {
			t.Errorf("braceExpand of 2^30 expansions gave %d words, want the word itself", len(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("braceExpand of 2^30 expansions did not return")
	}
}
