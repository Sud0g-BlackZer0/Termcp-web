package extensions

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// testShellCommand is a command that exists on every CI host and blocks until
// its stdin closes, so a probe finds it still running after the grace window.
func testShellCommand() string {
	return "cat"
}

// immediateExitCommand is a command the host actually has that exits at once
// with a nonzero code, so a probe observes the exit inside its grace window and
// reports a startup failure rather than a dial failure.
func immediateExitCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-Command", "exit 3"}
	}
	return "bash", []string{"-c", "echo boom; exit 3"}
}

// startReconciler stands up the whole runtime an extension needs: a loopback SSH
// server, a session manager, both stores, and a running reconciler. Registering
// cleanup after t.TempDir (LIFO) keeps the exit watchers ahead of the directory
// removal.
func startReconciler(t *testing.T) *Reconciler {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})
	r := NewReconciler(NewStore(dir), sessMgr, msgMgr, sshconfig.NewStore(dir), forward.NewForwardManager())
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Stop() })
	return r
}

// waitForStatus polls until want holds or the deadline passes. Extensions are
// asynchronous by design — the reconciler proves availability off the caller's
// thread — so the tests wait on the outcome rather than assuming it.
func waitForStatus(t *testing.T, r *Reconciler, name, want string) Status {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		st, ok := r.Status(name)
		if ok && st.Status == want {
			return st
		}
		if time.Now().After(deadline) {
			st, _ := r.Status(name)
			dumpReconciler(t, r, name)
			t.Fatalf("extension %q never reached %s (last: %+v)", name, want, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustSave(t *testing.T, store *Store, ext *Extension) {
	t.Helper()
	if err := store.Save(ext); err != nil {
		t.Fatal(err)
	}
}

// dumpReconciler prints everything about an extension and the sessions the
// manager holds, for diagnosing a reconciler test that stopped making progress.
func dumpReconciler(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	st, _ := r.Status(name)
	t.Logf("extension %q: %+v", name, st)
	r.mu.Lock()
	for n, s := range r.states {
		s.mu.Lock()
		t.Logf("state %q: status=%q session=%q shell=%q failures=%d", n, s.status, s.sessionID, s.shellID, len(s.failures))
		s.mu.Unlock()
	}
	r.mu.Unlock()
	for _, s := range r.sessMgr.ListAll() {
		t.Logf("session %s: name=%q status=%q ssh=%q", s.ID, s.Name, s.Status, s.SSHConfig)
	}
}

// waitForFailure polls until an extension has recorded a failure in stage. The
// initial status of any extension is unavailable, so waiting on the status alone
// cannot prove the reconciler ran — only a recorded failure can.
func waitForFailure(t *testing.T, r *Reconciler, name, stage string) Status {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		st, ok := r.Status(name)
		if ok {
			for _, f := range st.Failures {
				if f.Stage == stage {
					return st
				}
			}
		}
		if time.Now().After(deadline) {
			st, _ := r.Status(name)
			t.Fatalf("extension %q never recorded a %s failure (last: %+v)", name, stage, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReconcilerBringsUpSkillExtension pins the simplest full path: an enabled
// skill extension is connected by the reconciler, ends up available, and the
// session it created carries the restriction the template declared.
func TestReconcilerBringsUpSkillExtension(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/skills/docs/"}
	mustSave(t, r.store, &ext)

	st := waitForStatus(t, r, "docs", StatusAvailable)
	if st.SessionID == "" {
		t.Fatal("available extension reports no session")
	}
	if st.ShellID != "" {
		t.Errorf("a shell-less extension should report no primary shell, got %q", st.ShellID)
	}
	sess := r.sessMgr.Get(st.SessionID)
	if sess == nil {
		t.Fatal("reported session not found in the manager")
	}
	rest := sess.Restriction()
	if !rest.AllowsTool("file_read") {
		t.Error("the session should allow file_read")
	}
	if rest.AllowsTool("file_write") {
		t.Error("the session should not allow file_write")
	}
	if !rest.AllowsPath("/srv/skills/docs/SKILL.md") {
		t.Error("the session should reach into its declared path")
	}
	if rest.AllowsPath("/etc/passwd") {
		t.Error("the session should not reach outside its declared path")
	}
}

// TestReconcilerReportsStartupFailure pins that a process that cannot stay
// running is reported as unavailable with the reason it printed, rather than
// being silently retried forever or reported as up.
func TestReconcilerReportsStartupFailure(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindMCP)
	ext.Name = "broken"
	ext.Target = "internal"
	ext.Command, ext.Args = immediateExitCommand()
	mustSave(t, r.store, &ext)

	st := waitForFailure(t, r, "broken", "start")
	last := st.Failures[len(st.Failures)-1]
	if !strings.Contains(last.Reason, "exit code") {
		t.Errorf("failure should name the exit code, got %q", last.Reason)
	}
	if st.SessionID != "" {
		t.Error("a failed startup should not leave a session id behind")
	}
	if st.NextRetry == 0 {
		t.Error("a transient failure should schedule a retry")
	}
}

// TestReconcilerMissingTarget pins the unrecoverable case: a target profile that
// does not exist is reported once, and no session is ever created for it.
func TestReconcilerMissingTarget(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "nowhere"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)

	st := waitForFailure(t, r, "nowhere", "target")
	if st.NextRetry != 0 {
		t.Error("a missing target is not transient; no retry should be scheduled")
	}
	if len(r.sessMgr.ListAll()) != 0 {
		t.Errorf("no session should have been created, got %d", len(r.sessMgr.ListAll()))
	}
	// Give the loop a couple of walks to prove a spent, unbuildable template is
	// stepped over rather than re-probed: the failure line stays the one already
	// recorded, and nothing dials or builds a session for it.
	time.Sleep(2 * loopInterval)
	st, _ = r.Status("nowhere")
	if len(st.Failures) != 1 {
		t.Errorf("the walk should not re-probe a template it cannot build, got %d: %+v", len(st.Failures), st.Failures)
	}
	if st.NextRetry != 0 {
		t.Error("the budget stays spent while the template stays unbuildable")
	}
}

// TestReconcilerTargetAppearsLater pins the recovery path for a template that
// could not build: the profile it points at is created by hand, and the
// extension comes up without a nudge. This is why a target failure spends the
// budget instead of climbing the backoff ladder — nothing the timer did was
// going to create the profile.
func TestReconcilerTargetAppearsLater(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "late-target"
	ext.Target = "arrives"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)

	waitForFailure(t, r, "late-target", "target")

	if err := r.SSHConfigs().Save("arrives", []byte("kind = \"internal\"\n")); err != nil {
		t.Fatal(err)
	}

	st := waitForStatus(t, r, "late-target", StatusAvailable)
	if st.SessionID == "" {
		t.Error("the extension should run once its profile exists")
	}
}

// TestReconcilerDisabled pins that a disabled extension is left entirely alone:
// no session, no attempt, and the status says why nothing is happening.
func TestReconcilerDisabled(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "off"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.SetEnabled(false)
	mustSave(t, r.store, &ext)

	st := waitForStatus(t, r, "off", StatusDisabled)
	if st.SessionID != "" {
		t.Error("a disabled extension should have no session")
	}
	if len(r.sessMgr.ListAll()) != 0 {
		t.Errorf("a disabled extension should create no session, got %d", len(r.sessMgr.ListAll()))
	}
}

// TestReconcilerRetry pins that Retry short-circuits the backoff: fixing the
// underlying problem and asking again brings the extension up without waiting
// for the timer.
func TestReconcilerRetry(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "late"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)
	waitForFailure(t, r, "late", "target")

	ext.Target = "internal"
	mustSave(t, r.store, &ext)
	if err := r.Retry("late"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, r, "late", StatusAvailable)
}

// TestReconcilerRetryDisabledRefused pins that retrying a disabled extension is
// an error rather than a silent no-op, since the caller asked for something the
// config forbids.
func TestReconcilerRetryDisabledRefused(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "off"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.SetEnabled(false)
	mustSave(t, r.store, &ext)
	waitForStatus(t, r, "off", StatusDisabled)

	err := r.Retry("off")
	if err == nil {
		t.Fatal("retrying a disabled extension should fail")
	}
}

// TestReconcilerSessionDeathMarksUnavailable pins the recovery contract: a
// session that dies on its own takes its extension to unavailable with a
// recorded reason, and the reconciler then brings it back.
func TestReconcilerSessionDeathMarksUnavailable(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindMCP)
	ext.Name = "live"
	ext.Target = "internal"
	ext.Command = testShellCommand()
	ext.Args = nil
	mustSave(t, r.store, &ext)

	st := waitForStatus(t, r, "live", StatusAvailable)
	sess := r.sessMgr.Get(st.SessionID)
	if sess == nil {
		dumpReconciler(t, r, "live")
		t.Fatal("session missing for a live extension")
	}

	r.sessMgr.Terminate(sess.ID, true, 0)
	st = waitForStatus(t, r, "live", StatusUnavailable)
	found := false
	for _, f := range st.Failures {
		if f.Stage == "session" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the extension's death should be recorded, got %+v", st.Failures)
	}

	// The backoff is short enough that the loop restores it on its own.
	waitForStatus(t, r, "live", StatusAvailable)
}

// TestReconcilerForwardEstablished pins that a declared forward is set up with
// the extension and its bound address is reported, including the port chosen
// when the template asked for an ephemeral one.
func TestReconcilerForwardEstablished(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "tunnel"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.Forwards = []ForwardDecl{{
		Kind:       ForwardDynamic,
		LocalHost:  "127.0.0.1",
		LocalPort:  0,
		RemoteHost: "localhost",
		RemotePort: 0,
	}}
	mustSave(t, r.store, &ext)

	st := waitForStatus(t, r, "tunnel", StatusAvailable)
	if len(st.Forwards) != 1 {
		t.Fatalf("want 1 established forward, got %+v", st.Forwards)
	}
	fw := st.Forwards[0]
	if fw.ListenAddr == "" {
		t.Error("a dynamic forward should report the port it actually bound")
	}
	if !strings.Contains(fw.ListenAddr, "127.0.0.1:") {
		t.Errorf("listen address %q should be the declared local host", fw.ListenAddr)
	}
	if fw.ForwardID == "" || r.forwardMgr == nil {
		// forwardMgr is present in these tests; a missing id means nothing bound.
		if fw.ForwardID == "" {
			t.Error("the established forward should carry its forward id")
		}
	}
	if r.forwardMgr != nil {
		live := false
		for _, f := range r.forwardMgr.ListBySession(st.SessionID) {
			if f.ForwardID == fw.ForwardID {
				live = true
			}
		}
		if !live {
			t.Error("the declared forward is not registered with the forward manager")
		}
	}
}

// TestReconcilerEditRestartsSession pins that editing a template replaces its
// session instead of leaving a live session shaped like the old template.
func TestReconcilerEditRestartsSession(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindMCP)
	ext.Name = "edited"
	ext.Target = "internal"
	ext.Command = testShellCommand()
	ext.Args = nil
	mustSave(t, r.store, &ext)
	st := waitForStatus(t, r, "edited", StatusAvailable)
	first := st.SessionID

	ext.Command = "sort"
	mustSave(t, r.store, &ext)

	deadline := time.Now().Add(8 * time.Second)
	for {
		st, _ = r.Status("edited")
		if st.SessionID != "" && st.SessionID != first && st.Status == StatusAvailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session was not replaced after the edit (still %q)", st.SessionID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReconcilerDeleteTearsDown pins that removing an extension from the store
// closes the session it kept warm, so an extension that is gone is gone.
func TestReconcilerDeleteTearsDown(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "gone"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)
	st := waitForStatus(t, r, "gone", StatusAvailable)
	sid := st.SessionID

	if err := r.store.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, ok := r.Status("gone"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleted extension's state was never dropped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	sess := r.sessMgr.Get(sid)
	if sess == nil {
		return // deleted outright, which also satisfies "closed"
	}
	// markDead publishes the exited status after flushing the session's output
	// pipes, so it lands slightly after Terminate returns.
	shutdownDeadline := time.Now().Add(3 * time.Second)
	for sess.Info().Status == api.SessionRunning {
		if time.Now().After(shutdownDeadline) {
			t.Fatal("a deleted extension's session should not still be running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconcilerStoreChangeTriggers pins the wiring: saving an extension through
// the store reaches the reconciler without an explicit poke.
func TestReconcilerStoreChangeTriggers(t *testing.T) {
	r := startReconciler(t)
	ext := Preset(KindSkill)
	ext.Name = "wired"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)
	// No Trigger call: the store's own change callback is the path production uses.
	waitForStatus(t, r, "wired", StatusAvailable)
}

// TestReconcilerStatusUnknownName pins the lookup contract: a name the store
// does not hold is reported as absent, not as an empty status.
func TestReconcilerStatusUnknownName(t *testing.T) {
	r := startReconciler(t)
	if _, ok := r.Status("never-saved"); ok {
		t.Error("an unsaved extension should not report a status")
	}
	if err := r.Retry("never-saved"); !errors.Is(err, ErrNotFound) {
		t.Errorf("retry of a missing extension = %v, want ErrNotFound", err)
	}
}
