package main

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

func globalPathTermIterator(volumes []*serviceVolumeIndex, term string) (globalIDIterator, bool) {
	if term == "" {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(term) {
			return nil, false
		}
		if len(volumes) == 1 && vol.index.compactRecordCount() > serviceResidentChildRangeMaxRecords &&
			len(vol.subtreeOrder) == 0 && len(vol.childOffsets) > 0 {
			// Without persisted subtree order, estimating a broad component
			// would walk the entire child graph. Let the bounded single-volume
			// planner choose its indexed candidate source instead.
			return nil, false
		}
		// When persisted subtree order is absent, do not walk every compact
		// record and repeatedly test its parent chain. The exact path posting
		// traverses the child ranges once and is cached, preserving a global
		// set source without reintroducing a per-volume terminal search.
		if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.pathTermPosting(term)})
			continue
		}
		// Old indexes without child/subtree metadata retain the exact existing
		// scan semantics. They are a bounded compatibility fallback, not a
		// partially-correct posting source.
		if !vol.hasDescendantIndex() && vol.children == nil {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.pathTermPosting(term)})
			continue
		}
		nameIDs := vol.nameTermPosting(term)
		parts := make([]globalIDIterator, 0, 2)
		if len(nameIDs) > 0 {
			parts = append(parts, &globalRecordIterator{volume: volumeIndex, ids: nameIDs})
		}
		roots := vol.pathTermRootIDs(term)
		if len(roots) > 0 {
			if len(volumes) == 1 {
				for _, root := range roots {
					if vol.estimatedDescendantOrSelfCount(root) > serviceComponentTrigramExpansionMaxIDs {
						// Let the single-volume candidate planner choose its
						// bounded source; never eagerly materialize a huge
						// subtree just to return a small top-N page.
						return nil, false
					}
				}
			}
			parts = append(parts, newGlobalSubtreeIterator(volumeIndex, vol, roots))
		}
		if len(parts) == 0 {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: nil})
		} else {
			children = append(children, newGlobalMergeIterator(parts...))
		}
	}
	return newGlobalMergeIterator(children...), true
}

func sortGlobalPathProbeTerms(volumes []*serviceVolumeIndex, terms []string) []string {
	probes := append([]string(nil), terms...)
	sort.SliceStable(probes, func(i, j int) bool {
		return estimateGlobalPathTerm(volumes, probes[i]) < estimateGlobalPathTerm(volumes, probes[j])
	})
	return probes
}

func estimateGlobalPathTerm(volumes []*serviceVolumeIndex, term string) int {
	if term == "" {
		return 0
	}
	total := 0
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		candidate, ok := vol.componentPostingCountCandidate(term)
		if ok && candidate.mapped {
			// Root postings can themselves be large (e.g. Users).  Use their
			// count as a conservative selectivity estimate instead of decoding
			// the entire root list merely to choose probe order.
			if candidate.count > 10_000 {
				total += candidate.count
				continue
			}
			for _, root32 := range candidate.materialize() {
				root := int(root32)
				if root >= 0 && root < len(vol.subtreeStart) && root < len(vol.subtreeEnd) {
					start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
					if start != ^uint32(0) && start <= end {
						total += int(end - start)
					}
				}
			}
			continue
		}
		if ok {
			total += candidate.len()
			continue
		}
		total += len(vol.nameTermPosting(term))
	}
	return total
}

func globalUnderIterator(volumes []*serviceVolumeIndex, under string) (globalIDIterator, bool) {
	roots, ok := globalUnderRoots(volumes, under)
	if !ok {
		return nil, false
	}
	byVolume := make(map[int][]int)
	for _, root := range roots {
		byVolume[root.volume] = append(byVolume[root.volume], root.local)
	}
	children := make([]globalIDIterator, 0, len(byVolume))
	for volume, volumeRoots := range byVolume {
		children = append(children, newGlobalSubtreeIterator(volume, volumes[volume], volumeRoots))
	}
	return newGlobalMergeIterator(children...), true
}

func globalParentIterator(volumes []*serviceVolumeIndex, parent string) (globalIDIterator, bool) {
	if parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.parentIDs(parent)})
	}
	return newGlobalMergeIterator(children...), true
}

func globalAttributeIterator(volumes []*serviceVolumeIndex, filters []uint32) (globalIDIterator, bool) {
	if len(filters) == 0 {
		return nil, false
	}
	var current globalIDIterator
	for _, mask := range filters {
		children := make([]globalIDIterator, 0, len(volumes))
		for volumeIndex, vol := range volumes {
			if vol == nil || vol.index == nil {
				return nil, false
			}
			ids, ok := vol.attrIDsForMask(mask)
			if !ok {
				return nil, false
			}
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: ids})
		}
		next := newGlobalMergeIterator(children...)
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	return current, current != nil
}

func globalExtensionIterator(volumes []*serviceVolumeIndex, ext string, trace *searchTrace) (globalIDIterator, bool) {
	if ext == "" {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		posting, ok := vol.extPostingCountCandidate(ext)
		if !ok {
			trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false
		}
		postingIt := newGlobalPostingIteratorWithTrace(volumeIndex, posting, trace)
		children = append(children, &postingIt)
	}
	return newGlobalMergeIterator(children...), true
}

