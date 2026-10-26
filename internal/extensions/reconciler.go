package extensions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/transcript"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Reconciler keeps every enabled extension's session warm. It is the half of the
// extension layer that has a lifecycle: the Store answers "what did the author
// ask for" and this answers "is it actually working right now".
//
// A session is not per extension but per host and capability set: extensions with
// the same target and the same whitelist are served by one connection — the
// singleton, created when the first of them comes up, reused by the rest and
// rebuilt if it dies — and each of them drives its own channel inside it, or its
// own SFTP view, or its own tunnels. That is what makes one extension per tool
// affordable: an MCP server, a read-only skill window and a tunnel to the same
// machine cost one connection rather than three.
//
// The loop is deliberately passive about how an extension is used once it is up.
// Bringing an extension to available means having the group's connection and
// proving the extension's own channel (or, for a template that declares no shell,
// the connection alone) can stay up; nothing else. An agent then drives it with
// the ordinary shell/SFTP tools, gated by the session's Restriction. The
// reconciler never reads a skill or speaks JSON-RPC on the agent's behalf.
type Reconciler struct {
	store      *Store
	sessMgr    *session.Manager
	msgMgr     *message.Manager
	sshConfigs *sshconfig.Store
	forwardMgr *forward.ForwardManager

	// NoInternal refuses the built-in loopback profile as an extension target,
	// mirroring the flag the MCP server exposes.
	NoInternal bool

	// mu guards the two maps; a group's own session and an extension's own
	// attachment are behind their own locks, because a probe goroutine writes them
	// while the walk reads them. It also guards cancel, so Start and Stop can be
	// called from different goroutines.
	mu       sync.Mutex
	states   map[string]*extState
	groups   map[string]*extGroup
	onChange func()

	// syncs carries the names of templates whose saved file changed, and revive
	// carries "a profile was rewritten". They are queues, not signals to a walk: the
	// callback that fills them runs inside a store's write, so it must only drop the
	// name in and get out — doing the work there would deadlock on the store's own
	// lock, which is exactly what a synchronous subscriber did.
	syncs  chan string
	revive chan struct{}
	// ctx is the run's context, kept so an event that arrives at any time — a save
	// from a tool call, a deleted file — has a run to join.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// probes tracks the connection attempts the run loop has launched and not yet
	// heard back from. Stop waits on it so a reconciler that has been told to stop
	// cannot leave a session behind that it is no longer tracking — a probe that
	// dials after Stop returns would create exactly that.
	probes sync.WaitGroup
}

// Status names the reconciler's report on one extension. An extension that has
// never been tried, one whose dial is in flight, and one whose process exited
// five minutes ago are all reported distinctly, because the caller's next move
// differs for each.
const (
	StatusDisabled    = "disabled"    // enabled=false: the reconciler will not touch it
	StatusIdle        = "idle"        // nothing is running because nothing has asked; the next use starts it
	StatusUnavailable = "unavailable" // not up; either not yet tried or down after a failure
	StatusProbing     = "probing"     // a connection attempt is in flight
	StatusAvailable   = "available"   // session established, process alive, forwards up
)

// Failure is one recorded reason an extension was not brought up. History is
// kept rather than only the last entry because the shape of repeated failures is
// the diagnosis: always exit 127 says "wrong command", always a dial timeout
// says "wrong host".
type Failure struct {
	At     int64  `json:"at"`    // Unix ms
	Stage  string `json:"stage"` // target | dial | start | forward | session
	Reason string `json:"reason"`
}

// ForwardStatus is a declared forward as the reconciler actually established it.
// ListenAddr/TargetAddr are the bound addresses, which differ from the
// declaration whenever local_port was left at 0 ("pick one").
type ForwardStatus struct {
	Kind       string `json:"kind"`
	ForwardID  string `json:"forward_id,omitempty"`
	ListenAddr string `json:"listen_addr,omitempty"`
	TargetAddr string `json:"target_addr,omitempty"`
}

// Status is the runtime half of an extension. The template half stays in the
// Store; the two are merged for whoever asks (the MCP tool, the Web UI).
type Status struct {
	Name string `json:"name"`
	// Status is one of the five above.
	Status string `json:"status"`
	// Lazy is true for an extension the reconciler starts on demand rather than at
	// startup (every enabled template that declares no forward).
	Lazy bool `json:"lazy,omitempty"`
	// SessionID is the connection this extension is served by. Extensions that
	// share a host and a capability set report the same one.
	SessionID string `json:"session_id,omitempty"`
	// ShellID is the channel this extension runs on, and the handle an agent
	// drives it through. It is empty for a template that declares no shell: a
	// read-only file window has no channel of its own.
	ShellID  string          `json:"shell_id,omitempty"`
	Forwards []ForwardStatus `json:"forwards,omitempty"`
	Failures []Failure       `json:"failures,omitempty"`
	// NextRetry is when the reconciler tries again (Unix ms); 0 = no retry
	// scheduled.
	NextRetry int64 `json:"next_retry,omitempty"`
	// IdleTimeoutMs is how long a lazy extension stays up after its last use, and
	// IdleForMs is how long ago that was; both are reported so "why is this not
	// up" and "how long have I got" are facts rather than guesses.
	IdleTimeoutMs int64 `json:"idle_timeout_ms,omitempty"`
	IdleForMs     int64 `json:"idle_for_ms,omitempty"`
	// Instances is how many callers this extension is currently serving: one
	// channel each, all on the group's connection (issue #183).
	Instances int `json:"instances,omitempty"`
	// ClosedAt and ClosedSessionID describe the instance the reconciler took away:
	// a caller holding the old handle is told what happened to it instead of
	// reading "not found".
	ClosedAt        int64  `json:"closed_at,omitempty"`
	ClosedSessionID string `json:"closed_session_id,omitempty"`
	ClosedShellID   string `json:"closed_shell_id,omitempty"`
	ClosedReason    string `json:"closed_reason,omitempty"`
}

// maxFailures caps the history. Enough to see a pattern, not so much that a
// flapping extension makes the status unbounded.
const maxFailures = 8

// backoffSteps spaces the automatic retries. The steps are short on purpose:
// most extension failures are a wrong command or a sleeping laptop, and either
// is fixed by a human, so a retry exists to recover the moment the underlying
// condition clears.
var backoffSteps = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
}

// maxAutoRetries is how many times the reconciler brings an extension back up on
// its own. It is one per backoff step on purpose: the ladder and the budget are
// the same length, so a spent extension is one that has climbed the whole
// ladder. Three covers a transient (a laptop waking, a briefly missing profile,
// a slow listener); past it the extension waits for a nudge — the panel's retry
// key or the agent's extension(action=retry) — rather than hammering a host that
// is not coming back.
const maxAutoRetries = 3

// probeGrace is how long a started process gets to prove it can stay alive. A
// stdio MCP server with a bad command dies within milliseconds; a healthy one
// blocks on its first read forever, so the wait costs nothing for it.
const probeGrace = 700 * time.Millisecond

// failureTailBytes is the slice of output captured as the failure reason when a
// process exits during its grace window. It is short because it is a reason, not
// a transcript: the agent that wants the whole output reads the session itself.
const failureTailBytes = 512

// extGroup is the connection one or more extensions share: the singleton. It is
// keyed by what has to agree for two extensions to be served by the same session
// — the profile they dial and the capability set they were granted — and it holds
// nothing but that session until the last of its members goes away.
type extGroup struct {
	// mu serializes the group's session against the probes of its members: two
	// extensions coming up at once must open one connection, not two.
	mu        sync.Mutex
	key       string
	target    string
	namespace string
	// created is when this group was first resolved. The sweep uses it to leave
	// alone a connection that appeared while the walk was running: the walk's
	// picture of who wants what was taken at the top, and a group that did not exist
	// then exists precisely because somebody asked for it in the meantime.
	created time.Time
	// sessionID is the session the group is served by. A DEAD one stays named
	// here until the next attempt replaces it, so the corpse can be finalized
	// rather than left in the registry; "does this group have a session" is a
	// liveness question, which is what liveSession answers.
	sessionID string
}

// hostPlan is one template resolved into what it actually needs: the connection
// its group dials, and the channel this extension runs on that connection.
type hostPlan struct {
	// config builds the group's container — a connection with no channel of its
	// own, since every extension brings its own.
	config  session.Config
	channel string // the channel's name, so a shared session says which extension a shell belongs to
	command string
	args    []string
	shell   bool // the template declares a shell (false = SFTP/forwards only)
	pty     bool
	rows    int
	cols    int
}

