#!/system/bin/sh
# ModAVD: the Google Play Store of an instance (ModAVD/ModAVD#125), run as root by modavd (internal/emu/playstore.go):
#   sh playstore.sh install|status|uninstall <dir>
# <dir> holds files.txt ("<name> <guest path> <sha256>" per line) and, for install, the files it names. The files
# go into the instance's writable system overlay (adb remount's, which only instances with the execution bridge boot
# with); the image's own com.android.vending, a license-check placeholder, is hidden there. Each run ends with one
# line "RESULT <word>" that modavd reads:
#   install:   installed | not-writable | bad-file | failed
#   status:    installed | partial | absent
#   uninstall: reboot (cold boot, then run uninstall again) | gone | not-writable | failed
# Exit code 0 unless the result is a failure. MODAVD_PS_ROOT prefixes every absolute path (tests only; unset in the
# guest).
R=${MODAVD_PS_ROOT:-}
D=$2
LIST=$D/files.txt
STUB=/product/app/LicenseChecker
U=$R/mnt/scratch/overlay
PKG=com.android.vending

result() { echo "RESULT $1"; exit "${2:-0}"; }

[ -f "$LIST" ] || { echo "no $LIST"; result failed 1; }

sum() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }

writable() {
  remount >/dev/null 2>&1
  for p in product system_ext; do
    if ! touch "$R/$p/.modavd-w" 2>/dev/null; then
      echo "/$p is not writable"
      result not-writable 3
    fi
    rm -f "$R/$p/.modavd-w"
  done
}

# state prints a line per file (play: the store's copy is there; image: another file; absent) and the placeholder's,
# and sets STATE.
state() {
  while read -r name target want; do
    [ -n "$name" ] || continue
    got=$(sum "$R$target")
    if [ "$got" = "$want" ]; then echo "play $target"
    elif [ -n "$got" ]; then echo "image $target"
    else echo "absent $target"; fi
  done < "$LIST" > "$D/state.txt"
  cat "$D/state.txt"
  n=$(grep -c . "$D/state.txt"); ok=$(grep -c '^play ' "$D/state.txt")
  if [ -e "$R$STUB" ]; then echo "stub present"; stub=1; else echo "stub hidden"; stub=0; fi
  p=$(pm path $PKG 2>/dev/null | head -1); echo "package ${p#package:}"
  if [ "$ok" = "$n" ] && [ "$stub" = 0 ]; then STATE=installed
  elif [ "$ok" = 0 ] && [ "$stub" = 1 ]; then STATE=absent
  else STATE=partial; fi
}

case "$1" in
install)
  writable
  while read -r name target want; do
    [ -n "$name" ] || continue
    got=$(sum "$D/$name")
    if [ "$got" != "$want" ]; then
      echo "$name: sha256 ${got:-missing}, want $want"
      result bad-file 4
    fi
  done < "$LIST"
  while read -r name target want; do
    [ -n "$name" ] || continue
    t=$R$target; dir=${t%/*}
    if [ ! -d "$dir" ]; then
      mkdir -p "$dir" && chmod 755 "$dir" && chcon u:object_r:system_file:s0 "$dir" || result failed 1
    fi
    cp "$D/$name" "$t.new" && chown root:root "$t.new" && chmod 644 "$t.new" &&
      chcon u:object_r:system_file:s0 "$t.new" && mv "$t.new" "$t" || result failed 1
    echo "copied $target"
  done < "$LIST"
  rm -rf "$R$STUB" && echo "hid $STUB"
  pm enable $PKG >/dev/null 2>&1
  sync
  state
  [ "$STATE" = installed ] || result failed 1
  result installed
  ;;
status)
  state
  result "$STATE"
  ;;
uninstall)
  # disabled first, so the store cannot update itself again before the shutdown; the run after the cold boot
  # enables com.android.vending again (the placeholder by then)
  am force-stop $PKG >/dev/null 2>&1
  pm disable $PKG >/dev/null 2>&1
  for p in $PKG com.google.android.gms com.google.android.gsf com.google.android.partnersetup; do
    case "$(pm uninstall-system-updates $p 2>&1)" in *Success*) echo "removed updates of $p" ;; esac
  done
  writable
  [ -d "$U" ] || { echo "no $U"; result failed 1; }
  left=0
  while read -r name target want; do
    [ -n "$name" ] || continue
    rest=${target#/}; part=${rest%%/*}; rest=${rest#*/}
    if [ -e "$U/$part/upper/$rest" ]; then rm -f "$U/$part/upper/$rest" && echo "removed $target"; left=1; fi
  done < "$LIST"
  rmdir "$U/product/upper/priv-app/Phonesky" 2>/dev/null
  if [ -e "$U/product/upper/app/LicenseChecker" ]; then
    rm -f "$U/product/upper/app/LicenseChecker" && echo "restored $STUB"; left=1
  fi
  sync
  [ $left = 1 ] && result reboot
  case "$(pm path $PKG 2>/dev/null | head -1)" in
    "package:$STUB/"*) pm enable $PKG >/dev/null 2>&1; result gone ;;
  esac
  result reboot
  ;;
*)
  echo "usage: sh $0 install|status|uninstall <dir>"
  result failed 2
  ;;
esac