func globalComponentSubqueryIterator(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
	var current globalIDIterator
	intersect := func(next globalIDIterator) {
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		probes := sortGlobalPathProbeTerms(volumes, pathPlanProbeTerms(terms))
		if len(probes) == 0 {
			return nil, false
		}
		if estimateGlobalPathTerm(volumes, probes[0]) > serviceComponentMultiTermScanMaxIDs {
			return nil, false
		}
		it, ok := globalPathTermIterator(volumes, probes[0])
		if !ok {
			return nil, false
		}
		current = it
		for _, term := range probes[1:] {
			filtered := newGlobalPathTermFilterIterator(current, volumes, term)
			current = &filtered
		}
	}
	for _, dir := range pq.Dirs {
		it, ok := globalPathTermIterator(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if globalRegexLiteralSupported(pq) {
		it, ok := globalPathTermIterator(volumes, pq.RegexTerms[0])
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, parent := range pq.Parents {
		it, ok := globalParentIterator(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if len(pq.AttrFilters) > 0 {
		it, ok := globalAttributeIterator(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, extFilter := range mustGlobalExtFilters(pq) {
		it, ok := globalExtensionIterator(volumes, extFilter.ext, trace)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if pq.Type != "" {
		if current == nil {
			return nil, false
		}
		current = &globalTypeFilterIterator{base: current, volumes: volumes, typ: pq.Type}
	}
	return current, current != nil
}

func mustGlobalExtFilters(pq parsedQuery) []globalExtFilter {
	filters, ok := globalExtPostingFilters(pq)
	if !ok {
		return nil
	}
	return filters
}

func globalComponentQueryIterator(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
	var current globalIDIterator
	intersect := func(next globalIDIterator) {
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	if pq.Under != "" {
		under, ok := globalUnderIterator(volumes, pq.Under)
		if !ok {
			return nil, false
		}
		current = under
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		probes := sortGlobalPathProbeTerms(volumes, pathPlanProbeTerms(terms))
		if len(probes) == 0 {
			return nil, false
		}
		if estimateGlobalPathTerm(volumes, probes[0]) > serviceComponentMultiTermScanMaxIDs {
			// A multi-term plan drives off the smallest probe.  If that probe
			// is already unbounded, materializing the intersection would build
			// a slice proportional to the whole volume.  Decline so the caller
			// routes to the bounded exhaustive scan instead.
			return nil, false
		}
		it, ok := globalPathTermIterator(volumes, probes[0])
		if !ok {
			return nil, false
		}
		intersect(it)
		for _, term := range probes[1:] {
			filtered := newGlobalPathTermFilterIterator(current, volumes, term)
			current = &filtered
		}
	}
	for _, group := range pq.OrGroups {
		alternatives := make([]globalIDIterator, 0, len(group))
		for _, alt := range group {
			it, ok := globalComponentSubqueryIterator(volumes, alt, trace)
			if !ok {
				return nil, false
			}
			alternatives = append(alternatives, it)
		}
		intersect(newGlobalMergeIterator(alternatives...))
	}
	for _, dir := range pq.Dirs {
		it, ok := globalPathTermIterator(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if globalRegexLiteralSupported(pq) {
		it, ok := globalPathTermIterator(volumes, pq.RegexTerms[0])
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, parent := range pq.Parents {
		it, ok := globalParentIterator(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if len(pq.AttrFilters) > 0 {
		it, ok := globalAttributeIterator(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, extFilter := range mustGlobalExtFilters(pq) {
		it, ok := globalExtensionIterator(volumes, extFilter.ext, trace)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if pq.Type != "" {
		if current == nil {
			return nil, false
		}
		current = &globalTypeFilterIterator{base: current, volumes: volumes, typ: pq.Type}
	}
	for _, neg := range pq.NotGroups {
		if current == nil {
			all := make([]globalIDIterator, 0, len(volumes))
			for volumeIndex, vol := range volumes {
				if vol == nil || vol.index == nil {
					return nil, false
				}
				ids := make([]int, vol.index.compactRecordCount())
				for id := range ids {
					ids[id] = id
				}
				all = append(all, &globalRecordIterator{volume: volumeIndex, ids: ids})
			}
			current = newGlobalMergeIterator(all...)
		}
		excluded, ok := globalComponentSubqueryIterator(volumes, neg, trace)
		if !ok {
			return nil, false
		}
		current = newGlobalExclusionIterator(current, excluded)
	}
	return current, current != nil
}

func globalSimplePathORTerms(pq parsedQuery) ([]string, bool) {
	if !pq.MatchPath || len(pq.OrGroups) != 1 || len(pq.NotGroups) != 0 || pq.Type != "" ||
		pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.Exts) != 0 || len(pq.Dirs) != 0 ||
		len(pq.Globs) != 0 || len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		pq.CaseSensitive || len(pq.OrGroups[0]) == 0 {
		return nil, false
	}
	if len(nonVolumeTerms(pq.Terms)) != 0 {
		return nil, false
	}
	terms := make([]string, 0, len(pq.OrGroups[0]))
	for _, alt := range pq.OrGroups[0] {
		altTerms := nonVolumeTerms(alt.Terms)
		if len(altTerms) != 1 || len(alt.OrGroups) != 0 || len(alt.NotGroups) != 0 ||
			len(alt.Dirs) != 0 || len(alt.Globs) != 0 || len(alt.Regexps) != 0 ||
			len(alt.RegexTerms) != 0 || len(alt.Parents) != 0 || len(alt.Exts) != 0 ||
			len(alt.SizeFilters) != 0 || len(alt.DateFilters) != 0 || len(alt.AttrFilters) != 0 ||
			alt.Type != "" || alt.Under != "" || alt.Exists || alt.HasModAfter {
			return nil, false
		}
		terms = append(terms, altTerms[0])
	}
	return terms, true
}

func globalSimplePathORTopIDs(volumes []*serviceVolumeIndex, pq parsedQuery, limit int) ([]globalRecordID, bool) {
	if pq.Cancel != nil {
		return nil, false
	}
	terms, ok := globalSimplePathORTerms(pq)
	if !ok || limit <= 0 {
		return nil, false
	}
	seen := make(map[globalRecordID]struct{}, len(volumes)*limit*len(terms))
	out := make([]globalRecordID, 0, len(volumes)*limit*len(terms))
	for volumeIndex, vol := range volumes {
		if queryCanceled(pq) {
			return nil, false
		}
		if vol == nil || vol.index == nil {
			return nil, false
		}
		ranks := vol.rankForQuery(pq)
		for _, term := range terms {
			if queryCanceled(pq) {
				return nil, false
			}
			if !vol.pathComponentPostingAvailable(term) {
				return nil, false
			}
			for _, local := range topCandidateIDsByRank(vol.pathTermPosting(term), limit, vol.index, ranks) {
				id := globalRecordID{volume: volumeIndex, local: local}
				if _, exists := seen[id]; exists {
					continue
				}
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	return out, true
}

func globalSimplePathORCount(volumes []*serviceVolumeIndex, pq parsedQuery) (int, bool) {
	if pq.Cancel != nil {
		return 0, false
	}
	terms, ok := globalSimplePathORTerms(pq)
	if !ok {
		return 0, false
	}
	children := make([]globalIDIterator, 0, len(volumes)*len(terms))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return 0, false
		}
		for _, term := range terms {
			if !vol.pathComponentPostingAvailable(term) {
				return 0, false
			}
			it := newGlobalRecordIterator(volumeIndex, vol.pathTermPosting(term))
			children = append(children, &it)
		}
	}
	merged := newGlobalMergeIterator(children...)
	count, _, err := countGlobalVerifiedIterator(merged, volumes, nil, pq)
	return count, err == nil
}

func globalComponentTopIDs(volumes []*serviceVolumeIndex, pq parsedQuery, limit int) ([]globalRecordID, bool) {
	term, ok := globalExactPathComponentTerm(pq)
	if limit <= 0 || !ok {
		return nil, false
	}
	out := make([]globalRecordID, 0, len(volumes)*limit)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		var ids []int
		var ok bool
		if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
			// v8-compatible low-memory indexes retain exact child ranges but
			// omit SUBT rank metadata. The cached exact path posting is still a
			// complete set source; rank only that set instead of materializing
			// every match through the generic component iterator.
			ids = topCandidateIDsByRank(vol.pathTermPosting(term), limit, vol.index, vol.rankForQuery(pq))
			ok = true
		} else {
			ids, ok = vol.mappedComponentTopPosting(term, limit, pq)
		}
		if !ok {
			return nil, false
		}
		for _, id := range ids {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	return out, true
}

func globalExactPathComponentTerm(pq parsedQuery) (string, bool) {
	terms := nonVolumeTerms(pq.Terms)
	if !pq.MatchPath || len(terms) != 1 || pq.CaseSensitive ||
		pq.Type != "" || pq.Under != "" || pq.Exists || pq.HasModAfter ||
		len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 || pq.RootBias != "" || pq.CWDBias != "" {
		return "", false
	}
	return terms[0], true
}

func globalExtPostingIDs(volumes []*serviceVolumeIndex, ext string, limit int, trace *searchTrace) ([]globalRecordID, bool) {
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			trace.addDeclineForVolume("global-ext:missing-volume", "")
			return nil, false
		}
		posting, ok := vol.extPostingCountCandidate(ext)
		if !ok {
			trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false
		}
		it := newGlobalPostingIteratorWithTrace(volumeIndex, posting, trace)
		remaining := 0
		if limit > 0 {
			remaining = limit - len(out)
			if remaining <= 0 {
				return out, true
			}
		}
		out = append(out, collectGlobalIterator(&it, remaining)...)
	}
	return out, true
}

func globalComponentRootIDs(volumes []*serviceVolumeIndex, component string, limit int) ([]globalRecordID, bool) {
	if component == "" {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(component) {
			return nil, false
		}
		for _, id := range vol.pathComponentRootIDs(component) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
			if limit > 0 && len(out) >= limit {
				return out, true
			}
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalSubtreeIDs(volumes []*serviceVolumeIndex, roots []globalRecordID, limit int) ([]globalRecordID, bool) {
	if len(roots) == 0 {
		return nil, true
	}
	out := make([]globalRecordID, 0)
	seenByVolume := make(map[int]map[int]struct{})
	for _, root := range roots {
		if root.volume < 0 || root.volume >= len(volumes) {
			return nil, false
		}
		vol := volumes[root.volume]
		if vol == nil || vol.index == nil || root.local < 0 || root.local >= vol.index.compactRecordCount() {
			return nil, false
		}
		seen := seenByVolume[root.volume]
		if seen == nil {
			seen = make(map[int]struct{})
			seenByVolume[root.volume] = seen
		}
		var descendants []int
		if limit > 0 {
			descendants = vol.underDescendantsLimited(root.local, limit-len(out))
		} else {
			descendants = vol.underDescendants(root.local)
		}
		for _, id := range descendants {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, globalRecordID{volume: root.volume, local: id})
			if limit > 0 && len(out) >= limit {
				return out, true
			}
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func sortGlobalRecordIDs(ids []globalRecordID) {
	slices.SortFunc(ids, compareGlobalRecordID)
}

func searchServiceVolumesGlobalExtOnly(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalExtOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalExtOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	globalEnabled := globalPlannerEnabled()
	if !globalEnabled && !globalExtDefaultSupported(pq) {
		return nil, false, nil
	}
	if !globalExtOnlySupported(pq) {
		if globalEnabled {
			opts.Trace.replaceDecline("global-ext:unsupported-query")
		}
		return nil, false, nil
	}
	extFilters, _ := globalExtPostingFilters(pq)
	extFilter := extFilters[0]
	limit := normalizedLimit(opts.Limit, false)
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-ext:overlay-snapshot-missing")
		return nil, false, nil
	}
	if countOnly {
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, 0))
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}
	if pq.SortColumn != "" {
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}
	if !globalVolumesHaveRankForQuery(volumes, pq) {
		if globalSnapshotsHaveOverlayRecords(snapshots) {
			opts.Trace.replaceDecline("global-ext:rankless-overlay")
			return nil, false, nil
		}
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		results, err := entriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		sortSearchAllEntries(results, pq)
		if limit > 0 && len(results) > limit {
			results = results[:limit]
		}
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}

	ids := make([]globalRecordID, 0, limit*len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.addDeclineForVolume("global-ext:missing-volume", "")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
		posting, ok := vol.extPostingCountCandidate(extFilter.ext)
		if !ok {
			opts.Trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false, nil
		}
		if !globalSnapshotsHaveHidden(snapshots) {
			if localIDs, ok := vol.extTopPosting(extFilter.ext, limit, pq); ok {
				for _, local := range localIDs {
					ids = append(ids, globalRecordID{volume: volumeIndex, local: local})
				}
				continue
			}
		}
		it := newGlobalPostingIteratorWithTrace(volumeIndex, posting, opts.Trace)
		var source globalIDIterator = &it
		if globalSnapshotsHaveHidden(snapshots) {
			hiddenIt := newGlobalHiddenIterator(&it, snapshots)
			source = &hiddenIt
		}
		rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
		ids = append(ids, collectGlobalTopN([]globalIDIterator{source}, limit, func(id globalRecordID) int {
			return rankOf(id.local)
		})...)
	}
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setPlannerMode("global-ext")
	opts.Trace.addTerm(traceTerm{
		Term:      extFilter.ext,
		Kind:      "extension",
		Source:    "global:" + extFilter.source,
		CountHint: len(ids),
		Exact:     true,
	})
	opts.Trace.setSource("global:"+extFilter.source, len(ids))
	opts.Trace.setComplete(true)
	return results, true, nil
}

func searchServiceVolumesGlobalComponentsOnly(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalComponentsOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalComponentsOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	globalEnabled := globalPlannerEnabled()
	if !globalEnabled && !globalComponentDefaultSupported(pq, terms) {
		return nil, false, nil
	}
	if !globalComponentQuerySupportedMulti(pq, terms, len(volumes) > 1) {
		opts.Trace.replaceDecline("global-components:unsupported-query")
		return nil, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-components:overlay-snapshot-missing")
		return nil, false, nil
	}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-components:missing-volume")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
	}
	limit := normalizedLimit(opts.Limit, false)
	if countOnly {
		limit = 0
	}
	if !countOnly && (len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0) {
		if ids, ok := globalSimplePathORTopIDs(volumes, pq, limit); ok && !globalSnapshotsHaveHidden(snapshots) && !globalSnapshotsHaveOverlayRecords(snapshots) {
			topIt := newGlobalIDSliceIterator(ids)
			base, verified, err := collectGlobalVerifiedTopN(&topIt, volumes, snapshots, pq, limit)
			if err != nil {
				return nil, true, err
			}
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += verified
				opts.Trace.setPlannerMode("global-components")
				opts.Trace.setSource("global:boolean-persisted-top", len(ids))
				opts.Trace.setComplete(true)
			}
			return globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit)), true, nil
		}
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-components:boolean-missing-source")
			}
			return nil, false, nil
		}
		base, verified, err := collectGlobalVerifiedTopN(componentIt, volumes, snapshots, pq, limit)
		if err != nil {
			return nil, true, err
		}
		if opts.Trace != nil {
			opts.Trace.ComponentRecordsVerified += verified
			opts.Trace.setPlannerMode("global-components")
			opts.Trace.setSource("global:boolean-iterator", len(base))
			opts.Trace.setComplete(true)
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit))
		return results, true, nil
	}
	var ids []globalRecordID
	topUsed := false
	if !countOnly && !globalSnapshotsHaveHidden(snapshots) {
		if topIDs, ok := globalComponentTopIDs(volumes, pq, limit); ok {
			ids = topIDs
			topUsed = true
		}
	}
	if !topUsed {
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-components:missing-source")
			}
			return nil, false, nil
		}
		if !countOnly && limit > 0 {
			// Stream the iterator and verify on the fly, keeping only the
			// top-N in a bounded heap.  The old path materialized every
			// candidate id (potentially millions, e.g. a broad regex-literal
			// query) and then re-verified them in a second pass.  Streaming
			// keeps memory O(limit) and runs one verification pass, which is
			// the difference between 28s and tens of ms for regex-literal
			// queries.
			base, verified, err := collectGlobalVerifiedTopN(componentIt, volumes, snapshots, pq, limit)
			if err != nil {
				return nil, true, err
			}
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += verified
			}
			results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit))
			opts.Trace.setPlannerMode("global-components")
			addGlobalComponentTraceTerms(opts.Trace, pq, len(base))
			opts.Trace.setSource("global:components", len(base))
			opts.Trace.setComplete(true)
			return results, true, nil
		}
		canceled := false
		if globalSnapshotsHaveHidden(snapshots) {
			hidden := newGlobalHiddenIterator(componentIt, snapshots)
			ids, canceled = collectGlobalIteratorCancelable(&hidden, 0, func() bool { return queryCanceled(pq) })
		} else {
			ids, canceled = collectGlobalIteratorCancelable(componentIt, 0, func() bool { return queryCanceled(pq) })
		}
		if canceled {
			return nil, true, errQueryCanceled
		}
	}
	ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setPlannerMode("global-components")
	addGlobalComponentTraceTerms(opts.Trace, pq, len(ids))
	if topUsed {
		opts.Trace.setSource("global:component-top", len(ids))
	} else {
		opts.Trace.setSource("global:components", len(ids))
	}
	opts.Trace.setComplete(true)
	return results, true, nil
}

func searchServiceVolumesGlobalBoundedFallback(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalBoundedFallbackSnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalBoundedFallbackSnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	if len(volumes) < 2 {
		return nil, false, nil
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	if !globalPlannerEnabled() && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) && !globalBoundedFallbackDefaultSupported(pq) {
		return nil, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-bounded-scan:overlay-snapshot-missing")
		return nil, false, nil
	}
	if !globalBoundedScanBudgetOK(volumes, pq, 3) {
		opts.Trace.replaceDecline("global-bounded-scan:budget")
		return nil, false, nil
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if countOnly {
		limit = 0
	}
	ids := make([]globalRecordID, 0, 1024)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-bounded-scan:missing-volume")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
		volumePQ := pq
		volumePQ.Limit = limit
		volumePQ.CountOnly = countOnly
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		var localIDs []int
		var ok bool
		orderReady := len(vol.orderForQuery(volumePQ)) >= vol.index.compactRecordCount()
		if countOnly || !orderReady || volumePQ.RootBias != "" || volumePQ.CWDBias != "" {
			volumePQ.Limit = 0
			volumePQ.CountOnly = true
			var filter *boundedScanMembershipFilter
			exactEmpty, filterOK := vol.boundedScanPrefilter(volumePQ, &filter)
			if exactEmpty {
				localIDs = []int{}
				ok = true
			} else if filterOK {
				localIDs, ok = vol.boundedScanCandidatesFiltered(volumePQ, filter)
			} else {
				localIDs, ok = vol.boundedScanCandidates(volumePQ)
			}
		} else {
			hidden := hiddenBaseIDs{}
			if volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
				hidden = hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
			}
			// Pre-filter the name/id-order scan with a cheap exact posting
			// (ext:, glob-ext:, a bounded type:dir subtree, or a required regex
			// literal run) when present, so broad queries like "test ext:py",
			// "type:dir docs", or "regex:README\.(md|txt)$" skip non-matching
			// records instead of verifying every record.  The scan order is
			// unchanged, preserving the bounded scan's top-N semantics.
			var filter *boundedScanMembershipFilter
			exactEmpty, filterOK := vol.boundedScanPrefilter(volumePQ, &filter)
			if !exactEmpty && filterOK {
				localIDs, ok = vol.boundedScanCandidatesHiddenTopFiltered(volumePQ, hidden, limit, filter)
			} else if exactEmpty {
				localIDs = []int{}
				ok = true
			} else {
				localIDs, ok = vol.boundedScanCandidatesHiddenTop(volumePQ, hidden, limit)
			}
		}
		if !ok {
			opts.Trace.replaceDecline("global-bounded-scan:canceled")
			return nil, false, nil
		}
		for _, local := range localIDs {
			ids = append(ids, globalRecordID{volume: volumeIndex, local: local})
		}
	}
	ids = filterGlobalIDsHidden(ids, snapshots)
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setFallback("global-bounded-scan")
	opts.Trace.setPlannerMode("global-bounded-scan")
	opts.Trace.setSource("global:bounded-scan", len(ids))
	opts.Trace.setComplete(true)
	return results, true, nil
}

func countServiceVolumesGlobalOnly(volumes []*serviceVolumeIndex, opts queryOptions) (int, bool, error) {
	return countServiceVolumesGlobalOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts)
}

// globalBoundedScanBudgetOK reports whether the remaining query deadline can
// plausibly cover a full per-volume record scan.  A full scan is linear in the
// compact record count (roughly 30-140ms per million records measured on a
// 26.8M-record two-volume service: search-path candidate scans near 30ms/M,
// count-path verification near 140ms/M), so a pessimistic 100ms/M is a safe
// base.  Starting a scan when the remaining budget is too tight would only
// block until the deadline and then cancel, so the margin keeps the check
// generous while still avoiding clearly-doomed scans.  The scan itself remains
// deadline-cancellable, so a slightly optimistic estimate only costs waiting
// until the deadline rather than a wrong answer.
func globalBoundedScanBudgetOK(volumes []*serviceVolumeIndex, pq parsedQuery, margin float64) bool {
	if pq.DeadlineUnix <= 0 {
		return true
	}
	remaining := time.Until(time.Unix(0, pq.DeadlineUnix))
	if remaining <= 0 {
		return false
	}
	var records int64
	for _, vol := range volumes {
		if vol != nil && vol.index != nil {
			records += int64(vol.index.compactRecordCount())
		}
	}
	estimatedMS := float64(records) / 1e6 * 100
	return float64(remaining.Milliseconds()) >= estimatedMS*margin
}

// globalTypeTermCountShape reports whether pq is a bare `type:<typ> <term>`
// query: exactly one non-volume term and no other filter.  These shapes used
// to force the global bounded fallback into a full per-volume record scan.
func globalTypeTermCountShape(pq parsedQuery, typ string) (string, bool) {
	if pq.Type != typ {
		return "", false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) != 1 || len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.SizeFilters) != 0 ||
		len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 || len(pq.OrGroups) != 0 ||
		len(pq.NotGroups) != 0 || pq.CaseSensitive || pq.RootBias != "" || pq.CWDBias != "" {
		return "", false
	}
	return terms[0], true
}

// globalDirTermCountShape reports whether pq is a bare `type:dir <term>`
// query routed through the capped dir-posting count.
func globalDirTermCountShape(pq parsedQuery) (string, bool) {
	return globalTypeTermCountShape(pq, "dir")
}

// globalFileTermCountShape reports whether pq is a bare `type:file <term>`
// query routed through the capped term-posting count.
func globalFileTermCountShape(pq parsedQuery) (string, bool) {
	return globalTypeTermCountShape(pq, "file")
}

// boundedDirTermPosting returns the term's posting capped to
// serviceComponentMultiTermScanMaxIDs.  The path form reuses the ext-filter
// bounded builder (name matches plus capped descendant expansion); the name
// form uses the trigram-limited name posting, falling back to the scanned name
// posting only when its size is known to be within the cap.
func (vol *serviceVolumeIndex) boundedDirTermPosting(term string, matchPath bool) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	if matchPath {
		return vol.pathTermPostingForExtFilter(term, serviceComponentMultiTermScanMaxIDs)
	}
	if ids, ok := vol.completeNameTrigramNameTermPostingLimited(term, serviceComponentMultiTermScanMaxIDs); ok {
		return ids, true
	}
	ids := vol.nameTermPosting(term)
	if len(ids) > serviceComponentMultiTermScanMaxIDs {
		return nil, false
	}
	return ids, true
}

