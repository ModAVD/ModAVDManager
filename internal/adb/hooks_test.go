//go:build windows

package adb

import (
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/logx"
	"modavd/internal/root"
)

func TestHookGroupWait(t *testing.T) {
	var g hookGroup
	if !g.wait(0) {
		t.Fatal("an idle group must not wait")
	}
	g.add()
	start := time.Now()
	if g.wait(50 * time.Millisecond) {
		t.Fatal("wait returned true while a hook runs")
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Errorf("wait gave up after %v", d)
	}
	// a hook that starts while somebody waits is waited for too
	result := make(chan bool, 1)
	go func() { result <- g.wait(5 * time.Second) }()
	time.Sleep(20 * time.Millisecond)
	g.add()
	g.done() // the first one ends; the second still runs
	select {
	case <-result:
		t.Fatal("wait returned while the second hook runs")
	case <-time.After(50 * time.Millisecond):
	}
	g.done()
	select {
	case ok := <-result:
		if !ok {
			t.Error("wait = false after both hooks ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after the hooks ended")
	}
	// the group can be busy again afterwards
	g.add()
	if g.wait(10 * time.Millisecond) {
		t.Error("second busy period not waited for")
	}
	g.done()
	if !g.wait(time.Second) {
		t.Error("idle again = false")
	}
}

// hookClient is a Client whose log goes to the returned collector.
func hookClient(t *testing.T) (*Client, *logCollector) {
	t.Helper()
	logs := &logCollector{}
	return New(&root.Root{Dir: tempDir(t)}, logx.New(logs.sink)), logs
}

func TestWaitHooks(t *testing.T) {
	c, logs := hookClient(t)
	release := make(chan struct{})
	var ran []string
	var mu sync.Mutex
	hook := func(serial string) {
		<-release
		mu.Lock()
		ran = append(ran, serial)
		mu.Unlock()
	}
	c.OnReregistered = hook
	// a client derived for one call (emu's withLog) shares the tracking both ways
	d := c.Derive(nil, func(serial string) { hook("derived " + serial) })
	if d.Root != c.Root || d.OnReregistered == nil {
		t.Fatal("Derive lost the root or the hook")
	}
	c.runReregisterHook("emulator-20000")
	d.runReregisterHook("emulator-20010")

	start := time.Now()
	c.WaitHooks(50 * time.Millisecond)
	if el := time.Since(start); el < 40*time.Millisecond {
		t.Errorf("WaitHooks returned after %v with hooks running", el)
	}
	if logs.count("重新登记后的收尾") != 1 {
		t.Errorf("no warning that the hooks did not finish: %q", logs.msgs)
	}
	close(release)
	start = time.Now()
	d.WaitHooks(5 * time.Second)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("WaitHooks took %v after the hooks were released", el)
	}
	mu.Lock()
	got := strings.Join(ran, ",")
	mu.Unlock()
	if !strings.Contains(got, "emulator-20000") || !strings.Contains(got, "derived emulator-20010") {
		t.Errorf("hooks ran for %q", got)
	}
	if logs.count("重新登记后的收尾") != 1 {
		t.Errorf("warned although the hooks finished in time")
	}
}

func TestWaitHooksPanicAndLiteral(t *testing.T) {
	c, logs := hookClient(t)
	c.OnReregistered = func(string) { panic("boom") }
	c.runReregisterHook("emulator-20000")
	c.WaitHooks(5 * time.Second)
	if logs.count("emulator-20000 重新登记后的收尾失败: boom") != 1 {
		t.Errorf("panic not reported: %q", logs.msgs)
	}

	// a Client written as a literal still tracks its hooks
	release := make(chan struct{})
	lit := &Client{Root: c.Root, OnReregistered: func(string) { <-release }}
	lit.runReregisterHook("emulator-20000")
	done := make(chan struct{})
	go func() { lit.WaitHooks(5 * time.Second); close(done) }()
	select {
	case <-done:
		t.Fatal("literal client did not wait for its hook")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("literal client still waiting after the hook ended")
	}

	// no hook set: nothing runs, nothing to wait for
	c.OnReregistered = nil
	c.runReregisterHook("emulator-20000")
	start := time.Now()
	c.WaitHooks(time.Second)
	if time.Since(start) > 500*time.Millisecond {
		t.Error("WaitHooks waited without hooks")
	}
}
