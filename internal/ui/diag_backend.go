package ui

import (
	"context"

	"modavd/internal/components"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/report"
	"modavd/internal/root"
)

type realDiagBackend struct {
	r   *root.Root
	log *logx.Logger
}

var (
	diagComponents = func(r *root.Root) []emu.ComponentStatus { return emu.ComponentStatuses(r.Components()) }
	diagLicense    = components.License
	diagFetch      = components.Fetch
	diagCollect    = report.Collect
	diagConfig     = report.BuildConfig
)

func newRealDiagBackend(r *root.Root, log *logx.Logger) diagBackend {
	if log == nil {
		log = logx.Nop()
	}
	return &realDiagBackend{r: r, log: log}
}

func (b *realDiagBackend) Components(ctx context.Context) (*DiagComponentsResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.Canceled, err, "已取消")
	}
	return &DiagComponentsResult{Root: b.r.Dir, Components: toDiagComponents(diagComponents(b.r))}, nil
}

func (b *realDiagBackend) License(ctx context.Context, source components.SourceSettings) (*DiagLicenseResult, error) {
	text, from, err := diagLicense(ctx, source)
	if err != nil {
		return nil, err
	}
	return &DiagLicenseResult{Text: text, Source: from, SHA256: components.LicenseSHA256(text)}, nil
}

func (b *realDiagBackend) Fetch(ctx context.Context, accept bool, source components.SourceSettings, licenseSHA256 string, progress func(DiagProgress)) (*DiagFetchResult, error) {
	if !accept {
		return nil, errs.New(errs.LicenseDeclined, "没有同意许可协议，已退出，什么也没有下载")
	}
	res, err := diagFetch(ctx, b.r, components.Options{
		AcceptLicense: true,
		Input:         nil,
		Mirror:        source.Mirror,
		MirrorURLs:    append([]string(nil), source.MirrorURLs...),
		LicenseSHA256: licenseSHA256,
		SourceOrigin:  components.SourceFromManager,
		Log:           b.log,
		// the license text and the note under it: the user has just read them in the dialog, so they go to the
		// file log only, not as messages of the job (logs\fetch-components.log and downloads\ keep them too)
		Notice: func(s string) { b.log.Debugf("%s", s) },
		Progress: func(p components.Progress) {
			if progress != nil {
				progress(diagProgressFromComponent(p))
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &DiagFetchResult{Root: res.Root, License: res.License, LicenseSource: res.LicenseSource,
		Components: toDiagFetchComponents(res.Components), Downloads: res.Downloads}, nil
}

func (b *realDiagBackend) Collect(ctx context.Context, opt DiagCollectOptions, progress func(DiagProgress)) (*DiagCollectResult, error) {
	res, err := diagCollect(ctx, b.r, report.CollectOptions{
		OutDir: opt.OutDir, StageDir: opt.StageDir, KeepStage: opt.KeepStage, NoExplorer: true,
		Progress: func(msg string) {
			if progress != nil {
				progress(DiagProgress{Stage: "collect", Message: msg})
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &DiagCollectResult{ZipPath: res.ZipPath, ZipBytes: res.ZipBytes, ZipSize: res.ZipSize,
		Entries: res.Entries, Stage: res.Stage, StageKept: res.StageKept, StageError: res.StageError,
		Problems: res.Problems, MasFound: res.MasFound, Summary: res.Summary, PrivacyNote: res.PrivacyNote}, nil
}

func (b *realDiagBackend) ReportConfig(ctx context.Context) (string, error) {
	res, err := diagConfig(ctx, b.r, report.ConfigOptions{NoClipboard: true})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func toDiagComponents(in []emu.ComponentStatus) []DiagComponent {
	out := make([]DiagComponent, 0, len(in))
	for _, c := range in {
		out = append(out, DiagComponent{ID: c.ID, Name: c.Name, Version: c.Version, Build: c.Build,
			Installed: c.Installed, Optional: c.Optional, LocalSDK: c.LocalSDK})
	}
	return out
}

func toDiagFetchComponents(in []components.ComponentResult) []DiagFetchComponent {
	out := make([]DiagFetchComponent, 0, len(in))
	for _, c := range in {
		out = append(out, DiagFetchComponent{ID: c.ID, Name: c.Name, Version: c.Version,
			Installed: c.Installed, Downloaded: c.Downloaded, Skipped: c.Skipped, SetAside: c.SetAside})
	}
	return out
}

func diagProgressFromComponent(p components.Progress) DiagProgress {
	remaining := 0.0
	if p.Speed > 0 && p.Total > 0 && p.Done <= p.Total {
		remaining = float64(p.Total-p.Done) / p.Speed
	}
	return DiagProgress{Stage: p.Stage, Name: p.Name, Message: p.Message, Done: p.Done, Total: p.Total,
		Speed: p.Speed, Remaining: remaining}
}

var _ diagBackend = (*realDiagBackend)(nil)
