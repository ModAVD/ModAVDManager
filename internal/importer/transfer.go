//go:build windows

package importer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/root"
)

const transferTimeout = 30 * time.Minute

type Progress struct {
	Bytes int64  `json:"bytes"`
	Path  string `json:"path,omitempty"`
}

type Device struct {
	Client *adb.Client
	Serial string
}

func (d Device) run(ctx context.Context, raw bool, args ...string) adb.Result {
	return d.Client.Run(ctx, adb.RunOptions{Serial: d.Serial, Timeout: transferTimeout, Reregister: true, Raw: raw}, args...)
}

// ListAPKPaths returns the split APK paths reported by package manager. It only
// reads metadata and does not modify the source instance.
func (d Device) ListAPKPaths(ctx context.Context, packageName string) ([]string, error) {
	r := d.run(ctx, false, "shell", "pm", "path", packageName)
	if !r.OK() {
		return nil, fmt.Errorf("查询 %s 的 APK 失败：%s", packageName, r.Out)
	}
	var out []string
	for _, line := range strings.Split(r.Out, "\n") {
		line = strings.TrimSpace(line)
		if p, ok := strings.CutPrefix(line, "package:"); ok && p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("实例里没有找到 %s 的 APK", packageName)
	}
	return out, nil
}

func (d Device) FreeBytes(ctx context.Context) (int64, error) {
	r := d.run(ctx, false, "shell", "df", "-k", "/data")
	if !r.OK() {
		return 0, fmt.Errorf("读取目标数据盘剩余空间失败：%s", r.Out)
	}
	for _, line := range strings.Split(r.Out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[len(fields)-1] != "/data" {
			continue
		}
		if len(fields) >= 4 {
			n, err := strconv.ParseInt(fields[len(fields)-3], 10, 64)
			if err == nil {
				return n * 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("df 输出没有 /data：%s", r.Out)
}

// InstallMultiple pulls only APK bytes through a temporary host directory. Game
// data is handled by CopyGameData and never lands on the host.
func InstallMultiple(ctx context.Context, source, target Device, packageName, tempRoot string) error {
	paths, err := source.ListAPKPaths(ctx, packageName)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(tempRoot, "modavd-import-apk-")
	if err != nil {
		return fmt.Errorf("建 APK 临时目录失败：%w", err)
	}
	defer os.RemoveAll(dir)
	local := make([]string, 0, len(paths))
	for n, remote := range paths {
		name := filepath.Join(dir, fmt.Sprintf("split-%02d.apk", n))
		r := source.run(ctx, false, "pull", remote, name)
		if !r.OK() {
			return fmt.Errorf("拷贝 APK %s 失败：%s", remote, r.Out)
		}
		local = append(local, name)
	}
	args := append([]string{"install-multiple", "-r"}, local...)
	r := target.run(ctx, false, args...)
	if !r.OK() || !strings.Contains(strings.ToLower(r.Out), "success") {
		return fmt.Errorf("安装 %s 失败：%s", packageName, r.Out)
	}
	return nil
}

// CopyGameData streams a tar archive from source to target. The host only sees
// the pipe and a byte counter, so Android/data and Android/obb never get written
// to a host file.
func CopyGameData(ctx context.Context, source, target Device, packageName string, progress func(Progress)) error {
	free, err := target.FreeBytes(ctx)
	if err != nil {
		return err
	}
	if free < 100*1024*1024 {
		return fmt.Errorf("目标数据盘剩余空间不足 100 MiB（只有 %d 字节）", free)
	}
	portSrc := strconv.Itoa(source.Client.Root.AdbServerPort())
	portDst := strconv.Itoa(target.Client.Root.AdbServerPort())
	adbSrc := source.Client.Root.AdbExe()
	adbDst := target.Client.Root.AdbExe()
	argsSrc := []string{"-P", portSrc, "-s", source.Serial, "exec-out", "tar", "-cf", "-", "-C", "/sdcard", "Android/data/" + packageName, "Android/obb/" + packageName}
	argsDst := []string{"-P", portDst, "-s", target.Serial, "exec-in", "tar", "-xf", "-", "-C", "/sdcard"}
	src := exec.CommandContext(ctx, adbSrc, argsSrc...)
	dst := exec.CommandContext(ctx, adbDst, argsDst...)
	src.Env = source.Client.Env()
	dst.Env = target.Client.Env()
	pipe, err := src.StdoutPipe()
	if err != nil {
		return err
	}
	var srcErr, dstErr strings.Builder
	src.Stderr = &srcErr
	dst.Stderr = &dstErr
	dst.Stdin = &countingReader{r: pipe, path: packageName, progress: progress}
	if err := dst.Start(); err != nil {
		return fmt.Errorf("启动目标 adb exec-in 失败：%w", err)
	}
	if err := src.Start(); err != nil {
		_ = dst.Process.Kill()
		return fmt.Errorf("启动源 adb exec-out 失败：%w", err)
	}
	srcWait := src.Wait()
	dstWait := dst.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if srcWait != nil {
		return fmt.Errorf("读取源数据失败：%s", strings.TrimSpace(srcErr.String()))
	}
	if dstWait != nil {
		return fmt.Errorf("写入目标数据失败：%s", strings.TrimSpace(dstErr.String()))
	}
	return nil
}

type countingReader struct {
	r        io.Reader
	path     string
	progress func(Progress)
	n        int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	if n > 0 && r.progress != nil {
		r.progress(Progress{Bytes: r.n, Path: r.path})
	}
	return n, err
}

// RootDevice creates a Device for one ModAVD instance without opening it. The
// caller supplies the instance index and controls when adb is used.
func RootDevice(r *root.Root, i int) Device {
	return Device{Client: adb.New(r, nil), Serial: root.Serial(i)}
}
