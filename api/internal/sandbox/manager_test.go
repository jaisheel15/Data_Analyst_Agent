package sandbox_test

// Seam 2: Manager.Exec against a real Docker daemon. Skipped when Docker or
// the sandbox image is unavailable. Build the image with `make sandbox-image`.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dataanalyst/api/internal/platform/storage"
	"dataanalyst/api/internal/sandbox"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

const image = "daa-sandbox:dev"

type fixture struct {
	m      *sandbox.Manager
	layout storage.Layout
}

func newFixture(t *testing.T, tweak func(*sandbox.Config)) fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("docker test")
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	ctx := context.Background()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	if _, err := cli.ImageInspect(ctx, image); err != nil {
		t.Skipf("image %s not built: %v", image, err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	layout := storage.Layout{Root: root}
	cfg := sandbox.Config{
		Image:        image,
		Runtime:      "runc",
		Layout:       layout,
		HostRoot:     root,
		MaxSandboxes: 3,
		CallTimeout:  5 * time.Second,
		MemoryBytes:  256 << 20,
		NanoCPUs:     1_000_000_000,
		PidsLimit:    64,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	m, err := sandbox.New(ctx, cli, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(context.Background()) })
	return fixture{m: m, layout: layout}
}

func (f fixture) session(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	if err := f.layout.EnsureSession(id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f fixture) python(t *testing.T, id, code string) sandbox.Result {
	t.Helper()
	res, err := f.m.Exec(context.Background(), id, sandbox.Request{Mode: "python", Code: code})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return res
}

func TestExecRunsPythonAndWritesChartsToTheSessionOutDir(t *testing.T) {
	f := newFixture(t, nil)
	id := f.session(t)
	res := f.python(t, id, "import matplotlib.pyplot as plt\nplt.plot([1,2])\nprint('hi')")
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if len(res.Files) != 1 {
		t.Fatalf("files = %v", res.Files)
	}
	if _, err := os.Stat(filepath.Join(f.layout.OutDir(id), res.Files[0])); err != nil {
		t.Fatalf("chart not on host: %v", err)
	}
}

func TestNetworkIsBlocked(t *testing.T) {
	f := newFixture(t, nil)
	res := f.python(t, f.session(t),
		"import socket\nsocket.create_connection(('1.1.1.1', 53), timeout=2)")
	if res.Error == "" {
		t.Fatal("network call succeeded inside the sandbox")
	}
}

func TestLongRunningCodeIsKilledAtTheTimeout(t *testing.T) {
	f := newFixture(t, func(c *sandbox.Config) { c.CallTimeout = 2 * time.Second })
	id := f.session(t)
	start := time.Now()
	res := f.python(t, id, "while True: pass")
	if !res.TimedOut || !strings.Contains(res.Error, "timed out after 2s") {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	// The sandbox keeps working after a timeout.
	if res := f.python(t, id, "print(1)"); strings.TrimSpace(res.Stdout) != "1" {
		t.Fatalf("sandbox unusable after timeout: %+v", res)
	}
}

func TestMemoryLimitStopsRunawayAllocation(t *testing.T) {
	f := newFixture(t, func(c *sandbox.Config) { c.MemoryBytes = 128 << 20 })
	res := f.python(t, f.session(t), "x = b'x' * (512 * 1024 * 1024)\nprint('allocated')")
	if strings.Contains(res.Stdout, "allocated") || res.Error == "" {
		t.Fatalf("allocation beyond the limit succeeded: %+v", res)
	}
}

func TestDataIsReadOnlyAndRootFilesystemIsReadOnly(t *testing.T) {
	f := newFixture(t, nil)
	id := f.session(t)
	for _, path := range []string{"/data/x.txt", "/app/x.txt", "/etc/x.txt"} {
		res := f.python(t, id, "open('"+path+"', 'w').write('x')")
		if res.Error == "" {
			t.Errorf("write to %s succeeded", path)
		}
	}
	if res := f.python(t, id, "open('/scratch/x.txt', 'w').write('x')"); res.Error != "" {
		t.Errorf("scratch not writable: %s", res.Error)
	}
}

func TestForkBombIsContainedAndSandboxRecovers(t *testing.T) {
	f := newFixture(t, func(c *sandbox.Config) { c.CallTimeout = 3 * time.Second })
	id := f.session(t)
	res := f.python(t, id, "import os\nwhile True:\n    os.fork()")
	if res.Error == "" {
		t.Fatal("fork bomb reported no error")
	}
	if res := f.python(t, id, "print(2)"); strings.TrimSpace(res.Stdout) != "2" {
		t.Fatalf("sandbox unusable after fork bomb: %+v", res)
	}
}

func TestSandboxEnvironmentCarriesNoSecrets(t *testing.T) {
	t.Setenv("INTERNAL_TOKEN", "super-secret")
	f := newFixture(t, nil)
	res := f.python(t, f.session(t), "import os\nprint(sorted(os.environ))")
	if strings.Contains(res.Stdout, "TOKEN") || strings.Contains(res.Stdout, "KEY") {
		t.Fatalf("secret-looking env vars in sandbox: %s", res.Stdout)
	}
}

func TestCapacityLimitOnlyRejectsWhenEverySandboxIsBusy(t *testing.T) {
	f := newFixture(t, func(c *sandbox.Config) { c.MaxSandboxes = 1 })
	first, second := f.session(t), f.session(t)
	busy := make(chan sandbox.Result)
	go func() { busy <- f.python(t, first, "import time\ntime.sleep(2)\nprint('first done')") }()
	time.Sleep(700 * time.Millisecond) // first is now running code
	if f.m.HasCapacity(second) {
		t.Error("HasCapacity = true while the only sandbox is busy")
	}
	_, err := f.m.Exec(context.Background(), second, sandbox.Request{Mode: "python", Code: "print(1)"})
	if !errors.Is(err, sandbox.ErrAtCapacity) {
		t.Fatalf("err = %v, want ErrAtCapacity", err)
	}
	if res := <-busy; strings.TrimSpace(res.Stdout) != "first done" {
		t.Fatalf("first run disturbed: %+v", res)
	}
	// first is now idle, so second evicts it instead of waiting 15 minutes.
	if !f.m.HasCapacity(second) {
		t.Error("HasCapacity = false although the only sandbox is idle")
	}
	if res := f.python(t, second, "print(2)"); strings.TrimSpace(res.Stdout) != "2" {
		t.Fatalf("second = %+v", res)
	}
}

func TestIdleReaperNeverRemovesASandboxThatIsRunningCode(t *testing.T) {
	f := newFixture(t, func(c *sandbox.Config) { c.IdleTimeout = time.Millisecond })
	id := f.session(t)
	f.python(t, id, "print(0)") // create the sandbox, then let it look idle
	done := make(chan sandbox.Result)
	go func() { done <- f.python(t, id, "import time\ntime.sleep(2)\nprint('survived')") }()
	time.Sleep(700 * time.Millisecond)
	f.m.ReapIdle(time.Now().Add(time.Hour))
	if res := <-done; strings.TrimSpace(res.Stdout) != "survived" {
		t.Fatalf("exec was disturbed by the reaper: %+v", res)
	}
}

func TestBackgroundProcessesDoNotSurviveTheirCallOrLingerAsZombies(t *testing.T) {
	f := newFixture(t, nil)
	id := f.session(t)
	f.python(t, id, "import subprocess\nsubprocess.Popen(['sleep', '1000'], start_new_session=True)")
	res := f.python(t, id, `import os, time
time.sleep(0.3)
found = []
for pid in filter(str.isdigit, os.listdir('/proc')):
    try:
        cmd = open(f'/proc/{pid}/cmdline', 'rb').read().replace(b'\0', b' ').decode().strip()
        state = [l for l in open(f'/proc/{pid}/status') if l.startswith('State:')][0].split()[1]
    except OSError:
        continue
    if cmd == 'sleep 1000':
        found.append('leftover')
    if state == 'Z':
        found.append('zombie')
print(','.join(found) or 'clean')`)
	if strings.TrimSpace(res.Stdout) != "clean" {
		t.Fatalf("processes from the previous call remain: %+v", res)
	}
}

func TestMissingGVisorFallsBackToRunc(t *testing.T) {
	f := newFixture(t, func(c *sandbox.Config) { c.Runtime = "definitely-not-installed" })
	if f.m.Runtime() != "runc" {
		t.Fatalf("runtime = %q, want fallback to runc", f.m.Runtime())
	}
}
