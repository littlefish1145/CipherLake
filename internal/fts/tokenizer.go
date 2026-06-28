package fts

import (
	"strings"
	"unicode"
)

// Token represents a term with its position in the original text.
type Token struct {
	Term     string
	Position int
}

// stopWords is a set of common English stop words to filter out during tokenization.
var stopWords = map[string]bool{
	"a":    true,
	"an":   true,
	"and":  true,
	"are":  true,
	"as":   true,
	"at":   true,
	"be":   true,
	"by":   true,
	"for":  true,
	"from": true,
	"has":  true,
	"he":   true,
	"in":   true,
	"is":   true,
	"it":   true,
	"its":  true,
	"of":   true,
	"on":   true,
	"that": true,
	"the":  true,
	"to":   true,
	"was":  true,
	"were": true,
	"will": true,
	"with": true,
}

// Tokenize splits text into tokens using Unicode word boundaries,
// lowercases them, removes stop words, and applies Porter stemming.
func Tokenize(text string) []Token {
	var tokens []Token
	pos := 0

	// Use unicode word boundary segmentation
	var buf strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(unicode.ToLower(r))
		} else if buf.Len() > 0 {
			word := buf.String()
			buf.Reset()
			if !stopWords[word] && len(word) > 1 {
				stemmed := porterStem(word)
				tokens = append(tokens, Token{Term: stemmed, Position: pos})
				pos++
			}
		}
	}
	// Handle the last word
	if buf.Len() > 0 {
		word := buf.String()
		if !stopWords[word] && len(word) > 1 {
			stemmed := porterStem(word)
			tokens = append(tokens, Token{Term: stemmed, Position: pos})
		}
	}

	return tokens
}

