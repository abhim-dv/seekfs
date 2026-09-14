package main

import "testing"

func TestAppendUniqueGramKeysMatchesMapVersion(t *testing.T) {
	inputs := []string{
		"", "a", "ab", "abc", "abcd", "abcde",
		"README.md", "File name with spaces.TXT", "a/b\\c\\d.txt",
		"aaaa", "abababab", "cafebabecafebabe", "UPPER_lower_MiXeD.go",
		"node_modules\\@scope\\pkg\\index.min.js",
	}
	for _, s := range inputs {
		for _, n := range []int{3, 4} {
			got := appendUniqueFixedGramKeys(make([]uint32, 0, 8), s, n)
			want := uniqueFixedGramKeys(s, n)
			if !sameUint32Seq(got, want) {
				t.Fatalf("appendUniqueFixedGramKeys(%q, %d) = %v, want %v", s, n, got, want)
			}
			gotF := appendUniqueFixedGramKeysFoldASCII(make([]uint32, 0, 8), s, n)
			wantF := uniqueFixedGramKeysFoldASCII(s, n)
			if !sameUint32Seq(gotF, wantF) {
				t.Fatalf("appendUniqueFixedGramKeysFoldASCII(%q, %d) = %v, want %v", s, n, gotF, wantF)
			}
		}
	}
}

func TestAppendUniqueGramKeysReusesScratch(t *testing.T) {
	scratch := make([]uint32, 0, 64)
	scratch = appendUniqueTrigramKeys(scratch, "first")
	firstCap := cap(scratch)
	scratch = appendUniqueTrigramKeys(scratch, "second-value")
	if cap(scratch) != firstCap {
		t.Fatalf("scratch grew from %d to %d; it is reallocating per call", firstCap, cap(scratch))
	}
	if len(appendUniqueTrigramKeys(scratch, "ab")) != 0 {
		t.Fatal("short input should produce no grams")
	}
}

func sameUint32Seq(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
