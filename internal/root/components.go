package root

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"modavd/internal/playstore"
)

// ---- Google components -----------------------------------------------------------------------------------------
//
// adb (platform-tools) and the system image belong to Google and are left out of the release package; `modavd
// components fetch` (package components, fetch-components.ps1 before it) downloads them. SDKComponents is their one
// table: names, versions, target directories, the files that show they are there, and where and how fetch gets them.
// check and components status (Components) and fetch all read it; nothing else names those files or directories.
// The values are fetch-components.ps1's $Components, the same as AUTO-MAS constants.py REQUIRED_COMPONENTS: change
// them together with both and test a real download again.
//
// Two rules decide whether a component is there, on purpose:
//
//   - Present (check, components status, Missing, EnsureMetadata): AUTO-MAS components.py install_status, every
//     StatusFiles entry is a file. AUTO-MAS manages the same roots, so the two have to agree on what is missing.
//   - Complete (components fetch: what it skips, and what it requires before and after moving a component into
//     place): fetch-components.ps1 Test-ComponentFiles, every KeyFiles entry is a non-empty file; for a component
//     with KeyHashes (adb) also every pinned SHA-256 and exactly one Pkg.Revision line in source.properties, equal to
//     Version. A system image (no pins) of another revision is complete; fetch only notes the difference.
//
// StatusFiles is part of KeyFiles, so what fetch leaves complete is present for check. The reverse does not hold: an
// adb.exe without its DLLs, or another adb build, is present but not complete, and fetch sets that directory aside
// (<dir>.incomplete-<time>, never deleted) and installs the tested one. Check cannot take the stricter rule without
// reporting roots as broken that AUTO-MAS reports as ready.

// SDKComponent is one Google component under <sdk>.
type SDKComponent struct {
	ID          string   // platform-tools / system-image (the ComponentStatus ids)
	Name        string   // in check and components status (AUTO-MAS constants.py)
	FetchName   string   // in components fetch (fetch-components.ps1)
	Version     string   // the revision this release is tested with
	Target      string   // directory under <sdk>
	StatusFiles []string // Present: AUTO-MAS _KEY_FILES

	// What components fetch needs; URL is "" on hosts that have no catalogue yet, where Complete means
	// nothing.
	URL                string            // relative to a mirror's base URL
	ZipName            string            // its name in downloads\, "" for the URL's last part
	Size               int64             // of the zip
	SHA1               string            // of the zip, lower hex
	SHA256             string            // optional additional archive pin
	ArchiveRoot        string            // the zip's top directory, which becomes Target
	KeyFiles           []string          // Complete: each must be a non-empty file
	KeyHashes          map[string]string // Complete: file -> pinned SHA-256 (lower hex)
	ExtractedBytes     int64             // unpacked size, for the free-space check
	Properties         string            // source.properties written when the zip has none
	Executables        []string          // Complete: these files must retain an execute bit
	StrictRevision     bool              // require exactly Version even without per-file pins
	LicenseID          string            // empty uses the existing Android SDK license
	LicenseManifest    string            // relative repository XML containing LicenseID
	LicenseSHA256      string            // normalized license text plus LF
	RequiredProperties map[string]string // Complete: exact host/guest identity fields
}

func systemImageFiles() []string {
	return []string{"system.img", "vendor.img", "kernel-ranchu", "ramdisk.img"}
}

