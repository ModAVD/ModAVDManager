package playstore

import "runtime"

// Profile binds an optional Play component to one host's pinned guest image.
// Keeping the tables separate prevents accepting x86 files for an ARM64 SDK.
type Profile struct {
	API, ABI, Revision                string
	ImageURL, ImageSHA1, ImageSHA256  string
	ImageSize, ImageMemberSize        int64
	ImageMember, ArchiveRoot, ZipName string
	Files                             []File
}

func (p Profile) ExtractedBytes() int64 {
	var n int64
	for _, f := range p.Files {
		n += f.Size
	}
	return n
}

// WindowsProfile preserves the API34 x86_64 component and its existing pins.
func WindowsProfile() Profile {
	return Profile{API: "34", ABI: "x86_64", Revision: Revision,
		ImageURL: ImageURL, ImageSize: ImageSize, ImageSHA1: ImageSHA1,
		ImageMember: ImageMember, ImageMemberSize: ImageMemberSize,
		ArchiveRoot: "x86_64", ZipName: "google_apis_playstore-x86_64-34_r14.zip", Files: Files}
}

// CurrentProfile is the host's component table. Unsupported hosts retain the
// existing Windows status shape; the component entry points reject downloads.
func CurrentProfile() Profile {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return darwinProfile()
	}
	return WindowsProfile()
}

func darwinProfile() Profile {
	return Profile{API: "35", ABI: "arm64-v8a", Revision: "9",
		ImageURL:  "sys-img/google_apis_playstore/arm64-v8a-35_r09.zip",
		ImageSize: 1789211399, ImageSHA1: "b01416bc0bca12a85295dad179f9b8a1cae3d1e6",
		ImageSHA256: "97cbd9b347a92ce6cfeb9817504cca162d9a0f230ee38cd216e21920ae91fea8",
		ImageMember: "arm64-v8a/system.img", ImageMemberSize: 2203058176,
		ArchiveRoot: "arm64-v8a", ZipName: "google_apis_playstore-arm64-v8a-35_r09.zip",
		Files: darwinFiles}
}

// Filled from the size/SHA-1-verified official API35 ARM64 r9 Play archive.
var darwinFiles = []File{
	{"Phonesky.apk", "product", "priv-app/Phonesky/Phonesky.apk", 78672456,
		"27ec8f3dbb69caa2176b6faa4fc6f59ceb1a134ccdea0933b58908ac28c3ff71"},
	{"PrebuiltGmsCore.apk", "product", "priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk", 236728460,
		"5e093367651d0db1478e58a5cac463cbaf684edbe6ffc42743ce1390e545dee4"},
	{"GoogleServicesFramework.apk", "system_ext", "priv-app/GoogleServicesFramework/GoogleServicesFramework.apk", 5290526,
		"3d17213740c99cfa7961bb3d9509d6783914b2b7f634e366c4e73f5a0ad293eb"},
	{"PartnerSetupPrebuilt.apk", "product", "priv-app/PartnerSetupPrebuilt/PartnerSetupPrebuilt.apk", 1444170,
		"d9d0c6af51017eccff01a18c9593db14edba40f49f3bfd9764b79ad70a8496db"},
	{"play_store_fsi_cert.der", "product", "etc/security/fsverity/play_store_fsi_cert.der", 1422,
		"37aabd485044e3af6d84755268faf8392e226ca0d188be6a80335525ce0ec6ff"},
}
