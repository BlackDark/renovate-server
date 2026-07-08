package platform

import (
	"regexp"
	"strings"
)

// HasRenovateDebugMarker reports whether text contains renovate's debug
// HTML comment ("<!--renovate-debug:...-->"). Renovate appends it to every
// MR/PR description, so it reliably identifies renovate MRs even when the
// checkbox lines carry no markers.
func HasRenovateDebugMarker(text string) bool {
	return strings.Contains(text, "<!--renovate-debug:")
}

// BranchHasPrefix reports whether branch starts with any of the prefixes.
func BranchHasPrefix(prefixes []string, branch string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(branch, p) {
			return true
		}
	}
	return false
}

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
