//go:build windows

package avd

import (
	"fmt"
	"os"
	"testing"

	"modavd/internal/errs"
)

func assertDisplay(t *testing.T, in *Instance, want [3]int) {
	t.Helper()
	w, h, dpi := in.Display()
	if got := [3]int{w, h, dpi}; got != want {
		t.Fatalf("display = %v, want %v", got, want)
	}
	for n, key := range []string{"hw.lcd.width", "hw.lcd.height", "hw.lcd.density"} {
		if in.Config.Value(key) != fmt.Sprint(want[n]) {
			t.Fatalf("stored %s = %q, want %d", key, in.Config.Value(key), want[n])
		}
	}
}

func TestResolutionCreateSwitchAndReboot(t *testing.T) {
	for _, preset := range []int{0, 1080, 720} {
		for _, portrait := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/portrait=%t", preset, portrait), func(t *testing.T) {
				r := newRoot(t)
				in, err := Create(r, 0, CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60, Resolution: preset})
				if err != nil {
					t.Fatal(err)
				}
				initial := preset
				if initial == 0 {
					initial = 1080
				}
				if portrait {
					w, h, _ := in.Display()
					in.Config.Set("hw.lcd.width", fmt.Sprint(h))
					in.Config.Set("hw.lcd.height", fmt.Sprint(w))
					if err := WriteIni(ConfigPath(r, 0), in.Config); err != nil {
						t.Fatal(err)
					}
				}
				other := 720
				if initial == 720 {
					other = 1080
				}
				for step, resolution := range []int{initial, other, initial} {
					if step > 0 {
						applied, err := Update(r, 0, UpdateOptions{Resolution: &resolution})
						if err != nil || applied["resolution"] != resolution {
							t.Fatalf("update: %v %v", applied, err)
						}
					}
					want := [3]int{1280, 720, 240}
					if resolution == 1080 {
						want = [3]int{1920, 1080, 280}
					}
					if portrait {
						want[0], want[1] = want[1], want[0]
					}
					// Re-read from disk across repeated pre-boot writes, as independent starts do.
					for boot := 0; boot < 3; boot++ {
						if _, err := ApplyDisplay(r, 0); err != nil {
							t.Fatal(err)
						}
						in, err = Get(r, 0)
						if err != nil {
							t.Fatal(err)
						}
						if in.Resolution() != resolution {
							t.Fatalf("resolution = %d", in.Resolution())
						}
						assertDisplay(t, in, want)
					}
				}
			})
		}
	}
}

func TestResolutionLegacy(t *testing.T) {
	for _, tc := range []struct {
		name, cfg string
		want      [3]int
	}{
		{"missing", "x=1\n", [3]int{1280, 720, 240}},
		{"missing width", "hw.lcd.height=720\n", [3]int{720, 1280, 240}},
		{"missing height", "hw.lcd.width=1920\n", [3]int{1280, 720, 240}},
		{"bad width", "hw.lcd.width=bad\nhw.lcd.height=1\n", [3]int{720, 1280, 240}},
		{"720 no dpi", "hw.lcd.width=1280\nhw.lcd.height=720\n", [3]int{1280, 720, 240}},
		{"1080 wrong dpi", "hw.lcd.width=1920\nhw.lcd.height=1080\nhw.lcd.density=240\n", [3]int{1920, 1080, 280}},
		{"portrait 1080", "hw.lcd.width=1080\nhw.lcd.height=1920\n", [3]int{1080, 1920, 280}},
		{"custom", "hw.lcd.width=1440\nhw.lcd.height=2560\nhw.lcd.density=400\n", [3]int{720, 1280, 240}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRoot(t)
			writeFile(t, ConfigPath(r, 0), []byte(tc.cfg))
			if _, err := ApplyDisplay(r, 0); err != nil {
				t.Fatal(err)
			}
			cfg, err := ReadIni(ConfigPath(r, 0))
			if err != nil {
				t.Fatal(err)
			}
			assertDisplay(t, &Instance{Config: cfg}, tc.want)
			if wrote, err := ApplyDisplay(r, 0); err != nil || wrote {
				t.Fatalf("repeat: %t %v", wrote, err)
			}
		})
	}
}

func TestResolutionValidationBeforeWrite(t *testing.T) {
	if got, err := ValidateResolution(nil); err != nil || got != 1080 {
		t.Fatalf("default: %d %v", got, err)
	}
	r := newRoot(t)
	in, err := Create(r, 0, CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(ConfigPath(r, 0))
	for _, value := range []int{0, -1, 1440, 1920} {
		_, err := ValidateResolution(&value)
		wantCode(t, err, errs.InvalidArgument)
		_, err = Update(r, 0, UpdateOptions{Resolution: &value})
		wantCode(t, err, errs.InvalidArgument)
		after, _ := os.ReadFile(ConfigPath(r, 0))
		if string(before) != string(after) {
			t.Fatal("invalid update changed config")
		}
	}
	bad := 1440
	_, err = Create(r, 1, CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60, Resolution: bad})
	wantCode(t, err, errs.InvalidArgument)
	if _, err := os.Stat(Dir(r, 1)); !os.IsNotExist(err) {
		t.Fatal("invalid create left files")
	}
	assertDisplay(t, in, [3]int{1920, 1080, 280})
}
