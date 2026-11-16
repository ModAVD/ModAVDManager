package report

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The frame rate each running instance actually shows: SurfaceFlinger's refresh rate and a short timestats sample of
// what it presented, per layer. A user who says "it stutters" cannot tell whether the game draws 15 frames or the
// screen runs at 30 Hz; this says which (ModAVD/ModAVD#180 lets users pick any rate, so the two often differ).
// collect-info.ps1 never had it: a Go-only section and fps\ entries (goOnlySections, goOnlyEntry).

const sectionFrameRate = "实际帧率（SurfaceFlinger 采样）"

// frameRateSample is how long timestats records; the instances are sampled at the same time.
var frameRateSample = 10 * time.Second

type liveSerial struct{ name, serial string }

// frameRateCommand: the display's refresh rate and vsync period, then a timestats sample of frameRateSample seconds
// (cleared first, disabled and cleared afterwards so nothing keeps recording). One argument: adbLive joins a shell
// command's arguments as they are. grep reads to the end, so dumpsys gets no broken pipe.
func frameRateCommand(sample time.Duration) []string {
	ts := "dumpsys SurfaceFlinger --timestats "
	return []string{"dumpsys SurfaceFlinger 2>/dev/null | grep -E 'refresh-rate|VSYNC period'; " +
		ts + "-disable -clear >/dev/null 2>&1; " +
		ts + "-enable >/dev/null 2>&1 && sleep " + strconv.Itoa(int(sample.Seconds())) + " && " +
		ts + "-dump -maxlayers 5; " +
		ts + "-disable -clear >/dev/null 2>&1"}
}

func (c *collector) frameRate() error {
	c.section(sectionFrameRate)
	if len(c.liveSerials) == 0 {
		c.L("没有能连上的实例，跳过")
		return nil
	}
	dir := filepath.Join(c.stage, "fps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	c.progress(fmt.Sprintf("正在采样实际帧率（%d 秒）", int(frameRateSample.Seconds())))
	results := make([]adbResult, len(c.liveSerials))
	var wg sync.WaitGroup
	for i, t := range c.liveSerials {
		wg.Add(1)
		go func(i int, serial string) {
			defer wg.Done()
			results[i] = adbLive(c.ctx, c.adbPort, serial, frameRateSample+nativeTimeout, nil, "shell",
				frameRateCommand(frameRateSample)...)
		}(i, t.serial)
	}
	wg.Wait()
	for i, t := range c.liveSerials {
		res := results[i]
		if res.TimedOut {
			c.L(t.name + "：超时，已放弃")
			continue
		}
		text := adbTextMode(decodeText(res.Stdout))
		if netTrim(res.Stderr) != "" {
			text += "\r\n# stderr:\r\n" + adbTextMode(res.Stderr)
		}
		if err := os.WriteFile(filepath.Join(dir, t.name+".txt"), []byte(text), 0o644); err != nil {
			return err
		}
		c.L(t.name + "：" + summarizeFrameRate(decodeText(res.Stdout), frameRateSample, res.ExitCode))
	}
	return nil
}

var (
	refreshRateLine = regexp.MustCompile(`refresh-rate\s*:\s*([0-9.]+)\s*Hz`)
	tsField         = regexp.MustCompile(`^\s*(\w+) = (.*)$`)
	histBucket      = regexp.MustCompile(`([0-9]+)ms=([0-9]+)`)
)

type tsLayer struct {
	name        string
	totalFrames int
	averageFPS  float64
	p2p         map[int]int // present-to-present interval, ms bucket → frames
}

// summarizeFrameRate turns the command's output into one line: the refresh rate, then the busiest layers with their
// average frame rate and how even the intervals were (median and 95th percentile of present-to-present).
func summarizeFrameRate(out string, sample time.Duration, exit int) string {
	var parts []string
	if m := refreshRateLine.FindStringSubmatch(out); m != nil {
		parts = append(parts, "屏幕刷新率 "+trimFloat(m[1])+" Hz")
	} else {
		parts = append(parts, "读不到屏幕刷新率")
	}
	layers := parseTimestats(out)
	sort.SliceStable(layers, func(i, j int) bool { return layers[i].totalFrames > layers[j].totalFrames })
	shown := 0
	for _, l := range layers {
		if l.totalFrames == 0 || shown == 2 {
			continue
		}
		shown++
		s := fmt.Sprintf("%s %d 秒 %d 帧，平均 %.1f 帧/秒", l.name, int(sample.Seconds()), l.totalFrames, l.averageFPS)
		if med, p95, ok := histPercentiles(l.p2p); ok {
			s += fmt.Sprintf("，帧间隔中位 %d ms、95%% %d ms", med, p95)
		}
		parts = append(parts, s)
	}
	if shown == 0 {
		if len(layers) == 0 && exit != 0 {
			parts = append(parts, fmt.Sprintf("采样失败，退出码 %d", exit))
		} else {
			parts = append(parts, "采样期间画面没有更新")
		}
	}
	return strings.Join(parts, "；")
}

// parseTimestats reads the per-layer blocks of `dumpsys SurfaceFlinger --timestats -dump`: each starts with
// "layerName = …"; the global block before the first one is skipped.
func parseTimestats(out string) []tsLayer {
	var layers []tsLayer
	var cur *tsLayer
	nextHist := false
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if nextHist {
			nextHist = false
			if cur != nil {
				for _, m := range histBucket.FindAllStringSubmatch(line, -1) {
					ms, _ := strconv.Atoi(m[1])
					n, _ := strconv.Atoi(m[2])
					if n > 0 {
						cur.p2p[ms] += n
					}
				}
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "present2present histogram") {
			nextHist = true
			continue
		}
		m := tsField.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, val := m[1], strings.TrimSpace(m[2])
		switch {
		case key == "layerName":
			layers = append(layers, tsLayer{name: val, p2p: map[int]int{}})
			cur = &layers[len(layers)-1]
		case cur == nil:
		case key == "totalFrames":
			cur.totalFrames, _ = strconv.Atoi(val)
		case key == "averageFPS":
			cur.averageFPS, _ = strconv.ParseFloat(val, 64)
		}
	}
	return layers
}

// histPercentiles: the buckets holding the median and the 95th percentile frame.
func histPercentiles(h map[int]int) (med, p95 int, ok bool) {
	total := 0
	keys := make([]int, 0, len(h))
	for k, n := range h {
		total += n
		keys = append(keys, k)
	}
	if total == 0 {
		return 0, 0, false
	}
	sort.Ints(keys)
	at := func(q float64) int {
		need := int(q*float64(total) + 0.999999)
		seen := 0
		for _, k := range keys {
			if seen += h[k]; seen >= need {
				return k
			}
		}
		return keys[len(keys)-1]
	}
	return at(0.5), at(0.95), true
}

func trimFloat(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