// NewReconciler wires the reconciler to the stores and managers it drives. It
// subscribes to the extension store's change callback itself, so a saved
// extension is reconciled without the caller remembering to poke anything.
func NewReconciler(store *Store, sessMgr *session.Manager, msgMgr *message.Manager, sshConfigs *sshconfig.Store, forwardMgr *forward.ForwardManager) *Reconciler {
	r := &Reconciler{
		store:      store,
		sessMgr:    sessMgr,
		msgMgr:     msgMgr,
		sshConfigs: sshConfigs,
		forwardMgr: forwardMgr,
		states:     map[string]*extState{},
		groups:     map[string]*extGroup{},
		syncs:      make(chan string, 64),
		revive:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if store != nil {
		// The write names the template, so the runtime does that one and nothing
		// else. This is the whole of "the store changed" — there is no listing to
		// diff and no tick to wait for.
		store.SetOnChange(r.syncNamed)
	}
	if sshConfigs != nil {
		// A profile rewritten by hand is the fix a failing extension was waiting for.
		// It has no name to give (it is not an extension), so the extensions that gave
		// up on a missing target get one more attempt.
		sshConfigs.SetOnChange(func() {
			select {
			case r.revive <- struct{}{}:
			default:
			}
		})
	}
	if sessMgr != nil {
		sessMgr.AddOnDeadHook(r.onSessionDead)
	}
	return r
}

// Store returns the extension store the reconciler reconciles. Callers that
// write extensions (the MCP extension tool) go through it rather than keeping a
// second handle, so a save still reaches this reconciler through the store's
// own change subscription.
func (r *Reconciler) Store() *Store { return r.store }

// SSHConfigs returns the profile store the templates resolve against. A profile
// created after an extension that needed it is the recovery the walk is waiting
// for, and writing it through this handle notifies the reconciler at once.
func (r *Reconciler) SSHConfigs() *sshconfig.Store { return r.sshConfigs }

// SetOnChange subscribes to status transitions (the Web UI's panel and its push
// frames). The callback fires after the state has been written, so a reader sees
// what it is being told about.
func (r *Reconciler) SetOnChange(fn func()) {
	r.mu.Lock()
	r.onChange = fn
	r.mu.Unlock()
}

func (r *Reconciler) notifyChange() {
	r.mu.Lock()
	fn := r.onChange
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Start loads every extension and begins maintaining them. Returning an error
// only for a store that cannot be walked: one broken extension is reported in
// its own status, not as a failure to start the reconciler.
func (r *Reconciler) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.mu.Unlock()
	if _, err := r.store.List(); err != nil {
		r.mu.Lock()
		r.cancel = nil
		r.mu.Unlock()
		cancel()
		return err
	}
	r.sweepRestored()
	go r.run(cctx)
	return nil
}

// sweepRestored removes what a previous process left behind. An extension's
// session serves the run that created it: this process builds its own from the
// templates, so a restored placeholder — a connection nothing can be driven
// through — is dropped rather than kept as an entry that can never be reopened.
func (r *Reconciler) sweepRestored() {
	if r.sessMgr == nil {
		return
	}
	for _, s := range r.sessMgr.ListAll() {
		if ns.IsExtensions(s.Namespace) {
			slog.Debug("dropping restored extension session", "session_id", s.ID, "namespace", s.Namespace)
			_ = r.sessMgr.Delete(s.ID)
		}
	}
}

// Stop cancels the loop, waits for it to exit, and waits again for any probe it
// had in flight. Sessions that are already up stay where they are: they are
// ordinary sessions, and their own teardown follows process exit or the caller's
// shutdown.
func (r *Reconciler) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-r.done
	r.probes.Wait()
}

// syncNamed is the store's subscriber, and it only queues: it runs inside another
// component's write, holding that component's lock, so anything it did here would
// have to be deadlock-free against a lock it does not own. The run loop does the
// work, serialized, one template at a time.
func (r *Reconciler) syncNamed(name string) {
	select {
	case r.syncs <- name:
	default:
		// The queue is full, which means the loop is behind by dozens of writes. Drop
		// the oldest instead of losing this one: the newest state of a name is the one
		// that matters, and a dropped name would leave a template never re-read.
		select {
		case <-r.syncs:
		default:
		}
		select {
		case r.syncs <- name:
		default:
		}
	}
}

// reviveTargets is the profile store's subscriber: a rewritten profile is the fix
// an extension that could not resolve its target was waiting for, so every
// extension whose budget is spent on that stage gets one more attempt. It runs one
// pass over the states, dials nothing, and is caused by a write — the same shape as
// every other reaction here.
func (r *Reconciler) reviveTargets(ctx context.Context) {
	r.mu.Lock()
	pairs := make(map[string]*extState, len(r.states))
	for name, st := range r.states {
		pairs[name] = st
	}
	r.mu.Unlock()
	for name, st := range pairs {
		if _, autoRetry := st.flags(); autoRetry {
			continue
		}
		if st.lastStage() != "target" {
			continue
		}
		ext, err := r.store.Load(name)
		if err != nil || !r.rebuildable(ext) {
			continue
		}
		st.resetRetryBudget()
		r.notifyChange()
		r.retryNow(name)
	}
}

// Sync makes the runtime match one template: the write that changed it says which,
// so nothing is listed, nothing is diffed, and no walk is needed to find out what
// might have moved.
//
// It is called for the name a save or a delete named, and once per stored template
// at startup. Everything a periodic walk used to do now happens here, for the one
// extension the event was about.
func (r *Reconciler) Sync(ctx context.Context, name string) {
	ext, err := r.store.Load(name)
	if err != nil {
		// A file that no longer exists is a delete; one that no longer validates is
		// reported through its own status rather than dropped, because silently
		// forgetting a broken extension leaves "unavailable forever, reason unknown".
		if r.store.Exists(name) {
			st := r.state(name)
			st.leaveGroup()
			st.mark(StatusUnavailable)
			st.recordFailure("target", "extension config is no longer valid: "+err.Error())
			r.notifyChange()
			return
		}
		r.Forget(name)
		return
	}
	st := r.state(name)
	idleLimit, idleErr := ext.IdleTimeoutDuration()
	if idleErr != nil {
		st.leaveGroup()
		st.mark(StatusUnavailable)
		st.recordFailure("target", idleErr.Error())
		r.notifyChange()
		return
	}
	st.setRun(ctx)
	st.setLazy(ext.Lazy(), idleLimit)

	if !ext.IsEnabled() {
		// A parked extension holds nothing: its channels and its tunnels are closed,
		// and the connection it shared is left to the extensions that are still
		// enabled. Its retry budget is handed back too, so enabling it again is an
		// immediate attempt rather than a resumed wait.
		had := st.leaveGroup()
		st.stopIdle()
		if had || st.statusOf() != StatusDisabled {
			r.detach(st)
			st.mark(StatusDisabled)
			st.resetRetryBudget()
			r.notifyChange()
		}
		return
	}

	// Both readings happen unconditionally and are combined after: setGroup answers
	// "did the connection move", changed answers "is this a different template", and
	// chaining them with || was the bug — a template that moved groups short-circuited
	// the fingerprint update, so the next pass saw a stale fingerprint, called it a
	// change, and tore down the channel it had just built.
	moved := st.setGroup(groupKey(ext))
	differed := st.changed(ext)
	if moved || differed {
		// SAVING A TEMPLATE STOPS WHAT IT STARTED. A process running the old command
		// is not what the file now describes, and leaving the two disagreeing means
		// every later question ("why is it behaving like that") has two answers. The
		// channels go, the tunnels go, and the file is read again for whatever comes
		// next — for a lazy extension that is the next ask, for a resident one it is
		// the attempt below.
		r.closeInstances(st, fingerprint(ext), "the template was edited")
		r.detach(st)
		st.stopIdle()
		st.mark(StatusUnavailable)
		st.resetRetryBudget()
		r.notifyChange()
	}

	// The connection is not a caller's to hold: a channel belongs to the caller that
	// asked for it, and the connection under it is the install's, kept for whoever
	// asks next. Registering the group is what keeps it alive.
	r.group(groupKey(ext), ext)

	if ext.Lazy() {
		// ON DEMAND: NOTHING IS STARTED HERE (issue #183). A channel exists because a
		// caller asked for one; this only keeps what exists honest — drop the ones
		// that ended, close the ones whose silence has run out — and re-arms the
		// window from the template as it is now.
		r.sweepInstances(name, ext, st, idleLimit)
		r.armIdle(name, st)
		return
	}
	// A resident extension declares a forward: a listener has no caller whose
	// absence could mean "idle", so it is brought up and kept.
	st.setIdleLimit(idleLimit)
	r.retryNow(name)
}

// Forget drops an extension that is no longer stored: its channels, its tunnels,
// its state, and the connection nothing else names.
func (r *Reconciler) Forget(name string) {
	st := r.state(name)
	st.stopIdle()
	st.leaveGroup()
	r.closeInstances(st, "", "the extension was removed")
	r.detach(st)
	r.mu.Lock()
	delete(r.states, name)
	groups := make([]*extGroup, 0, len(r.groups))
	for _, g := range r.groups {
		groups = append(groups, g)
	}
	r.mu.Unlock()
	for _, g := range groups {
		if r.groupNamed(g.key) {
			continue
		}
		r.closeGroup(g)
		r.mu.Lock()
		delete(r.groups, g.key)
		r.mu.Unlock()
		slog.Debug("extension session closed", "group", g.key, "target", g.target)
	}
	r.notifyChange()
	slog.Debug("extension removed", "name", name)
}

// groupNamed reports whether any state still resolves to this connection.
func (r *Reconciler) groupNamed(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.states {
		if st.groupOf() == key {
			return true
		}
	}
	return false
}

// run reconciles on events, not on a clock. Every write that can change the
// runtime — saving a template, deleting one, parking one, a profile a failing
// extension was waiting for — arrives through Trigger, so the work happens when
// something happened and never because a timer ticked.
//
// THE PERIODIC WALK IS GONE ON PURPOSE (issue #183). Its jobs went to the thing
// that owns each deadline: an idle channel closes on its own timer, a failed
// attempt is retried on its own timer, and a channel whose process ended is
// noticed by whoever reads it next. What is left is "the store says something
// different from what I hold", which is exactly what an event means.
func (r *Reconciler) run(ctx context.Context) {
	defer close(r.done)
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	// The startup pass: resident extensions are established here, because a
	// template that declares a forward has no caller whose ask could start it.
	if names, err := r.store.List(); err == nil {
		for _, name := range names {
			r.Sync(ctx, name)
		}
	} else {
		slog.Warn("extension start failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-r.syncs:
			r.Sync(ctx, name)
		case <-r.revive:
			r.reviveTargets(ctx)
		}
	}
}

// startProbe marks the extension probing and runs the attempt in its own
// goroutine, because a dial to an unreachable host blocks for its timeout and
// must not delay the other extensions or the walk.
func (r *Reconciler) startProbe(ctx context.Context, name string) {
	st := r.state(name)
	st.mu.Lock()
	if st.status == StatusProbing {
		st.mu.Unlock()
		return
	}
	st.status = StatusProbing
	st.mu.Unlock()
	r.notifyChange()
	r.probes.Add(1)
	go func() {
		defer r.probes.Done()
		r.probe(ctx, name)
	}()
}

// probe is one attempt to bring an extension to available. The connection is the
// group's — reused when it is up, created when it is not — and everything else
// belongs to this extension: its channel, its tunnels, its failure history.
func (r *Reconciler) probe(ctx context.Context, name string) {
	ext, err := r.store.Load(name)
	if err != nil || !ext.IsEnabled() {
		// Removed or parked between the scheduling and the attempt.
		return
	}
	st := r.state(name)
	if r.groupFor(st) == nil {
		return
	}
	// The reconciler's own attempt: it reuses the channel the extension already has,
	// because a retry after a tunnel failure must not restart the process.
	_, _, _, _, _ = r.bringUp(ctx, ext, st, false)
}

