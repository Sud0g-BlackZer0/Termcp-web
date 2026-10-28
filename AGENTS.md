## Security & Privacy

禁止在项目中出现以下敏感信息和敏感文件：

- 本机 hostname、本地用户名称
- 真实密钥、API keys、tokens、passwords
- 邮箱地址、手机号等个人信息
- .env 文件、私钥文件（PEM、SSH key 等）
- 禁止出现 `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>` 相关字样
- 只有在人工同意后才能push和commit。在多次commit后，人工要求push再进行push，绝对不要主动push。

## Concurrency & Deadlock Prevention

**建议优先使用 `channel` + `context.WithTimeout` + `sync/atomic` 替代互斥锁（`sync.Mutex`）。**

本项目在网络层、传输层和会话生命周期中，曾因使用互斥锁包裹跨网络写入、握手或等待外部对端响应，导致了 **10 次以上的死锁与雪崩 bug**（#189、#206、#207、#212、#218、#219、#220、#221、#222 等）：

1. **绝对禁止持锁进行网络 I/O、对端握手或等待不可控的外部响应**：
   - 目标主机卡死（如用户态无响应但内核仍应答 TCP）、对端停止读取套接字时，任何同步的 `Write`、`Signal`、`Close` 都可能在操作系统的 TCP 发送队列上永久阻塞；
   - 一旦在互斥锁内部阻塞，持有锁的 goroutine 永不释放，后续所有请求（甚至跨会话的关闭请求）全部排队死锁，导致整个实例挂起。
2. **串行化推荐使用「带缓冲的一槽 channel 令牌」**：
   - 例如 `tok := make(chan struct{}, 1); tok <- struct{}{}`；
   - 获取令牌必须带超时：`select { case <-tok: ... case <-timer.C: ... case <-ctx.Done(): ... }`；
   - 超时后必须明确报错或放弃，**绝不允许无限期挂死在 `Lock()` 上**。
3. **并发单飞与通知使用「`channel` 广播」替代锁内等待**：
   - 第一个请求负责建立连接/执行握手，完成后关闭 `done chan struct{}`；
   - 并发的后续请求监听 `select { case <-done: ... case <-ctx.Done(): ... }`，确保随时能响应自身的上下文超时并干净退出。
4. **生命周期终止（如 `Terminate`）必须具备有界超时与底层强拆兜底**：
   - 优雅关闭动作必须异步执行并受 `context.WithTimeout` 约束；
   - 超时后唯一的有效解法是主动调用底层 `net.Conn.Close()`，使内核中所有阻塞在底层读写上的协程立即报错并解开。
5. **纯计数器与单状态标记一律使用 `sync/atomic`**：
   - 纯数值累加使用 `atomic.Int64` / `atomic.Uint64`；
   - 状态开闭使用 `atomic.Bool`；
   - 避免为了一个数字的累加引入全局锁（如曾经的 `forwardCounter`）。
6. **无跨 key 事务的纯 map 查增删，一律使用 `sync.Map`**：
   - 别再写 `type X struct { mu sync.Mutex; m map[K]V }` —— AI 写锁基本全是 bug（漏解锁、死锁、嵌套锁）；
   - 单 key 的查、增、删用 `Load` / `Store` / `LoadOrStore` / `LoadAndDelete`，不需要任何互斥锁；
   - 遍历快照用 `Range` 收集；
   - 本项目已将 `aliasTable`、`task.Manager`、`ForwardManager`、`sessionListHub`、`uiNotifyHub`、`viewerRegistry`、`sshserver.pending`、`sshserver.builtins`、`cdpState`、`Server.recvCursors`、`Server.closedReads` 等全部换成 `sync.Map`，消除了 12+ 处死锁可能。

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Project docs live under `docs/` (`docs/api.md`, `docs/architecture.md`, `docs/design/`).

### Releases

`dev` is the rebased development line; `main` only receives release PRs. Use
the process in `docs/releasing.md` before tagging. Write the `## Unreleased`
notes **before** cutting the release — one short paragraph per user-visible
change since the last release, not a design note; `make prepare-release` only
moves them. Give the PR a `release/vX.Y.Z` head branch or `release: vX.Y.Z`
title, run `make prepare-release RELEASE_VERSION=vX.Y.Z`, and review the
generated changelog and `server.json` changes. CI checks both files before
merge.

A `vX.Y.Z-rc.N` tag is an internal test build: it adds a dated changelog
section, **leaves `server.json` alone** (no registry entry, no `:latest` move)
and is marked as a GitHub pre-release. Its branch and PR name the rc itself
(`release/v0.2.8-rc.1`), because CI requires the release signal to match the
files exactly — the final release is its own PR (`release/v0.2.8`).
`docs/releasing.md` has the details.

### Tests run in CI on three OSes

Whenever you write or modify a test, remember it must pass in
`.github/workflows/test.yml` — ubuntu, macos and windows, via
`go test ./... -count=1 -shuffle=on -race` (`make test` locally, with a tighter
`-timeout`: CI widens it because a shared runner is several times slower, and a
run killed by `-timeout` names no failing test). For races the detector cannot
see — filesystem and lifecycle
races — run `make test-stress` locally before pushing. Assume concurrency, a
fresh runner, and dirty state from other tests or processes. Never hardcode a
shared absolute path; `t.TempDir()` is the default. See
`docs/agents/testing.md`.

## Driving Termcp's MCP tools

Working in this repo usually means driving Termcp itself. The reference is the
running instance, not a file: `help` lists every directive, `help <directive>`
teaches one, and an empty `term()` call returns the guide. The REST entrance
`POST /api/term` speaks the same command language, and `GET /api.md` publishes
the transport-level reference. Three invariants are cheap to keep in mind without being told:

- **Turns are the scarce resource.** An MCP call costs 2–10 ms; an assistant
  turn costs 6–9 s — ~1000×. Minimise *turns*, not calls: whatever one turn can
  hold, keep it in one turn.
- **One command = one batch.** `shell_input` (types) → `shell_key(enter)`
  (executes) → `shell_output` (reads) is *tool semantics, not turn boundaries* —
  run all three inside a single `mcpScript` turn, and let
  `shell_output(timeout≤3)` do the only waiting there is. The loop is
  input → execute → read → next input, with nothing in between.
- **`shell_id` is the I/O target, `session_id` is the connection.** Every I/O
  tool takes `shell_id`. One task gets one session (give it a `name`).

Split across turns only when a human must act first (credential prompt → read
it, then `notify_user(level="warn", duration_seconds=0, session_id=...)`), a
destructive action needs confirmation, or you must inspect intermediate state
to decide the next step.
