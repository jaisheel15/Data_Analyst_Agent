// Package sandbox runs agent-written SQL and Python in a locked-down Docker
// container, one per session. It is the only code that talks to Docker.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"dataanalyst/api/internal/platform/storage"
	"dataanalyst/api/internal/platform/telemetry"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ErrAtCapacity means MaxSandboxes containers are already running.
var ErrAtCapacity = errors.New("sandbox: all sandboxes are busy")

// errBeforeStart marks failures that happened before the agent's code began
// running, so retrying in a fresh container cannot run the code twice.
var errBeforeStart = errors.New("sandbox: exec could not start")

// execEnv is the complete environment the agent's code sees: an allowlist,
// so nothing from the image or this service leaks in.
var execEnv = []string{
	"PATH=/usr/local/bin:/usr/bin:/bin",
	"HOME=/tmp",
	"LANG=C.UTF-8",
	"MPLBACKEND=Agg",
	"MPLCONFIGDIR=/tmp/mpl",
	"PYTHONDONTWRITEBYTECODE=1",
}

const (
	labelSandbox = "daa.sandbox"
	labelSession = "daa.session"
	sandboxUID   = "10001"
	// Extra time beyond CallTimeout before Go gives up on the exec itself.
	execGrace = 2 * time.Second
)

type Config struct {
	Image   string
	Runtime string // "runc" (plain Docker, the default) or "runsc" (gVisor, optional); falls back to runc if missing
	Layout  storage.Layout
	// HostRoot is Layout.Root as the Docker daemon sees it. They differ when
	// this service itself runs in a container with the data dir bind-mounted.
	HostRoot     string
	MaxSandboxes int
	CallTimeout  time.Duration
	IdleTimeout  time.Duration
	MemoryBytes  int64
	NanoCPUs     int64
	PidsLimit    int64
}

// Request is one tool call. Mode is "python" or "sql".
type Request struct {
	Mode           string `json:"mode"`
	Code           string `json:"code"`
	MaxRows        int    `json:"max_rows,omitempty"`
	MaxOutputChars int    `json:"max_output_chars,omitempty"`
}

// Result mirrors runner.py's output. Error is empty when the call succeeded.
type Result struct {
	Stdout    string   `json:"stdout"`
	Stderr    string   `json:"stderr"`
	Error     string   `json:"error"`
	Truncated bool     `json:"truncated"`
	Files     []string `json:"files"`
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	TotalRows int      `json:"total_rows"`
	TimedOut  bool     `json:"timed_out"`

	// Fault is set by the Manager when the sandbox itself failed (timeout,
	// out of memory, crashed runner), as opposed to the agent's code failing.
	// That is a Tool failure, not a Tool error.
	Fault bool `json:"-"`
}

// Failed reports whether the sandbox failed to do its job (a Tool failure).
func (r Result) Failed() bool { return r.Fault || r.TimedOut }

// box is one session's sandbox. Lock order: Manager.mu, then box.mu (only
// ever TryLock'd while Manager.mu is held, so the order cannot deadlock).
type box struct {
	mu          sync.Mutex // held while the box is created and during each exec
	containerID string     // guarded by mu

	lastUsed time.Time // guarded by Manager.mu
	removed  bool      // guarded by Manager.mu: dropped from the map, container going away
}

type Manager struct {
	cli     *client.Client
	cfg     Config
	runtime string

	mu    sync.Mutex
	boxes map[string]*box
}

func New(ctx context.Context, cli *client.Client, cfg Config) (*Manager, error) {
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker info: %w", err)
	}
	runtime := cfg.Runtime
	if _, ok := info.Info.Runtimes[runtime]; !ok {
		slog.Warn("sandbox runtime not installed, falling back to runc", "requested", runtime)
		runtime = "runc"
	}
	m := &Manager{cli: cli, cfg: cfg, runtime: runtime, boxes: map[string]*box{}}
	m.removeLeftovers(ctx)
	return m, nil
}

// HasCapacity reports whether sessionID has a sandbox or one could be had now,
// either free or by evicting another session's idle sandbox.
func (m *Manager) HasCapacity(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.boxes[sessionID]; ok || len(m.boxes) < m.cfg.MaxSandboxes {
		return true
	}
	for _, b := range m.boxes {
		if b.mu.TryLock() {
			b.mu.Unlock()
			return true
		}
	}
	return false
}

// Runtime reports the container runtime actually in use.
func (m *Manager) Runtime() string { return m.runtime }