// fail records a reason, marks the extension unavailable, and arms the backoff
// ladder. It is the shape of every failure a redial or a reopened channel might
// clear — a dial that timed out, a process that exited, a port that was taken —
// so the four things that must happen together happen in one place.
func (r *Reconciler) fail(st *extState, stage, reason string) {
	st.recordFailure(stage, reason)
	st.mark(StatusUnavailable)
	if wait, ok := st.scheduleBackoff(); ok {
		// THE LADDER IS A TIMER, NOT A POLL (issue #183). The walk used to re-read
		// "has the backoff elapsed" every five seconds, which made a retry due at
		// 1.5s fire somewhere between 1.5 and 6.5 — and it made the reconciler the
		// owner of a deadline it could not keep. One timer per extension keeps the
		// deadline it printed.
		r.armRetry(st, wait)
	}
	r.notifyChange()
}

// failForGood is fail for a failure no retry can fix: the ladder is spent rather
// than climbed, so the walk stops re-probing a condition the probe cannot change
// and the extension waits for an edit, the panel's retry key, or the agent's.
func (r *Reconciler) failForGood(st *extState, stage, reason string) {
	st.recordFailure(stage, reason)
	st.mark(StatusUnavailable)
	st.exhaustRetryBudget()
	r.notifyChange()
}

// rebuildable reports whether the template resolves to a connection at all. It is
// the recovery check the walk makes once the automatic budget is spent: nothing
// dials, nothing is created, and the answer costs a store lookup. A template that
// failed to resolve and now resolves is a fix (a profile recreated by hand), so
// the extension gets its retries back; one that still does not resolve keeps
// waiting for a nudge.
func (r *Reconciler) rebuildable(ext *Extension) bool {
	_, err := r.resolve(ext)
	return err == nil
}

// resolve reads the template's target profile and turns the pair into a plan. It
// is the one place an extension's fields are interpreted, so a session created by
// the reconciler and one created by hand from the same profile are the same
// session.
func (r *Reconciler) resolve(ext *Extension) (hostPlan, error) {
	if r.sshConfigs == nil {
		return hostPlan{}, errors.New("no ssh config store configured")
	}
	ent, err := r.sshConfigs.Load(ext.Target)
	if err != nil {
		if errors.Is(err, sshconfig.ErrNotFound) {
			return hostPlan{}, fmt.Errorf("target profile %q does not exist", ext.Target)
		}
		return hostPlan{}, fmt.Errorf("cannot read target profile %q: %w", ext.Target, err)
	}
	if ent.Kind == sshconfig.KindInternal && r.NoInternal {
		return hostPlan{}, fmt.Errorf("target profile %q is the built-in loopback profile, which is disabled", ext.Target)
	}
	var remote *session.RemoteSSH
	if ent.Kind != sshconfig.KindInternal {
		remote, err = sshconfig.RemoteFromEntry(ent, r.sshConfigs.ConfigDir(ext.Target))
		if err != nil {
			return hostPlan{}, fmt.Errorf("target profile %q: %w", ext.Target, err)
		}
	}
	restriction, err := session.NewRestriction(ext.Allow, ext.Paths)
	if err != nil {
		return hostPlan{}, fmt.Errorf("restriction: %w", err)
	}

	cmd, args := sshconfig.EffectiveCommand(ent, ext.Command, ext.Args)
	if ext.URL != "" {
		// The endpoint form runs the in-process relay instead of a program: the
		// template names a server, and what executes on the channel is termcp's own
		// bridge (see internal/mcpbridge).
		//
		// That relay is termcp's own process, so the channel has to be the in-memory
		// one: on a remote target this command would be looked up on the far host,
		// where it does not exist. Refusing here rather than at dial is what turns
		// "command not found" into a sentence about the template.
		if ent.Kind != sshconfig.KindInternal {
			return hostPlan{}, fmt.Errorf("url requires target = \"internal\": the bridge runs inside termcp, so the endpoint is reached from this host (not from %q)", ext.Target)
		}
		cmd, args = bridgeCommand(ext)
	}
	mode := sshconfig.EffectiveMode(ent, ext.Mode)
	rows, cols := ext.Rows, ext.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	// The container's name is the namespace it carries, so a leftover is
	// recognizable in a manifest listing or a log line rather than being a session
	// with a generated name and no owner.
	name := ns.ForExtensions(ext.Target)
	return hostPlan{
		config: session.Config{
			Name:      name,
			SSHConfig: ext.Target,
			// The container has no terminal: rows and cols are the channel's, and the
			// session record merely carries the conventional defaults.
			Rows:         24,
			Cols:         80,
			Remote:       remote,
			DefaultShell: sshconfig.EffectiveDefaultShell(ent),
			// An extension is pre-authorised by whoever wrote its template; the
			// profile's review default would gate an unattended MCP server behind an
			// approval queue nothing is watching.
			Approval: false,
			// The connection outlives any one extension's process: a pipe command
			// that finishes says nothing about the transport, and the group's next
			// member may be about to open its channel on it.
			OnExit: session.OnExitKeep,
			// No channel of its own: an extension brings its own, and one opened here
			// would be a process on the target that nothing drives and nothing
			// watches.
			NoShell:     true,
			Restriction: restriction,
			Namespace:   name,
		},
		channel: ext.Name,
		command: cmd,
		args:    args,
		shell:   ext.Mode != "",
		pty:     mode == "pty",
		rows:    rows,
		cols:    cols,
	}, nil
}

// bridgeCommand renders a URL template as the argv that carries it: the built-in
// relay's name, then the endpoint and its headers. Rendering happens here rather
// than being stored because a stored argv is a second copy of the URL that can
// drift from it — an edit would have to remember to rewrite both.
func bridgeCommand(ext *Extension) (string, []string) {
	args := []string{ext.URL}
	for _, name := range sortedKeys(ext.Headers) {
		args = append(args, "--header", name+"="+ext.Headers[name])
	}
	return mcpbridge.BuiltinCommand, args
}

// groupKey is the identity of the connection an extension is served by: the host
// it dials, and the capability set it was granted. Extensions that agree on both
// are served by one session — the second one adds a channel to a connection that
// already exists rather than dialing the host again.
//
// The capability set is part of the identity rather than merely a property of the
// session because the two travel together: a session enforces one whitelist, so
// two extensions with different whitelists sharing a session would each be able
// to do what the other was allowed. Sharing is therefore exactly coincident with
// "the same calls are permitted", and no extension can reach past its own
// template — which is the whole promise of the layer.
func groupKey(ext *Extension) string {
	return strings.TrimSpace(ext.Target) + "\x00" + restrictionKey(ext.Allow, ext.Paths)
}

// restrictionKey renders a whitelist pair as a comparable string. Entries are
// sorted and trimmed first: two templates that list the same tools in a different
// order, or with stray whitespace, describe the same capability set and must land
// on the same connection.
func restrictionKey(allow, paths []string) string {
	var b strings.Builder
	writeSet(&b, allow)
	b.WriteByte('\x01')
	writeSet(&b, paths)
	return b.String()
}

// sortedKeys returns a map's keys in order, so a rendering that walks them is
// deterministic (two runs of one template must produce one argv, and a
// fingerprint over it must not depend on map iteration order).
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeSet(b *strings.Builder, list []string) {
	sorted := make([]string, 0, len(list))
	for _, s := range list {
		sorted = append(sorted, strings.TrimSpace(s))
	}
	sort.Strings(sorted)
	for _, s := range sorted {
		b.WriteString(s)
		b.WriteByte('\x00')
	}
}

// group returns the group a key names, creating it on first sight. Its target and
// namespace come from the extension that created it, which the key already fixes:
// members of a group dial the same profile under the same namespace.
func (r *Reconciler) group(key string, ext *Extension) *extGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.groups[key]; ok {
		return g
	}
	g := &extGroup{key: key, target: ext.Target, namespace: ns.ForExtensions(ext.Target), created: time.Now()}
	r.groups[key] = g
	return g
}

// groupFor returns the group an extension's state names, or nil when it has none
// (a parked extension, or one whose template no longer loads).
func (r *Reconciler) groupFor(st *extState) *extGroup {
	key := st.groupOf()
	if key == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groups[key]
}

func (r *Reconciler) groupList() []*extGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*extGroup, 0, len(r.groups))
	for _, g := range r.groups {
		out = append(out, g)
	}
	return out
}

// ensureGroupSession returns the connection the group is served by: the one it
// already has when that is still running, a new one otherwise. The group's own
// lock is what makes two members starting at once share the answer.
func (r *Reconciler) ensureGroupSession(ctx context.Context, g *extGroup, base session.Config) (*session.Session, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sess := r.liveSession(g.sessionID); sess != nil {
		return sess, nil
	}
	// The previous session is DEAD and has served its purpose — it held the
	// failure's output for anyone watching — and the group is about to be served
	// by a new one. Finalizing it here keeps a failing group from leaving one dead
	// connection per attempt behind.
	if g.sessionID != "" {
		_ = r.sessMgr.Delete(g.sessionID)
		g.sessionID = ""
	}
	cfg := base
	cfg.Ctx = ctx
	sess, err := r.sessMgr.Create(cfg)
	if err != nil {
		return nil, err
	}
	g.sessionID = sess.ID
	slog.Debug("extension session up", "group", g.key, "target", g.target, "session_id", sess.ID)
	return sess, nil
}

// liveSession returns a session that is still running. DEAD sessions stay in the
// registry (their output stays readable), so presence is not liveness.
func (r *Reconciler) liveSession(id string) *session.Session {
	if id == "" || r.sessMgr == nil {
		return nil
	}
	sess := r.sessMgr.Get(id)
	if sess == nil || sess.Info().Status != api.SessionRunning {
		return nil
	}
	return sess
}

// attachedSession returns the running session this extension is attached to, or
// nil when the connection it was served by is gone or it is not attached yet.
func (r *Reconciler) attachedSession(st *extState) *session.Session {
	return r.liveSession(st.sessionOf())
}

