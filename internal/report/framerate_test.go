package report

import (
	"strings"
	"testing"
	"time"
)

// A trimmed real dump (Android 14 guest at hw.lcd.vsync=48, scrolling Settings): the global block, then the layers.
const timestatsSample = `         present offset:         0 ns	        VSYNC period:  20833333 ns
  refresh-rate              : 48.00 Hz
SurfaceFlinger TimeStats:
statsStart = 1794743320
totalFrames = 647
missedFrames = 0
present2present histogram is as below:
20ms=600
layerName = com.android.settings/com.android.settings.homepage.SettingsHomepageActivity#201
packageName =
totalFrames = 646
averageFPS = 44.168
present2present histogram is as below:
0ms=0 18ms=1 19ms=2 20ms=507 21ms=123 23ms=1 86ms=9 350ms=3
post2present histogram is as below:
19ms=18 20ms=544
layerName = StatusBar#88
packageName =
totalFrames = 3
averageFPS = 0.3
present2present histogram is as below:
1000ms=3
layerName = none
packageName =
totalFrames = 0
`

func TestSummarizeFrameRate(t *testing.T) {
	got := summarizeFrameRate(timestatsSample, 10*time.Second, 0)
	want := "屏幕刷新率 48 Hz；" +
		"com.android.settings/com.android.settings.homepage.SettingsHomepageActivity#201 10 秒 646 帧，平均 44.2 帧/秒，帧间隔中位 20 ms、95% 21 ms；" +
		"StatusBar#88 10 秒 3 帧，平均 0.3 帧/秒，帧间隔中位 1000 ms、95% 1000 ms"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestSummarizeFrameRateEmpty(t *testing.T) {
	for _, c := range []struct {
		out  string
		exit int
		want string
	}{
		{"", 1, "读不到屏幕刷新率；采样失败，退出码 1"},
		{"  refresh-rate : 60.00 Hz\nlayerName = none\ntotalFrames = 0\n", 0, "屏幕刷新率 60 Hz；采样期间画面没有更新"},
		{"  refresh-rate : 59.94 Hz\n", 0, "屏幕刷新率 59.94 Hz；采样期间画面没有更新"},
	} {
		if got := summarizeFrameRate(c.out, 10*time.Second, c.exit); got != c.want {
			t.Errorf("%q: got %q, want %q", c.out, got, c.want)
		}
	}
}

func TestFrameRateCommandStopsRecording(t *testing.T) {
	cmd := frameRateCommand(10 * time.Second)
	if len(cmd) != 1 {
		t.Fatalf("want one shell argument, got %d", len(cmd))
	}
	s := cmd[0]
	if !strings.Contains(s, "sleep 10 &&") || !strings.HasSuffix(s, "--timestats -disable -clear >/dev/null 2>&1") {
		t.Errorf("command does not sample 10 s and stop recording at the end: %s", s)
	}
	if strings.Contains(s, "grep -m") {
		t.Error("grep -m quits early and gives dumpsys a broken pipe")
	}
}

func TestHistPercentiles(t *testing.T) {
	if _, _, ok := histPercentiles(map[int]int{}); ok {
		t.Error("empty histogram gave percentiles")
	}
	med, p95, _ := histPercentiles(map[int]int{16: 90, 33: 5, 50: 5})
	if med != 16 || p95 != 33 {
		t.Errorf("med %d p95 %d, want 16 33", med, p95)
	}
	med, p95, _ = histPercentiles(map[int]int{16: 90, 33: 4, 50: 6})
	if med != 16 || p95 != 50 {
		t.Errorf("med %d p95 %d, want 16 50", med, p95)
	}
}