// Exec runs one request in the session's sandbox, creating it if needed.
// Failures of the agent's code come back in Result.Error, not as an error.
func (m *Manager) Exec(ctx context.Context, sessionID string, req Request) (Result, error) {
	b, err := m.tracedLockedBox(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	defer b.mu.Unlock()
	defer m.touch(b)

	ctx, span := telemetry.Start(ctx, "sandbox.exec",
		trace.WithAttributes(attribute.String("sandbox.mode", req.Mode), attribute.String("sandbox.runtime", m.runtime)))
	defer span.End()
	res, err := m.exec(ctx, sessionID, b, req)
	span.SetAttributes(attribute.Bool("sandbox.timed_out", res.TimedOut))
	switch {
	case ctx.Err() != nil: // the Run was cancelled: not a fault
		span.SetAttributes(attribute.Bool("sandbox.cancelled", true))
	case err != nil:
		span.RecordError(err)
		telemetry.Fail(span, err.Error())
	case res.Failed():
		telemetry.Fail(span, res.Error)
	}
	return res, err
}

// tracedLockedBox is lockedBox inside a sandbox.acquire span that says
// whether the call reused a sandbox or had to start one (a cold start).
func (m *Manager) tracedLockedBox(ctx context.Context, sessionID string) (*box, error) {
	ctx, span := telemetry.Start(ctx, "sandbox.acquire")
	defer span.End()
	b, how, err := m.lockedBox(ctx, sessionID)
	span.SetAttributes(attribute.String("sandbox.acquire", how))
	if err != nil {
		span.RecordError(err)
		telemetry.Fail(span, err.Error())
	}
	return b, err
}

func (m *Manager) exec(ctx context.Context, sessionID string, b *box, req Request) (Result, error) {
	res, err := m.execOnce(ctx, b, req)
	switch {
	case err == nil || ctx.Err() != nil:
		return res, err
	case errors.Is(err, errHung):
		// The code started but never let go (for example a fork bomb). Throw
		// the container away; files in /scratch survive on the host.
		slog.WarnContext(ctx, "sandbox hung, recreating container", "session", sessionID)
		trace.SpanFromContext(ctx).SetAttributes(attribute.Bool("sandbox.recreated", true))
		if rerr := m.recreate(ctx, sessionID, b); rerr != nil {
			return Result{}, rerr
		}
		return m.timedOut(), nil
	case errors.Is(err, errBeforeStart):
		// The container is probably dead (for example OOM-killed). The code
		// never ran, so recreating and retrying once is safe.
		slog.WarnContext(ctx, "sandbox unavailable, recreating container", "session", sessionID, "err", err)
		trace.SpanFromContext(ctx).SetAttributes(attribute.Bool("sandbox.recreated", true))
		if rerr := m.recreate(ctx, sessionID, b); rerr != nil {
			return Result{}, errors.Join(err, rerr)
		}
		return m.execOnce(ctx, b, req)
	default:
		return Result{}, err
	}
}

var errHung = errors.New("sandbox: exec did not finish")

func (m *Manager) timedOut() Result {
	return Result{TimedOut: true, Fault: true,
		Error: fmt.Sprintf("timed out after %s: the code was stopped", m.cfg.CallTimeout)}
}

func (m *Manager) execOnce(ctx context.Context, b *box, req Request) (Result, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	secs := strconv.Itoa(int(m.cfg.CallTimeout.Seconds()))
	ex, err := m.cli.ExecCreate(ctx, b.containerID, client.ExecCreateOptions{
		User:         sandboxUID,
		Env:          execEnv,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		// env -i drops the image's environment; timeout kills the process group.
		Cmd: append(append([]string{"env", "-i"}, execEnv...),
			"timeout", "-s", "KILL", secs, "python", "/app/runner.py"),
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: create: %v", errBeforeStart, err)
	}
	att, err := m.cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("%w: attach: %v", errBeforeStart, err)
	}
	defer att.Close()

	start := time.Now()
	if _, err := att.Conn.Write(payload); err != nil {
		return Result{}, fmt.Errorf("write request: %w", err)
	}
	_ = att.CloseWrite()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { _, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			return Result{}, fmt.Errorf("read output: %w", err)
		}
	case <-time.After(m.cfg.CallTimeout + execGrace):
		return Result{}, errHung
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	elapsed := time.Since(start)
	m.killStragglers(ctx, b)

	exit, err := m.exitCode(ctx, ex.ID)
	if err != nil {
		return Result{}, err
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("sandbox.exit_code", exit),
		attribute.Int64("sandbox.elapsed_ms", elapsed.Milliseconds()))
	return m.interpret(exit, elapsed, stdout.Bytes(), stderr.String()), nil
}

func (m *Manager) interpret(exit int, elapsed time.Duration, stdout []byte, stderr string) Result {
	var res Result
	if exit == 0 {
		// Decode only the first JSON value: stray output can follow it.
		// UseNumber keeps 1000000 and big int64 IDs exact (no float64 detour).
		dec := json.NewDecoder(bytes.NewReader(stdout))
		dec.UseNumber()
		if err := dec.Decode(&res); err == nil {
			return res
		}
		return Result{Fault: true, Error: "sandbox runner produced invalid output: " + tail(stderr, 500)}
	}
	if exit == 137 && elapsed >= m.cfg.CallTimeout-200*time.Millisecond {
		return m.timedOut()
	}
	if exit == 137 {
		return Result{Fault: true, Error: fmt.Sprintf("process was killed, most likely for exceeding the %d MB memory limit",
			m.cfg.MemoryBytes>>20)}
	}
	return Result{Fault: true, Error: fmt.Sprintf("sandbox runner exited with code %d: %s", exit, tail(stderr, 500))}
}

// killStragglers ends any process the agent's code left behind (for example
// a child started in its own session, which escapes timeout's process group).
// kill -1 signals every process the sandbox user may signal except PID 1 (the
// container's idle process) and the caller. The image has no kill binary, so
// it runs as the shell builtin.
func (m *Manager) killStragglers(ctx context.Context, b *box) {
	ex, err := m.cli.ExecCreate(ctx, b.containerID, client.ExecCreateOptions{
		User: sandboxUID, Env: execEnv, Cmd: []string{"sh", "-c", "kill -9 -1 2>/dev/null; true"},
	})
	if err != nil {
		slog.Warn("kill stragglers", "err", err)
		return
	}
	if _, err := m.cli.ExecStart(ctx, ex.ID, client.ExecStartOptions{}); err != nil {
		slog.Warn("kill stragglers", "err", err)
		return
	}
	// ExecStart returns at once; wait so the next call starts on a clean slate.
	if code, err := m.exitCode(ctx, ex.ID); err != nil || code != 0 {
		slog.Warn("kill stragglers did not finish cleanly", "code", code, "err", err)
	}
}

func (m *Manager) exitCode(ctx context.Context, execID string) (int, error) {
	for range 50 {
		ins, err := m.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("exec inspect: %w", err)
		}
		if !ins.Running {
			return ins.ExitCode, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, errors.New("exec still running after output closed")
}

// Ways a call gets its sandbox, as recorded on the sandbox.acquire span.
const (
	acquireReused  = "reused"
	acquireCreated = "created"
	acquireEvicted = "created_after_evicting" // another session's idle sandbox made room
)

// lockedBox returns the session's sandbox with its mu held, creating it (and
// evicting an idle sandbox if at capacity) when needed. how is one of the
// acquire* values.
func (m *Manager) lockedBox(ctx context.Context, sessionID string) (b *box, how string, err error) {
	for {
		b, how, err = m.acquire(ctx, sessionID)
		if err != nil {
			return nil, how, err
		}
		b.mu.Lock()
		m.mu.Lock()
		removed := b.removed
		m.mu.Unlock()
		if !removed {
			return b, how, nil
		}
		b.mu.Unlock() // reaped or evicted while we waited: get a fresh one
	}
}

func (m *Manager) acquire(ctx context.Context, sessionID string) (*box, string, error) {
	m.mu.Lock()
	if b, ok := m.boxes[sessionID]; ok {
		b.lastUsed = time.Now() // so the reaper does not pick it while we wait for b.mu
		m.mu.Unlock()
		return b, acquireReused, nil
	}
	how := acquireCreated
	var victim *box
	if len(m.boxes) >= m.cfg.MaxSandboxes {
		victim = m.evictLocked()
		if victim == nil {
			m.mu.Unlock()
			return nil, "at_capacity", ErrAtCapacity
		}
		how = acquireEvicted
	}
	// Reserve the slot with a locked placeholder, then create the container
	// without holding m.mu: Docker calls are slow.
	b := &box{lastUsed: time.Now()}
	b.mu.Lock()
	m.boxes[sessionID] = b
	m.mu.Unlock()
	if victim != nil {
		m.remove(victim.containerID)
	}

	id, err := m.create(ctx, sessionID)
	if err != nil {
		m.mu.Lock()
		if m.boxes[sessionID] == b {
			delete(m.boxes, sessionID)
		}
		b.removed = true
		m.mu.Unlock()
		b.mu.Unlock()
		return nil, how, err
	}
	b.containerID = id
	b.mu.Unlock()
	return b, how, nil
}

// evictLocked drops the least recently used sandbox that is not running code.
// m.mu must be held. The victim's container must be removed by the caller.
func (m *Manager) evictLocked() *box {
	var victimID string
	var victim *box
	for sid, b := range m.boxes {
		if (victim == nil || b.lastUsed.Before(victim.lastUsed)) && b.mu.TryLock() {
			if victim != nil {
				victim.mu.Unlock()
			}
			victim, victimID = b, sid
		}
	}
	if victim == nil {
		return nil
	}
	victim.removed = true
	delete(m.boxes, victimID)
	victim.mu.Unlock()
	return victim
}

func (m *Manager) create(ctx context.Context, sessionID string) (string, error) {
	l := m.cfg.Layout
	pids := m.cfg.PidsLimit
	created, err := m.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:           m.cfg.Image,
			User:            sandboxUID,
			NetworkDisabled: true,
			Labels:          map[string]string{labelSandbox: "1", labelSession: sessionID},
		},
		HostConfig: &container.HostConfig{
			Runtime:        m.runtime,
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Tmpfs:          map[string]string{"/tmp": "rw,size=64m,mode=1777"},
			Binds: []string{
				m.hostPath(l.DataDir(sessionID)) + ":/data:ro",
				m.hostPath(l.OutDir(sessionID)) + ":/out:rw",
				m.hostPath(l.ScratchDir(sessionID)) + ":/scratch:rw",
			},
			Resources: container.Resources{
				Memory:     m.cfg.MemoryBytes,
				MemorySwap: m.cfg.MemoryBytes, // no swap
				NanoCPUs:   m.cfg.NanoCPUs,
				PidsLimit:  &pids,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create sandbox: %w", err)
	}
	if _, err := m.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		m.remove(created.ID)
		return "", fmt.Errorf("start sandbox: %w", err)
	}
	return created.ID, nil
}

// recreate swaps b's container for a fresh one. b.mu must be held.
func (m *Manager) recreate(ctx context.Context, sessionID string, b *box) error {
	m.remove(b.containerID)
	id, err := m.create(ctx, sessionID)
	if err != nil {
		b.containerID = ""
		m.mu.Lock()
		if m.boxes[sessionID] == b {
			delete(m.boxes, sessionID)
		}
		b.removed = true
		m.mu.Unlock()
		return err
	}
	b.containerID = id
	return nil
}

func (m *Manager) touch(b *box) {
	m.mu.Lock()
	b.lastUsed = time.Now()
	m.mu.Unlock()
}

// Stop removes the session's sandbox, if any, waiting for a running exec.
func (m *Manager) Stop(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	b, ok := m.boxes[sessionID]
	if ok {
		delete(m.boxes, sessionID)
		b.removed = true
	}
	m.mu.Unlock()
	if ok {
		b.mu.Lock()
		id := b.containerID
		b.mu.Unlock()
		m.remove(id)
	}
	return nil
}

// ReapIdle removes sandboxes unused for longer than IdleTimeout. A sandbox
// that is running code is never reaped, however old its lastUsed.
func (m *Manager) ReapIdle(now time.Time) {
	m.mu.Lock()
	var ids []string
	for sid, b := range m.boxes {
		if now.Sub(b.lastUsed) <= m.cfg.IdleTimeout || !b.mu.TryLock() {
			continue
		}
		b.removed = true
		delete(m.boxes, sid)
		ids = append(ids, b.containerID)
		b.mu.Unlock()
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.remove(id)
	}
}

// Run reaps idle sandboxes every minute until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.ReapIdle(now)
		}
	}
}

// Shutdown removes every sandbox this manager created.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	boxes := m.boxes
	m.boxes = map[string]*box{}
	for _, b := range boxes {
		b.removed = true
	}
	m.mu.Unlock()
	for _, b := range boxes {
		b.mu.Lock()
		id := b.containerID
		b.mu.Unlock()
		m.remove(id)
	}
}

func (m *Manager) removeLeftovers(ctx context.Context) {
	list, err := m.cli.ContainerList(ctx, client.ContainerListOptions{
		All: true, Filters: client.Filters{}.Add("label", labelSandbox+"=1"),
	})
	if err != nil {
		return
	}
	for _, c := range list.Items {
		m.remove(c.ID)
	}
}

func (m *Manager) remove(id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = m.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
}

func (m *Manager) hostPath(local string) string {
	rel, err := filepath.Rel(m.cfg.Layout.Root, local)
	if err != nil {
		return local
	}
	return filepath.Join(m.cfg.HostRoot, rel)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
