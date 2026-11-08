//go:build linux

package claude

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This is an actual second-language flock owner, not a fake successful lock.
// No installed CLI, browser, network, default account or credentials are used.
const pythonActivation = `import fcntl,os,sys,time
root,mode=sys.argv[1:]
f=os.open(root+'/.activation.lock',os.O_RDWR|os.O_CREAT,0o600)
if mode=='idle':
 print('idle',flush=True)
 sys.stdin.readline()
elif mode=='hold':
 fcntl.flock(f,fcntl.LOCK_EX)
 print('held',flush=True)
 sys.stdin.readline()
elif mode=='contend':
 try:
  fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
  print('unexpected-free',flush=True)
 except BlockingIOError:
  print('blocked',flush=True)
 sys.stdin.readline()
 deadline=time.monotonic()+5
 while True:
  try:
   fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
   break
  except BlockingIOError:
   if time.monotonic()>=deadline: raise SystemExit(3)
   time.sleep(.01)
 print('acquired',flush=True)
os.close(f)
`

type pythonLockChild struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	waited bool
}

func startPythonLock(t *testing.T, root, mode string) *pythonLockChild {
	t.Helper()
	cmd := exec.Command("python3", "-c", pythonActivation, root, mode)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &pythonLockChild{cmd: cmd, input: input, output: bufio.NewReader(output)}
	t.Cleanup(func() {
		if !child.waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			child.waited = true
		}
	})
	return child
}
func (c *pythonLockChild) line(t *testing.T) string {
	t.Helper()
	type response struct {
		line string
		err  error
	}
	done := make(chan response, 1)
	go func() { line, err := c.output.ReadString('\n'); done <- response{line, err} }()
	select {
	case value := <-done:
		if value.err != nil {
			t.Fatal(value.err)
		}
		return value.line
	case <-time.After(8 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		t.Fatal("stand-in did not reach declared lock barrier")
	}
	return ""
}
func (c *pythonLockChild) release(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(c.input, "release"); err != nil {
		t.Fatal(err)
	}
}
func (c *pythonLockChild) finish(t *testing.T) {
	t.Helper()
	_ = c.input.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		c.waited = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		c.waited = true
		t.Fatal("stand-in did not terminate after release")
	}
}

