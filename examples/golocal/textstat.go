// Package textstat is plain Go code used from Gocpy with import "./textstat.go".
package textstat

import (
	"errors"
	"strings"
	"unicode"
)

// MaxWords limits the input of Words.
const MaxWords = 1000

// Words counts the words in s.
func Words(s string) int { return len(strings.Fields(s)) }

// Title capitalizes the first letter of every word.
func Title(s string) string {
	ws := strings.Fields(s)
	for i, w := range ws {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		ws[i] = string(r)
	}
	return strings.Join(ws, " ")
}

// Ratio returns the share of letters in s.
func Ratio(s string) (float64, error) {
	if s == "" {
		return 0, errors.New("empty text")
	}
	return float64(letters(s)) / float64(len([]rune(s))), nil
}
