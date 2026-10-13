package extensions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
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
// The loop is deliberately passive about how an extension is used once it is up.
// Bringing an extension to available means creating its session from the stored
// template and proving the process can stay running; nothing else. An agent then
// drives that session with the ordinary shell/SFTP tools, gated by the session's
// Restriction. The reconciler never reads a skill or speaks JSON-RPC on the
// agent's behalf.
type Reconciler struct {
	store      *Store
	sessMgr    *session.Manager
	msgMgr     *message.Manager
	sshConfigs *sshconfig.Store
	forwardMgr *forward.ForwardManager

	// NoInternal refuses the built-in loopback profile as an extension target,
	// mirroring the flag the MCP server exposes.
	NoInternal bool

	mu       sync.Mutex
	states   map[string]*extState
	onChange func()

	trigger chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}

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
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	SessionID string          `json:"session_id,omitempty"`
	ShellID   string          `json:"shell_id,omitempty"`
	Forwards  []ForwardStatus `json:"forwards,omitempty"`
	Failures  []Failure       `json:"failures,omitempty"`
	NextRetry int64           `json:"next_retry,omitempty"` // Unix ms; 0 = no retry scheduled
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

// loopInterval is how often the reconciler re-walks the store even with no
// signal. It is the recovery path for conditions nothing notifies about — a
// profile recreated by hand, a forward that died quietly — so it is a backstop,
// not the primary driver.
const loopInterval = 5 * time.Second

// probeGrace is how long a started process gets to prove it can stay alive. A
// stdio MCP server with a bad command dies within milliseconds; a healthy one
// blocks on its first read forever, so the wait costs nothing for it.
const probeGrace = 700 * time.Millisecond

// failureTailBytes is the slice of output captured as the failure reason when a
// process exits during its grace window. It is short because it is a reason, not
// a transcript: the agent that wants the whole output reads the session itself.
const failureTailBytes = 512

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
		trigger:    make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if store != nil {
		store.SetOnChange(r.Trigger)
	}
	if sshConfigs != nil {
		// A profile written by hand is the fix a template failure was waiting on,
		// so the walk hears about it when it happens rather than at the next loop
		// tick.
		sshConfigs.SetOnChange(r.Trigger)
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
	if r.cancel != nil {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	if _, err := r.store.List(); err != nil {
		cancel()
		return err
	}
	go r.run(cctx)
	return nil
}

// Stop cancels the loop, waits for it to exit, and waits again for any probe it
// had in flight. Sessions that are already up stay where they are: they are
// ordinary sessions, and their own teardown follows process exit or the caller's
// shutdown.
func (r *Reconciler) Stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.probes.Wait()
	r.cancel = nil
}

// Trigger asks the reconciler to re-walk the store as soon as it can. It never
// blocks: the channel has room for one signal, and a second call while one is
// pending is the same walk anyway.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *Reconciler) run(ctx context.Context) {
	defer close(r.done)
	for {
		if err := r.reconcileAll(ctx); err != nil {
			slog.Warn("extension reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
		case <-time.After(loopInterval):
		}
	}
}

// reconcileAll walks the store once: anything absent from it is torn down,
// anything disabled is torn down and left alone, and anything else is pushed
// toward available. Reads of the store and of state are kept separate locks, so
// a slow dial in one probe goroutine never blocks this walk.
func (r *Reconciler) reconcileAll(ctx context.Context) error {
	names, err := r.store.List()
	if err != nil {
		return err
	}
	seen := map[string]bool{}

	for _, name := range names {
		seen[name] = true
		ext, err := r.store.Load(name)
		if err != nil {
			// A file that no longer validates is reported through its own status
			// rather than dropped: silently ignoring a broken extension would let
			// it stay "unavailable forever, reason unknown".
			st := r.state(name)
			st.mark(StatusUnavailable)
			st.recordFailure("target", "extension config is no longer valid: "+err.Error())
			r.notifyChange()
			continue
		}
		st := r.state(name)
		if !ext.IsEnabled() {
			if st.statusOf() != StatusDisabled {
				r.teardown(st, StatusDisabled)
				r.notifyChange()
			}
			continue
		}
		if st.changed(ext) {
			r.teardown(st, StatusUnavailable)
			// An edit is an author's nudge: it is what "change the command and see if
			// it works now" looks like, so the automatic retries come with it.
			st.resetRetryBudget()
			r.notifyChange()
		}
		status, autoRetry := st.flags()
		switch status {
		case StatusProbing:
			continue // an attempt is already in flight
		case StatusAvailable:
			if !st.alive(r.sessMgr) {
				r.teardown(st, StatusUnavailable)
				st.recordFailure("session", "session is no longer running")
				r.notifyChange()
				continue
			}
			if err := r.ensureForwards(ext, st); err != nil {
				st.recordFailure("forward", err.Error())
				r.notifyChange()
				continue
			}
			continue // available and fully forwarded: nothing to do this walk
		default:
			if !autoRetry {
				/* The budget is spent. One condition is still worth re-checking
				   without a nudge: the target profile itself. Recreated by hand, it is
				   a fix rather than a transient, and reading it costs one store lookup
				   instead of a dial. A host whose profile resolves fine but is not
				   answering keeps waiting for the panel key or the agent — that is the
				   case the budget exists to stop. */
				if st.lastStage() != "target" || !r.rebuildable(ext) {
					continue
				}
				st.resetRetryBudget()
				r.notifyChange()
			}
			if st.readyAt.After(time.Now()) {
				continue // backoff window not elapsed
			}
		}
		r.startProbe(ctx, name)
	}

	r.mu.Lock()
	var gone []string
	for name := range r.states {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	r.mu.Unlock()
	for _, name := range gone {
		st := r.state(name)
		r.teardown(st, StatusUnavailable)
		r.mu.Lock()
		delete(r.states, name)
		r.mu.Unlock()
		r.notifyChange()
		slog.Debug("extension removed", "name", name)
	}
	return nil
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
		r.Trigger()
	}()
}

