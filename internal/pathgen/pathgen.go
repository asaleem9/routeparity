// Package pathgen builds request paths: a path that a pattern matches, and
// variations of a path that probe one behaviour each. Everything here is a
// pure string function; nothing is parsed or cleaned as a URL, because the
// exact bytes are the point.
package pathgen

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
)

// ErrUnsampleable is returned when no matching path can be produced for a
// pattern, either because it uses syntax Go's regexp package does not support
// (ingress-nginx patterns are PCRE) or because the generated path fails to
// match on verification.
var ErrUnsampleable = errors.New("cannot generate a path matching the pattern")

// maxRepeat bounds how often a repeated element is expanded.
const maxRepeat = 3

// Sample returns a path that the pattern matches, taken as ingress-nginx
// takes it: a regular expression anchored at the start of the path.
//
// The result is verified against the pattern before it is returned, so a
// successful Sample is a real match and never a guess.
func Sample(pattern string) (string, error) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", fmt.Errorf("%w %q: %w", ErrUnsampleable, pattern, err)
	}
	var b strings.Builder
	if err := sample(&b, re.Simplify()); err != nil {
		return "", fmt.Errorf("%w %q: %w", ErrUnsampleable, pattern, err)
	}
	out := b.String()

	check, err := regexp.Compile(`^(?:` + pattern + `)`)
	if err != nil || !check.MatchString(out) {
		return "", fmt.Errorf("%w %q: generated %q does not match", ErrUnsampleable, pattern, out)
	}
	if !strings.HasPrefix(out, "/") || strings.ContainsAny(out, " \t\r\n") {
		return "", fmt.Errorf("%w %q: generated %q is not a usable request path", ErrUnsampleable, pattern, out)
	}
	return out, nil
}

func sample(b *strings.Builder, re *syntax.Regexp) error {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return nil
	case syntax.OpLiteral:
		b.WriteString(string(re.Rune))
		return nil
	case syntax.OpCharClass:
		r, ok := pickRune(re.Rune)
		if !ok {
			return errors.New("empty character class")
		}
		b.WriteRune(r)
		return nil
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		b.WriteByte('x')
		return nil
	case syntax.OpCapture:
		return sample(b, re.Sub[0])
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if err := sample(b, sub); err != nil {
				return err
			}
		}
		return nil
	case syntax.OpAlternate:
		return sample(b, re.Sub[0])
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest:
		// One repetition: enough to show the element is there, and it keeps
		// the path short.
		return sample(b, re.Sub[0])
	case syntax.OpRepeat:
		n := re.Min
		if n == 0 && re.Max != 0 {
			n = 1
		}
		if n > maxRepeat && (re.Max == -1 || re.Max > maxRepeat) && re.Min <= maxRepeat {
			n = maxRepeat
		}
		for i := 0; i < n; i++ {
			if err := sample(b, re.Sub[0]); err != nil {
				return err
			}
		}
		return nil
	case syntax.OpNoMatch:
		return errors.New("pattern matches nothing")
	}
	return fmt.Errorf("unsupported regular expression element %v", re.Op)
}

// pickRune chooses a member of a character class given as rune range pairs,
// preferring characters that are unremarkable in a URL path.
func pickRune(ranges []rune) (rune, bool) {
	if len(ranges) < 2 {
		return 0, false
	}
	in := func(r rune) bool {
		for i := 0; i+1 < len(ranges); i += 2 {
			if ranges[i] <= r && r <= ranges[i+1] {
				return true
			}
		}
		return false
	}
	for _, preferred := range "aA0-_.~" {
		if in(preferred) {
			return preferred, true
		}
	}
	for r := rune('!'); r <= '~'; r++ {
		if in(r) && !strings.ContainsRune(" /?#%", r) {
			return r, true
		}
	}
	for r := rune('!'); r <= '~'; r++ {
		if in(r) {
			return r, true
		}
	}
	return ranges[0], true
}

// splitQuery separates a request-target into path and query, keeping the "?"
// with the query so the two can be joined back unchanged.
func splitQuery(target string) (path, query string) {
	if i := strings.IndexByte(target, '?'); i >= 0 {
		return target[:i], target[i:]
	}
	return target, ""
}

// CaseFlip swaps the case of every letter in the path. It returns false when
// the path has no letters to flip.
func CaseFlip(target string) (string, bool) {
	path, query := splitQuery(target)
	changed := false
	flipped := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsUpper(r):
			changed = true
			return unicode.ToLower(r)
		case unicode.IsLower(r):
			changed = true
			return unicode.ToUpper(r)
		}
		return r
	}, path)
	return flipped + query, changed
}

// Extend appends a further path segment.
func Extend(target, segment string) string {
	path, query := splitQuery(target)
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return path + segment + query
}

// Append adds characters to the last path segment without a separator, so
// /foo becomes /foorp. It probes whether a match respects segment boundaries.
// It returns false for a path ending in a slash, where there is no segment to
// lengthen.
func Append(target, suffix string) (string, bool) {
	path, query := splitQuery(target)
	if strings.HasSuffix(path, "/") {
		return "", false
	}
	return path + suffix + query, true
}

// StripSlash removes one trailing slash. It returns false when the path is
// "/" or does not end in a slash.
func StripSlash(target string) (string, bool) {
	path, query := splitQuery(target)
	if path == "/" || !strings.HasSuffix(path, "/") {
		return "", false
	}
	return strings.TrimSuffix(path, "/") + query, true
}

// AddSlash appends a trailing slash. It returns false when the path already
// ends in one.
func AddSlash(target string) (string, bool) {
	path, query := splitQuery(target)
	if strings.HasSuffix(path, "/") {
		return "", false
	}
	return path + "/" + query, true
}

// DotSegment prefixes the path with a segment and a ".." that cancels it, so
// the path only matches its rule after dot segments are resolved.
func DotSegment(target string) string {
	return "/rp-x/.." + target
}

// CurrentDir inserts a "." segment after the leading slash.
func CurrentDir(target string) string {
	return "/." + target
}

// DoubleSlash doubles the leading slash.
func DoubleSlash(target string) string {
	return "/" + target
}

// EncodeFirstLetter percent-encodes the first letter of the path, so the path
// only matches its rule after percent-decoding. It returns false when the path
// has no ASCII letter.
func EncodeFirstLetter(target string) (string, bool) {
	path, query := splitQuery(target)
	for i := 0; i < len(path); i++ {
		c := path[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			return fmt.Sprintf("%s%%%02X%s%s", path[:i], c, path[i+1:], query), true
		}
	}
	return "", false
}