// WindowsComponents is the Windows table, platform-tools then system-image (a fresh copy each time).
func WindowsComponents() []SDKComponent {
	return []SDKComponent{
		{
			ID: "platform-tools", Name: PlatformToolsName, FetchName: PlatformToolsName, Version: "37.0.1",
			Target: "platform-tools", StatusFiles: []string{"adb.exe"},
			URL: "platform-tools_r37.0.1-win.zip", Size: 8044989, SHA1: "e03e78b1d80b396f1c3358e31251cb31740e1110",
			ArchiveRoot: "platform-tools", KeyFiles: []string{"adb.exe", "AdbWinApi.dll", "AdbWinUsbApi.dll"},
			// Pinned files from the SHA-1-verified Google 37.0.1 archive.
			KeyHashes: map[string]string{
				"adb.exe":          "b4a6b455702684652cccf7b46258b29e653538904359a58fd4931cf3ef286b3f",
				"AdbWinApi.dll":    "c1d653030b4bde65d3e07e4d0b0979e17be56df1436cdd15528630f27808050d",
				"AdbWinUsbApi.dll": "0710e894d9b40f71a670c13c694079d564c92c1279da382cfe4850983aaebe1b",
			},
			ExtractedBytes: 20 << 20,
			Properties:     "Pkg.UserSrc=false\nPkg.Revision=37.0.1\n",
		},
		{
			ID: "system-image", Name: SystemImageName, FetchName: "系统镜像（Android 14 Google APIs x86_64）", Version: "14",
			Target: SystemImageDir, StatusFiles: systemImageFiles(),
			URL: "sys-img/google_apis/x86_64-34_r14.zip", Size: 1563721130, SHA1: "e0f6c9a0691aa27bd597d0deb1bcfdc943ac8ca7",
			ArchiveRoot: "x86_64", KeyFiles: systemImageFiles(), ExtractedBytes: 4500 << 20,
			Properties: "Pkg.Desc=System Image x86_64 with Google APIs.\nPkg.Revision=14\nPkg.Dependencies=emulator#34.2.16\n" +
				"AndroidVersion.ApiLevel=34\nAndroidVersion.ExtensionLevel=7\nAndroidVersion.IsBaseSdk=true\n" +
				"SystemImage.Abi=x86_64\nSystemImage.TagId=google_apis\nSystemImage.TagDisplay=Google APIs\n" +
				"SystemImage.GpuSupport=true\nAddon.VendorId=google\nAddon.VendorDisplay=Google Inc.\n",
		},
	}
}

// PlayStoreDir is where the optional Google Play Store component goes, under <sdk>.
const PlayStoreDir = "playstore"

// PlayStoreName is the Google Play Store component's name.
const PlayStoreName = "Google Play 商店"

// PlayStoreComponent is the optional Google Play Store (package playstore): the store and the Google-signed Play
// services, GSF and Partner Setup taken out of Google's Play image of the same release, kept in <sdk>\playstore. It is
// not in SDKComponents, so check, components status and AUTO-MAS never count it as missing; only `components fetch
// --play-store` fetches it, and only instances created or set up with the store use it. Complete pins every file's
// SHA-256. The zip unpacks into a host-specific system.img that fetch reads the files out of and deletes (ExtractedBytes
// counts both); on hosts without a catalogue the URL is "".
func PlayStoreComponent() SDKComponent {
	p := playstore.CurrentProfile()
	files := make([]string, len(p.Files))
	hashes := map[string]string{}
	for k, f := range p.Files {
		files[k] = f.Name
		hashes[f.Name] = f.SHA256
	}
	c := SDKComponent{ID: "play-store", Name: PlayStoreName,
		FetchName: "Google Play 商店（取自 Android 14 Google Play x86_64 镜像 r14）", Version: p.Revision,
		Target: PlayStoreDir, StatusFiles: files, KeyFiles: files, KeyHashes: hashes}
	darwin := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
	if runtime.GOOS == "windows" || darwin {
		c.URL, c.Size, c.SHA1, c.ArchiveRoot = p.ImageURL, p.ImageSize, p.ImageSHA1, p.ArchiveRoot
		c.SHA256, c.ZipName = p.ImageSHA256, p.ZipName
		c.ExtractedBytes = p.ImageMemberSize + p.ExtractedBytes()
		c.Properties = "Pkg.Desc=Google Play Store, Google Play services, GSF and Partner Setup from " +
			"system-images;android-" + p.API + ";google_apis_playstore;" + p.ABI + ".\nPkg.Revision=" + p.Revision + "\n"
	}
	if darwin {
		c.FetchName = "Google Play 商店（取自 Android 15 Google Play arm64-v8a 镜像 r9）"
		c.LicenseID, c.LicenseManifest = "android-sdk-arm-dbt-license", "sys-img/google_apis_playstore/sys-img2-3.xml"
		c.LicenseSHA256 = "0682da42e0ca119ce29503c265587f8ae17277094f9262b54c1d2bb99557c6f5"
		c.RequiredProperties = map[string]string{"SystemImage.Abi": p.ABI, "AndroidVersion.ApiLevel": p.API,
			"SystemImage.TagId": "google_apis_playstore"}
		c.Properties += "SystemImage.Abi=" + p.ABI + "\nAndroidVersion.ApiLevel=" + p.API + "\nSystemImage.TagId=google_apis_playstore\n"
	}
	return c
}

