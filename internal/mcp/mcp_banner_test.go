package mcp

import (
	"strings"
	"testing"
)

// A STDIO PEER WRITES ITS OWN NOISE ONTO THE SAME STREAM, and that noise can start
// with the byte a frame starts with. Both of these banners were measured on real
// servers, and both begin with '[' — the exact byte the frame scan used to trust.
//
// The failure was not "the frame is late". It was "the parse started in the wrong
// place", reported to the caller as "not yet" — for ever.
func TestRecvFindsTheFrameBehindABannerStartingWithABracket(t *testing.T) {
	banners := []struct {
		name   string
		banner string
	}{
		{
			// chrome-devtools-mcp 1.12.0, after notifications/initialized.
			name:   "chrome-devtools-mcp",
			banner: `[chrome-devtools-mcp] The connecting client did not negotiate the MCP roots capability.`,
		},
		{
			// @modelcontextprotocol/server-filesystem 2.0.0, same place.
			name:   "server-filesystem",
			banner: `Client does not support MCP Roots, using allowed directories set from server args: [ 'C:\tc' ]`,
		},
	}
	for _, b := range banners {
		t.Run(b.name, func(t *testing.T) {
			s := newTestServer(t)
			h := openPipe(t, s, `sh -c 'printf `+b.banner+`\n; `+
				`printf "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n"'`)
			waitForShellToExit(t, s, shellIDFromHandle(t, s, h))
			defer termText(t, s, map[string]any{"cmd": "close " + h})

			got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
			if strings.Contains(got, "no complete frame") {
				t.Fatalf("the banner stopped the scan:\n%s", got)
			}
			if !strings.Contains(got, `"ok":true`) {
				t.Errorf("the frame behind the banner was not delivered:\n%s", got)
			}
			// And the banner itself must not be handed back as if it were the frame.
			if strings.Contains(got, "chrome-devtools-mcp]") || strings.Contains(got, "server args:") {
				t.Errorf("the banner was returned as the frame:\n%s", got)
			}
		})
	}
}

// THE CURSOR MUST ADVANCE OVER THE NOISE, not stall on it. A recv that returned the
// frame but left the position inside the banner would deliver the same frame again.
func TestRecvAdvancesPastTheBannerToTheNextFrame(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, `sh -c 'printf "[peer] starting\n"; `+
		`printf "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"n\":1}}\n"; `+
		`printf "{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"n\":2}}\n"'`)
	waitForShellToExit(t, s, shellIDFromHandle(t, s, h))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	first := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
	if !strings.Contains(first, `"n":1`) {
		t.Fatalf("the first frame was not delivered:\n%s", first)
	}
	second := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
	if !strings.Contains(second, `"n":2`) {
		t.Errorf("the second frame was not delivered after the first:\n%s", second)
	}
}

// THE MESSAGE HAS TO SAY WHICH PROBLEM IT IS. "no complete frame yet" is the same
// sentence for a peer that is slow and a peer whose bytes will never parse, and only
// one of them is worth waiting on.
func TestRecvSaysWhenTheBytesInFrontAreNotAFrame(t *testing.T) {
	s := newTestServer(t)
	// A banner, then nothing: the buffer holds bytes that will never become a frame.
	h := openPipe(t, s, `sh -c 'printf "just a banner, no frame\n"; sleep 5'`)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
	if !strings.Contains(got, "no complete frame") {
		t.Fatalf("expected the not-yet answer, got:\n%s", got)
	}
	if !strings.Contains(got, "not a frame") {
		t.Errorf("the reply did not name the unparseable prefix:\n%s", got)
	}
	if !strings.Contains(got, "just a banner") {
		t.Errorf("the reply did not quote what it found:\n%s", got)
	}
}

// A HALF-ARRIVED VALUE IS STILL "NOT YET", and saying anything else would be wrong
// in the other direction: the caller should wait, and there is nothing to explain.
func TestRecvKeepsThePlainNotYetAnswerForAPartialFrame(t *testing.T) {
	s := newTestServer(t)
	// An opening brace and nothing else: the frame is still being written.
	h := openPipe(t, s, `sh -c 'printf "{\"jsonrpc\":"; sleep 5'`)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
	if !strings.Contains(got, "no complete frame") {
		t.Fatalf("expected the not-yet answer, got:\n%s", got)
	}
	if strings.Contains(got, "not a frame") {
		t.Errorf("a half-arrived value was reported as noise:\n%s", got)
	}
}

// THE SCAN TRIES EVERY CANDIDATE, not just the first bracket. A banner can hold
// several, and only one of them is the frame.
func TestFirstJSONValueTriesEveryCandidate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"banner then frame", `[warn] x [y] {"a":1}`, `{"a":1}`},
		{"an array frame is a frame too", `[warn] [1,2,3]`, `[1,2,3]`},
		{"noise with braces that never close", `{oops {"a":1}`, `{"a":1}`},
		{"nothing but noise", `[warn] nothing here`, ""},
		{"a partial frame at the end", `[warn] {"a":`, ""},
		{"the value comes first", `{"a":1} trailing`, `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := firstJSONValue(tc.in)
			if tc.want == "" {
				if ok {
					t.Errorf("firstJSONValue(%q) = %q, want no value", tc.in, got)
				}
				return
			}
			if !ok {
				t.Fatalf("firstJSONValue(%q) found nothing, want %q", tc.in, tc.want)
			}
			if got != tc.want {
				t.Errorf("firstJSONValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