// ensureMemberShell returns the extension's own channel in the session, opening
// one when it has none. A template that declares no shell (a read-only file
// window) is served by the connection alone and reports no channel; one whose
// channel is still running keeps it, so a retry after a tunnel failure does not
// restart the process.
func (r *Reconciler) ensureMemberShell(plan hostPlan, st *extState, sess *session.Session) (string, error) {
	if !plan.shell {
		return "", nil
	}
	if shell := liveShell(sess, st.shellOf()); shell != nil {
		return shell.ID, nil
	}
	shell, err := sess.CreateChildShell(plan.command, plan.args, plan.pty, plan.rows, plan.cols, plan.channel)
	if err != nil {
		return "", err
	}

	// A process that cannot start says so immediately. Waiting a beat turns "the
	// extension is unavailable" from a bare fact into the actual error line,
	// which is what makes the failure history worth reading.
	select {
	case <-shell.Done():
		return "", r.memberExit(sess, shell, "process exited during startup")
	case <-time.After(probeGrace):
	}
	// The window can close in the same instant the process ends, and a slow starter
	// that dies on its own schedule would otherwise be reported as up for one walk
	// and as a mystery afterwards. One more look is what keeps "started" honest.
	if liveShell(sess, shell.ID) == nil {
		return "", r.memberExit(sess, shell, "process exited during startup")
	}
	return shell.ID, nil
}

// memberExit closes a channel whose process ended before the extension could use
// it and returns the failure to record: what the process printed, which is the
// part that tells an author what to fix. The channel is closed rather than left
// behind because a failing extension retries, and one dead tab per attempt is how
// a shared session fills up with corpses.
func (r *Reconciler) memberExit(sess *session.Session, shell *session.ChildShell, fallback string) error {
	reason := exitReason(r.msgMgr, r.sessMgr, shell)
	_ = sess.CloseChildShell(shell.ID)
	if reason == "" {
		return errors.New(fallback)
	}
	return errors.New(reason)
}

// channelEndReason describes a channel that ended after the extension was up. The
// exit line is read from the channel itself — a shell that has exited keeps its
// transcript for exactly this kind of read — so a process that dies later is
// reported as informatively as one that dies during startup.
func (r *Reconciler) channelEndReason(sess *session.Session, st *extState) string {
	if sess != nil {
		if shell := sess.GetChildShell(st.shellOf()); shell != nil {
			if reason := exitReason(r.msgMgr, r.sessMgr, shell); reason != "" {
				return reason
			}
		}
	}
	return "the extension's process is no longer running"
}

// liveShell returns the extension's channel when it is still a running channel: a
// shell id that no longer resolves in this session, or one whose process has
// ended, is not a handle the extension can be driven through.
func liveShell(sess *session.Session, shellID string) *session.ChildShell {
	if sess == nil || shellID == "" {
		return nil
	}
	shell := sess.GetChildShell(shellID)
	if shell == nil || shell.Info().Status != api.SessionRunning {
		return nil
	}
	return shell
}

// detach closes what this extension holds on its group's session — its own
// channel and its tunnels — and forgets the handles. The session itself is left
// alone: it belongs to the group, another member may be using it, and one nobody
// is using is closed by the walk when the group loses its last member.
func (r *Reconciler) detach(st *extState) {
	sid, shellID, forwards := st.takeAttachment()
	if shellID != "" {
		if sess := r.sessMgr.Get(sid); sess != nil {
			_ = sess.CloseChildShell(shellID)
		}
	}
	r.closeForwards(forwards)
}

// closeForwards releases tunnels that a detach or a teardown is taking away.
func (r *Reconciler) closeForwards(forwards []ForwardStatus) {
	if r.forwardMgr == nil {
		return
	}
	for _, fw := range forwards {
		if fw.ForwardID != "" {
			r.forwardMgr.Close(fw.ForwardID)
		}
	}
}

// closeGroup closes the connection a group keeps and forgets it: the teardown for
// "nothing is served by this any more". The session is finalized rather than left
// DEAD — it is infrastructure, not a transcript anyone asked for, and a hidden
// dead session only accumulates on disk while the next member dials a fresh one.
func (r *Reconciler) closeGroup(g *extGroup) {
	g.mu.Lock()
	sid := g.sessionID
	g.sessionID = ""
	g.mu.Unlock()
	if sid == "" || r.sessMgr == nil {
		return
	}
	r.sessMgr.Terminate(sid, true, 0)
	_ = r.sessMgr.Delete(sid)
}

// onSessionDead is the DEAD subscription. A session that dies on its own — the
// transport dropped, the far end went away — takes every extension it was serving
// with it, and the reason is recorded so each status explains the drop instead of
// merely showing it.
func (r *Reconciler) onSessionDead(sessionID string) {
	served := false
	r.mu.Lock()
	states := make([]*extState, 0, len(r.states))
	for _, st := range r.states {
		states = append(states, st)
	}
	r.mu.Unlock()

	reason := "session ended"
	if sess := r.sessMgr.Get(sessionID); sess != nil {
		if info := sess.Info(); info.ExitCode != nil {
			reason = fmt.Sprintf("process exited with code %d", *info.ExitCode)
		}
	}
	for _, st := range states {
		// THE INSTANCES GO WITH THE CONNECTION, AND THEY SAY SO. They are channels
		// in it, so a handle to one of them is a handle to nothing — and an
		// on-demand extension has no attachment of its own for detachFrom to find,
		// so dropping the instances is the ONLY news this hook has for it. Leaving
		// the drop silent sent it to `idle` with no reason and no failure: the
		// caller's handle answered "not found", and the panel showed a healthy
		// on-demand card for a connection that had died.
		dropped := st.dropInstancesOnSession(sessionID)
		if !st.detachFrom(sessionID) && len(dropped) == 0 {
			// Not one of this session's extensions: the hook fans out to every
			// state, and only the ones attached have anything to say about it.
			continue
		}
		for _, shellID := range dropped {
			st.recordClosed(sessionID, shellID, reason)
		}
		served = true
		r.fail(st, "session", reason)
	}
	if served {
		// The failure armed its own retry timer; there is no pass to wake.
		_ = served
	}
	// The group keeps the dead session's id rather than clearing it: the next
	// attempt replaces the corpse with a live connection, and resolving "does this
	// group have a session" is a liveness question everywhere else.
}

// Status reports the runtime state of one extension. ok is false for a name the
// store does not hold.
func (r *Reconciler) Status(name string) (Status, bool) {
	// ONE STAT, NOT A DIRECTORY LISTING. This used to call store.List() — a ReadDir
	// plus a Stat per entry — to answer "is this name configured", so a caller
	// asking about every extension did N×(N+1) filesystem calls. `ls ext` with 80
	// templates was 6480 of them (issue #152); the reply was correct either way, and
	// on a machine that scans each one it was slower than the whole rest of the call.
	if !r.store.Exists(name) {
		return Status{}, false
	}
	st := r.state(name)
	r.reapEnded(name, st)
	return st.snapshot(), true
}

// reapEnded drops the channels whose process is gone, and says so.
//
// THIS IS A PULL, NOT A PUSH (issue #183). No walk ticks behind the reconciler
// any more, so a channel whose process exited is noticed by whoever asks about it
// next — which is the caller that was about to drive it. Nothing is lost by the
// delay: a caller reading the status gets the truth, and a caller reading the
// channel gets the exit reason from the read itself. The alternative was a
// goroutine polling every extension every five seconds to keep a line in a
// listing fresh.
func (r *Reconciler) reapEnded(name string, st *extState) {
	st.mu.Lock()
	empty, lazy := len(st.extra) == 0, st.lazy
	st.mu.Unlock()
	if empty || !lazy {
		return
	}
	sess := r.attachedSession(st)
	if sess == nil {
		return
	}
	changed := false
	for _, shellID := range st.extraShells() {
		if shellID == "" || liveShell(sess, shellID) != nil {
			// A template with no shell (a read-only file window) is served by the
			// connection itself, so its instance has no channel to test and no
			// process to outlive. Its liveness is the session's, which is the hook's
			// news — not this loop's.
			continue
		}
		if !st.dropExtra(shellID) {
			continue
		}
		reason := r.instanceEndReason(sess, shellID)
		st.recordFailure("session", reason)
		st.recordClosed(sessIDOf(sess), shellID, reason)
		st.mark(StatusUnavailable)
		slog.Debug("extension instance ended", "name", name, "shell_id", shellID, "reason", reason)
		changed = true
	}
	if changed {
		r.settle(name, st)
		r.notifyChange()
	}
}

// List reports every extension's runtime state, sorted the way the store sorts
// names.
func (r *Reconciler) List() []Status {
	names, err := r.store.List()
	if err != nil {
		return nil
	}
	out := make([]Status, 0, len(names))
	for _, n := range names {
		out = append(out, r.state(n).snapshot())
	}
	return out
}

// Retry forces an immediate attempt on an extension, clearing its backoff. Both
// the human and the agent reach for the same thing after fixing the underlying
// problem, and either should be served at once rather than at the next step. The
// connection the extension shares is kept: what a retry re-establishes is the
// extension's own channel and tunnels, and a session that is gone is rebuilt by
// the attempt itself.
func (r *Reconciler) Retry(name string) error {
	ext, err := r.store.Load(name)
	if err != nil {
		return err
	}
	if !ext.IsEnabled() {
		return errors.New("extension is disabled; enable it before retrying")
	}
	if ext.Lazy() {
		// ON DEMAND, "RETRY" MEANS "GIVE ME AN INSTANCE NOW". There is no channel for
		// the walk to repair — a lazy extension holds nothing until a caller asks —
		// so the retry is the ask, and it answers with the same failure the first
		// attempt did when the template is what is wrong (issue #183).
		_, err := r.Acquire(name, 0)
		return err
	}
	st := r.state(name)
	// A manual retry is an ask: for a resident extension it is what starts it again
	// after a failure the ladder gave up on. Nothing is woken here — the attempt is
	// started directly, which is what the walk used to do a tick later.
	st.want()
	r.detach(st)
	st.mark(StatusUnavailable)
	st.resetRetryBudget()
	r.retryNow(name)
	return nil
}

