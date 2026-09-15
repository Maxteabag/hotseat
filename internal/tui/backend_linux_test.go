package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackendRootAndShutdownReapOwnedChildren(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "hotseat")
	if err := os.Mkdir(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(pkg, "__init__.py"), nil, 0600)
	script := `import subprocess,time
from pathlib import Path
p=subprocess.Popen(['sleep','60'])
Path(__file__).with_name('child.pid').write_text(str(p.pid))
time.sleep(60)
`
	_ = os.WriteFile(filepath.Join(pkg, "tui_bridge.py"), []byte(script), 0600)
	b := &PythonBackend{Python: "python3", Root: root}
	defer b.Close()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = b.Snapshot(context.Background(), true) }()
	pidFile := filepath.Join(pkg, "child.pid")
	deadline := time.Now().Add(5 * time.Second)
	var pid []byte
	var err error
	for time.Now().Before(deadline) {
		pid, err = os.ReadFile(pidFile)
		if err == nil && len(pid) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pid) == 0 {
		t.Fatal("specified backend root was not used")
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("backend did not stop")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile("/proc/" + strings.TrimSpace(string(pid)) + "/stat")
		if os.IsNotExist(err) {
			return
		}
		fields := strings.Fields(string(data))
		if len(fields) > 2 && fields[2] == "Z" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("backend descendant remained running")
}
