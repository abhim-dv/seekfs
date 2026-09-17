package main

import "testing"

// globalComponentDefaultHasRoot used to accept a query as rooted once the
// first OR group validated, silently ignoring later groups. Every group has to
// be rooted now, matching the per-group walk in globalComponentDefaultTermsLong.
func TestGlobalComponentDefaultHasRootRequiresEveryOrGroup(t *testing.T) {
	rooted := parsedQuery{Dirs: []string{"windows"}}
	unrooted := parsedQuery{Terms: []string{"log"}}

	if !globalComponentDefaultHasRoot(rooted) {
		t.Fatal("expected a dir-rooted query to be rooted")
	}
	if globalComponentDefaultHasRoot(unrooted) {
		t.Fatal("expected a bare term query to be unrooted")
	}
	if !globalComponentDefaultHasRoot(parsedQuery{OrGroups: [][]parsedQuery{{rooted}, {rooted}}}) {
		t.Fatal("expected all-rooted OR groups to be accepted")
	}
	if globalComponentDefaultHasRoot(parsedQuery{OrGroups: [][]parsedQuery{{rooted}, {unrooted}}}) {
		t.Fatal("expected an unrooted later OR group to be rejected")
	}
	if globalComponentDefaultHasRoot(parsedQuery{OrGroups: [][]parsedQuery{{}}}) {
		t.Fatal("expected an empty OR group to be rejected")
	}
}