// retryNow starts one attempt for an extension the reconciler owns. It is what the
// retry timer, the panel's key and the MCP call all funnel through, so there is one
// place that decides whether an attempt is allowed to start.
func (r *Reconciler) retryNow(name string) {
	ext, err := r.store.Load(name)
	if err != nil || !ext.IsEnabled() {
		return
	}
	if ext.Lazy() {
		// On demand, an attempt is a caller's ask, and nobody asked here.
		return
	}
	st := r.state(name)
	if st.statusOf() == StatusProbing {
		return
	}
	if _, autoRetry := st.flags(); !autoRetry {
		/* The budget is spent. One condition is still worth re-checking without a
		   nudge: the target profile itself. Recreated by hand, it is a fix rather
		   than a transient, and reading it costs one store lookup instead of a dial.
		   A host whose profile resolves fine but is not answering keeps waiting for
		   the panel's key or the agent — that is the case the budget exists to stop. */
		if st.lastStage() != "target" || !r.rebuildable(ext) {
			return
		}
		st.resetRetryBudget()
		r.notifyChange()
	}
	if wait, ok := st.backoffPending(); ok && wait > 0 {
		// Still inside the window the failure printed. Re-arm rather than ignore, so
		// the deadline is kept even if this call arrived early.
		r.armRetry(st, wait)
		return
	}
	ctx := st.runContext()
	if ctx == nil {
		return
	}
	r.startProbe(ctx, name)
}

// backoffPending reports how much of the current backoff window is left.
func (st *extState) backoffPending() (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.readyAt.IsZero() {
		return 0, false
	}
	return time.Until(st.readyAt), true
}

// runCtx is the reconciler's run context, or nil before Start.
func (r *Reconciler) runCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctx
}

// runContext is the run this state belongs to, for calls that start an attempt.
func (st *extState) runContext() context.Context {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.ctx
}

// name is the extension this state is filed under, needed by the timers that have
// to look the template up again when they fire.
func (st *extState) name() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.extName
}

// Acquire is how a caller asks for an instance of an on-demand extension: it
// reuses one that has been silent long enough to be nobody's, or brings up a new
// one, and returns the handles (issue #183). It is the whole of `open ext <name>`.
//
// THREE BRANCHES, and they are the caller's rule verbatim:
//
//	one of the instances has been silent for the whole idle window
//
// t                            -> nobody is using it; take it
//
//	there is no instance at all   -> bring one up
//	they were all used recently   -> bring up another one
//
// A template that declares a forward is not on demand and has exactly one
// instance, so its callers all get that one; the reconciler owns its lifecycle
// and there is nothing to choose between.
//
// The wait bounds how long the caller waits for a DIAL, not whether it gets an
// instance: a host that is slow to answer returns `probing` with what to poll
// rather than a failure the caller would have to guess was not one.
func (r *Reconciler) Acquire(name string, wait time.Duration) (Status, error) {
	ext, err := r.store.Load(name)
	if err != nil {
		return Status{}, err
	}
	st, ok := r.Status(name)
	if !ok {
		return Status{}, fmt.Errorf("extension %q not found", name)
	}
	if !ext.IsEnabled() {
		return st, nil
	}
	limit, err := ext.IdleTimeoutDuration()
	if err != nil {
		// A template carrying an unreadable window is a broken template, and
		// refusing it is the only honest answer to a caller asking it to start one.
		return Status{}, err
	}
	if !ext.Lazy() {
		// Resident: the reconciler keeps exactly one instance and everyone shares
		// it. Wanting it is what starts it, and the attempt begins now rather than
		// at whatever moment a sweep would have come round.
		r.state(name).want()
		r.retryNow(name)
		return r.await(name, wait)
	}

	// EVERY ASK GETS ITS OWN CHANNEL — not a free one, not a reused one. A channel
	// somebody else may still be driving is the one thing that cannot be handed
	// over, and deciding "is that one free" costs more code than a fork does. The
	// process is the cheap part; callers never share a cursor.
	state := r.state(name)
	state.want()
	state.setIdleLimit(limit)
	sid, shellID, _, _, err := r.bringUp(context.Background(), ext, state, true)
	if err != nil {
		// The failure is already recorded and its retry armed; the caller's answer is
		// the status, which carries the reason.
		got, _ := r.Status(name)
		return got, nil
	}
	return r.instanceStatus(name, sid, shellID), nil
}

// closeInstances closes every instance of an on-demand extension, recording why.
// It is what an edit and a delete have in common: the processes that were running
// no longer match what the template says, so they go, and the handles that named
// them answer with the reason.
func (r *Reconciler) closeInstances(st *extState, keepFP, reason string) {
	sess := r.sessionOfRecord(st)
	for _, stale := range st.extraShellsNotBuiltFrom(keepFP) {
		if st.dropExtra(stale) {
			if sess != nil {
				_ = sess.CloseChildShell(stale)
			}
			st.recordClosed(sessIDOf(sess), stale, reason)
		}
	}
}

// sessionOfRecord returns the session object this extension names, live or not.
// Closing a channel is about the channel, not about the connection's health: a
// session that has gone DEAD still holds the shells that were in it, and a label
// held by a channel nobody closed is a label the next instance cannot have.
func (r *Reconciler) sessionOfRecord(st *extState) *session.Session {
	if id := st.sessionOf(); id != "" && r.sessMgr != nil {
		return r.sessMgr.Get(id)
	}
	return nil
}

// instanceEndReason reads why a channel ended, for the failure a caller sees. It
// is channelEndReason for a shell id the extension no longer reports as its own.
func (r *Reconciler) instanceEndReason(sess *session.Session, shellID string) string {
	if sess != nil {
		if shell := sess.GetChildShell(shellID); shell != nil {
			if reason := exitReason(r.msgMgr, r.sessMgr, shell); reason != "" {
				return reason
			}
		}
	}
	return "the instance's process is no longer running"
}

// bringUp is the only way an extension is brought to available: resolve the
// template, make sure the connection exists, open the channel it needs, and record
// what happened.
//
// FRESH ASKS FOR A NEW CHANNEL RATHER THAN THE EXTENSION'S OWN (issue #183). An
// on-demand caller gets a process nobody else is driving; the reconciler's own
// extension reuses the channel it already has, so a retry after a tunnel failure
// does not restart the process. Everything else — the resolve, the dial, the
// failure stages, the budget, the status — is the same work, which is why it is one
// function: two copies of it is how the two paths start disagreeing about what
// "dial failed" means.
func (r *Reconciler) bringUp(ctx context.Context, ext *Extension, st *extState, fresh bool) (sessionID, shellID, label, stage string, err error) {
	name := st.name()
	plan, err := r.resolve(ext)
	if err != nil {
		// The stage is the diagnosis: a template that does not resolve is fixed by
		// editing it, not by waiting, so the budget is spent rather than climbed.
		r.failForGood(st, "target", err.Error())
		return "", "", "", "target", err
	}
	g := r.groupFor(st)
	if g == nil {
		// Nothing has registered this extension's group yet: the first ask can arrive
		// before the first sync.
		g = r.group(groupKey(ext), ext)
	}
	sess, err := r.ensureGroupSession(ctx, g, plan.config)
	if err != nil {
		reason := sshclient.DescribeDialError(err)
		r.fail(st, "dial", reason)
		return "", "", "", "dial", errors.New(reason)
	}

	if fresh {
		label = ""
		if plan.shell {
			label = st.nextLabel(ext)
			shell, cerr := sess.CreateChildShell(plan.command, plan.args, plan.pty, plan.rows, plan.cols, label)
			if cerr != nil {
				r.fail(st, "start", cerr.Error())
				return "", "", "", "start", cerr
			}
			shellID = shell.ID
		}
		// A template with no shell is served by the connection alone: a read-only file
		// window has no channel to hand out, and its instance is the session.
		st.addExtra(sess.ID, shellID, label, fingerprint(ext))
		r.armIdle(name, st)
		st.mark(StatusAvailable)
		st.resetRetryBudget()
		r.notifyChange()
		slog.Debug("extension instance up", "name", name, "session_id", sess.ID, "shell_id", shellID)
		return sess.ID, shellID, label, "", nil
	}

	shellID, err = r.ensureMemberShell(plan, st, sess)
	if err != nil {
		// The connection is up and shared; only this extension's process failed to
		// start, so the retry is a channel open rather than a redial.
		r.fail(st, "start", err.Error())
		return "", "", "", "start", err
	}
	st.attach(sess.ID, shellID)
	// A fresh instance starts with a fresh clock, or the first sync after it came up
	// reads the moment of the ASK as its last use and reaps it early.
	st.touch()

	if err := r.ensureForwards(ext, st); err != nil {
		// The session is up and usable; only the tunnel is missing, so the retry
		// targets the forward alone instead of tearing the connection down.
		r.fail(st, "forward", err.Error())
		return sess.ID, shellID, "", "forward", err
	}
	// Proven up: the budget is whole again, so a later failure starts the ladder over
	// rather than resuming a count from before the extension was working.
	st.resetRetryBudget()
	st.mark(StatusAvailable)
	slog.Debug("extension available", "name", name, "session_id", sess.ID, "shell_id", shellID)
	r.notifyChange()
	return sess.ID, shellID, "", "", nil
}

// nextLabel names an instance's channel. The label has to be unique inside the
// session (#188 refuses a duplicate), and it is what a human reading `ls <session>`
// sees, so the first instance keeps the extension's own name and the rest count up
// from there. The counter never goes back, for the reason it exists.
func (st *extState) nextLabel(ext *Extension) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.labels++
	if st.labels == 1 {
		return ext.Name
	}
	return fmt.Sprintf("%s#%d", ext.Name, st.labels)
}

// instanceStatus renders a status for the instance just handed out: the
// extension's own state with THIS caller's handles on it, which is what the
// reply needs and what `Status` alone cannot say when there is more than one.
func (r *Reconciler) instanceStatus(name, sessionID, shellID string) Status {
	got, _ := r.Status(name)
	got.SessionID, got.ShellID = sessionID, shellID
	return got
}

