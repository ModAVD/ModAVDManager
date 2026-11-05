//go:build darwin && cgo

package macos

import (
	"bytes"
	"errors"
	"strings"
)

// Environment returns the original launch environment from KERN_PROCARGS2.
// Like CommandLine, the native query verifies this process's creation identity
// before and after reading it. Malformed or duplicate entries are ambiguous and
// must not authorize another component to adopt the process.
func (p Process) Environment() (map[string]string, error) {
	_, buf, err := p.processArguments()
	if err != nil {
		return nil, err
	}
	env := make(map[string]string)
	for len(buf) > 0 && buf[0] != 0 {
		end := bytes.IndexByte(buf, 0)
		if end < 0 {
			return nil, errors.New("进程环境不完整")
		}
		key, value, ok := strings.Cut(string(buf[:end]), "=")
		if !ok || key == "" {
			return nil, errors.New("进程环境条目无效")
		}
		if _, duplicate := env[key]; duplicate {
			return nil, errors.New("进程环境含重复变量")
		}
		env[key] = value
		buf = buf[end+1:]
	}
	return env, nil
}
