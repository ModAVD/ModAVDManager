package components

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Progress is one report of a download or an unpack: at most one every progressEvery, plus a final one each.
type Progress struct {
	Stage   string  // downloading / extracting / verifying (ImportPlayStore reading the given files)
	Name    string  // the component (fetch-components.ps1 display name)
	Done    int64   // bytes so far (a resumed download counts what was there)
	Total   int64   // bytes in all
	Speed   float64 // bytes per second: smoothed while downloading, the average in the final report; 0 for an unpack
	Final   bool
	Message string // the line fetch-components.ps1 shows for it
}

// progressEvery is how often the script refreshes its progress line (a variable for tests).
var progressEvery = 500 * time.Millisecond

// meter turns byte counts into Progress reports for f.o.Progress. Downloads get the script's speed estimate: sampled
// once a second at most, smoothed 0.7 / 0.3, and the time left from it.
type meter struct {
	report func(Progress)
	stage  string
	name   string
	total  int64
	start  int64 // bytes there before this transfer (resume)
	t0     time.Time
	last   time.Time

	sampleT time.Duration
	sampleB int64
	speed   float64
}

func newMeter(report func(Progress), stage, name string, total, start int64) *meter {
	return &meter{report: report, stage: stage, name: name, total: total, start: start, t0: time.Now(), sampleB: start}
}

// add is called after each chunk with the bytes done so far.
func (m *meter) add(done int64) {
	if m.stage == "downloading" {
		if t := time.Since(m.t0); t-m.sampleT >= time.Second {
			inst := float64(done-m.sampleB) / (t - m.sampleT).Seconds()
			if m.speed <= 0 {
				m.speed = inst
			} else {
				m.speed = 0.7*m.speed + 0.3*inst
			}
			m.sampleT, m.sampleB = t, done
		}
	}
	if m.report == nil || (!m.last.IsZero() && time.Since(m.last) < progressEvery) {
		return
	}
	m.last = time.Now()
	p := Progress{Stage: m.stage, Name: m.name, Done: done, Total: m.total, Speed: m.speed}
	if m.stage == "downloading" {
		eta := -1.0
		if m.speed > 0 {
			eta = float64(m.total-done) / m.speed
		}
		p.Message = fmt.Sprintf("  %s  %5s%%  %s / %s  %s/s  剩余 %s", m.name, fixed(percent(done, m.total), 1),
			formatSize(float64(done)), formatSize(float64(m.total)), formatSize(m.speed), formatDuration(eta))
	} else {
		p.Message = fmt.Sprintf("  %s %s  %5s%%  %s / %s", m.verb(), m.name, fixed(percent(done, m.total), 1),
			formatSize(float64(done)), formatSize(float64(m.total)))
	}
	m.report(p)
}

// verb names what a stage other than downloading does: 校验 (verifying, an import reading the user's files) or 解压.
func (m *meter) verb() string {
	if m.stage == "verifying" {
		return "校验"
	}
	return "解压"
}

// finish is the final report.
func (m *meter) finish(done int64) {
	if m.report == nil {
		return
	}
	p := Progress{Stage: m.stage, Name: m.name, Done: done, Total: m.total, Final: true}
	if m.stage == "downloading" {
		if t := time.Since(m.t0).Seconds(); t > 0 {
			p.Speed = float64(done-m.start) / t
		}
		p.Message = fmt.Sprintf("  %s  %5s%%  %s / %s  平均 %s/s", m.name, fixed(percent(done, m.total), 1),
			formatSize(float64(done)), formatSize(float64(m.total)), formatSize(p.Speed))
	} else {
		p.Message = fmt.Sprintf("  %s %s  100.0%%  %s", m.verb(), m.name, formatSize(float64(done)))
	}
	m.report(p)
}

func percent(done, total int64) float64 {
	if total <= 0 {
		return 100
	}
	return 100 * float64(done) / float64(total)
}

// fixed is .NET's "0.0" / "0.00" formatting: the digits after the point, halves rounded away from zero.
func fixed(v float64, digits int) string {
	p := math.Pow(10, float64(digits))
	return strconv.FormatFloat(math.Round(v*p)/p, 'f', digits, 64)
}

// formatSize is the script's Format-Size: 1.46 GB, 12.3 MB, 512 KB, 80 B.
func formatSize(b float64) string {
	switch {
	case b >= 1<<30:
		return fixed(b/(1<<30), 2) + " GB"
	case b >= 1<<20:
		return fixed(b/(1<<20), 1) + " MB"
	case b >= 1<<10:
		return fixed(b/(1<<10), 0) + " KB"
	}
	return fixed(b, 0) + " B"
}

// formatDuration is the script's Format-Duration: mm:ss, h:mm:ss from an hour on, --:-- when unknown.
func formatDuration(seconds float64) string {
	if seconds < 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) {
		return "--:--"
	}
	// [math]::Round rounds halves to even.
	s := int64(math.RoundToEven(seconds))
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60%60, s%60)
}

// seconds is a duration in the script's messages: "20", or "0.2" for the short ones tests use.
func seconds(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) }