// await polls until the extension has an answer: up, or down with a reason. Used
// by the resident path, where the walk owns the dial.
func (r *Reconciler) await(name string, wait time.Duration) (Status, error) {
	deadline := time.Now().Add(wait)
	for {
		got, ok := r.Status(name)
		if !ok {
			return Status{}, fmt.Errorf("extension %q not found", name)
		}
		switch got.Status {
		case StatusAvailable, StatusDisabled:
			return got, nil
		case StatusUnavailable:
			// Down WITH a reason is an answer; down without one is still the first
			// attempt. The distinction is what the caller does next: report the
			// failure, or keep waiting.
			if len(got.Failures) > 0 {
				return got, nil
			}
		}
		if time.Now().After(deadline) {
			return got, nil
		}
		time.Sleep(acquirePoll)
	}
}

// acquirePoll is how often Acquire re-reads the status. The walk itself runs on
// the loop interval; this only decides how quickly the answer is noticed, and a
// dial to a healthy host answers in tens of milliseconds.
const acquirePoll = 25 * time.Millisecond

// sweepInstances is the walk's whole job for an on-demand extension: check that
// the channels it handed out are still there, close the ones whose silence has run
// out, and say what the extension is holding (issue #183).
//
// It never opens anything. An instance exists because a caller asked for one, and
// the only thing that can happen to it here is that it ends — because its process
// died, or because nobody has touched it for the whole idle window.
func (r *Reconciler) sweepInstances(name string, ext *Extension, st *extState, limit time.Duration) {
	// THE CHANNELS THAT ENDED ARE DROPPED, NOT REOPENED. That is the difference
	// between an extension's own channel — infrastructure the reconciler repairs —
	// and a caller's instance, which is a lease it lets lapse. The body is
	// reapEnded's: a status read does the same thing, and one of them having a
	// special case the other lacks is how a listing starts lying.
	r.reapEnded(name, st)
	if r.reapDue(name, st, limit) > 0 {
		r.notifyChange()
	}
	r.settle(name, st)
}

// reapDue closes the channels whose silence has run out and reports how many it
// closed. Both the timer and the walk call it: the timer because the window is a
// promise, the walk because a promise kept only by a timer is one missed callback
// away from a process nobody will ever close.
func (r *Reconciler) reapDue(name string, st *extState, limit time.Duration) int {
	due := st.dueExtras(limit)
	if len(due) == 0 {
		return 0
	}
	sess := r.sessionOfRecord(st)
	for _, shellID := range due {
		if sess != nil {
			_ = sess.CloseChildShell(shellID)
		}
		st.recordClosed(sessIDOf(sess), shellID,
			"idle for "+limit.Round(time.Second).String()+" with nothing asked of it")
		slog.Info("extension instance idled out", "name", name, "shell_id", shellID)
	}
	return len(due)
}

// armIdle points the extension's timer at its earliest silence deadline. Called
// wherever a channel is opened, renewed or dropped, because those are the only
// moments the answer changes.
func (r *Reconciler) armIdle(name string, st *extState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	limit := st.idleLimit
	if limit <= 0 || len(st.extra) == 0 {
		return
	}
	var next time.Time
	for _, ex := range st.extra {
		if d := ex.lastUse.Add(limit); next.IsZero() || d.Before(next) {
			next = d
		}
	}
	st.timer = time.AfterFunc(time.Until(next), func() {
		if st.ctx != nil && st.ctx.Err() != nil {
			return
		}
		r.idleFired(name)
	})
}

// stopIdle disarms the timer for a state that is going away.
func (st *extState) stopIdle() {
	st.mu.Lock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.mu.Unlock()
}

// idleFired is the window running out. It closes what is due and re-arms for the
// next channel, rather than closing everything: channels opened at different
// moments run out at different moments.
func (r *Reconciler) idleFired(name string) {
	ext, err := r.store.Load(name)
	if err != nil || !ext.Lazy() {
		return
	}
	limit, err := ext.IdleTimeoutDuration()
	if err != nil {
		return
	}
	st := r.state(name)
	if r.reapDue(name, st, limit) > 0 {
		r.settle(name, st)
		r.notifyChange()
	}
	r.armIdle(name, st)
}

// settle makes the reported status follow what is actually held: something up is
// available, nothing up is idle ("nothing is running because nothing asked"), and
// a recorded failure outranks both.
func (r *Reconciler) settle(name string, st *extState) {
	switch {
	case len(st.snapshot().Failures) > 0 && st.instanceCount() == 0:
		// leave the failure standing
	case st.instanceCount() > 0:
		if st.statusOf() != StatusAvailable {
			st.mark(StatusAvailable)
			r.notifyChange()
		}
	default:
		if st.statusOf() != StatusIdle {
			st.mark(StatusIdle)
			r.notifyChange()
		}
	}
}

// sessIDOf is the session id of a live session, or empty for none.
func sessIDOf(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	return sess.ID
}

// Touch renews the idle clock of whichever instance this call addressed. It is
// called wherever a tool call resolves a session or a channel, because that is the
// only moment the daemon learns that somebody is still driving an on-demand
// extension — the alternative is reaping a channel in the middle of a read.
//
// The shell id is the precise answer and is tried first; a call that named only
// the session renews every instance on it, which is the honest reading of "this
// connection is in use". A session that serves no extension, or one that is not
// on demand, is a no-op.
func (r *Reconciler) Touch(shellID, sessionID string) {
	if shellID == "" && sessionID == "" {
		return
	}
	// The name comes along because renewing has to re-arm that extension's timer,
	// and the timer goes back through the store by name.
	r.mu.Lock()
	pairs := make(map[string]*extState, len(r.states))
	for name, st := range r.states {
		pairs[name] = st
	}
	r.mu.Unlock()
	for name, st := range pairs {
		if shellID != "" && st.renew(shellID) {
			r.armIdle(name, st)
			return
		}
		if sessionID != "" && st.renewSession(sessionID) {
			r.armIdle(name, st)
			return
		}
	}
}

// Release hands back an instance a caller is done with. It is the explicit half of
// the idle window: the caller that knows it has finished says so, and the instance
// stops costing a process on the target immediately instead of when its silence
// runs out (issue #183).
//
// The shell id names which instance; an empty one releases every instance of the
// extension, which is what "I am done with this extension" means when the caller
// did not keep the handle. It reports how many were released, so the reply can say
// whether the caller's handle was one of them.
func (r *Reconciler) Release(name, shellID string) (int, error) {
	ext, err := r.store.Load(name)
	if err != nil {
		return 0, err
	}
	if !ext.Lazy() {
		// A resident extension's connection is the reconciler's, not the caller's:
		// its template declares a listener, so there is nothing to hand back.
		return 0, errors.New("this extension is not on demand: it declares a forward, so its connection stays up")
	}
	st := r.state(name)
	var targets []string
	if shellID != "" {
		if !st.hasExtra(shellID) {
			return 0, fmt.Errorf("extension %q is not running an instance with shell id %q", name, shellID)
		}
		targets = []string{shellID}
	} else {
		targets = st.extraShells()
	}
	sess := r.sessionOfRecord(st)
	n := 0
	for _, id := range targets {
		if !st.dropExtra(id) {
			continue
		}
		if sess != nil {
			_ = sess.CloseChildShell(id)
		}
		st.recordClosed(sessIDOf(sess), id, "released by the caller")
		n++
	}
	if st.instanceCount() == 0 && st.statusOf() == StatusAvailable {
		st.mark(StatusIdle)
	}
	r.armIdle(name, st)
	r.notifyChange()
	return n, nil
}

// hasExtra reports whether this shell id names one of the extension's instances.
// anyInstanceOn reports whether one of this extension's instances runs on the
// session, which is what "this call is about this extension" means for a lazy one.
func (st *extState) anyInstanceOn(sessionID string) bool {
	for _, ex := range st.extra {
		if ex.sessionID == sessionID {
			return true
		}
	}
	return false
}

func (st *extState) hasExtra(shellID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, ex := range st.extra {
		if ex.shellID == shellID {
			return true
		}
	}
	return false
}

// ExplainClosed answers "what happened to the handle I was using" for a session
// an on-demand extension gave up. It is what turns the bare not-found reply into
// the fact and the way forward (issue #183).
func (r *Reconciler) ExplainClosed(sessionID, shellID string) (name, reason string, at time.Time, ok bool) {
	if sessionID == "" && shellID == "" {
		return "", "", time.Time{}, false
	}
	r.mu.Lock()
	states := make(map[string]*extState, len(r.states))
	for n, st := range r.states {
		states[n] = st
	}
	r.mu.Unlock()
	for n, st := range states {
		st.mu.Lock()
		match := (sessionID != "" && st.closedSessionID == sessionID) ||
			(shellID != "" && st.closedShellID == shellID)
		reason, at = st.closedReason, st.closedAt
		st.mu.Unlock()
		if match {
			return n, reason, at, true
		}
	}
	return "", "", time.Time{}, false
}

// state returns the state for name, creating it on first sight. An extension
// starts unavailable by design: termcp has to prove an extension works before it
// reports it as ready to use.
func (r *Reconciler) state(name string) *extState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[name]; ok {
		return st
	}
	st := &extState{status: StatusUnavailable, autoRetry: true, extName: name}
	r.states[name] = st
	return st
}