func TestSwitchPythonHeldActivationRefusesThenReadsPublishedToken(t *testing.T) {
	f := newSwitchFixture(t)
	child := startPythonLock(t, filepath.Join(f.root, "profiles"), "hold")
	if got := child.line(t); got != "held\n" {
		t.Fatal(got)
	}
	original, err := os.ReadFile(filepath.Join(f.root, ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = Switch(f.backend, "other", 0, true)
	var lockErr *ActivationLockError
	if !errors.As(err, &lockErr) || lockErr.Code != "activation_busy" {
		t.Fatalf("expected typed activation timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("single admission budget exceeded: %s", elapsed)
	}
	after, _ := os.ReadFile(filepath.Join(f.root, ".credentials.json"))
	if string(after) != string(original) {
		t.Fatal("failed lock admission modified working default")
	}
	// Model a complete guarded publication before the next admitted Switch.
	writeJSON(t, filepath.Join(f.root, "profiles", "other", "credentials.json"), credentialsFixture("NEW", future))
	child.release(t)
	child.finish(t)
	if _, err := Switch(f.backend, "other", 0, true); err != nil {
		t.Fatal(err)
	}
	if got := f.token(filepath.Join(f.root, ".credentials.json")); got != "NEW" {
		t.Fatalf("activated stale token: %s", got)
	}
}

type pausedActivationBackend struct {
	*LinuxBackend
	entered chan struct{}
	release chan struct{}
}

func (b *pausedActivationBackend) active() (*Account, error) {
	close(b.entered)
	select {
	case <-b.release:
		return b.LinuxBackend.active()
	case <-time.After(8 * time.Second):
		return nil, fmt.Errorf("test activation barrier was not released")
	}
}
func TestSwitchHoldsSamePythonMutexAcrossReadSnapshotActivate(t *testing.T) {
	f := newSwitchFixture(t)
	backend := &pausedActivationBackend{LinuxBackend: f.backend, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := Switch(backend, "other", 0, true); done <- err }()
	released, joined := false, false
	defer func() {
		if !released {
			close(backend.release)
		}
		if !joined {
			select {
			case <-done:
			case <-time.After(9 * time.Second):
				t.Error("Switch worker failed final join")
			}
		}
	}()
	select {
	case <-backend.entered:
	case <-time.After(8 * time.Second):
		t.Fatal("Switch never reached protected read")
	}
	child := startPythonLock(t, filepath.Join(f.root, "profiles"), "contend")
	if got := child.line(t); got != "blocked\n" {
		t.Fatalf("Python entered during Go activation: %s", got)
	}
	close(backend.release)
	released = true
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Switch worker did not join")
	}
	child.release(t)
	if got := child.line(t); got != "acquired\n" {
		t.Fatal(got)
	}
	child.finish(t)
}

func TestActivationReleasesAfterHolderDeathAndRejectsReboundOrSymlink(t *testing.T) {
	f := newSwitchFixture(t)
	root := filepath.Join(f.root, "profiles")
	child := startPythonLock(t, root, "hold")
	if got := child.line(t); got != "held\n" {
		t.Fatal(got)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.cmd.Wait()
	child.waited = true
	lock, err := lockSwitchFile(root, ".activation.lock", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, ".activation.lock")
	if err := os.Rename(original, original+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := lock.check(); err == nil {
		t.Fatal("rebound lock inode accepted")
	}
	lock.release()
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original+".old", original); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockSwitchFile(root, ".activation.lock", time.Now().Add(time.Second)); err == nil {
		lock.release()
		t.Fatal("symlink lock accepted")
	}
}

func TestSwitchUncontendedCustomRootRoutingRemainsLegacy(t *testing.T) {
	f := newSwitchFixture(t)
	custom := filepath.Join(f.home, "custom")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	writeJSON(t, filepath.Join(custom, ".claude.json"), map[string]any{"keep": "CUSTOM"})
	target := filepath.Join(custom, "profiles", "other")
	writeJSON(t, filepath.Join(target, "credentials.json"), credentialsFixture("CUSTOM", future))
	writeJSON(t, filepath.Join(target, "oauthAccount.json"), map[string]any{"emailAddress": "custom@example.invalid", "organizationUuid": "custom-org"})
	before, err := os.ReadFile(filepath.Join(custom, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Switch(NewLinuxBackend(custom), "other", 0, true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(custom, ".claude.json"))
	if string(after) != string(before) {
		t.Fatal("934 changed custom-root identity routing")
	}
	identity := getMap(readJSONFile(t, filepath.Join(f.home, ".claude.json")), "oauthAccount")
	if getString(identity, "organizationUuid") != "custom-org" {
		t.Fatal("legacy HOME identity write changed; backlog 46 is separate")
	}
}

func TestActivationDescriptorDoesNotSurviveUnrelatedExec(t *testing.T) {
	f := newSwitchFixture(t)
	root := filepath.Join(f.root, "profiles")
	first, err := lockSwitchFile(root, ".activation.lock", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	child := startPythonLock(t, root, "idle")
	if got := child.line(t); got != "idle\n" {
		t.Fatal(got)
	}
	first.release()
	// The unrelated exec remains alive: an inherited flock would keep this busy.
	second, err := lockSwitchFile(root, ".activation.lock", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second.release()
	child.release(t)
	child.finish(t)
}

func TestSwitchCanonicalProfileWaitSharesOriginalAdmissionBudget(t *testing.T) {
	f := newSwitchFixture(t)
	root := filepath.Join(f.root, "profiles")
	selected, err := lockSwitchFile(filepath.Join(root, "other"), ".lock", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer selected.release()
	before, _ := os.ReadFile(filepath.Join(f.root, ".credentials.json"))
	start := time.Now()
	_, err = Switch(f.backend, "other", 0, true)
	var refusal *ActivationLockError
	if !errors.As(err, &refusal) || refusal.Code != "activation_busy" {
		t.Fatalf("expected canonical lock timeout, got %v", err)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("profile wait exceeded the shared five-second budget")
	}
	after, _ := os.ReadFile(filepath.Join(f.root, ".credentials.json"))
	if string(after) != string(before) {
		t.Fatal("failed target admission changed working default")
	}
	// No reverse nesting or leaked activation lock after the inner refusal.
	global, err := lockSwitchFile(root, ".activation.lock", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	global.release()
}
