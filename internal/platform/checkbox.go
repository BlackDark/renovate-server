package platform

import "regexp"

var checkedItem = regexp.MustCompile(`(?mi)^\s*(?:[-*+]|\d+\.)\s+\[x\]`)

// CheckedItems counts checked markdown todo items ("- [x] ...") in text.
// Used to detect Renovate checkbox ticks in MR/issue descriptions: a tick
// is a transition where the checked count increases.
func CheckedItems(text string) int {
	return len(checkedItem.FindAllString(text, -1))
}

var checkedMarkerItem = regexp.MustCompile(`(?mi)^\s*(?:[-*+]|\d+\.)\s+\[x\][^\n]*<!--[^>]*-->`)

// CheckedMarkerItems counts checked todo items that carry a Renovate HTML
// comment marker (e.g. "- [x] <!-- rebase-check -->..."). Renovate embeds
// such markers in every actionable checkbox, so this filters out ordinary
// human task lists.
func CheckedMarkerItems(text string) int {
	return len(checkedMarkerItem.FindAllString(text, -1))
}