// probe is one attempt to bring an extension to available, and the only place a
// session is created for it.
func (r *Reconciler) probe(ctx context.Context, name string) {
	ext, err := r.store.Load(name)
	if err != nil {
		// Deleted between the walk and the attempt; the next walk drops the state.
		return
	}
	st := r.state(name)

	// A session that is still running only needs its forwards reworked; throwing
	// the connection away to rebuild it would cost a redial for a tunnel problem.
	if st.alive(r.sessMgr) {
		if err := r.ensureForwards(ext, st); err != nil {
			r.fail(st, "forward", err.Error())
			return
		}
		st.mark(StatusAvailable)
		st.resetRetryBudget()
		r.notifyChange()
		return
	}

	// The previous attempt's DEAD session has served its purpose (it held the
	// failure's output for anyone watching) and is cleared before the next one so
	// a failing extension does not pile up dead tiles.
	st.dropDeadSession(r.sessMgr)

	cfg, err := r.buildConfig(ext)
	if err != nil {
		// The template does not build a session at all — a missing profile, a bad
		// path rule. No retry can clear this: the ladder exists for conditions a
		// redial fixes, and this one is fixed only by editing the template. Its
		// recovery path is the walk's rebuildable check, which costs a store
		// lookup and hands the budget back the moment the template builds again,
		// so the budget is spent now rather than climbed over twenty seconds —
		// and the missing profile stops re-probing every walk in the meantime.
		r.failForGood(st, "target", err.Error())
		return
	}

	sess, err := r.sessMgr.Create(cfg)
	if err != nil {
		r.fail(st, "dial", sshclient.DescribeDialError(err))
		return
	}

	st.mu.Lock()
	st.sessionID = sess.ID
	st.shellID = sess.PrimaryShellID()
	st.mu.Unlock()

	// A process that cannot start says so immediately. Waiting a beat turns "the
	// extension is unavailable" from a bare fact into the actual error line,
	// which is what makes the failure history worth reading.
	if shell := sess.GetChildShell(sess.PrimaryShellID()); shell != nil {
		select {
		case <-shell.Done():
			reason := exitReason(r.msgMgr, r.sessMgr, shell)
			st.mu.Lock()
			st.sessionID = ""
			st.shellID = ""
			st.mu.Unlock()
			_ = r.sessMgr.Delete(sess.ID)
			if reason == "" {
				reason = "process exited during startup"
			}
			r.fail(st, "start", reason)
			return
		case <-time.After(probeGrace):
		}
	}

	if err := r.ensureForwards(ext, st); err != nil {
		// The session is up and usable for its own sake; only the tunnel is
		// missing, so the failure is recorded and the retry targets the forward
		// alone rather than tearing the connection down to rebuild it.
		r.fail(st, "forward", err.Error())
		return
	}

	// Proven up: the budget is whole again, so a later failure starts the ladder
	// over rather than resuming a count from before the extension was working.
	st.resetRetryBudget()

	st.mu.Lock()
	st.status = StatusAvailable
	sid := st.sessionID
	shellID := st.shellID
	st.mu.Unlock()
	slog.Debug("extension available", "name", name, "session_id", sid, "shell_id", shellID)
	r.notifyChange()
}

