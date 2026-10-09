package admin

import (
	"regexp"
	"strings"
	"testing"
)

// The panel is one big string constant with no build step and no view layer, so
// nothing but a test will notice when a column is added and the empty-state
// colspan is not. A wrong colspan does not throw: the browser simply renders a
// row that does not span the table, which looks like a layout glitch rather than
// a mistake and is easy to ship.
func TestEveryTableColumnCountMatchesItsEmptyState(t *testing.T) {
	tableRe := regexp.MustCompile(`(?s)<table id="([a-z]+)"><thead><tr>(.*?)</tr></thead>`)
	matches := tableRe.FindAllStringSubmatch(dashboardHTML, -1)
	if len(matches) == 0 {
		t.Fatal("no tables found; the panel markup changed shape and this guard is now blind")
	}

	for _, m := range matches {
		id := m[1]
		want := strings.Count(m[2], "<th>")
		if want == 0 {
			t.Fatalf("table %q has no headers", id)
		}

		// The row-building function for this table, found by the querySelector it
		// uses, so the two halves cannot drift apart unnoticed.
		bodyRe := regexp.MustCompile(`(?s)function render` + capitalize(id) + `\(list\) \{(.*?)\n\}`)
		body := bodyRe.FindStringSubmatch(dashboardHTML)
		if body == nil {
			t.Fatalf("no render function found for table %q", id)
		}

		// Every colspan in that function must equal the column count. A function
		// may legitimately have none, in which case there is nothing to check.
		for _, c := range regexp.MustCompile(`colspan="(\d+)"`).FindAllStringSubmatch(body[1], -1) {
			if c[1] != itoa(want) {
				t.Errorf("table %q has %d columns but its render function uses colspan=%s", id, want, c[1])
			}
		}

		// And the row itself must produce that many cells. Counting the literal
		// <td> tags is enough here: these rows are built by concatenation, not by
		// a loop, so the count is static.
		rows := regexp.MustCompile(`(?s)return '<tr>' \+\s*(.*?)\n\s*'</tr>';`).FindStringSubmatch(body[1])
		if rows != nil {
			got := strings.Count(rows[1], "'<td")
			if got != want {
				t.Errorf("table %q has %d columns but a rendered row emits %d cells", id, want, got)
			}
		}
	}
}

// The quota and region columns must be keyed on the fields the API actually
// sends. A typo here fails silently: the cell renders as an em dash forever, and
// an empty column looks like "nothing to report" rather than "this is broken".
func TestTheNewColumnsReadTheFieldsTheAPISends(t *testing.T) {
	for _, field := range []string{"quota_until", "region_blocked_for"} {
		if !strings.Contains(dashboardHTML, field) {
			t.Errorf("the panel never reads %q", field)
		}
	}
}

// Both actions exist because the proxy cannot observe what they report, so both
// must be wired to the endpoints that do exist. A button calling a path the
// server does not serve is a dead control.
func TestThePanelActionsCallEndpointsThatExist(t *testing.T) {
	for _, ep := range []string{"'/uncool'", "'/clear-region'"} {
		if !strings.Contains(dashboardHTML, ep) {
			t.Errorf("no panel action calls %s", ep)
		}
	}
}

// capitalize turns a table id into the render function's suffix. Written out
// rather than using strings.Title, which is deprecated.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