// SDKComponents is this host's table: a complete catalogue for Windows and Apple Silicon macOS, or only the
// component status paths on hosts that do not support downloading yet.
func SDKComponents() []SDKComponent {
	if runtime.GOOS == "windows" {
		return WindowsComponents()
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return DarwinComponents()
	}
	p := CurrentPlatform()
	return []SDKComponent{
		{ID: "platform-tools", Name: PlatformToolsName, FetchName: PlatformToolsName, Target: "platform-tools",
			StatusFiles: []string{p.ADB}},
		{ID: "system-image", Name: p.SystemImageName, FetchName: p.SystemImageName, Target: p.SystemImageDir,
			StatusFiles: systemImageFiles()},
	}
}

// DarwinComponents pins the official stable archives used by the Apple Silicon
// package. SHA-1 and license identities come from Google's repository XML;
// SHA-256 pins also bind the bytes of the independently verified archives.
func DarwinComponents() []SDKComponent {
	p := CurrentPlatform()
	return []SDKComponent{
		{
			ID: "platform-tools", Name: PlatformToolsName, FetchName: PlatformToolsName, Version: "37.0.1",
			Target: "platform-tools", StatusFiles: []string{p.ADB},
			URL: "platform-tools_r37.0.1-darwin.zip", Size: 16110554,
			SHA1:        "6ae73f4de6452dc57e62ec02b68eed92a4c21661",
			SHA256:      "ee39ad5967e95c2a07f04dbcbde96b1a0c916ba376096db5d2f498b7727a5d1d",
			ArchiveRoot: "platform-tools", KeyFiles: []string{p.ADB, "lib64/libc++.dylib"},
			KeyHashes: map[string]string{
				p.ADB:                "1811e253b21b12cbfda7201ebaf86c10e7ddcb5c606a7a81f7c82b4c429c2d3b",
				"lib64/libc++.dylib": "861dfb2d769b81267c118cb061aab1b2637686363a2ecf162411774fa6a37faf",
			},
			Executables: []string{p.ADB}, ExtractedBytes: 60000000, StrictRevision: true,
			Properties: "Pkg.UserSrc=false\nPkg.Revision=37.0.1\n",
			LicenseID:  "android-sdk-license", LicenseManifest: "repository2-3.xml",
			LicenseSHA256: "1f8729233617b193fd619213792ae16a41b95d2bbbf525dfe66998252ba68b16",
		},
		{
			ID: "system-image", Name: p.SystemImageName,
			FetchName: "系统镜像（Android 15 Google APIs arm64-v8a）", Version: "9",
			Target: p.SystemImageDir, StatusFiles: systemImageFiles(),
			URL: "sys-img/google_apis/arm64-v8a-35_r09.zip", Size: 1778933980,
			SHA1:        "16f5bceca236b2737008977c4aaf826e46a8de7d",
			SHA256:      "d2e7c6076e5df54d3677605a324daf9edf813b2808c216bdb1687f6b2b6aed99",
			ArchiveRoot: "arm64-v8a", KeyFiles: systemImageFiles(), ExtractedBytes: 6000000000,
			StrictRevision:     true,
			RequiredProperties: map[string]string{"SystemImage.Abi": p.GuestABI, "AndroidVersion.ApiLevel": "35", "SystemImage.TagId": "google_apis"},
			Properties: "Pkg.Desc=System Image arm64-v8a with Google APIs.\nPkg.Revision=9\nPkg.Dependencies=emulator#33.1.24\n" +
				"AndroidVersion.ApiLevel=35\nAndroidVersion.ExtensionLevel=13\nAndroidVersion.IsBaseSdk=true\n" +
				"SystemImage.Abi=arm64-v8a\nSystemImage.TagId=google_apis\nSystemImage.TagDisplay=Google APIs\n" +
				"SystemImage.GpuSupport=true\nAddon.VendorId=google\nAddon.VendorDisplay=Google Inc.\n",
			LicenseID: "android-sdk-arm-dbt-license", LicenseManifest: "sys-img/google_apis/sys-img2-3.xml",
			LicenseSHA256: "0682da42e0ca119ce29503c265587f8ae17277094f9262b54c1d2bb99557c6f5",
		},
	}
}

