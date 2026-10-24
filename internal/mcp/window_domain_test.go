package mcp

import (
	"context"
	"strings"
	"testing"
)

// The window's definition domain, and what a caller learns from leaving it.
//
// The time and space window is termcp's limit on how long one call may wait and how
// much one call may put into the caller's context. Both are costs the caller pays, and
// the protocol refuses to let either grow without bound. A value outside the domain is
// therefore not a preference to clamp — clamping it would answer a request nobody made
// while hiding that the request was refused.

// TestOnlyTheWallThatIsRealIsRefused pins where the line sits.
//
// Time is refused above its cap because the transport in front of the caller gives up
// first, so a longer window loses the reply. Space is not: the caller pays for what lands
// in its context and is the only party that can judge the trade, so a big window is
// honoured (32 KiB is the size to stay under, published as such). One reply cannot be
// both — a number refused here and a number accepted there must mean something different.
func TestOnlyTheWallThatIsRealIsRefused(t *testing.T) {
	s := newTestServer(t)
	got := s.Term(context.Background(), "ls", map[string]any{
		"cmd":               "ls",
		"timeout":           99999,
		"bytes_limit":       99999,
		"lines_limit":       99999,
		"total_bytes_limit": 99999,
	}).Text()

	if !strings.Contains(got, "timeout must be <= 60") {
		t.Errorf("a wait that would lose the reply must be refused:\n%s", got)
	}
	for _, name := range []string{"bytes_limit", "lines_limit", "total_bytes_limit"} {
		if strings.Contains(got, name+" must be") {
			t.Errorf("%s is a cost the caller chose, and it was refused:\n%s", name, got)
		}
	}
}

// TestAnOutOfDomainWindowTeachesTheDesign pins the other half: the refusal carries the
// reason, because the caller that sent `timeout: 320` was not asking for a typo to be
// caught, it was asking to WAIT — and the answer it needs is that this window is not a
// wait for completion.
func TestAnOutOfDomainWindowTeachesTheDesign(t *testing.T) {
	s := newTestServer(t)
	got := s.Term(context.Background(), "ls", map[string]any{"cmd": "ls", "timeout": 320}).Text()

	// The bound itself.
	if !strings.Contains(got, "timeout must be <= 60") {
		t.Fatalf("the bound is not stated:\n%s", got)
	}
	// The design: bounded time AND bounded space, both for the caller's sake.
	for _, want := range []string{
		"how long a call may wait",
		"how much it may bring into your context",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal must state the design it comes from (%q):\n%s", want, got)
		}
	}
	// The reason the number is 60 rather than a rounder larger one.
	if !strings.Contains(got, "transport") {
		t.Errorf("the refusal must say why the bound is where it is:\n%s", got)
	}
	// And what to do instead of waiting: work that outlives the call.
	if !strings.Contains(got, "--argv") {
		t.Errorf("the refusal must name the alternative for long work:\n%s", got)
	}

	// Space carries the same advice without the refusal: the recommendation lives in the
	// schema the caller reads before choosing, which is where a hint can do any work.
	//
	// BOTH NUMBERS, because the per-slot budget and the whole-reply budget are the caller's
	// two numbers and one has to cover the other: a 50 KiB slot under the default 32 KiB
	// total is refused, and that refusal is not a size limit — it says which of the two
	// numbers to raise.
	space := s.Term(context.Background(), "ls", map[string]any{
		"cmd": "ls", "bytes_limit": 50000, "total_bytes_limit": 50000,
	}).Text()
	if strings.Contains(space, "400") {
		t.Errorf("a large byte window is the caller's own budget:\n%s", space)
	}
	decl, _ := termArgDeclFor("bytes_limit")
	if !strings.Contains(decl.BoundNote, "--cursor") {
		t.Errorf("the recommendation must name the way to read without one huge window:\n%s", decl.BoundNote)
	}
}

// TestASlotLimitAboveTheReplyCapIsRefusedOnlyWhenAsked pins the cross-field rule and
// its one exception.
//
// `bytes_limit: 32768` under a 1000 total is silently cut to the total: the caller
// gets fewer bytes than it named and no line saying so. That is refused. But a caller
// that only lowers the total is asking for a small reply, and the per-slot default
// being larger is how that is expressed — refusing it would make "cap the whole reply"
// impossible to say.
func TestASlotLimitAboveTheReplyCapIsRefusedOnlyWhenAsked(t *testing.T) {
	s := newTestServer(t)

	both := s.Term(context.Background(), "ls", map[string]any{
		"cmd": "ls", "bytes_limit": 32768, "total_bytes_limit": 1000,
	}).Text()
	if !strings.Contains(both, "larger than total_bytes_limit") {
		t.Errorf("a named per-slot limit above the reply cap must be refused:\n%s", both)
	}

	// Only the total: legal, and the reply is capped by it.
	only := s.Term(context.Background(), "ls", map[string]any{
		"cmd": "ls", "total_bytes_limit": 1000,
	}).Text()
	if strings.Contains(only, "larger than total_bytes_limit") {
		t.Errorf("lowering the total alone must not be refused:\n%s", only)
	}
}
