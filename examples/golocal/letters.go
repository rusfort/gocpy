package textstat

import "unicode"

// letters is unexported: only the exported API is visible from Gocpy.
func letters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}