// countDirTermLive is an exact, bounded count for `type:dir <term>`.  The
// resident dir list is the type:dir posting; the term's bounded name/path
// posting drives an intersection, so a selective term never scans the whole
// volume.  Live recentIDs are reconciled the same way extTopPosting does, so
// the count stays complete while the legacy engine is live-updating base
// records in place.
func (vol *serviceVolumeIndex) countDirTermLive(term string, matchPath bool, hidden hiddenBaseIDs) (int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil || !vol.queryIndex.dirsReady {
		return 0, false
	}
	dirs := vol.queryIndex.dirs
	ids, ok := vol.boundedDirTermPosting(term, matchPath)
	if !ok || len(ids) > serviceComponentMultiTermScanMaxIDs {
		return 0, false
	}
	var seen map[int]struct{}
	if len(vol.recentIDs) > 0 {
		seen = make(map[int]struct{}, len(ids))
	}
	count := 0
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		pos := sort.Search(len(dirs), func(i int) bool { return dirs[i] >= uint32(id) })
		if pos >= len(dirs) || dirs[pos] != uint32(id) {
			continue
		}
		count++
		if seen != nil {
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		if seen != nil {
			if _, exists := seen[id]; exists {
				continue
			}
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if matchPath {
			if vol.index.compactPathContainsTerm(id, term) {
				count++
			}
		} else if strings.Contains(vol.index.compactLowerNameAt(id), term) {
			count++
		}
	}
	return count, true
}

// countFileTermLive is the type:file mirror of countDirTermLive.  The term's
// bounded name/path posting drives the intersection; each candidate is kept
// only when its record is not a directory.  There is no persisted file posting
// (a complement of dirs would be the full volume), so the predicate is applied
// per candidate exactly as the verified path does, keeping count/search parity.
func (vol *serviceVolumeIndex) countFileTermLive(term string, matchPath bool, hidden hiddenBaseIDs) (int, bool) {
	if vol == nil || vol.index == nil {
		return 0, false
	}
	ids, ok := vol.boundedDirTermPosting(term, matchPath)
	if !ok || len(ids) > serviceComponentMultiTermScanMaxIDs {
		return 0, false
	}
	var seen map[int]struct{}
	if len(vol.recentIDs) > 0 {
		seen = make(map[int]struct{}, len(ids))
	}
	count := 0
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		count++
		if seen != nil {
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		if seen != nil {
			if _, exists := seen[id]; exists {
				continue
			}
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		if matchPath {
			if vol.index.compactPathContainsTerm(id, term) {
				count++
			}
		} else if strings.Contains(vol.index.compactLowerNameAt(id), term) {
			count++
		}
	}
	return count, true
}

func countServiceVolumesGlobalBoundedFallbackSnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {
	if len(snapshot.volumes) < 2 {
		return 0, false, nil
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	if !globalPlannerEnabled() && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) && !globalBoundedFallbackDefaultSupported(pq) {
		return 0, false, nil
	}
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-bounded-scan:overlay-snapshot-missing")
		return 0, false, nil
	}
	if !globalBoundedScanBudgetOK(snapshot.volumes, pq, 3) {
		opts.Trace.replaceDecline("global-bounded-scan:budget")
		return 0, false, nil
	}
	total := 0
	for volumeIndex, vol := range snapshot.volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-bounded-scan:missing-volume")
			return 0, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return 0, true, err
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		hidden := hiddenBaseIDs{}
		if volumeIndex < len(snapshot.overlays) && snapshot.overlays[volumeIndex] != nil {
			snap := snapshot.overlays[volumeIndex]
			hidden = hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
		}
		if term, ok := globalDirTermCountShape(volumePQ); ok {
			if count, ok := vol.countDirTermLive(term, volumePQ.MatchPath, hidden); ok {
				total += count
				continue
			}
		}
		if term, ok := globalFileTermCountShape(volumePQ); ok {
			if count, ok := vol.countFileTermLive(term, volumePQ.MatchPath, hidden); ok {
				total += count
				continue
			}
		}
		cache := make(map[int]string)
		for id := 0; id < vol.index.compactRecordCount(); id++ {
			if id&1023 == 0 && queryCanceled(pq) {
				return 0, true, errQueryCanceled
			}
			if !hidden.empty() && hidden.contains(id) {
				continue
			}
			if _, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id, cache, true, false); ok {
				total++
			}
		}
	}
	total += globalOverlayMatchCount(snapshot.volumes, snapshot.overlays, pq)
	opts.Trace.setFallback("global-bounded-scan")
	opts.Trace.setPlannerMode("global-bounded-scan")
	opts.Trace.setSource("global:bounded-scan", total)
	opts.Trace.setComplete(true)
	return total, true, nil
}

func countServiceVolumesGlobalOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	globalEnabled := globalPlannerEnabled()
	extOnlySupported := globalExtOnlySupported(pq)
	terms := nonVolumeTerms(pq.Terms)
	if !globalEnabled && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) {
		return 0, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-count:overlay-snapshot-missing")
		return 0, false, nil
	}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-count:missing-volume")
			return 0, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return 0, true, err
		}
	}
	if extOnlySupported {
		extFilters, _ := globalExtPostingFilters(pq)
		extFilter := extFilters[0]
		baseCount := 0
		if globalSnapshotsHaveHidden(snapshots) {
			ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
			if !ok {
				return 0, false, nil
			}
			baseCount = len(filterGlobalIDsHidden(ids, snapshots))
		} else {
			for _, vol := range volumes {
				count, ok := vol.countExtPostingWithRecent(extFilter.ext, pq)
				if !ok {
					opts.Trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
					return 0, false, nil
				}
				baseCount += count
			}
		}
		overlayCount := globalOverlayMatchCount(volumes, snapshots, pq)
		opts.Trace.setPlannerMode("global-count-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: baseCount,
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, baseCount)
		opts.Trace.setComplete(true)
		return baseCount + overlayCount, true, nil
	}
	if !globalEnabled && !globalComponentDefaultSupported(pq, terms) {
		return 0, false, nil
	}
	if !globalComponentQuerySupportedMulti(pq, terms, len(volumes) > 1) {
		opts.Trace.replaceDecline("global-count:unsupported-query")
		return 0, false, nil
	}
	if term, ok := globalExactPathComponentTerm(pq); ok {
		baseCount := 0
		for volumeIndex, vol := range volumes {
			if coverage, fastOK := vol.mappedComponentSubstringCoverageForTop(term); fastOK {
				var hidden func(int) bool
				if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
					h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
					hidden = h.contains
				}
				intervalCount, intervalVerified := coverage.countLive(vol, hidden)
				gramSelfCount, gramVisited, gramExact, gramOK := vol.countMappedComponentSelfNameGramHits(term, coverage, hidden, pq)
				if gramOK {
					baseCount += intervalCount + gramSelfCount
					driver := "mapped-pngc-self-gram"
					if gramExact {
						driver = "mapped-pngr-exact-zero"
					}
					opts.Trace.addComponentStats(driver, coverage.rootCount, len(coverage.intervals), coverage.cardinality+gramSelfCount, gramSelfCount, intervalVerified+gramVisited, false)
					continue
				}
				selfCount, selfVisited, scanOK := vol.countMappedComponentSelfHits(term, coverage, hidden, pq)
				if scanOK {
					baseCount += intervalCount + selfCount
					opts.Trace.addComponentStats("mapped-lowr-count", coverage.rootCount, len(coverage.intervals), coverage.cardinality+selfCount, selfCount, intervalVerified+selfVisited, false)
					continue
				}
			}
			coverage, fastOK := vol.mappedComponentCoverageForQuery(term, pq)
			if !fastOK {
				coverage, fastOK = vol.mappedComponentSubstringCoverage(term)
			}
			if fastOK {
				var hidden func(int) bool
				if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
					h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
					hidden = h.contains
				}
				count, verified := coverage.countLive(vol, hidden)
				baseCount += count
				opts.Trace.addComponentStats("interval-count", coverage.rootCount, len(coverage.intervals), coverage.cardinality, len(coverage.selfIDs), verified, false)
				continue
			}
			var hidden func(int) bool
			if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
				h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
				hidden = h.contains
			}
			// Count the exact path component without materializing the full
			// posting slice.  This is exact for a bare term and avoids building
			// a huge []int that the verified iterator would only walk once.
			if strings.ContainsAny(term, `\/*?[]:`) {
				it := newGlobalRecordIterator(volumeIndex, vol.pathTermPosting(term))
				count, verified, err := countGlobalVerifiedIterator(&it, volumes, snapshots, pq)
				if err != nil {
					return 0, true, err
				}
				baseCount += count
				if opts.Trace != nil {
					opts.Trace.ComponentRecordsVerified += verified
				}
				continue
			}
			count := vol.countPathTermPostingLive(term, hidden)
			if queryCanceled(pq) {
				return 0, true, errQueryCanceled
			}
			baseCount += count
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += count
			}
		}
		total := baseCount + globalOverlayMatchCount(volumes, snapshots, pq)
		opts.Trace.setPlannerMode("global-count-components")
		addGlobalComponentTraceTerms(opts.Trace, pq, baseCount)
		opts.Trace.setSource("global:components", baseCount)
		opts.Trace.setComplete(true)
		return total, true, nil
	}
	if len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 {
		if count, ok := globalSimplePathORCount(volumes, pq); ok && !globalSnapshotsHaveHidden(snapshots) && !globalSnapshotsHaveOverlayRecords(snapshots) {
			opts.Trace.setPlannerMode("global-count-components")
			opts.Trace.setSource("global:boolean-persisted-count", count)
			opts.Trace.setComplete(true)
			return count, true, nil
		}
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-count-components:boolean-missing-source")
			}
			return 0, false, nil
		}
		baseCount, verified, err := countGlobalVerifiedIterator(componentIt, volumes, snapshots, pq)
		if err != nil {
			return 0, true, err
		}
		baseCount += globalOverlayMatchCount(volumes, snapshots, pq)
		if opts.Trace != nil {
			opts.Trace.ComponentRecordsVerified += verified
			opts.Trace.setPlannerMode("global-count-components")
			opts.Trace.setSource("global:boolean-iterator", baseCount)
			opts.Trace.setComplete(true)
		}
		return baseCount, true, nil
	}
	componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
	if !ok {
		if opts.Trace == nil || opts.Trace.Decline == "" {
			opts.Trace.replaceDecline("global-count-components:missing-source")
		}
		return 0, false, nil
	}
	var ids []globalRecordID
	canceled := false
	if globalSnapshotsHaveHidden(snapshots) {
		hidden := newGlobalHiddenIterator(componentIt, snapshots)
		ids, canceled = collectGlobalIteratorCancelable(&hidden, 0, func() bool { return queryCanceled(pq) })
	} else {
		ids, canceled = collectGlobalIteratorCancelable(componentIt, 0, func() bool { return queryCanceled(pq) })
	}
	if canceled {
		return 0, true, errQueryCanceled
	}
	ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	baseCount, err := countVerifiedGlobalIDs(volumes, ids, pq)
	if err != nil {
		return 0, true, err
	}
	overlayCount := globalOverlayMatchCount(volumes, snapshots, pq)
	opts.Trace.setPlannerMode("global-count-components")
	addGlobalComponentTraceTerms(opts.Trace, pq, len(ids))
	opts.Trace.setSource("global:components", len(ids))
	opts.Trace.setComplete(true)
	return baseCount + overlayCount, true, nil
}

