package ui

import (
	"context"

	"modavd/internal/components"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

// unavailableDiagBackend is a fallback adapter used when the diagnostic
// backend is not wired in a particular build. It keeps the UI seam stable.
type unavailableDiagBackend struct{}

func unavailableDiagError() error {
	return errs.New(errs.NotImplemented, "组件和问题信息功能暂不可用，等待诊断后端接入")
}

func (unavailableDiagBackend) Components(context.Context) (*DiagComponentsResult, error) {
	return nil, unavailableDiagError()
}
func (unavailableDiagBackend) License(context.Context, components.SourceSettings) (*DiagLicenseResult, error) {
	return nil, unavailableDiagError()
}
func (unavailableDiagBackend) Fetch(context.Context, bool, components.SourceSettings, string, func(DiagProgress)) (*DiagFetchResult, error) {
	return nil, unavailableDiagError()
}
func (unavailableDiagBackend) Collect(context.Context, DiagCollectOptions, func(DiagProgress)) (*DiagCollectResult, error) {
	return nil, unavailableDiagError()
}
func (unavailableDiagBackend) ReportConfig(context.Context) (string, error) {
	return "", unavailableDiagError()
}

var _ diagBackend = unavailableDiagBackend{}

// Keep the logger argument in the factory signature so a future adapter can
// use the same per-job logger as the rest of the manager.
func newUnavailableDiagBackend(_ *logx.Logger) diagBackend { return unavailableDiagBackend{} }