// Dir is the component's directory in the SDK sdk.
func (c SDKComponent) Dir(sdk string) string { return filepath.Join(sdk, c.Target) }

// Present is the rule of check and components status (AUTO-MAS install_status): every StatusFiles entry is a file in
// dir, of any size.
func (c SDKComponent) Present(dir string) bool { return allFiles(dir, c.StatusFiles) }

// Complete is the rule of components fetch (fetch-components.ps1 Test-ComponentFiles), see the comment above
// SDKComponent. Anything unreadable counts as not complete.
func (c SDKComponent) Complete(dir string) bool {
	for _, name := range c.KeyFiles {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() <= 0 {
			return false
		}
	}
	for _, name := range c.Executables {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return false
		}
	}
	if len(c.KeyHashes) == 0 && !c.StrictRevision && len(c.RequiredProperties) == 0 {
		return true
	}
	for name, want := range c.KeyHashes {
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil || !strings.EqualFold(got, want) {
			return false
		}
	}
	return c.MetadataMatches(dir)
}

// MetadataMatches checks the pinned revision and guest identity without hashing
// large files. Status polling uses it; publication still requires Complete.
func (c SDKComponent) MetadataMatches(dir string) bool {
	data, err := ReadFileShared(filepath.Join(dir, "source.properties"))
	if err != nil {
		return false
	}
	for key, want := range c.RequiredProperties {
		values := propertyValues(data, key)
		if len(values) != 1 || values[0] != want {
			return false
		}
	}
	revisions := revisionValues(data)
	// PowerShell's -ne compares strings case-insensitively.
	return len(revisions) == 1 && strings.EqualFold(revisions[0], c.Version)
}

// Revision is the first non-empty Pkg.Revision of dir\source.properties, "" when there is none (fetch-components.ps1
// Get-Revision, for fetch's "已有" line). Check and status read the version the AUTO-MAS way (readProperties).
func Revision(dir string) string {
	data, err := ReadFileShared(filepath.Join(dir, "source.properties"))
	if err != nil {
		return ""
	}
	for _, v := range revisionValues(data) {
		if v != "" {
			return v
		}
	}
	return ""
}

// revisionValues is the trimmed value of every line that matches the script's '^\s*Pkg\.Revision\s*=' (PowerShell's
// -match ignores case), lines split like .NET ReadAllLines (CRLF, LF or CR; a UTF-8 BOM dropped).
func revisionValues(data []byte) []string {
	return propertyValues(data, "Pkg.Revision")
}

func propertyValues(data []byte, key string) []string {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimLeftFunc(line, unicode.IsSpace)
		if len(s) < len(key) || !strings.EqualFold(s[:len(key)], key) {
			continue
		}
		s = strings.TrimLeftFunc(s[len(key):], unicode.IsSpace)
		if !strings.HasPrefix(s, "=") {
			continue
		}
		out = append(out, strings.TrimSpace(s[1:]))
	}
	return out
}

// fileSHA256 is the file's SHA-256 in lower hex, read with delete sharing (OpenShared).
func fileSHA256(path string) (string, error) {
	f, err := OpenShared(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
