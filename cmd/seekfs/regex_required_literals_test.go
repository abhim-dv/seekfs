package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestRegexRequiredLiteralAlternativesCoverage(t *testing.T) {
	cases := []struct {
		pattern string
		want    [][]string
	}{
		{`README\.(md|txt)$`, [][]string{{"readme.md"}, {"readme.txt"}}},
		{`^.*\.(go|rs|py|js)$`, [][]string{{".go"}, {".rs"}, {".py"}, {".js"}}},
		{`Assets.*\.(dat|txt)$`, [][]string{{"assets", ".dat"}, {"assets", ".txt"}}},
		{`.*\.(tar\.gz|zip)$`, [][]string{{".tar.gz"}, {".zip"}}},
		{`[a-z]+_test\.go`, [][]string{{"_test.go"}}},
		{`(foo|bar)baz`, [][]string{{"foobaz"}, {"barbaz"}}},
		{`(foobar|foobaz)`, [][]string{{"foobar"}, {"foobaz"}}},
		{`.*filtered-volume-cleaned.*`, [][]string{{"filtered-volume-cleaned"}}},
		{`report-\d{4}\.pdf`, [][]string{{"report-", ".pdf"}}},
		// optional groups are dropped, so both runs must still be present.
		{`config(\.local)?\.json`, [][]string{{"config", ".json"}}},
		// declines: no run of length >= 3 is provable.
		{`a|b`, nil},
		{`ab`, nil},
		// declines: unrelaxable constructs.
		{`(a)\1`, nil},
		{`foo\Kbar`, nil},
		// declines: letter escapes are not the literal letter, and declining
		// them must not let their argument (\x41, {Greek}) become a literal.
		{`\x41bc`, nil},
		{`\Afoo`, nil},
		{`foo\z`, nil},
		{`\Qfoo.bar\E`, nil},
		{`\p{Greek}\.txt`, nil},
		// escaped punctuation is still a literal.
		{`report\-[0-9]+\.pdf`, [][]string{{"report-", ".pdf"}}},
		// a repeated atom (min>=1) breaks the run: literals on either side must
		// not be merged across it.
		{`ab+cd`, nil},
		{`ab?cd`, nil},
		{`abc+def`, [][]string{{"abc", "def"}}},
		{`foo+bar`, [][]string{{"foo", "bar"}}},
		// \0 consumes its octal digits; they must not become literals.
		{`\0123`, nil},
		{`.\0111`, nil},
		// non-ASCII patterns decline: the parser is byte-based while regex
		// quantifiers bind to runes.
		{`abcé?`, nil},
		{`café.*\.txt`, nil},
	}
	for _, tc := range cases {
		got := regexRequiredLiteralAlternatives(tc.pattern)
		if !sameDisjuncts(got, tc.want) {
			t.Errorf("regexRequiredLiteralAlternatives(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func sameDisjuncts(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// TestRegexRequiredLiteralAlternativesSound is the property that makes the
// filter safe: for any haystack the regex matches, at least one returned
// disjunct has all of its runs present. Implemented over a corpus of patterns
// and inputs, including inputs that match and inputs that do not.
func TestRegexRequiredLiteralAlternativesSound(t *testing.T) {
	patterns := []string{
		`README\.(md|txt)$`,
		`^.*\.(go|rs|py|js)$`,
		`Assets.*\.(dat|txt)$`,
		`.*\.(tar\.gz|zip)$`,
		`[a-z]+_test\.go`,
		`(foo|bar)baz`,
		`(foobar|foobaz)`,
		`report-\d{4}\.pdf`,
		`config(\.local)?\.json`,
		`DSC_\d+\.jpg`,
		`src/.*\.go$`,
		`a{2,3}bcdef`,
		`ab+cd`,
		`abc+def`,
		`x{2,3}bcdef`,
		`fo+bar`,
	}
	haystacks := []string{
		"c:\\users\\exampleuser\\downloads\\readme.md",
		"c:\\users\\exampleuser\\downloads\\readme.txt",
		"c:\\readme.rst",
		"c:\\repo\\src\\main.go",
		"c:\\repo\\main.rs",
		"c:\\repo\\app.py",
		"c:\\repo\\app.js",
		"c:\\repo\\app.ts",
		"assets\\image.dat",
		"assets\\image.txt",
		"backup\\archive.tar.gz",
		"backup\\archive.zip",
		"backup\\archive.7z",
		"pkg\\foo_test.go",
		"pkg\\bar_test.go",
		"x\\foobaz",
		"x\\barbaz",
		"x\\foobar",
		"report-2024.pdf",
		"report-99.pdf",
		"config.json",
		"config.local.json",
		"dsc_1234.jpg",
		"src\\nested\\deep\\file.go",
		"x ExchangePrincipal",
		"// ExchangePrincipal",
		"foobaz",
		"aaaaabcdef",
		"zzz",
		"abcd",
		"abbcd",
		"abbbcd",
		"abcdef",
		"abccdef",
		"xxbcdef",
		"xxxbcdef",
		"fobar",
		"foooobar",
	}
	for _, pattern := range patterns {
		alts := regexRequiredLiteralAlternatives(pattern)
		if alts == nil {
			continue
		}
		re := regexp.MustCompile("(?i)" + pattern)
		for _, hay := range haystacks {
			if !re.MatchString(hay) {
				continue
			}
			lower := strings.ToLower(hay)
			ok := false
			for _, disjunct := range alts {
				all := true
				for _, run := range disjunct {
					if !strings.Contains(lower, run) {
						all = false
						break
					}
				}
				if all {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("pattern %q matched %q but no disjunct was satisfied by %v", pattern, hay, alts)
			}
		}
	}
}
