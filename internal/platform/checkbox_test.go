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

func TestCheckedMarkerItems(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"plain checked no marker", "- [x] rebase", 0},
		{"rebase-check marker", "- [x] <!-- rebase-check -->If you want to rebase/retry this MR, check this box", 1},
		{"dashboard approve marker", " - [x] <!-- approve-branch=renovate/foo -->build(deps): update foo", 1},
		{"unchecked marker", "- [ ] <!-- rebase-check -->rebase", 0},
		{"marker before checkbox does not count", "<!-- x --> - [x] plain", 0},
		{"mixed", "- [x] <!-- create-all-rate-limited-prs -->all\n- [x] plain\n- [ ] <!-- m -->no", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckedMarkerItems(tc.text); got != tc.want {
				t.Errorf("CheckedMarkerItems(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}
