package main

import (
	"strings"
	"testing"
)

func TestDebugIteratorStreams(t *testing.T) {
	volumes := deterministicMultiVolumeCorpus(t, 42)
	for _, query := range []string{"Downloads md !draft", "C: .nrrd !backup"} {
		pq := mustParseQuery(t, queryOptions{Query: query, MatchPath: true, Limit: 20})
		t.Logf("query=%q terms=%q implicit=%q exts=%q not=%+v dirs=%q", query, pq.Terms, pq.ImplicitPathTerms, pq.Exts, pq.NotGroups, pq.Dirs)
		for _, term := range []string{"Downloads", "md", "draft", ".nrrd", "backup"} {
			it, ok := globalPathTermIterator(volumes, term)
			if !ok {
				continue
			}
			ids := collectGlobalIterator(it, 0)
			t.Logf("term=%q ids=%d product=%v scenario=%v archive=%v backup=%v draft=%v", term, len(ids), globalDebugHas(volumes, ids, "product_summary.md"), globalDebugHas(volumes, ids, "scenario_region_summary.md"), globalDebugHas(volumes, ids, "archive-volume.nrrd"), globalDebugHas(volumes, ids, "backup-volume.nrrd"), globalDebugHas(volumes, ids, "draft-notes.md"))
		}
		it, ok := globalComponentQueryIterator(volumes, pq, &searchTrace{})
		if !ok {
			t.Log("query iterator declined")
			continue
		}
		ids := collectGlobalIterator(it, 0)
		t.Logf("query ids=%d product=%v scenario=%v archive=%v backup=%v draft=%v", len(ids), globalDebugHas(volumes, ids, "product_summary.md"), globalDebugHas(volumes, ids, "scenario_region_summary.md"), globalDebugHas(volumes, ids, "archive-volume.nrrd"), globalDebugHas(volumes, ids, "backup-volume.nrrd"), globalDebugHas(volumes, ids, "draft-notes.md"))
	}
	base, _ := globalPathTermIterator(volumes, "Downloads")
	filtered := newGlobalPathTermFilterIterator(base, volumes, "md")
	excluded, _ := globalPathTermIterator(volumes, "draft")
	baseIDs := collectGlobalIterator(&filtered, 0)
	excludedIDs := collectGlobalIterator(excluded, 0)
	baseIt := newGlobalIDSliceIterator(baseIDs)
	excludedIt := newGlobalIDSliceIterator(excludedIDs)
	kept := newGlobalExclusionIterator(&baseIt, &excludedIt)
	t.Logf("manual base=%d product=%v scenario=%v draft=%v exclude=%d kept product=%v scenario=%v draft=%v", len(baseIDs), globalDebugHas(volumes, baseIDs, "product_summary.md"), globalDebugHas(volumes, baseIDs, "scenario_region_summary.md"), globalDebugHas(volumes, baseIDs, "draft-notes.md"), len(excludedIDs), globalDebugHas(volumes, collectGlobalIterator(kept, 0), "product_summary.md"), globalDebugHas(volumes, baseIDs, "scenario_region_summary.md"), globalDebugHas(volumes, baseIDs, "draft-notes.md"))
	allBase, _ := globalPathTermIterator(volumes, "Downloads")
	allExcluded, _ := globalPathTermIterator(volumes, "draft")
	allKept := newGlobalExclusionIterator(allBase, allExcluded)
	allIDs := collectGlobalIterator(allKept, 0)
	t.Logf("manual broad base=%d kept=%d product=%v scenario=%v draft=%v", len(globalDebugPaths(volumes, mustGlobalIDs(volumes, "Downloads"))), len(allIDs), globalDebugHas(volumes, allIDs, "product_summary.md"), globalDebugHas(volumes, allIDs, "scenario_region_summary.md"), globalDebugHas(volumes, allIDs, "draft-notes.md"))
	extIt, _ := globalExtensionIterator(volumes, "md", &searchTrace{})
	negIt, _ := globalPathTermIterator(volumes, "draft")
	keptExt := newGlobalExclusionIterator(extIt, negIt)
	extIDs := collectGlobalIterator(keptExt, 0)
	t.Logf("manual ext kept=%d product=%v scenario=%v draft=%v", len(extIDs), globalDebugHas(volumes, extIDs, "product_summary.md"), globalDebugHas(volumes, extIDs, "scenario_region_summary.md"), globalDebugHas(volumes, extIDs, "draft-notes.md"))
	pathIt, _ := globalPathTermIterator(volumes, "Downloads")
	extIt, _ = globalExtensionIterator(volumes, "md", &searchTrace{})
	joined := newGlobalIntersectionIterator(pathIt, extIt)
	negIt, _ = globalPathTermIterator(volumes, "draft")
	joinedKept := newGlobalExclusionIterator(joined, negIt)
	joinedIDs := collectGlobalIterator(joinedKept, 0)
	t.Logf("manual path+ext kept=%d product=%v scenario=%v draft=%v", len(joinedIDs), globalDebugHas(volumes, joinedIDs, "product_summary.md"), globalDebugHas(volumes, joinedIDs, "scenario_region_summary.md"), globalDebugHas(volumes, joinedIDs, "draft-notes.md"))
	pathIt, _ = globalPathTermIterator(volumes, "Downloads")
	negIt, _ = globalPathTermIterator(volumes, "draft")
	pathKept := newGlobalExclusionIterator(pathIt, negIt)
	extIt, _ = globalExtensionIterator(volumes, "md", &searchTrace{})
	pathExt := newGlobalIntersectionIterator(pathKept, extIt)
	pathExtIDs := collectGlobalIterator(pathExt, 0)
	t.Logf("manual path-neg+ext kept=%d product=%v scenario=%v draft=%v paths=%v", len(pathExtIDs), globalDebugHas(volumes, pathExtIDs, "product_summary.md"), globalDebugHas(volumes, pathExtIDs, "scenario_region_summary.md"), globalDebugHas(volumes, pathExtIDs, "draft-notes.md"), globalDebugPaths(volumes, pathExtIDs))
}

func mustGlobalIDs(volumes []*serviceVolumeIndex, term string) []globalRecordID {
	it, ok := globalPathTermIterator(volumes, term)
	if !ok {
		return nil
	}
	return collectGlobalIterator(it, 0)
}

func globalDebugPaths(volumes []*serviceVolumeIndex, ids []globalRecordID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id.volume >= 0 && id.volume < len(volumes) && id.local >= 0 && id.local < volumes[id.volume].index.compactRecordCount() {
			out = append(out, volumes[id.volume].index.reconstructCompactPath(id.local))
		}
	}
	return out
}

func globalDebugHas(volumes []*serviceVolumeIndex, ids []globalRecordID, name string) bool {
	for _, path := range globalDebugPaths(volumes, ids) {
		if strings.HasSuffix(strings.ToLower(path), strings.ToLower(name)) {
			return true
		}
	}
	return false
}
