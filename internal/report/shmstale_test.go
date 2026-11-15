package report

import (
	"errors"
	"strings"
	"testing"

	"modavd/internal/root"
)

// shmHost is a Host whose only working part is shmReporter.
type shmHost struct {
	Host
	shms map[string][]Process
	err  error
}

func (h shmHost) ScreenshotShm(name string) (bool, []Process, error) {
	ps, ok := h.shms[name]
	return ok, ps, h.err
}

func TestStaleScreenshotShm(t *testing.T) {
	plan := root.PortPlan{Base: 20000}
	c := &collector{
		h:         shmHost{shms: map[string][]Process{"SHM_videmulator20000": {{PID: 41, Name: "maawoa.exe"}}, "SHM_videmulator20010": nil, "SHM_videmulator20020": nil}},
		instances: []string{"mas_0", "mas_1", "mas_2", "mas_3", "other"},
		plan:      plan,
		running:   []liveTarget{{name: "mas_2"}}, // its own emulator holds it: not listed
	}
	c.staleScreenshotShm()
	got := strings.Join(c.lines, "\n")
	want := "\n== 没在运行的实例留下的截图共享内存（有程序还拿着它，下次开机截图共享可能开不起来） ==\n" +
		"mas_0：SHM_videmulator20000 还在（拿着它的程序：maawoa.exe（pid 41））\n" +
		"mas_1：SHM_videmulator20010 还在（没认出是哪个程序）"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	// nothing left over: no section at all (the golden summaries stay as they are)
	c = &collector{h: shmHost{}, instances: []string{"mas_0"}, plan: plan}
	c.staleScreenshotShm()
	if len(c.lines) != 0 {
		t.Errorf("empty: %q", c.lines)
	}

	c = &collector{h: shmHost{shms: map[string][]Process{"SHM_videmulator20000": nil}, err: errors.New("读不到系统句柄表")},
		instances: []string{"mas_0"}, plan: plan}
	c.staleScreenshotShm()
	if !strings.Contains(strings.Join(c.lines, "\n"), "mas_0：SHM_videmulator20000 还在（查不出是哪个程序：读不到系统句柄表）") {
		t.Errorf("error: %q", c.lines)
	}
}
