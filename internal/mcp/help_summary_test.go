package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// The summary is what a caller reads instead of the page, so it has to be the thing
// that is cheap — and the page has to be unchanged for the caller who asks for it.
func TestHelpSummaryIsCheaperThanThePage(t *testing.T) {
	s := newTestServer(t)

	summary := termText(t, s, map[string]any{"cmd": "help read", "bytes_limit": 32768})
	page := termText(t, s, map[string]any{"cmd": "help read --all", "bytes_limit": 32768})

	if len(summary) >= len(page)/2 {
		t.Errorf("summary is %d bytes and the page is %d: not a smaller question", len(summary), len(page))
	}
	// The page is still reachable, and reachable from the summary.
	if !strings.Contains(summary, "help read --all") {
		t.Error("the summary must name the page, or the page is lost to a caller who does not guess --all")
	}
	if !strings.Contains(page, "READ IS A CURSOR") {
		t.Error("--all must still return the page, not a summary of it")
	}
	// The template and the flag list are in both: they are what a caller scans.
	for _, want := range []string{"read <session> <index>", "Flags: --"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q", want)
		}
	}
}

// A verb that has not migrated keeps its page, so the surface can move one verb at a
// time without a half-migrated table anywhere.
func TestUnmigratedVerbStillReturnsItsPage(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "help run", "bytes_limit": 32768})
	if !strings.Contains(got, "THIS TYPES INTO A TERMINAL") {
		t.Error("a verb with no summary must return its page, unchanged")
	}
	if strings.Contains(got, "--all`") {
		t.Error("a verb with no summary must not advertise a page it already returned")
	}
}

// The table walks the declaration, so a flag cannot go unmentioned — the failure mode
// of a hand-kept list is silence, and silence is what a caller cannot notice.
func TestEveryDeclaredFlagOfAMigratedVerbHasALine(t *testing.T) {
	op, ok := directive.Lookup("read")
	if !ok {
		t.Fatal("read is not declared")
	}
	if !directive.HasSummaries("read") {
		t.Fatal("read should be migrated")
	}
	for _, flag := range op.Flags {
		if _, ok := directive.FlagSummary("read", flag); !ok {
			t.Errorf("--%s is declared by read but has no summary line", flag)
		}
	}
}

// The summary quotes a price for the page. It has to be the bill: a caller deciding
// whether to spend a byte window on it is reading that number.
func TestSummaryQuotesTheRealPageSize(t *testing.T) {
	s := newTestServer(t)

	summary := termText(t, s, map[string]any{"cmd": "help read", "bytes_limit": 32768})
	page := termText(t, s, map[string]any{"cmd": "help read --all", "bytes_limit": 32768})

	const marker = "The full page is "
	i := strings.Index(summary, marker)
	if i < 0 {
		t.Fatal("the summary does not name the page's size")
	}
	rest := summary[i+len(marker):]
	j := strings.Index(rest, " bytes")
	if j < 0 {
		t.Fatal("the quoted size has no unit")
	}
	quoted := rest[:j]

	// The rendered text is headers, a blank line, then the body; the price is the body.
	if sep := strings.Index(page, "\n\n"); sep >= 0 {
		page = page[sep+2:]
	}
	if got := fmt.Sprintf("%d", len(page)); got != quoted {
		t.Errorf("summary quotes %s bytes but `--all` returns %s", quoted, got)
	}
}
