package platform

import "testing"

func TestCheckedItems(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"unchecked only", "- [ ] rebase\n- [ ] retry", 0},
		{"one checked dash", "- [x] rebase", 1},
		{"uppercase X", "- [X] rebase", 1},
		{"asterisk list", "* [x] item", 1},
		{"numbered list", "1. [x] item", 1},
		{"indented", "  - [x] nested", 1},
		{"mixed", "- [x] a\n- [ ] b\n- [x] c", 2},
		{"not a list item", "text [x] inline", 0},
		{"checkbox mid-line", "- foo [x] bar", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckedItems(tc.text); got != tc.want {
				t.Errorf("CheckedItems(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}
