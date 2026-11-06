// Package playstore is the Google Play Store a ModAVD instance can have (ModAVD/ModAVD#125). The instances run
// Google's "Google APIs" image, which has Google Play services but no store; the store comes from Google's "Google
// Play" image of the same release (system-images;android-34;google_apis_playstore;x86_64 r14), downloaded on the
// user's computer like the other Google components and never shipped by ModAVD.
//
// The store alone does not work there: the Google APIs image signs Play services and the Google Services Framework
// with the Android test certificate, the store is signed by Google, and Google's components check each other's
// signature. So the files taken are the store and the Google-signed copies of Play services, GSF and Partner Setup
// (same versions as the image's own; Partner Setup writes GSF settings and crashes at every boot otherwise), plus the
// store's fs-verity certificate. They go into the instance's writable system overlay, which only instances with the
// execution bridge have; the image's own com.android.vending, a license-check placeholder, is hidden there.
//
// Files is the table of those files; Extract takes them out of the Play image's system.img (GPT, then the dynamic
// partitions of its super partition, then ext4) without mounting anything.
package playstore

import "path"

// The Play image the files come from, as Google's repository lists it (sys-img/google_apis_playstore/sys-img2-3.xml).
const (
	ImageURL  = "sys-img/google_apis_playstore/x86_64-34_r14.zip" // relative to a mirror's base URL
	ImageSize = 1510752654
	ImageSHA1 = "01d32617fd1937e540faf8731d70cf50e35af854"
	// ImageMember is the system image inside the zip, ImageMemberSize its size.
	ImageMember     = "x86_64/system.img"
	ImageMemberSize = 3101687808
	// Revision is Pkg.Revision of the image; the instances' Google APIs image is r14 too.
	Revision = "14"
)

// File is one file of the store.
type File struct {
	Name      string // its name on the host (sdk\playstore\<Name>) and where it is pushed to in the guest
	Partition string // the dynamic partition it is on: product or system_ext
	Path      string // its path inside that partition
	Size      int64
	SHA256    string // lower hex
}

// Guest is where the file goes in the instance: /<partition>/<path>.
func (f File) Guest() string { return "/" + f.Partition + "/" + f.Path }

// Files are the store's files, all from the r14 Play image (sizes and SHA-256 as found there).
var Files = []File{
	{"Phonesky.apk", "product", "priv-app/Phonesky/Phonesky.apk", 74664153,
		"2616771999b1cde77fd000ae17a43784dded1f76b21ec12881137c84d5313dd2"},
	{"PrebuiltGmsCore.apk", "product", "priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk", 144060779,
		"63aaf087cdfad32fd9eca45f36966afe2f44db7e125e099f3e344ca99cee13c2"},
	{"GoogleServicesFramework.apk", "system_ext", "priv-app/GoogleServicesFramework/GoogleServicesFramework.apk", 5744939,
		"58999d24e4dbdde61f90b59a89c86edabc69e0908ddaaa2742091493d7e3b975"},
	{"PartnerSetupPrebuilt.apk", "product", "priv-app/PartnerSetupPrebuilt/PartnerSetupPrebuilt.apk", 726118,
		"20190a60924f36baae9ef9b7687691c445dd9a764b42c4751474293baf5ea15c"},
	{"play_store_fsi_cert.der", "product", "etc/security/fsverity/play_store_fsi_cert.der", 1422,
		"37aabd485044e3af6d84755268faf8392e226ca0d188be6a80335525ce0ec6ff"},
}

// Stub is the Google APIs image's own com.android.vending, a license-check placeholder the store replaces.
const Stub = "/product/app/LicenseChecker"

// Package is the store's package name.
const Package = "com.android.vending"

// ExtractedBytes is what the files take on disk.
func ExtractedBytes() int64 {
	var n int64
	for _, f := range Files {
		n += f.Size
	}
	return n
}

// Manifest is the file list the guest script reads: "<name> <guest path> <sha256>" per line, LF.
func Manifest() string {
	s := ""
	for _, f := range Files {
		s += f.Name + " " + path.Clean(f.Guest()) + " " + f.SHA256 + "\n"
	}
	return s
}