// extState is the mutable runtime half of one extension. Every field behind its
// own mutex because a probe goroutine writes it while the walk reads it.
type extState struct {
	mu        sync.Mutex
	status    string
	groupKey  string // the connection this extension is served by; "" = none
	sessionID string // the group's session, while this extension is attached
	shellID   string // this extension's own channel; "" for a template with no shell
	forwards  []ForwardStatus
	failures  []Failure
	// consecutive counts consecutive failures, for the backoff ladder.
	consecutive int
	readyAt     time.Time // earliest moment a retry may run
	autoRetry   bool      // the automatic budget is unspent; false once it is
	fingerprint string    // last template applied; a change rebuilds what it holds

	// Lazy and the two fields under it are the on-demand half (issue #183): lazy
	// says the reconciler never starts this extension by itself, idleLimit is the
	// window it was given, and lastUse is when anything last asked for or drove an
	// instance of it.
	lazy      bool
	idleLimit time.Duration
	lastUse   time.Time

	// What was taken away, for a caller still holding the handle: "not found" is
	// true and useless where "this was closed at 14:02 because it was idle for 10m"
	// is the answer to the question the caller actually has.
	closedAt        time.Time
	closedSessionID string
	closedShellID   string
	closedReason    string

	// labels counts the instances this extension has EVER opened in this run. It
	// only ever goes up, because a label must not be reused while the channel that
	// held it may still exist: deriving it from the instance count gave the second
	// instance the first one's name the moment the first was dropped from the
	// bookkeeping but not from the session, and the session refused it (#188).
	labels int

	// extra are the channels this extension has handed out. Each is one caller's
	// own process on the group's connection, so a second caller costs a fork, not a
	// dial — and no two callers ever share a cursor (issue #183).
	extra []*extraChannel

	// ctx is the run this state belongs to. The deadline callback checks it, so a
	// timer that was already scheduled when Stop ran does nothing instead of closing
	// a channel on a reconciler nobody is watching — which is why Stop does not have
	// to walk every state and disarm them.
	ctx context.Context

	// extName is what this state is filed under. The timers need it: they fire
	// outside any call, and have to read the template again to decide.
	extName string

	// retry is the pending automatic attempt, or nil. One timer, replaced on every
	// failure — the ladder is a deadline the status prints, so it must be kept.
	retry *time.Timer

	// timer fires when the earliest channel's silence runs out. It is what makes
	// idle_timeout a promise instead of a walk-tick approximation: a caller that
	// asked for ten minutes gets ten, not ten and a bit because the sweep happened
	// to be five seconds away. The walk keeps its own check as a backstop — a timer
	// that fails to fire must not keep a process alive for ever.
	timer *time.Timer
}

// setLazy records whether the reconciler starts this extension on demand, and
// the window it was given. Called by every walk, so a template that gains or
// loses its forwards changes the answer without any other bookkeeping.
func (st *extState) setRun(ctx context.Context) {
	st.mu.Lock()
	st.ctx = ctx
	st.mu.Unlock()
}

func (st *extState) setLazy(lazy bool, limit time.Duration) {
	st.mu.Lock()
	st.lazy, st.idleLimit = lazy, limit
	st.mu.Unlock()
}

// setIdleLimit records the window without touching anything else, for the calls
// that arrive before the first walk has run (an Acquire on a cold daemon).
func (st *extState) setIdleLimit(limit time.Duration) {
	st.mu.Lock()
	st.idleLimit = limit
	st.mu.Unlock()
}

// want stamps the clock and reports whether this is a fresh ask, which is what
// decides between "already up" and "starting" on the resident path.
func (st *extState) want() (fresh bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	fresh = st.lastUse.IsZero()
	st.lastUse = time.Now()
	return fresh
}

// touch renews the clock: something addressed this extension just now. It does
// NOT set wanted — an extension nobody asked for is not kept up by a stray read
// of a retained transcript.
func (st *extState) touch() {
	st.mu.Lock()
	st.lastUse = time.Now()
	st.mu.Unlock()
}

// idleFor reports how long it has been since the extension was last asked for or
// addressed, and false when it has never been (nothing to reap).
func (st *extState) idleFor() (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastUse.IsZero() {
		return 0, false
	}
	return time.Since(st.lastUse), true
}

// recordClosed remembers the instance that was taken away, so the handle the
// caller is still holding has an answer.
func (st *extState) recordClosed(sessionID, shellID, reason string) {
	st.mu.Lock()
	st.closedAt = time.Now()
	st.closedSessionID, st.closedShellID, st.closedReason = sessionID, shellID, reason
	st.mu.Unlock()
}

// renewSession stamps every instance served by this session, for the tool calls
// that name a session rather than one channel. It reports whether the session is
// this extension's.
func (st *extState) renewSession(sessionID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessionID != sessionID && !st.anyInstanceOn(sessionID) {
		return false
	}
	st.lastUse = time.Now()
	for _, ex := range st.extra {
		if ex.sessionID == sessionID {
			ex.lastUse = st.lastUse
		}
	}
	return true
}

// changed reports whether the template differs from the one this extension was
// last brought up from, and remembers the new one. A change rebuilds what the
// extension holds — its channel and its tunnels — without touching the connection
// it shares, which is deliberately coarse: deciding which fields are cosmetic is
// how a live channel drifts from its template.
func (st *extState) changed(ext *Extension) bool {
	fp := fingerprint(ext)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fingerprint == fp {
		return false
	}
	st.fingerprint = fp
	return true
}

// fingerprint summarizes the template as a comparable string. The target and the
// whitelists are in it as well as in the group key, so a template that moved to
// another connection is also seen as changed — the two are independent readings of
// the same edit.
func fingerprint(ext *Extension) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%q|", ext.Kind, ext.Target, ext.Mode, ext.Command)
	for _, a := range ext.Args {
		fmt.Fprintf(&b, "%q,", a)
	}
	// The endpoint and its headers describe what reaches the channel as much as the
	// command does — the URL form's argv is derived from them, so an edit here is
	// an edit to what runs.
	fmt.Fprintf(&b, "|%q|", ext.URL)
	for _, name := range sortedKeys(ext.Headers) {
		fmt.Fprintf(&b, "%q=%q,", name, ext.Headers[name])
	}
	b.WriteByte('|')
	for _, a := range ext.Allow {
		b.WriteString(a)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, p := range ext.Paths {
		b.WriteString(p)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, f := range ext.Forwards {
		fmt.Fprintf(&b, "%s:%s:%s:%d:%s:%d|", f.Kind, f.BindHost, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort)
	}
	fmt.Fprintf(&b, "|%d|%d|%t", ext.Rows, ext.Cols, ext.IsEnabled())
	return b.String()
}

// setGroup records the connection this extension is served by and reports whether
// it moved. The first resolution is not a move: nothing was held yet.
func (st *extState) setGroup(key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.groupKey == key {
		return false
	}
	moved := st.groupKey != ""
	st.groupKey = key
	return moved
}

// leaveGroup forgets the connection this extension was served by and reports
// whether it had one.
func (st *extState) leaveGroup() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	had := st.groupKey != ""
	st.groupKey = ""
	return had
}

func (st *extState) groupOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.groupKey
}

// extraChannel is one instance of an on-demand extension: a caller's channel on
// the group's connection, plus the clock that says whether anybody is still
// driving it.
type extraChannel struct {
	// sessionID is the connection this instance runs on. It is per instance rather
	// than per extension because an instance outlives a detach: an edit closes the
	// channels built from the old template and clears the extension's own
	// attachment, and the instances a caller still holds keep naming the connection
	// they are on.
	sessionID string
	shellID   string
	label     string
	lastUse   time.Time
	// fp is the template this channel was built from. An edit closes the channels
	// built from the OLD one — and only those, so a channel a caller asked for after
	// the edit survives the walk that noticed it. Without this the walk closes the
	// very channel the caller just received, and a caller that asks again after an
	// edit gets nothing but the edit.
	fp string
}

// addExtra records a channel handed to a caller. The session comes with it: an
// on-demand extension's instances all live on the group's connection, so the first
// one is also what names the connection the status reports.
func (st *extState) addExtra(sessionID, shellID, label, fp string) {
	st.mu.Lock()
	if st.sessionID == "" {
		// The first channel also names the connection, and the name OUTLIVES the
		// channel: the connection is kept for the next caller, so "which session is
		// this extension served by" must still answer after the last channel goes.
		st.sessionID = sessionID
	}
	st.extra = append(st.extra, &extraChannel{sessionID: sessionID, shellID: shellID, label: label, lastUse: time.Now(), fp: fp})
	st.mu.Unlock()
}

// dropInstancesOnSession forgets every instance served by a connection that is
// gone. It reports whether there were any.
func (st *extState) dropInstancesOnSession(sessionID string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.extra[:0]
	var dropped []string
	for _, ex := range st.extra {
		if ex.sessionID == sessionID {
			dropped = append(dropped, ex.shellID)
			continue
		}
		kept = append(kept, ex)
	}
	st.extra = kept
	return dropped
}

// renew stamps the instance this shell id names, for the tool calls that keep
// using it. It reports whether the shell is one of this extension's instances.
func (st *extState) renew(shellID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, ex := range st.extra {
		if ex.shellID == shellID {
			ex.lastUse = time.Now()
			return true
		}
	}
	return false
}

// dropExtra forgets one instance and reports whether it had it.
func (st *extState) dropExtra(shellID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, ex := range st.extra {
		if ex.shellID == shellID {
			st.extra = append(st.extra[:i], st.extra[i+1:]...)
			return true
		}
	}
	return false
}

// dueExtras removes and returns the instances whose silence has run out. Taking
// them out of the state is what keeps the walk from handing one out while it is
// being closed.
func (st *extState) dueExtras(limit time.Duration) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	kept := st.extra[:0]
	var due []string
	for _, ex := range st.extra {
		// A channel-less instance is the connection, and the connection is kept for
		// the next caller — so there is nothing for a window to run out on.
		if ex.shellID != "" && now.Sub(ex.lastUse) >= limit {
			due = append(due, ex.shellID)
			continue
		}
		kept = append(kept, ex)
	}
	st.extra = kept
	return due
}

// extraShells lists the instances beyond the primary, for the walk's liveness
// check.
// extraShellsNotBuiltFrom lists the channels whose template is not keepFP: the
// ones an edit invalidated, as opposed to the ones a caller asked for afterwards.
func (st *extState) extraShellsNotBuiltFrom(keepFP string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, ex := range st.extra {
		if ex.fp != keepFP {
			out = append(out, ex.shellID)
		}
	}
	return out
}

func (st *extState) extraShells() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.extra))
	for _, ex := range st.extra {
		out = append(out, ex.shellID)
	}
	return out
}

func (st *extState) instanceCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := len(st.extra)
	if st.shellID != "" {
		n++
	}
	return n
}

// sessionOf and shellOf describe the extension's FIRST instance, which is what
// the group, the session-death hook and the status surface need: they answer
// "which connection carries this extension" rather than "which channel is this
// caller using".
func (st *extState) sessionOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessionID != "" {
		return st.sessionID
	}
	// An on-demand extension has no attachment of its own: its connection is the one
	// its first instance runs on.
	if len(st.extra) > 0 {
		return st.extra[0].sessionID
	}
	return ""
}

func (st *extState) shellOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.shellID
}