func addGlobalComponentTraceTerms(trace *searchTrace, pq parsedQuery, countHint int) {
	if trace == nil {
		return
	}
	for _, term := range nonVolumeTerms(pq.Terms) {
		trace.addTerm(traceTerm{Term: term, Kind: "path-substring", Source: "global:component-subtree", CountHint: countHint, Exact: false})
	}
	for _, dir := range pq.Dirs {
		trace.addTerm(traceTerm{Term: dir, Kind: "directory-component", Source: "global:dir", CountHint: countHint, Exact: false})
	}
	if globalRegexLiteralSupported(pq) {
		trace.addTerm(traceTerm{Term: pq.RegexTerms[0], Kind: "regex-literal", Source: "global:regex-literal", CountHint: countHint, Exact: false})
	}
	for _, parent := range pq.Parents {
		trace.addTerm(traceTerm{Term: parent, Kind: "parent", Source: "global:parent", CountHint: countHint, Exact: true})
	}
	if pq.Under != "" {
		trace.addTerm(traceTerm{Term: pq.Under, Kind: "under", Source: "global:under", CountHint: countHint, Exact: true})
	}
	if pq.Type != "" {
		trace.addTerm(traceTerm{Term: pq.Type, Kind: "type", Source: "global:type", CountHint: countHint, Exact: true})
	}
	for _, mask := range pq.AttrFilters {
		trace.addTerm(traceTerm{Term: attribMaskString(mask), Kind: "attribute", Source: "global:attribute", CountHint: countHint, Exact: true})
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		trace.addTerm(traceTerm{Term: extFilter.ext, Kind: "extension", Source: "global:" + extFilter.source, CountHint: countHint, Exact: true})
	}
}

