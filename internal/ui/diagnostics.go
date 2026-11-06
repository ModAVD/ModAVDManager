package ui

import (
	"context"

	"modavd/internal/components"
)

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
	SHA256 string `json:"license_sha256"`
}

type DiagProgress struct {
	Stage     string  `json:"stage"`
	Name      string  `json:"name,omitempty"`
	Message   string  `json:"message"`
	Done      int64   `json:"done"`
	Total     int64   `json:"total"`
	Speed     float64 `json:"speed"`
	Remaining float64 `json:"remaining"`
}

type DiagFetchComponent struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Installed  bool   `json:"installed"`
	Downloaded bool   `json:"downloaded"`
	Skipped    bool   `json:"skipped"`
	SetAside   string `json:"set_aside"`
}

type DiagFetchResult struct {
	Root          string               `json:"root"`
	License       string               `json:"license"`
	LicenseSource string               `json:"license_source"`
	Components    []DiagFetchComponent `json:"components"`
	Downloads     []string             `json:"downloads"`
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
	ZipPath     string              `json:"zip_path"`
	ZipBytes    int64               `json:"zip_bytes"`
	ZipSize     string              `json:"zip_size"`
	Entries     []string            `json:"entries"`
	Stage       string              `json:"stage"`
	StageKept   bool                `json:"stage_kept"`
	StageError  string              `json:"stage_error"`
	Problems    []string            `json:"problems"`
	MasFound    bool                `json:"mas_found"`
	Summary     string              `json:"summary"`
	PrivacyNote string              `json:"privacy_note"`
	Errors      []string            `json:"errors,omitempty"`
	Sections    []DiagSectionResult `json:"sections,omitempty"`
}

type diagBackend interface {
	Components(context.Context) (*DiagComponentsResult, error)
	License(context.Context, components.SourceSettings) (*DiagLicenseResult, error)
	Fetch(context.Context, bool, components.SourceSettings, string, func(DiagProgress)) (*DiagFetchResult, error)
	Collect(context.Context, DiagCollectOptions, func(DiagProgress)) (*DiagCollectResult, error)
	ReportConfig(context.Context) (string, error)
}
