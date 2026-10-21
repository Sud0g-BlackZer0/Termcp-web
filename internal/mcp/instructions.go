package mcp

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. Keep it TERSE: many clients re-send it every
// model turn, so each rule is paid for repeatedly.
//
// IT DESCRIBES THE WORLD, AND THE WORLD IS SMALL ENOUGH TO DESCRIBE.
//
// Four drafts led here. The first was four numbered rules, three of them "never do X":
// a reader finished knowing every way to fail and nothing about what it held. The
// second described the terrain in prose — correct but wordy, because prose must say
// "and also" for each alternative a grammar states once. The third was the full
// directive table, which is right in an on-demand guide and wrong here: it is re-sent
// every turn, and most of those words are for verbs the caller is not using yet.
//
// This one is ordered the way the tool is built, which is also the order a caller
// needs it: what the thing is, what a call carries, where capability lives, and what
// to look at before building anything.
//
// TWO CENTRES, NOT TWENTY-THREE ROWS. `ext` answers "what can I do"; `ps` answers
// "what is running and what can I do to it". Everything else is an action on one of
// those. A caller holding this much can derive the rest from `help`, which is why the
// rest is not here.
//
// The tests in docs_test.go and token_budget_test.go hold this line in both
// directions: the hard boundaries must stay, and the directive table must not
// migrate back into the text.

const mcpServerInstructions = `termcp — a shell-like DSL controlling hosts over SSH. Local ("internal", if enabled)
and remote are identical.

cmd = the DSL. sh = a shell command. cmd holds directives only; sh enters as a VALUE
through args ($1, $2) or vars ($NAME), never as syntax. cmd supports && || ; &
  run a1f3:1 $1   args: ["make && test"]   one sh line
  run a1f3:1 make && notify done           two cmd directives

session = one SSH link (host opens one); channel = one shell on it, <session>:<index>,
an address you copy from ps, never spell.

TWO CENTRES
  ext list  CAPABILITY: resident, capability-trimmed extensions (mcp servers, skills,
            prompts). Their notes are the manual. LOOK FIRST.
  ps        STATE. learn_mode:true expands every verb taking an address it printed.

ACTIONS — every capability belongs to a session
  pty <host> stays open   pipe <host> --argv one command + exit code   run <ch> <line>
  type (no Enter)  key (ctrl+c/esc)  read (new bytes)  file/fwd (sftp, tunnels)
  mcp/cdp open <session> <url> reaches the peer THROUGH that session's network
  notify <text> reaches the human before sudo/password/choice, then waits

help lists all 23; learn_mode:true attaches any reply's usage line.

HARD BOUNDARY
  notify FIRST (--level warn|error --sticky --on <session>)
  before any password/sudo/passphrase/MFA, confirmation, approval, choice or input.
  Never guess, paste, echo or store a secret; never bypass sudo/auth/approval (sudo -n,
  NOPASSWD, setuid, sudoers, reword, encode, retry loops).
  Human at a prompt: notify, then WAIT. A long command's reply carries what arrived, so
  read a slow task in pieces rather than polling.

REVIEW — review_pending=true means HELD: nothing ran, no id to poll. Do NOT resubmit or
retry; notify, then wait.`