func globalComponentQueryIDs(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	var ids []globalRecordID
	haveIDs := false
	var underRoots []globalRecordID
	underPending := false
	if pq.Under != "" {
		var ok bool
		underRoots, ok = globalUnderRoots(volumes, pq.Under)
		if !ok {
			return nil, false
		}
		underPending = true
	}
	intersect := func(next []globalRecordID) {
		if underPending {
			ids = filterGlobalIDsBySubtrees(volumes, underRoots, next)
			underPending = false
			haveIDs = true
			return
		}
		if !haveIDs {
			ids = next
			haveIDs = true
			return
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(next)
		ids = intersectGlobalIterators(&left, &right, 0)
	}
	materializeUnder := func() bool {
		if !underPending {
			return true
		}
		var ok bool
		ids, ok = globalSubtreeIDs(volumes, underRoots, 0)
		underPending = false
		haveIDs = ok
		return ok
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		var ok bool
		termIDs, ok := globalComponentPathIDs(volumes, terms)
		if !ok {
			return nil, false
		}
		intersect(termIDs)
	}
	for _, group := range pq.OrGroups {
		var groupIDs []globalRecordID
		for altIndex, alt := range group {
			altIDs, ok := globalComponentSubqueryIDs(volumes, alt, trace)
			if !ok {
				return nil, false
			}
			if altIndex == 0 {
				groupIDs = altIDs
				continue
			}
			left := newGlobalIDSliceIterator(groupIDs)
			right := newGlobalIDSliceIterator(altIDs)
			groupIDs = unionGlobalIterators(&left, &right, 0)
		}
		intersect(groupIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.NotGroups) > 0 && !materializeUnder() {
		return nil, false
	}
	for _, neg := range pq.NotGroups {
		negIDs, ok := globalComponentSubqueryIDs(volumes, neg, trace)
		if !ok {
			return nil, false
		}
		if haveIDs {
			left := newGlobalIDSliceIterator(ids)
			right := newGlobalIDSliceIterator(negIDs)
			ids = excludeGlobalIterator(&left, &right, 0)
		}
		if len(ids) == 0 {
			break
		}
	}
	for _, dir := range pq.Dirs {
		dirIDs, ok := globalPathTermIDs(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(dirIDs)
		if len(ids) == 0 {
			break
		}
	}
	if globalRegexLiteralSupported(pq) {
		regexIDs, ok := globalRegexLiteralIDs(volumes, pq)
		if !ok {
			return nil, false
		}
		intersect(regexIDs)
	}
	for _, parent := range pq.Parents {
		parentIDs, ok := globalParentIDs(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(parentIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.AttrFilters) > 0 {
		attrIDs, ok := globalAttrIDs(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(attrIDs)
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		extIDs, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, trace)
		if !ok {
			return nil, false
		}
		intersect(extIDs)
		if len(ids) == 0 {
			break
		}
	}
	if !materializeUnder() || !haveIDs {
		return nil, false
	}
	return ids, true
}

func globalComponentSubqueryIDs(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	var ids []globalRecordID
	haveIDs := false
	intersect := func(next []globalRecordID) {
		if !haveIDs {
			ids = next
			haveIDs = true
			return
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(next)
		ids = intersectGlobalIterators(&left, &right, 0)
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		termIDs, ok := globalComponentPathIDs(volumes, terms)
		if !ok {
			return nil, false
		}
		intersect(termIDs)
	}
	for _, dir := range pq.Dirs {
		dirIDs, ok := globalPathTermIDs(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(dirIDs)
		if len(ids) == 0 {
			break
		}
	}
	if globalRegexLiteralSupported(pq) {
		regexIDs, ok := globalRegexLiteralIDs(volumes, pq)
		if !ok {
			return nil, false
		}
		intersect(regexIDs)
	}
	for _, parent := range pq.Parents {
		parentIDs, ok := globalParentIDs(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(parentIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.AttrFilters) > 0 {
		attrIDs, ok := globalAttrIDs(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(attrIDs)
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		extIDs, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, trace)
		if !ok {
			return nil, false
		}
		intersect(extIDs)
		if len(ids) == 0 {
			break
		}
	}
	if pq.Type != "" {
		if !haveIDs {
			return nil, false
		}
		ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	}
	if !haveIDs {
		return nil, false
	}
	return ids, true
}

func globalUnderIDs(volumes []*serviceVolumeIndex, under string) ([]globalRecordID, bool) {
	roots, ok := globalUnderRoots(volumes, under)
	if !ok || len(roots) == 0 {
		return roots, ok
	}
	return globalSubtreeIDs(volumes, roots, 0)
}

func globalUnderRoots(volumes []*serviceVolumeIndex, under string) ([]globalRecordID, bool) {
	if under == "" {
		return nil, false
	}
	under = filepath.Clean(under)
	underVolume := strings.ToUpper(filepath.VolumeName(under))
	roots := make([]globalRecordID, 0, 1)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		if underVolume != "" && vol.index.Volume != "" && !strings.EqualFold(vol.index.Volume, underVolume) {
			continue
		}
		for _, rootID := range vol.underRootIDs(under) {
			roots = append(roots, globalRecordID{volume: volumeIndex, local: rootID})
		}
	}
	if len(roots) == 0 {
		return []globalRecordID{}, true
	}
	return roots, true
}

func filterGlobalIDsBySubtrees(volumes []*serviceVolumeIndex, roots, ids []globalRecordID) []globalRecordID {
	base := newGlobalIDSliceIterator(ids)
	filtered := newGlobalSubtreeFilterIterator(&base, volumes, roots)
	return collectGlobalIterator(&filtered, 0)
}

func filterGlobalIDsByType(volumes []*serviceVolumeIndex, ids []globalRecordID, typ string) []globalRecordID {
	if typ == "" {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		isDir := vol.index.compactRecord(id.local).Mode&uint32(os.ModeDir) != 0
		if (typ == "file" && !isDir) || (typ == "dir" && isDir) {
			out = append(out, id)
		}
	}
	return out
}

func globalComponentPathIDs(volumes []*serviceVolumeIndex, terms []string) ([]globalRecordID, bool) {
	probes := pathPlanProbeTerms(terms)
	if len(probes) == 0 {
		return nil, true
	}
	ids, ok := globalPathTermIDs(volumes, probes[0])
	if !ok {
		return nil, false
	}
	for _, term := range probes[1:] {
		base := newGlobalIDSliceIterator(ids)
		filtered := newGlobalPathTermFilterIterator(&base, volumes, term)
		ids = collectGlobalIterator(&filtered, 0)
		if len(ids) == 0 {
			break
		}
	}
	return ids, true
}

func globalPathTermIDs(volumes []*serviceVolumeIndex, term string) ([]globalRecordID, bool) {
	if term == "" {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(term) {
			return nil, false
		}
		for _, id := range vol.pathPlanTermPosting(term) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalRegexLiteralIDs(volumes []*serviceVolumeIndex, pq parsedQuery) ([]globalRecordID, bool) {
	if !globalRegexLiteralSupported(pq) {
		return nil, false
	}
	return globalPathTermIDs(volumes, pq.RegexTerms[0])
}

func globalParentIDs(volumes []*serviceVolumeIndex, parent string) ([]globalRecordID, bool) {
	if parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		for _, id := range vol.parentIDs(parent) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalAttrIDs(volumes []*serviceVolumeIndex, filters []uint32) ([]globalRecordID, bool) {
	if len(filters) == 0 {
		return nil, false
	}
	var ids []globalRecordID
	haveIDs := false
	for _, mask := range filters {
		maskIDs := make([]globalRecordID, 0)
		for volumeIndex, vol := range volumes {
			if vol == nil || vol.index == nil {
				return nil, false
			}
			localIDs, ok := vol.attrIDsForMask(mask)
			if !ok {
				return nil, false
			}
			for _, id := range localIDs {
				maskIDs = append(maskIDs, globalRecordID{volume: volumeIndex, local: id})
			}
		}
		sortGlobalRecordIDs(maskIDs)
		if !haveIDs {
			ids = maskIDs
			haveIDs = true
			continue
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(maskIDs)
		ids = intersectGlobalIterators(&left, &right, 0)
		if len(ids) == 0 {
			break
		}
	}
	if !haveIDs {
		return nil, false
	}
	return ids, true
}

func entriesFromGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) ([]Entry, error) {
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, err
	}
	return globalRankedEntriesToEntries(ranked), nil
}

func countVerifiedGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) (int, error) {
	pathCaches := make([]map[int]string, len(volumes))
	count := 0
	for _, id := range ids {
		if queryCanceled(pq) {
			return 0, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		if _, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, false); ok {
			count++
		}
	}
	return count, nil
}

func rankedEntriesFromGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) ([]globalRankedEntry, error) {
	pathCaches := make([]map[int]string, len(volumes))
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	results := make([]globalRankedEntry, 0, len(ids))
	for _, id := range ids {
		if queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		entry, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, compactCandidateCanSkipEntryMatches(volumePQ, true))
		if ok {
			rank := int(^uint(0) >> 1)
			if rankers[id.volume] != nil {
				rank = rankers[id.volume](id.local)
			}
			results = append(results, globalRankedEntry{entry: entry, rank: rank, volume: id.volume, tie: entry.Path})
		}
	}
	return results, nil
}

func globalRankedEntriesToEntries(ranked []globalRankedEntry) []Entry {
	out := make([]Entry, len(ranked))
	for i, item := range ranked {
		out[i] = item.entry
	}
	return out
}

func mergeGlobalOverlayEntries(volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, base []globalRankedEntry, pq parsedQuery, limit int) []globalRankedEntry {
	if len(snapshots) == 0 {
		sortGlobalRankedEntries(base, pq)
		if limit > 0 && len(base) > limit {
			return base[:limit]
		}
		return base
	}
	out := append([]globalRankedEntry(nil), base...)
	for volumeIndex, snap := range snapshots {
		if snap == nil || volumeIndex < 0 || volumeIndex >= len(volumes) {
			continue
		}
		vol := volumes[volumeIndex]
		if vol == nil {
			continue
		}
		pathCache := make(map[int]string)
		for _, overlay := range vol.overlayRankedMatches(snap, pq, pathCache) {
			out = append(out, globalRankedEntry{entry: overlay.entry, rank: overlay.rank, volume: volumeIndex, tie: overlay.entry.Path, overlay: true})
		}
	}
	sortGlobalRankedEntries(out, pq)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortGlobalRankedEntries(entries []globalRankedEntry, pq parsedQuery) {
	global := globalRankedEntriesSpanMultipleVolumes(entries)
	for _, entry := range entries {
		if entry.overlay {
			global = true
			break
		}
	}
	slices.SortStableFunc(entries, func(a, b globalRankedEntry) int {
		if global {
			// Once multiple volumes participate, persisted ranks are local to a
			// volume.  Compare actual entries before rank/tie fallbacks so an
			// overlay or equal local rank cannot change global deterministic order.
			if n := compareSearchAllEntries(a.entry, b.entry, pq); n != 0 {
				return n
			}
		}
		if n := cmp.Compare(a.rank, b.rank); n != 0 {
			return n
		}
		if n := cmp.Compare(a.volume, b.volume); n != 0 {
			return n
		}
		return strings.Compare(a.tie, b.tie)
	})
}

func globalRankedEntriesSpanMultipleVolumes(entries []globalRankedEntry) bool {
	first := -1
	for _, entry := range entries {
		if first < 0 {
			first = entry.volume
			continue
		}
		if entry.volume != first {
			return true
		}
	}
	return false
}

func globalOverlayMatchCount(volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, pq parsedQuery) int {
	total := 0
	for i, snap := range snapshots {
		if snap == nil || i < 0 || i >= len(volumes) || volumes[i] == nil {
			continue
		}
		total += volumes[i].overlayLiveMatchCount(snap, pq)
	}
	return total
}

func globalOverlaySnapshots(volumes []*serviceVolumeIndex) ([]*volumeSnapshot, bool) {
	snapshots := make([]*volumeSnapshot, len(volumes))
	for i, vol := range volumes {
		if vol == nil || !vol.hasActiveOverlay() {
			continue
		}
		snap := vol.snap.Load()
		if snap == nil {
			return nil, false
		}
		snapshots[i] = snap
	}
	return snapshots, true
}

func globalSnapshotsHaveHidden(snapshots []*volumeSnapshot) bool {
	for _, snap := range snapshots {
		if snap != nil && (len(snap.tombstoneIDs) > 0 || len(snap.shadowedIDs) > 0) {
			return true
		}
	}
	return false
}

func globalSnapshotsHaveOverlayRecords(snapshots []*volumeSnapshot) bool {
	for _, snap := range snapshots {
		if snap != nil && len(snap.records) > 0 {
			return true
		}
	}
	return false
}

func globalVolumesHaveRankForQuery(volumes []*serviceVolumeIndex, pq parsedQuery) bool {
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			return false
		}
		recordCount := vol.index.compactRecordCount()
		if len(vol.rankForQuery(pq)) >= recordCount {
			continue
		}
		if pq.SortColumn == "" && len(vol.index.CompactNameOrder) >= recordCount {
			continue
		}
		return false
	}
	return true
}

func globalHiddenContains(snapshots []*volumeSnapshot, id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(snapshots) || snapshots[id.volume] == nil {
		return false
	}
	hidden := hiddenBaseIDs{tombstone: snapshots[id.volume].tombstoneIDs, shadowed: snapshots[id.volume].shadowedIDs}
	return hidden.contains(id.local)
}

func filterGlobalIDsHidden(ids []globalRecordID, snapshots []*volumeSnapshot) []globalRecordID {
	if !globalSnapshotsHaveHidden(snapshots) {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if !globalHiddenContains(snapshots, id) {
			out = append(out, id)
		}
	}
	return out
}

func globalRankerForVolumes(volumes []*serviceVolumeIndex, pq parsedQuery) func(globalRecordID) int {
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	return func(id globalRecordID) int {
		if id.volume < 0 || id.volume >= len(rankers) || rankers[id.volume] == nil {
			return int(^uint(0) >> 1)
		}
		return rankers[id.volume](id.local)
	}
}

func (it *globalIDSliceIterator) CountHint() int {
	if it == nil || it.pos >= len(it.ids) {
		return 0
	}
	return len(it.ids) - it.pos
}

func (it *globalIDSliceIterator) Next() (globalRecordID, bool) {
	if it == nil || it.pos >= len(it.ids) {
		return globalRecordID{}, false
	}
	id := it.ids[it.pos]
	it.pos++
	return id, true
}

func (it *globalIDSliceIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	for it.pos < len(it.ids) && compareGlobalRecordID(it.ids[it.pos], target) < 0 {
		it.pos++
	}
	return it.Next()
}

func (it *globalHiddenIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalHiddenIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if !globalHiddenContains(it.snapshots, id) {
			return id, true
		}
	}
}

func (it *globalHiddenIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	for ok && globalHiddenContains(it.snapshots, id) {
		id, ok = it.base.Next()
	}
	return id, ok
}

func (it *globalSubtreeFilterIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalSubtreeFilterIterator) contains(id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(it.volumes) {
		return false
	}
	vol := it.volumes[id.volume]
	if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
		return false
	}
	for _, root := range it.roots[id.volume] {
		if vol.isDescendantOrSelf(id.local, root) {
			return true
		}
	}
	return false
}

func (it *globalSubtreeFilterIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if it.contains(id) {
			return id, true
		}
	}
}

func (it *globalSubtreeFilterIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	if ok && it.contains(id) {
		return id, true
	}
	return it.Next()
}

func (it *globalPathTermFilterIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalPathTermFilterIterator) contains(id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(it.volumes) {
		return false
	}
	vol := it.volumes[id.volume]
	if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
		return false
	}
	if set := it.nameIDs[id.volume]; len(set) > 0 {
		if _, ok := set[id.local]; ok {
			return true
		}
	}
	if it.fast[id.volume] {
		// PCMP represents exact directory components.  For a filtered
		// candidate, walk only its parent chain instead of copying the full
		// root posting (Users can have millions of descendants).
		if strings.Contains(vol.index.compactLowerNameAt(id.local), it.term) {
			return true
		}
		for current := id.local; current >= 0 && current < vol.index.compactRecordCount(); {
			if current != id.local && strings.EqualFold(vol.index.compactLowerNameAt(current), it.term) {
				return true
			}
			rec := vol.index.compactRecord(current)
			if rec.Parent < 0 || int(rec.Parent) == current {
				break
			}
			current = int(rec.Parent)
		}
		return false
	}
	if roots := it.roots[id.volume]; len(roots) > 0 {
		if id.local < len(vol.subtreeStart) {
			pos := vol.subtreeStart[id.local]
			if pos == ^uint32(0) {
				return false
			}
			idx := sort.Search(len(roots), func(i int) bool { return roots[i].start > pos }) - 1
			if idx >= 0 && pos < roots[idx].end {
				return true
			}
		}
		return false
	}
	return vol.index.compactPathContainsTerm(id.local, it.term)
}

func (it *globalPathTermFilterIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if it.contains(id) {
			return id, true
		}
	}
}

func (it *globalPathTermFilterIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	if ok && it.contains(id) {
		return id, true
	}
	return it.Next()
}
