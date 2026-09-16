package main

import (
	"sort"
	"strings"
)

type candidatePlan struct {
	vol               *serviceVolumeIndex
	pq                parsedQuery
	sources           []candidatePlanSource
	empty             bool
	underPathFallback string
}

type candidatePlanSource struct {
	name       string
	ids        []int
	posting    postingCountCandidate
	hasPosting bool
	union      []candidatePlanSource
	vol        *serviceVolumeIndex
	roots      []int
}

func traceTermForCandidateSource(source candidatePlanSource, volume string) traceTerm {
	term := traceTerm{
		Source:    source.name,
		CountHint: source.len(),
		Exact:     true,
		Volume:    volume,
	}
	switch {
	case strings.HasPrefix(source.name, "ext:"):
		term.Kind = "extension"
		term.Term = strings.TrimPrefix(source.name, "ext:")
	case strings.HasPrefix(source.name, "glob-ext:"):
		term.Kind = "glob-extension"
		term.Term = strings.TrimPrefix(source.name, "glob-ext:")
	case strings.HasPrefix(source.name, "parent:"):
		term.Kind = "parent"
		term.Term = strings.TrimPrefix(source.name, "parent:")
	case strings.HasPrefix(source.name, "attrib:"):
		term.Kind = "attribute"
		term.Term = strings.TrimPrefix(source.name, "attrib:")
	case strings.HasPrefix(source.name, "dir:"):
		term.Kind = "directory-component"
		term.Term = strings.TrimPrefix(source.name, "dir:")
	case strings.HasPrefix(source.name, "path-term:"):
		term.Kind = "path-substring"
		term.Term = strings.TrimPrefix(source.name, "path-term:")
		term.Exact = false
	case strings.HasPrefix(source.name, "term:"):
		term.Kind = "name-substring"
		term.Term = strings.TrimPrefix(source.name, "term:")
		term.Exact = false
	case source.name == "type:dir":
		term.Kind = "type"
		term.Term = "dir"
	case source.name == "under":
		term.Kind = "under"
		term.Term = "under"
	case strings.HasPrefix(source.name, "or-group"):
		term.Kind = "or"
		term.Term = "or-group"
	default:
		term.Kind = "source"
		term.Term = source.name
	}
	return term
}

func (source candidatePlanSource) len() int {
	switch {
	case source.hasPosting:
		return source.posting.len()
	case len(source.union) > 0:
		total := 0
		for _, part := range source.union {
			total += part.len()
		}
		return total
	case len(source.roots) > 0:
		if source.vol != nil {
			if estimate := source.vol.estimateUnderDescendantCount(source.roots); estimate >= 0 {
				return estimate
			}
		}
		return len(source.roots)
	default:
		return len(source.ids)
	}
}

func (source candidatePlanSource) materialize() []int {
	switch {
	case source.hasPosting:
		return uint32sToInts(source.posting.materialize())
	case len(source.union) > 0:
		total := 0
		for _, part := range source.union {
			total += part.len()
		}
		out := make([]int, 0, total)
		for _, part := range source.union {
			out = append(out, part.materialize()...)
		}
		sort.Ints(out)
		return uniqueSortedInts(out)
	case len(source.roots) > 0:
		seen := make(map[int]struct{}, 256)
		out := make([]int, 0, source.len())
		if source.vol == nil {
			return nil
		}
		for _, rootID := range source.roots {
			for _, id := range source.vol.underDescendants(rootID) {
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
		sort.Ints(out)
		return out
	default:
		return append([]int(nil), source.ids...)
	}
}

func (source candidatePlanSource) intersect(out []int) []int {
	if len(out) == 0 {
		return out
	}
	if source.hasPosting {
		if source.posting.mapped {
			return intersectSortedIntsWithPostingIterator(out, source.posting.it)
		}
		return intersectSortedIntsWithUint32s(out, source.posting.ids)
	}
	return intersectSortedInts(out, source.materialize())
}

func intersectSortedIntsWithUint32s(a []int, b []uint32) []int {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		av := a[i]
		bv := int(b[j])
		switch {
		case av == bv:
			out = append(out, av)
			i++
			j++
		case av < bv:
			i++
		default:
			j++
		}
	}
	return out
}

func intersectSortedIntsWithPostingIterator(a []int, it postingBlockIterator) []int {
	out := a[:0]
	cursor := 0
	for cursor < len(a) && it.next < it.end {
		block, meta, ok := it.nextBlock()
		if !ok {
			return nil
		}
		if len(block) == 0 {
			continue
		}
		for cursor < len(a) && a[cursor] < int(meta.minID) {
			cursor++
		}
		if cursor >= len(a) {
			break
		}
		if a[cursor] > int(meta.maxID) {
			continue
		}
		j := 0
		for cursor < len(a) && j < len(block) {
			av := a[cursor]
			if av > int(meta.maxID) {
				break
			}
			bv := int(block[j])
			switch {
			case av == bv:
				out = append(out, av)
				cursor++
				j++
			case av < bv:
				cursor++
			default:
				j++
			}
		}
	}
	return out
}
