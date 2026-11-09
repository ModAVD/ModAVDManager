#!/system/bin/sh
# ModAVD: the Google Play Store of an instance (ModAVD/ModAVD#125), run as root by modavd (internal/emu/playstore.go):
#   sh playstore.sh install|status|uninstall|gsfid <dir>
# <dir> holds files.txt ("<name> <guest path> <sha256>" per line) and, for install, the files it names. The files
# go into the instance's writable system overlay (adb remount's, which only instances with the execution bridge boot
# with); the image's own com.android.vending, a license-check placeholder, is hidden there. Each run ends with one
# line "RESULT <word>" that modavd reads:
#   install:   installed | not-writable | bad-file | failed (what this run put in is taken back) | partial (not all
#              of it could be taken back; its last line before RESULT says why the install failed)
#   status:    installed | partial | absent
#   uninstall: reboot (cold boot, then run uninstall again) | gone | not-writable | failed
#   gsfid:     ok (lines "decimal <id>" and "hex <id>": GSF's android_id, the documentation site's step 6) | no-id
#              (GSF has not got one yet: it registers once online) | failed
# Exit code 0 unless the result is a failure. MODAVD_PS_ROOT prefixes every absolute path (tests only; unset in the
# guest).
R=${MODAVD_PS_ROOT:-}
D=$2
LIST=$D/files.txt
STUB=/product/app/LicenseChecker
U=$R/mnt/scratch/overlay
PKG=com.android.vending
GSFDB=/data/data/com.google.android.gsf/databases/gservices.db

result() { echo "RESULT $1"; exit "${2:-0}"; }

[ -f "$LIST" ] || { echo "no $LIST"; result failed 1; }

sum() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }

# upper is where the overlay keeps its own copy of guest path $1 (or the whiteout that hides the image's)
upper() { rest=${1#/}; echo "$U/${rest%%/*}/upper/${rest#*/}"; }

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

# state prints a line per file (play: the store's copy is there; image: another file; absent), a line per copy an
# install left half way (staged) and the placeholder's, and sets STATE.
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
  new=0
  while read -r name target want; do
    [ -n "$name" ] || continue
    if [ -e "$R$target.new" ]; then echo "staged $target.new"; new=1; fi
  done < "$LIST"
  if [ -e "$R$STUB" ]; then echo "stub present"; stub=1; else echo "stub hidden"; stub=0; fi
  p=$(pm path $PKG 2>/dev/null | head -1); echo "package ${p#package:}"
  if [ "$new" = 0 ] && [ "$ok" = "$n" ] && [ "$stub" = 0 ]; then STATE=installed
  elif [ "$new" = 0 ] && [ "$ok" = 0 ] && [ "$stub" = 1 ]; then STATE=absent
  else STATE=partial; fi
}

# mkdirs makes directory $1 and the missing ones above it (755, system_file), each noted in dirs.txt for undo.
mkdirs() {
  [ -d "$1" ] && return 0
  [ -n "$1" ] || return 1
  mkdirs "${1%/*}" || return 1
  mkdir "$1" && chmod 755 "$1" && chcon u:object_r:system_file:s0 "$1" || return 1
  echo "$1" >> "$D/dirs.txt"
}

# undo takes back what this install put in and ends the run: RESULT failed when none of it is left, partial when some
# is. The staged copies, the files the image did not have and the directories made for them go through the merged
# view at once; a file that replaced one of the image's and the placeholder's whiteout are removed from the overlay's
# upper layer, which the guest sees from its next boot (the line "undone from the next boot" says so). Its last line
# says why the install failed.
undo() {
  echo "undoing the install: $1"
  left=0
  boot=0
  while read -r name target want; do
    [ -n "$name" ] || continue
    rm -f "$R$target.new" 2>/dev/null
    if [ -e "$R$target.new" ]; then echo "left $target.new"; left=1; fi
  done < "$LIST"
  while read -r pre target; do
    [ -n "$target" ] || continue
    if [ "$pre" = absent ]; then f=$R$target; else f=$(upper "$target"); boot=1; fi
    rm -f "$f" 2>/dev/null
    if [ -e "$f" ]; then echo "left $target"; left=1; else echo "took back $target"; fi
  done < "$D/moved.txt"
  rev=
  while read -r d; do rev="$d $rev"; done < "$D/dirs.txt"
  for d in $rev; do rmdir "$d" 2>/dev/null; done
  if [ "$hid" = 1 ]; then
    w=$(upper "$STUB")
    rm -rf "$w" 2>/dev/null
    boot=1
    if [ -e "$w" ]; then echo "left $STUB hidden"; left=1; else echo "restored $STUB"; fi
  fi
  sync
  [ "$boot" = 1 ] && echo "undone from the next boot"
  echo "install failed: $1"
  [ "$left" = 0 ] && result failed 1
  result partial 1
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
  # Staged first: each file that is not the store's copy already goes in as <path>.new, and only once all of them are
  # there do they take their places, so running out of space (the overlay's) leaves nothing but .new files to remove.
  : > "$D/staged.txt"; : > "$D/moved.txt"; : > "$D/dirs.txt"; hid=0
  while read -r name target want; do
    [ -n "$name" ] || continue
    t=$R$target
    rm -f "$t.new" 2>/dev/null
    if [ "$(sum "$t")" = "$want" ]; then echo "kept $target"; continue; fi
    mkdirs "${t%/*}" || undo "cannot make ${target%/*}"
    cp "$D/$name" "$t.new" && chown root:root "$t.new" && chmod 644 "$t.new" &&
      chcon u:object_r:system_file:s0 "$t.new" || undo "cannot copy $target"
    echo "$target" >> "$D/staged.txt"
  done < "$LIST"
  while read -r target; do
    [ -n "$target" ] || continue
    t=$R$target
    if [ -e "$t" ]; then pre=image; else pre=absent; fi
    mv "$t.new" "$t" || undo "cannot move $target into place"
    echo "$pre $target" >> "$D/moved.txt"
    echo "copied $target"
  done < "$D/staged.txt"
  if [ -e "$R$STUB" ]; then
    hid=1
    rm -rf "$R$STUB" || undo "cannot hide $STUB"
    echo "hid $STUB"
  fi
  pm enable $PKG >/dev/null 2>&1
  sync
  state
  [ "$STATE" = installed ] || undo "the store is $STATE after copying"
  result installed
  ;;
status)
  state
  result "$STATE"
  ;;
gsfid)
  # the documentation site's step 6 (https://modavd.github.io/guide/playstore)
  id=
  [ -f "$R$GSFDB" ] && id=$(sqlite3 "$R$GSFDB" "select value from main where name='android_id';" 2>/dev/null | head -1)
  case "$id" in
    "" | 0 | - | *[!0-9-]* | ?*-*)
      echo "no GSF ID yet${id:+ ($id)}: wait a minute or two with the network on, then try again"
      result no-id 5
      ;;
  esac
  echo "decimal $id"
  printf 'hex %x\n' "$id"
  result ok
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
  echo "usage: sh $0 install|status|uninstall|gsfid <dir>"
  result failed 2
  ;;
esac
