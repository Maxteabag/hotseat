package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheReadChild(t *testing.T) {
	root := os.Getenv("HOTSEAT_TEST_CACHE_ROOT")
	if root == "" {
		return
	}
	c := Cache{Root: root, Fetch: func(ctx context.Context, _ string) (Summary, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", os.Getenv("HOTSEAT_TEST_USAGE_URL"), nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return Summary{}, err
		}
		defer res.Body.Close()
		var s Summary
		err = json.NewDecoder(res.Body).Decode(&s)
		return s, err
	}}
	if _, err := c.Read(context.Background(), "fake-process-token"); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentProcessesShareOneUsageRequest(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 4)
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-gate
		fmt.Fprint(w, `{"used_5h":10,"used_7d":20}`)
	}))
	defer server.Close()
	root := t.TempDir()
	var cmds []*exec.Cmd
	for range 3 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCacheReadChild$")
		cmd.Env = append(os.Environ(), "HOTSEAT_TEST_CACHE_ROOT="+root, "HOTSEAT_TEST_USAGE_URL="+server.URL)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(gate)
		t.Fatal("no child reached fake usage server")
	}
	// Keep the first response in flight while the other independent processes
	// reach the same cache; every duplicate enters the counted HTTP handler.
	time.Sleep(250 * time.Millisecond)
	close(gate)
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("usage requests=%d, want one shared request", n)
	}
}

func TestCancelledCacheWaitDoesNotStartAnotherFetch(t *testing.T) {
	entered := make(chan struct{})
	gate := make(chan struct{})
	var calls atomic.Int32
	c := Cache{Root: t.TempDir(), Fetch: func(ctx context.Context, _ string) (Summary, error) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-gate
		} else {
			return Summary{}, ctx.Err()
		}
		return Summary{}, nil
	}}
	done := make(chan error, 1)
	go func() { _, err := c.Read(context.Background(), "fake-token"); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Read(ctx, "fake-token")
	close(gate)
	if err != context.Canceled {
		t.Fatalf("cancelled waiter returned %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("cancelled waiter started another fetch: %d", n)
	}
}
