package platform

import "regexp"

var checkedItem = regexp.MustCompile(`(?mi)^\s*(?:[-*+]|\d+\.)\s+\[x\]`)

// CheckedItems counts checked markdown todo items ("- [x] ...") in text.
// Used to detect Renovate checkbox ticks in MR/issue descriptions: a tick
// is a transition where the checked count increases.
func CheckedItems(text string) int {
	return len(checkedItem.FindAllString(text, -1))
}
