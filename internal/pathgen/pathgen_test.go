package pathgen

import (
	"errors"
	"regexp"
	"testing"
)

func TestSampleProducesAPathThePatternMatches(t *testing.T) {
	patterns := []string{
		"/",
		"/Header",
		"/[A-Z]{3}",
		"/api/v[0-9]+",
		"/svc/(.*)",
		"/items/[0-9]{2,5}/detail",
		"/(foo|bar)/baz",
		"/files/.+\\.(png|jpg)",
		"/users/[^/]+/profile",
		"/a?b*c+",
		"/x{0,2}y",
		"/long/[a-z]{12}",
		"^/anchored$",
		"/(?i)mixed",
		"/api.v1",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			got, err := Sample(pattern)
			if err != nil {
				t.Fatalf("Sample: %v", err)
			}
			// Checked independently of Sample's own verification.
			if !regexp.MustCompile("^(?:" + pattern + ")").MatchString(got) {
				t.Errorf("Sample = %q, which the pattern does not match", got)
			}
			if got[0] != '/' {
				t.Errorf("Sample = %q, want a path starting with a slash", got)
			}
		})
	}
}

func TestSampleIsDeterministic(t *testing.T) {
	a, _ := Sample("/api/v[0-9]+/(users|orders)")
	b, _ := Sample("/api/v[0-9]+/(users|orders)")
	if a != b || a == "" {
		t.Errorf("Sample returned %q then %q; probe IDs depend on it being stable", a, b)
	}
}

func TestSampleRefusesWhatItCannotMatch(t *testing.T) {
	patterns := map[string]string{
		"PCRE lookahead":             "/foo(?=bar)",
		"PCRE backreference":         `/(a)\1`,
		"unbalanced":                 "/foo(",
		"does not start with slash":  "foo/bar",
		"forces whitespace":          "/a b",
		"empty class":                "/[^\\x00-\\x{10FFFF}]",
		"possessive quantifier PCRE": "/a++b",
	}
	for name, pattern := range patterns {
		t.Run(name, func(t *testing.T) {
			got, err := Sample(pattern)
			if err == nil {
				t.Fatalf("Sample(%q) = %q, want an error", pattern, got)
			}
			if !errors.Is(err, ErrUnsampleable) {
				t.Errorf("error %v is not ErrUnsampleable", err)
			}
		})
	}
}

func TestMutations(t *testing.T) {
	type result struct {
		out string
		ok  bool
	}
	wrap := func(s string) result { return result{s, true} }
	pair := func(s string, ok bool) result { return result{s, ok} }

	tests := []struct {
		name string
		got  result
		want result
	}{
		{"case flip", pair(CaseFlip("/Api/v1")), result{"/aPI/V1", true}},
		{"case flip leaves the query alone", pair(CaseFlip("/a?Key=Value")), result{"/A?Key=Value", true}},
		{"case flip with no letters", pair(CaseFlip("/123/")), result{"/123/", false}},

		{"extend", wrap(Extend("/api", "rp-extra")), result{"/api/rp-extra", true}},
		{"extend after a slash", wrap(Extend("/api/", "rp-extra")), result{"/api/rp-extra", true}},
		{"extend the root", wrap(Extend("/", "rp-extra")), result{"/rp-extra", true}},
		{"extend keeps the query", wrap(Extend("/api?x=1", "rp-extra")), result{"/api/rp-extra?x=1", true}},

		{"append", pair(Append("/foo", "rp")), result{"/foorp", true}},
		{"append keeps the query", pair(Append("/foo?x=1", "rp")), result{"/foorp?x=1", true}},
		{"append after a slash", pair(Append("/foo/", "rp")), result{"", false}},

		{"strip slash", pair(StripSlash("/app/")), result{"/app", true}},
		{"strip slash keeps the query", pair(StripSlash("/app/?x=1")), result{"/app?x=1", true}},
		{"strip slash on the root", pair(StripSlash("/")), result{"", false}},
		{"strip slash with none", pair(StripSlash("/app")), result{"", false}},

		{"add slash", pair(AddSlash("/app")), result{"/app/", true}},
		{"add slash keeps the query", pair(AddSlash("/app?x=1")), result{"/app/?x=1", true}},
		{"add slash when present", pair(AddSlash("/app/")), result{"", false}},

		{"dot segment", wrap(DotSegment("/uuid")), result{"/rp-x/../uuid", true}},
		{"current dir", wrap(CurrentDir("/uuid")), result{"/./uuid", true}},
		{"double slash", wrap(DoubleSlash("/uuid")), result{"//uuid", true}},

		{"encode first letter", pair(EncodeFirstLetter("/uuid")), result{"/%75uid", true}},
		{"encode skips non-letters", pair(EncodeFirstLetter("/1/Zed?a=b")), result{"/1/%5Aed?a=b", true}},
		{"encode with no letters", pair(EncodeFirstLetter("/123")), result{"", false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, %v; want %q, %v", tt.got.out, tt.got.ok, tt.want.out, tt.want.ok)
			}
		})
	}
}
