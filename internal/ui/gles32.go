package ui

// The reported OpenGL ES version (ModAVD/ModAVD#131, experimental) in the manager: an instance's 「报告 GLES 3.2」
// switch (emu GLES32). The switch's state shows on each instance (InstanceInfo.gles32).

import (
	"context"
	"fmt"
	"net/http"
)

// handleGLES32 is POST /api/instances/{i}/gles32-on and gles32-off: a job running emu's GLES32. What it refuses before
// touching the instance (no bridge, not there, being created) is answered at once, with errs.Reason in the details,
// so the page can say why.
func (s *server) handleGLES32(action string) http.HandlerFunc {
	kind, doing, done := kindGLES32On, "正在开启报告 GLES 3.2（会冷启动一次）", "已开启报告 GLES 3.2"
	if action == "off" {
		kind, doing, done = kindGLES32Off, "正在关闭报告 GLES 3.2（会冷启动一次）", "已关闭报告 GLES 3.2"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		i, ok := pathIndex(w, r)
		if !ok || !decode(w, r, &struct{}{}) {
			return
		}
		if err := s.checkFree(kind, i); err != nil {
			writeError(w, err)
			return
		}
		if err := s.mgr.GLES32Check(i, action); err != nil {
			writeError(w, err)
			return
		}
		s.respondJob(w)(s.startJob(kind, i, func(ctx context.Context, j *job, b backend) (any, error) {
			s.setProgress(j, jobProgress{Step: kind, Message: fmt.Sprintf("实例 %d %s", i, doing), Total: progressTotal})
			res, err := b.GLES32(ctx, i, action)
			if err != nil {
				return nil, err
			}
			s.setProgress(j, jobProgress{Step: "done", Message: fmt.Sprintf("实例 %d %s", i, done), Done: progressTotal, Total: progressTotal})
			return res, nil
		}))
	}
}
