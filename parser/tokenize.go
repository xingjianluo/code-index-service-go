package parser

import (
	"strings"
	"unicode"
)

// Tokenize implements index-rules v2 normalization.
//
// v2 rules (deterministic):
//  1. Split the input on any run of non-alphanumeric characters.
//  2. Within each alphanumeric run, additionally split identifier boundaries:
//     snake_case / kebab-case are already split; CamelCase and digits are
//     split (e.g. "GetUserName2FA" -> get,user,name,2,fa).
//  3. Lower-case every token.
//  4. Drop empty tokens and tokens longer than 64 runes (garbage), drop
//     pure-digit tokens longer than 8 runes, but keep all other tokens
//     including single letters.
//  5. De-duplicate while preserving first-seen order.
//
// The v1 rules only performed steps 1, 3 and a length>=2 filter without
// camel-case splitting; store migration re-tokenizes v1 chunks with v2.
func Tokenize(s string) []string {
	seen := make(map[string]bool, 16)
	var out []string

	emit := func(tok string) {
		if tok == "" || seen[tok] || len(tok) > 64 {
			return
		}
		if isAllDigits(tok) && len(tok) > 8 {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}

	var run strings.Builder
	flushRun := func() {
		if run.Len() == 0 {
			return
		}
		for _, w := range splitCamel(run.String()) {
			emit(strings.ToLower(w))
		}
		run.Reset()
	}

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			run.WriteRune(r)
			continue
		}
		flushRun()
	}
	flushRun()
	return out
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// splitCamel splits an alphanumeric run at lowercase->uppercase,
// uppercase-run boundaries before an uppercase-tail ("HTTPServer" ->
// HTTP,Server) and letter->digit / digit->letter transitions.
func splitCamel(s string) []string {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		splitHere := false
		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			splitHere = true
		case unicode.IsLetter(prev) && unicode.IsDigit(cur):
			splitHere = true
		case unicode.IsDigit(prev) && unicode.IsLetter(cur):
			splitHere = true
		case unicode.IsUpper(prev) && unicode.IsUpper(cur):
			// Boundary inside an uppercase run if next-next is lowercase:
			// "HTTPServer": split between P and S.
			if i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				splitHere = true
			}
		}
		if splitHere {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	words = append(words, string(runes[start:]))
	return words
}