// attach records the session and the channel this extension is now driven
// through.
func (st *extState) attach(sessionID, shellID string) {
	st.mu.Lock()
	st.sessionID = sessionID
	st.shellID = shellID
	st.mu.Unlock()
}

// takeAttachment removes and returns what this extension holds, so a detach can
// close it without holding the lock while it does.
func (st *extState) takeAttachment() (sessionID, shellID string, forwards []ForwardStatus) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sessionID, shellID, forwards = st.sessionID, st.shellID, st.forwards
	st.sessionID, st.shellID, st.forwards = "", "", nil
	return sessionID, shellID, forwards
}

// detachFrom forgets the attachment when it is the one named, and reports whether
// it was.
func (st *extState) detachFrom(sessionID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessionID != sessionID {
		return false
	}
	st.sessionID, st.shellID, st.forwards = "", "", nil
	return true
}

// mark sets the status.
func (st *extState) mark(status string) {
	st.mu.Lock()
	st.status = status
	st.mu.Unlock()
}

// statusOf and flags are the two reads the walk makes on one extension: the
// status alone, and the status together with the retry budget that decides
// whether the walk may act on it.
func (st *extState) statusOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status
}

func (st *extState) flags() (status string, autoRetry bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status, st.autoRetry
}

// scheduleBackoff reports how long to wait before the next attempt, and whether
// there is one at all. Once the budget is spent it schedules nothing, so nothing
// keeps probing forever: the timer is the only thing that would.
func (st *extState) scheduleBackoff() (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.consecutive > maxAutoRetries {
		st.autoRetry = false
		st.readyAt = time.Time{}
		return 0, false
	}
	step := backoffSteps[0]
	if st.consecutive > 1 {
		i := st.consecutive - 1
		if i >= len(backoffSteps) {
			i = len(backoffSteps) - 1
		}
		step = backoffSteps[i]
	}
	st.readyAt = time.Now().Add(step)
	return step, true
}

// armRetry schedules the next attempt. Bound to the run's context for the same
// reason the idle deadline is: a callback that fires after Stop must do nothing.
func (r *Reconciler) armRetry(st *extState, wait time.Duration) {
	st.mu.Lock()
	if st.retry != nil {
		st.retry.Stop()
		st.retry = nil
	}
	ctx := st.ctx
	st.mu.Unlock()
	if wait <= 0 {
		return
	}
	t := time.AfterFunc(wait, func() {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		name := st.name()
		if name == "" {
			return
		}
		ext, err := r.store.Load(name)
		if err != nil || !ext.IsEnabled() {
			return
		}
		r.retryNow(name)
	})
	st.mu.Lock()
	st.retry = t
	st.mu.Unlock()
}

// exhaustRetryBudget spends the automatic budget without scheduling a retry.
// It is the answer to a failure no retry can fix, where recovery is something
// else entirely — for a template that cannot resolve, the walk's rebuildable
// check. Climbing the ladder there would only stretch the wait, and leaving the
// budget whole would re-probe every walk for a condition the probe cannot
// change.
func (st *extState) exhaustRetryBudget() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.autoRetry = false
	st.readyAt = time.Time{}
}

// resetRetryBudget hands the extension its automatic retries back. Called on the
// paths that mean "this is a fresh start": a probe that succeeded, a manual
// retry, a template edit.
func (st *extState) resetRetryBudget() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.consecutive = 0
	st.autoRetry = true
	st.readyAt = time.Time{}
}

// lastStage is the stage of the most recent failure, empty when there is no
// history. It tells the spent-budget path what it is waiting on.
func (st *extState) lastStage() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := len(st.failures); n > 0 {
		return st.failures[n-1].Stage
	}
	return ""
}

// recordFailure appends a reason, deduplicating an identical consecutive entry
// so a permanently broken extension does not bury its history under one repeated
// line while the count still climbs.
func (st *extState) recordFailure(stage, reason string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.failures) > 0 {
		last := st.failures[len(st.failures)-1]
		if last.Stage == stage && last.Reason == reason {
			st.consecutive++
			return
		}
	}
	st.failures = append(st.failures, Failure{
		At:     clock.Now(),
		Stage:  stage,
		Reason: truncate(reason, 300),
	})
	if len(st.failures) > maxFailures {
		st.failures = append([]Failure(nil), st.failures[len(st.failures)-maxFailures:]...)
	}
	st.consecutive++
}

func (st *extState) snapshot() Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := Status{
		Status:    st.status,
		SessionID: st.sessionID,
		ShellID:   st.shellID,
		Lazy:      st.lazy,
		Instances: len(st.extra),
	}
	// THE CHANNEL REPORTED IS ONE THE CALLER CAN DRIVE. For a template with its own
	// channel that is the reconciler's; for an on-demand extension it is the first
	// instance it handed out, because there is no other channel to name.
	if st.shellID != "" {
		out.Instances++
	} else if len(st.extra) > 0 {
		out.ShellID = st.extra[0].shellID
	}
	if out.SessionID == "" && len(st.extra) > 0 {
		out.SessionID = st.extra[0].sessionID
	}
	if st.lazy {
		out.IdleTimeoutMs = st.idleLimit.Milliseconds()
		if !st.lastUse.IsZero() {
			out.IdleForMs = time.Since(st.lastUse).Milliseconds()
		}
	}
	if !st.closedAt.IsZero() {
		out.ClosedAt = st.closedAt.UnixMilli()
		out.ClosedSessionID, out.ClosedShellID, out.ClosedReason = st.closedSessionID, st.closedShellID, st.closedReason
	}
	if len(st.forwards) > 0 {
		out.Forwards = append([]ForwardStatus(nil), st.forwards...)
	}
	if len(st.failures) > 0 {
		out.Failures = append([]Failure(nil), st.failures...)
	}
	if !st.readyAt.IsZero() {
		out.NextRetry = st.readyAt.UnixMilli()
	}
	return out
}

// ensureForwards establishes the forwards the template declares, skipping the
// ones still listening. Returning an error leaves the session standing: a tunnel
// is a capability of the extension, not its reason to exist.
func (r *Reconciler) ensureForwards(ext *Extension, st *extState) error {
	if r.forwardMgr == nil || len(ext.Forwards) == 0 {
		return nil
	}
	st.mu.Lock()
	if st.sessionID == "" {
		st.mu.Unlock()
		return errors.New("no session to attach forwards to")
	}
	sessID := st.sessionID
	existing := append([]ForwardStatus(nil), st.forwards...)
	st.mu.Unlock()

	live := map[string]bool{}
	for _, fw := range r.forwardMgr.ListBySession(sessID) {
		live[fw.ForwardID] = true
	}

	sess := r.sessMgr.Get(sessID)
	if sess == nil {
		return errors.New("session for forwards is gone")
	}
	info := sess.Info()
	sshCli := sess.SSHClient()

	built := make([]ForwardStatus, len(ext.Forwards))
	// A forward's position in the declaration is what makes it the same forward
	// across walks, so the slice is laid out by index and a missing entry is an empty
	// one. Keeping what already worked when a later entry fails is what makes the
	// retry target only the one that did not: recording nothing until the whole list
	// succeeded left the earlier listeners live but unrecorded, so the next attempt
	// rebuilt them onto a port it already held and the extension could never come
	// back up.
	for i, f := range ext.Forwards {
		if i < len(existing) && live[existing[i].ForwardID] {
			built[i] = existing[i]
			continue
		}
		fw, err := r.createForward(f, sess, sshCli, info)
		if err != nil {
			// Record what this attempt did establish before reporting the failure:
			// the walk reads these entries to decide what is already listening.
			st.mu.Lock()
			st.forwards = append([]ForwardStatus(nil), built...)
			st.mu.Unlock()
			return fmt.Errorf("%s forward on %s: %w", f.Kind, listenSpec(f), err)
		}
		// Tear down with the session rather than outliving it as a dangling
		// listener, the same way the forward tool attaches its own.
		sess.AttachCleanup(func() { r.forwardMgr.Close(fw.ForwardID) })
		built[i] = ForwardStatus{
			Kind:       f.Kind,
			ForwardID:  fw.ForwardID,
			ListenAddr: fw.ListenAddr,
			TargetAddr: fw.TargetAddr,
		}
	}

	st.mu.Lock()
	st.forwards = built
	st.mu.Unlock()
	return nil
}

func (r *Reconciler) createForward(f ForwardDecl, sess *session.Session, sshCli *ssh.Client, info api.Session) (*forward.ForwardInfo, error) {
	switch f.Kind {
	case ForwardDynamic:
		return r.forwardMgr.CreateDynamic(sess.ID, info.Name, f.BindHost, f.LocalPort, sshCli, info.SSHEndpoint == "internal")
	case ForwardLocal:
		return r.forwardMgr.CreateLocal(sess.ID, info.Name, f.BindHost, f.RemoteHost, f.RemotePort, f.LocalPort, sshCli)
	case ForwardRemote:
		return r.forwardMgr.CreateRemote(sess.ID, info.Name, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort, sshCli)
	}
	return nil, fmt.Errorf("unknown forward kind %q", f.Kind)
}

// exitReason reads the last bytes a dying process produced, so a startup failure
// carries the error line the process printed instead of only its exit code.
func exitReason(msgMgr *message.Manager, sessMgr *session.Manager, shell *session.ChildShell) string {
	if msgMgr == nil || sessMgr == nil || shell == nil {
		return ""
	}
	src, err := transcript.Resolve(sessMgr, msgMgr, shell.ID)
	if err != nil {
		return ""
	}
	w, err := transcript.Read(context.Background(), src, transcript.Params{
		TailBytes: true,
		MaxBytes:  failureTailBytes,
		MaxLines:  12,
		StripAnsi: true,
	})
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(w.Output)
	info := shell.Info()
	if info.ExitCode == nil {
		return text
	}
	if text == "" {
		return fmt.Sprintf("exit code %d", *info.ExitCode)
	}
	return fmt.Sprintf("exit code %d: %s", *info.ExitCode, text)
}

// listenSpec renders a forward's declaration for an error message.
func listenSpec(f ForwardDecl) string {
	if f.LocalPort > 0 {
		return fmt.Sprintf("%s:%d", f.LocalHost, f.LocalPort)
	}
	return "ephemeral port"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