// porterStem implements a simplified Porter stemmer for English words.
// This covers the core steps of the Porter stemming algorithm.
func porterStem(word string) string {
	if len(word) <= 2 {
		return word
	}

	w := word

	// Step 1a: plural forms
	if strings.HasSuffix(w, "sses") {
		w = w[:len(w)-2]
	} else if strings.HasSuffix(w, "ies") {
		w = w[:len(w)-2]
	} else if strings.HasSuffix(w, "ss") {
		// do nothing
	} else if strings.HasSuffix(w, "s") {
		w = w[:len(w)-1]
	}

	// Step 1b: past participle / progressive
	step1bExtra := false
	if strings.HasSuffix(w, "eed") {
		if measure(w[:len(w)-3]) > 0 {
			w = w[:len(w)-1]
		}
	} else if strings.HasSuffix(w, "ed") {
		if containsVowel(w[:len(w)-2]) {
			w = w[:len(w)-2]
			step1bExtra = true
		}
	} else if strings.HasSuffix(w, "ing") {
		if containsVowel(w[:len(w)-3]) {
			w = w[:len(w)-3]
			step1bExtra = true
		}
	}

	if step1bExtra {
		if strings.HasSuffix(w, "at") || strings.HasSuffix(w, "bl") || strings.HasSuffix(w, "iz") {
			w = w + "e"
		} else if doubleConsonant(w) && !strings.HasSuffix(w, "l") && !strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "z") {
			w = w[:len(w)-1]
		} else if measure(w) == 1 && cvcPattern(w) {
			w = w + "e"
		}
	}

	// Step 1c: y -> i
	if strings.HasSuffix(w, "y") && containsVowel(w[:len(w)-1]) {
		w = w[:len(w)-1] + "i"
	}

	// Step 2: common suffix transformations
	suffixes2 := []struct {
		suffix    string
		replacement string
	}{
		{"ational", "ate"},
		{"tional", "tion"},
		{"enci", "ence"},
		{"anci", "ance"},
		{"izer", "ize"},
		{"abli", "able"},
		{"alli", "al"},
		{"entli", "ent"},
		{"eli", "e"},
		{"ousli", "ous"},
		{"ization", "ize"},
		{"ation", "ate"},
		{"ator", "ate"},
		{"alism", "al"},
		{"iveness", "ive"},
		{"fulness", "ful"},
		{"ousness", "ous"},
		{"aliti", "al"},
		{"iviti", "ive"},
		{"biliti", "ble"},
	}
	for _, s := range suffixes2 {
		if strings.HasSuffix(w, s.suffix) {
			stem := w[:len(w)-len(s.suffix)]
			if measure(stem) > 0 {
				w = stem + s.replacement
			}
			break
		}
	}

	// Step 3
	suffixes3 := []struct {
		suffix      string
		replacement string
	}{
		{"icate", "ic"},
		{"ative", ""},
		{"alize", "al"},
		{"iciti", "ic"},
		{"ical", "ic"},
		{"ful", ""},
		{"ness", ""},
	}
	for _, s := range suffixes3 {
		if strings.HasSuffix(w, s.suffix) {
			stem := w[:len(w)-len(s.suffix)]
			if measure(stem) > 0 {
				w = stem + s.replacement
			}
			break
		}
	}

	// Step 4
	suffixes4 := []string{
		"al", "ance", "ence", "er", "ic", "able", "ible", "ant",
		"ement", "ment", "ent", "ion", "ou", "ism", "ate", "iti",
		"ous", "ive", "ize",
	}
	for _, s := range suffixes4 {
		if strings.HasSuffix(w, s) {
			stem := w[:len(w)-len(s)]
			if s == "ion" {
				if len(stem) > 0 && (stem[len(stem)-1] == 's' || stem[len(stem)-1] == 't') {
					if measure(stem) > 1 {
						w = stem
					}
				}
			} else {
				if measure(stem) > 1 {
					w = stem
				}
			}
			break
		}
	}

	// Step 5a
	if strings.HasSuffix(w, "e") {
		stem := w[:len(w)-1]
		if measure(stem) > 1 {
			w = stem
		} else if measure(stem) == 1 && !cvcPattern(stem) {
			w = stem
		}
	}

	// Step 5b
	if doubleConsonant(w) && strings.HasSuffix(w, "ll") && measure(w) > 1 {
		w = w[:len(w)-1]
	}

	return w
}

// measure computes the "measure" m of a stem, which is the number of
// VC (vowel-consonant) sequences in the word.
func measure(word string) int {
	if len(word) == 0 {
		return 0
	}

	m := 0
	i := 0

	// Skip initial consonants
	for i < len(word) && !isVowel(word[i]) {
		i++
	}

	for i < len(word) {
		// Count vowel sequence
		for i < len(word) && isVowel(word[i]) {
			i++
		}
		if i >= len(word) {
			break
		}
		// Count consonant sequence
		for i < len(word) && !isVowel(word[i]) {
			i++
		}
		m++
	}

	return m
}

// containsVowel checks if the word contains a vowel.
func containsVowel(word string) bool {
	for i := 0; i < len(word); i++ {
		if isVowel(word[i]) {
			return true
		}
	}
	return false
}

// isVowel checks if a character is a vowel (a, e, i, o, u).
func isVowel(c byte) bool {
	return c == 'a' || c == 'e' || c == 'i' || c == 'o' || c == 'u'
}

// doubleConsonant checks if the word ends with a double consonant.
func doubleConsonant(word string) bool {
	if len(word) < 2 {
		return false
	}
	return word[len(word)-1] == word[len(word)-2] && !isVowel(word[len(word)-1])
}

// cvcPattern checks if the word ends with consonant-vowel-consonant
// where the last consonant is not w, x, or y.
func cvcPattern(word string) bool {
	if len(word) < 3 {
		return false
	}
	last := len(word) - 1
	return !isVowel(word[last-2]) && isVowel(word[last-1]) && !isVowel(word[last]) &&
		word[last] != 'w' && word[last] != 'x' && word[last] != 'y'
}
