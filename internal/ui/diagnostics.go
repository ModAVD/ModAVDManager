package ui

import (
	"context"
)

// DiagComponent is the component row shown by the manager.  It deliberately
// mirrors the JSON contract without importing internal/components or
// internal/report; those packages have an independent implementation owner.
type DiagComponent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Build     string `json:"build,omitempty"`
	Installed bool   `json:"installed"`
	Optional  bool   `json:"optional"`
	LocalSDK  bool   `json:"local_sdk"`
}

type DiagComponentsResult struct {
	Root       string          `json:"root"`
	Components []DiagComponent `json:"components"`
}

type DiagLicenseResult struct {
	Text   string `json:"license"`
	Source string `json:"license_source"`
}

// DiagProgress is shared by component fetching and report collection. Done and
// Total are bytes when known; Speed is bytes/second and Remaining is seconds.
type DiagProgress struct {
	Stage     string  `json:"stage"`
	Message   string  `json:"message"`
	Done      int64   `json:"done"`
	Total     int64   `json:"total"`
	Speed     float64 `json:"speed"`
	Remaining float64 `json:"remaining"`
}

type DiagFetchResult struct {
	Root       string          `json:"root"`
	License    string          `json:"license"`
	LicenseSrc string          `json:"license_source"`
	Components []DiagComponent `json:"components"`
	Downloads  []string        `json:"downloads"`
}

type DiagCollectOptions struct {
	OutDir    string `json:"out_dir"`
	StageDir  string `json:"stage_dir"`
	KeepStage bool   `json:"keep_stage"`
}

type DiagSectionResult struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

type DiagCollectResult struct {
	ZipPath  string              `json:"zip_path"`
	Stage    string              `json:"stage"`
	Summary  string              `json:"summary"`
	Entries  []string            `json:"entries"`
	Errors   []string            `json:"errors"`
	Sections []DiagSectionResult `json:"sections"`
}

// diagBackend is the narrow seam between the UI and the components/report
// packages.  A real adapter can be added without changing HTTP, jobs or web.
// Every operation receives the job context, so cancellation must stop work
// and return errs.Canceled (or a wrapped equivalent).
type diagBackend interface {
	Components(context.Context) (*DiagComponentsResult, error)
	License(context.Context) (*DiagLicenseResult, error)
	Fetch(context.Context, bool, func(DiagProgress)) (*DiagFetchResult, error)
	Collect(context.Context, DiagCollectOptions, func(DiagProgress)) (*DiagCollectResult, error)
	ReportConfig(context.Context) (string, error)
}
