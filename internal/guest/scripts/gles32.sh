#!/system/bin/sh
# ModAVD: report GLES 3.2 (experimental, ModAVD/ModAVD#131), run as root by modavd (internal/emu/gles32.go):
#   sh gles32.sh on|off|status
# Some apps list GLES 3.2 in their manifest and Play hides them from a device whose ro.opengles.version says less. This
# only changes the version the guest reports (196610 = 3.2, the emulator renders 3.1); nothing renders differently.
# init.ranchu.rc sets ro.opengles.version in early-init, after build.prop was read, so a line in /vendor/build.prop
# is taken first. /vendor is writable (adb remount's overlay) only on instances with the execution bridge. The line
# goes in after a marker comment, which is all that tells it from anything else in the file. A change shows after the
# next cold boot. Each run ends with one line "RESULT <word>" that modavd reads:
#   on:     reboot (written; cold boot, then status) | already | not-writable
#   off:    reboot (taken out) | already | not-writable
#   status: lines "prop <ro.opengles.version>" and "line yes|no", then
#           on | off | pending-on (the line is there, the property is not 196610 yet) | pending-off (the line is gone,
#           the property still is 196610)
# Exit code 0 unless the result is not-writable (3) or the usage is wrong (2). MODAVD_GLES_ROOT prefixes every
# absolute path (tests only; unset in the guest).
R=${MODAVD_GLES_ROOT:-}
F=$R/vendor/build.prop
MARK='# modavd: report GLES 3.2 (ModAVD/ModAVD#131)'
LINE='ro.opengles.version=196610'
WANT=196610

result() { echo "RESULT $1"; exit "${2:-0}"; }

writable() {
  remount >/dev/null 2>&1
  if ! touch "$R/vendor/.modavd-w" 2>/dev/null; then
    echo "/vendor is not writable"
    result not-writable 3
  fi
  rm -f "$R/vendor/.modavd-w"
}

# marked: our line is in the file
marked() { [ -f "$F" ] && grep -qxF "$MARK" "$F"; }

case "$1" in
on)
  writable
  marked && result already
  # appended to the file itself (its inode, mode and SELinux label stay); a file without a final newline gets one first
  if [ -s "$F" ] && [ -n "$(tail -c 1 "$F")" ]; then echo >> "$F" || result not-writable 3; fi
  { echo "$MARK"; echo "$LINE"; } >> "$F" || result not-writable 3
  sync
  echo "wrote $LINE"
  result reboot
  ;;
off)
  writable
  marked || result already
  tmp=$F.modavd-tmp
  # the marker line and the one after it go; everything else is copied as it is
  skip=0
  while IFS= read -r l || [ -n "$l" ]; do
    if [ "$skip" = 1 ]; then skip=0; continue; fi
    if [ "$l" = "$MARK" ]; then skip=1; continue; fi
    printf '%s\n' "$l"
  done < "$F" > "$tmp" || { rm -f "$tmp"; echo "cannot read $F"; result not-writable 3; }
  # over the file itself, not moved: its inode, mode and SELinux label stay
  cat "$tmp" > "$F" || { rm -f "$tmp"; echo "cannot write $F"; result not-writable 3; }
  rm -f "$tmp"
  sync
  echo "took out $LINE"
  result reboot
  ;;
status)
  prop=$(getprop ro.opengles.version 2>/dev/null)
  echo "prop $prop"
  if marked; then echo "line yes"; line=1; else echo "line no"; line=0; fi
  if [ "$prop" = "$WANT" ]; then isw=1; else isw=0; fi
  if [ $line = 1 ] && [ $isw = 1 ]; then result on
  elif [ $line = 0 ] && [ $isw = 0 ]; then result off
  elif [ $line = 1 ]; then result pending-on
  else result pending-off; fi
  ;;
*)
  echo "usage: sh $0 on|off|status"
  result failed 2
  ;;
esac
