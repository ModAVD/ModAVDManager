package guest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (g *psGuest) nativeOverlay() {
	g.write(g.root+"/proc/mounts", "overlay /product overlay rw,lowerdir=/product,upperdir=/mnt/scratch/overlay/product/upper,workdir=/mnt/scratch/overlay/product/work 0 0\n"+
		"overlay /system_ext overlay rw,lowerdir=/system_ext,upperdir=/mnt/scratch/overlay/system_ext/upper,workdir=/mnt/scratch/overlay/system_ext/work 0 0\n")
}

func TestNativePlayStoreScriptRequiresKnownOverlay(t *testing.T) {
	g := newPSGuest(t)
	g.files(false)
	for _, action := range []string{"install", "uninstall"} {
		if r, out := g.run(action, "native"); r != "not-writable" || !strings.Contains(out, "supported overlay") {
			t.Fatalf("%s without overlay: %s\n%s", action, r, out)
		}
		if g.exists(g.root+"/pm.log") || g.exists(g.root+psStore) {
			t.Fatal("unknown overlay changed guest packages")
		}
	}
}

func TestNativePlayStoreScriptPreservesForeignOverlay(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "upper", true: "staged"}[staged], func(t *testing.T) {
			g := newPSGuest(t)
			g.nativeOverlay()
			g.files(false)
			foreign := g.root + "/mnt/scratch/overlay/product/upper/priv-app/Phonesky/Phonesky.apk"
			if staged {
				foreign = g.root + psStore + ".new"
			}
			g.write(foreign, "another tool's copy")
			for _, action := range []string{"install", "uninstall"} {
				if r, out := g.run(action, "native"); r != "failed" || !strings.Contains(out, "foreign") {
					t.Fatalf("%s: %s\n%s", action, r, out)
				}
				if got, _ := g.read(foreign); got != "another tool's copy" {
					t.Fatal("foreign file was changed")
				}
				if g.exists(g.root + "/pm.log") {
					t.Fatal("foreign overlay refusal changed packages")
				}
			}
		})
	}
}

func TestNativePlayStoreScriptInterruptedStagingCanBeRemoved(t *testing.T) {
	g := newPSGuest(t)
	g.nativeOverlay()
	g.files(false)
	g.write(g.root+psGms+".new", "interrupted copy")
	g.write(g.root+"/data/local/modavd-playstore/owned.txt", psGms+"\n")
	if r, out := g.run("uninstall", "native"); r != "gone" || !strings.Contains(out, "removed staged ") {
		t.Fatalf("remove interrupted stage: %s\n%s", r, out)
	}
	if g.exists(g.root + psGms + ".new") {
		t.Fatal("owned stage left behind")
	}
	if r, out := g.run("status", "native"); r != "absent" {
		t.Fatalf("status: %s\n%s", r, out)
	}
}

func TestNativePlayStoreScriptInstallRemove(t *testing.T) {
	g := newPSGuest(t)
	g.nativeOverlay()
	g.files(false)
	if r, out := g.run("install", "native"); r != "installed" {
		t.Fatalf("install: %s\n%s", r, out)
	}
	owned := g.root + "/data/local/modavd-playstore/owned.txt"
	if b, _ := g.read(owned); !strings.Contains(b, psStore) || !strings.Contains(b, "stub\n") {
		t.Fatalf("missing durable ownership: %q", b)
	}
	up := g.root + "/mnt/scratch/overlay"
	g.write(up+"/product/upper/priv-app/Phonesky/Phonesky.apk", "store")
	g.write(up+"/product/upper/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk", "play services")
	g.write(up+"/system_ext/upper/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk", "gsf")
	g.write(up+"/product/upper/app/LicenseChecker", "") // fake whiteout
	if r, out := g.run("uninstall", "native"); r != "reboot" {
		t.Fatalf("uninstall: %s\n%s", r, out)
	}
	// The real overlay's merged view returns to the stock lower files after boot.
	for _, target := range []string{psStore, psGms, psGSF} {
		if err := os.Remove(filepath.FromSlash(g.root + target)); err != nil {
			t.Fatal(err)
		}
	}
	g.write(g.root+psStub, "placeholder")
	if r, out := g.run("uninstall", "native"); r != "gone" {
		t.Fatalf("second uninstall: %s\n%s", r, out)
	}
	if g.exists(owned) {
		t.Fatal("confirmed removal left ownership journal")
	}
	if r, out := g.run("status", "native"); r != "absent" {
		t.Fatalf("status: %s\n%s", r, out)
	}
}