// fail records a reason, marks the extension unavailable, and arms the backoff
// ladder. It is the shape of every failure a redial might clear — a dial that
// timed out, a process that exited, a port that was taken — so the four things
// that must happen together happen in one place.
func (r *Reconciler) fail(st *extState, stage, reason string) {
	st.recordFailure(stage, reason)
	st.mark(StatusUnavailable)
	st.scheduleBackoff()
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

// rebuildable reports whether the template can be turned into a session config
// at all. It is the recovery check the walk makes once the automatic budget is
// spent: nothing dials, nothing is created, and the answer costs a store lookup.
// A template that failed to build and now builds is a fix (a profile recreated
// by hand), so the extension gets its retries back; a template that still does
// not build keeps waiting for a nudge.
func (r *Reconciler) rebuildable(ext *Extension) bool {
	_, err := r.buildConfig(ext)
	return err == nil
}

// buildConfig resolves the stored template into the session.Config that realizes
// it. It is the one place the extension's fields are interpreted, so a session
// created by hand and one created by the reconciler from the same template are
// the same session.
func (r *Reconciler) buildConfig(ext *Extension) (session.Config, error) {
	if r.sshConfigs == nil {
		return session.Config{}, errors.New("no ssh config store configured")
	}
	ent, err := r.sshConfigs.Load(ext.Target)
	if err != nil {
		if errors.Is(err, sshconfig.ErrNotFound) {
			return session.Config{}, fmt.Errorf("target profile %q does not exist", ext.Target)
		}
		return session.Config{}, fmt.Errorf("cannot read target profile %q: %w", ext.Target, err)
	}
	if ent.Kind == sshconfig.KindInternal && r.NoInternal {
		return session.Config{}, fmt.Errorf("target profile %q is the built-in loopback profile, which is disabled", ext.Target)
	}
	var remote *session.RemoteSSH
	if ent.Kind != sshconfig.KindInternal {
		remote, err = sshconfig.RemoteFromEntry(ent, r.sshConfigs.ConfigDir(ext.Target))
		if err != nil {
			return session.Config{}, fmt.Errorf("target profile %q: %w", ext.Target, err)
		}
	}

	cmd, args := sshconfig.EffectiveCommand(ent, ext.Command, ext.Args)
	mode := sshconfig.EffectiveMode(ent, ext.Mode)
	rows, cols := ext.Rows, ext.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	restriction, err := session.NewRestriction(ext.Allow, ext.Paths)
	if err != nil {
		return session.Config{}, fmt.Errorf("restriction: %w", err)
	}
	return session.Config{
		Command:      cmd,
		Args:         args,
		Mode:         api.SessionMode(mode),
		Name:         ext.Name,
		SSHConfig:    ext.Target,
		Rows:         rows,
		Cols:         cols,
		Remote:       remote,
		DefaultShell: sshconfig.EffectiveDefaultShell(ent),
		// An extension is pre-authorised by whoever wrote its template; the
		// profile's review default would gate an unattended MCP server behind an
		// approval queue nothing is watching.
		Approval:    false,
		OnExit:      session.OnExitPolicy(ext.OnExit),
		NoShell:     ext.Mode == "",
		Restriction: restriction,
		Ctx:         context.Background(),
	}, nil
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
	for i, f := range ext.Forwards {
		if i < len(existing) && live[existing[i].ForwardID] {
			built[i] = existing[i]
			continue
		}
		fw, err := r.createForward(f, sess, sshCli, info)
		if err != nil {
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
		return r.forwardMgr.CreateDynamic(sess.ID, info.Name, f.LocalPort, sshCli, info.SSHEndpoint == "internal")
	case ForwardLocal:
		return r.forwardMgr.CreateLocal(sess.ID, info.Name, f.RemoteHost, f.RemotePort, f.LocalPort, sshCli)
	case ForwardRemote:
		return r.forwardMgr.CreateRemote(sess.ID, info.Name, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort, sshCli)
	}
	return nil, fmt.Errorf("unknown forward kind %q", f.Kind)
}

// alive reports whether the extension's session is still a running session. DEAD
// sessions stay in the registry (their output stays readable), so presence is
// not liveness.
func (st *extState) alive(sessMgr *session.Manager) bool {
	st.mu.Lock()
	sid := st.sessionID
	st.mu.Unlock()
	if sid == "" || sessMgr == nil {
		return false
	}
	sess := sessMgr.Get(sid)
	if sess == nil {
		return false
	}
	return sess.Info().Status == api.SessionRunning
}

// teardown closes the extension's session and forgets its forwards. The state
// sessionID is cleared before the session is terminated, because termination
// fires the DEAD hook this reconciler subscribes to — leaving the id in place
// would log the teardown the reconciler itself caused as a failure.
func (r *Reconciler) teardown(st *extState, status string) {
	st.mu.Lock()
	sid := st.sessionID
	st.sessionID = ""
	st.shellID = ""
	st.forwards = nil
	st.status = status
	st.mu.Unlock()
	if sid != "" {
		r.sessMgr.Terminate(sid, true, 0)
	}
}

// onSessionDead is the DEAD subscription. A session that dies on its own — the
// process exited, the transport dropped — takes its extension with it, and the
// reason is recorded so the status explains the drop instead of merely showing
// it.
func (r *Reconciler) onSessionDead(sessionID string) {
	name := r.extensionForSession(sessionID)
	if name == "" {
		return
	}
	st := r.state(name)
	st.mu.Lock()
	if st.sessionID != sessionID {
		st.mu.Unlock()
		return
	}
	st.sessionID = ""
	st.shellID = ""
	st.forwards = nil
	st.mu.Unlock()
	sess := r.sessMgr.Get(sessionID)
	reason := "session ended"
	if sess != nil {
		info := sess.Info()
		if info.ExitCode != nil {
			reason = fmt.Sprintf("process exited with code %d", *info.ExitCode)
		}
	}
	r.fail(st, "session", reason)
	r.Trigger()
}

// extensionForSession finds the extension owning a session id. Sessions do not
// record the extension that created them (an extension session is an ordinary
// session), so the mapping lives here in the reconciler's state.
func (r *Reconciler) extensionForSession(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, st := range r.states {
		st.mu.Lock()
		sid := st.sessionID
		st.mu.Unlock()
		if sid == sessionID {
			return name
		}
	}
	return ""
}

// Status reports the runtime state of one extension. ok is false for a name the
// store does not hold.
func (r *Reconciler) Status(name string) (Status, bool) {
	names, err := r.store.List()
	if err != nil {
		return Status{}, false
	}
	for _, n := range names {
		if n == name {
			return r.state(name).snapshot(), true
		}
	}
	return Status{}, false
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
// problem, and either should be served at once rather than at the next step.
func (r *Reconciler) Retry(name string) error {
	ext, err := r.store.Load(name)
	if err != nil {
		return err
	}
	if !ext.IsEnabled() {
		return errors.New("extension is disabled; enable it before retrying")
	}
	st := r.state(name)
	r.teardown(st, StatusUnavailable)
	st.resetRetryBudget()
	r.Trigger()
	return nil
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
	st := &extState{status: StatusUnavailable, autoRetry: true}
	r.states[name] = st
	return st
}

// extState is the mutable runtime half of one extension. Every field behind its
// own mutex because a probe goroutine writes it while the walk reads it.
type extState struct {
	mu          sync.Mutex
	status      string
	sessionID   string
	shellID     string
	forwards    []ForwardStatus
	failures    []Failure
	consecutive int       // consecutive failures, for the backoff ladder
	readyAt     time.Time // earliest moment a retry may run
	autoRetry   bool      // the automatic budget is unspent; false once it is
	fingerprint string    // last template applied; a change restarts the session
}

// changed reports whether the template differs from the one this session was
// built from, and remembers the new one. Anything a session is built from is in
// the fingerprint, so an edit that only touched the description also restarts —
// deliberately, since deciding which fields are cosmetic is how a live session
// drifts from its template.
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

// fingerprint summarizes the template as a comparable string.
func fingerprint(ext *Extension) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%q|%q|", ext.Kind, ext.Target, ext.Mode, ext.Command, ext.OnExit)
	for _, a := range ext.Args {
		fmt.Fprintf(&b, "%q,", a)
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
		fmt.Fprintf(&b, "%s:%s:%d:%s:%d|", f.Kind, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort)
	}
	fmt.Fprintf(&b, "|%d|%d|%t", ext.Rows, ext.Cols, ext.IsEnabled())
	return b.String()
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

// scheduleBackoff sets the earliest retry from the consecutive-failure count.
// Once the budget is spent it schedules nothing and clears the flag, so the walk
// stops bringing the extension up on its own; the timer, not a counter, is what
// would otherwise keep it probing forever.
func (st *extState) scheduleBackoff() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.consecutive > maxAutoRetries {
		st.autoRetry = false
		st.readyAt = time.Time{}
		return
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
}

// exhaustRetryBudget spends the automatic budget without scheduling a retry.
// It is the answer to a failure no retry can fix, where recovery is something
// else entirely — for a template that cannot build, the walk's rebuildable
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

// dropDeadSession removes the previous attempt's leftover session, if it is still
// around. A failing extension otherwise leaves one dead tile per attempt in the
// session list.
func (st *extState) dropDeadSession(sessMgr *session.Manager) {
	st.mu.Lock()
	sid := st.sessionID
	st.sessionID = ""
	st.shellID = ""
	st.forwards = nil
	st.mu.Unlock()
	if sid != "" && sessMgr != nil {
		_ = sessMgr.Delete(sid)
	}
}

func (st *extState) snapshot() Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := Status{
		Status:    st.status,
		SessionID: st.sessionID,
		ShellID:   st.shellID,
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
