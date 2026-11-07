#!/system/bin/sh
# libfix.sh - bind-mount patched copies of guest libraries over the image's own and restart zygote once, so that every
# app gets them (zygote preloads the Vulkan driver and loads the ARM translation layer before it forks any app). Run
# as root by modavd (internal/emu/guestfix.go, ModAVD/ModAVD#131):
#   sh libfix.sh mount  <target> <cache> <upload> <orig sha256> <fix sha256> [<target> <cache> <upload> <orig> <fix>]...
#   sh libfix.sh revert <target> <fix sha256> [<target> <fix sha256>]...
# The patched files are made on the host from the image's own libraries (internal/vkfix, internal/ndkfix) and never
# shipped. The image is never written: a mount lasts until the guest shuts down, revert takes it away sooner.
#
# Each patched copy stays in its <cache> (in /data/local/tmp) for the next boots. The cache is relabelled like
# <target>, which adb cannot write any more, so the host uploads to <upload> and the script takes that in.
#
# mount prints one line per library, "<target> <STATUS> [details]":
#   ALREADY <sha256>         something is mounted over it this boot already (its hash follows): left alone
#   ABSENT                   the image has no such file: nothing to do
#   IMAGE_PATCHED            the image's own file already is the patched one: nothing to do
#   SKIPPED image <sha256>   the image's file has an unexpected hash: left alone
#   SKIPPED patched <sha256> the cached copy was bad: removed, not mounted; the next run asks for an upload
#   NEED_UPLOAD              no valid cached copy: upload the patched file to <upload> and run again
#   READY                    could be mounted, but another library waits for its upload: not mounted this run
#   MOUNTED <sha256>         mounted now
#   FAILED <step> ...        chcon / mount did not take: nothing left mounted over it
# A first, read-only round checks them all (taking a valid upload into the cache is its only write); while any says
# NEED_UPLOAD the run mounts none. So whatever is mounted is mounted in the same run as the zygote restart: a mount
# without the restart would leave zygote on the old file while mountinfo already says mounted. The last line is
# RESTARTED (something was mounted, zygote restarted once) or UNCHANGED.
#
# revert unmounts the patched copy (<fix sha256>) from <target>: "<target> REVERTED <sha256 now>", NOT_MOUNTED,
# OTHER <sha256> (something else is mounted there: left alone) or FAILED umount; the last line as above. The unmount
# is lazy (processes that have the patched file mapped keep it until they end, and the zygote restart ends them).
#
# MODAVD_LIBFIX_ROOT prefixes every absolute path (tests only; unset in the guest).
R=${MODAVD_LIBFIX_ROOT:-}
MI=$R/proc/self/mountinfo

sum() { sha256sum "$R$1" 2>/dev/null | cut -d' ' -f1; }
label() { ls -Z "$R$1" 2>/dev/null | cut -d' ' -f1; }
mounted() { grep -qF " $1 " "$MI"; }

finish() {
  if [ "$1" = 1 ]; then setprop ctl.restart zygote; echo RESTARTED; else echo UNCHANGED; fi
  exit 0
}

case $1 in
mount)
  shift
  [ $# -gt 0 ] && [ $(($# % 5)) -eq 0 ] || { echo "usage: libfix.sh mount (<target> <cache> <upload> <orig> <fix>)..."; exit 2; }
  need=0
  ready=
  while [ $# -ge 5 ]; do
    t=$1 c=$2 u=$3 o=$4 f=$5
    shift 5
    if mounted "$t"; then echo "$t ALREADY $(sum "$t")"; continue; fi
    [ -e "$R$t" ] || { echo "$t ABSENT"; continue; }
    cur=$(sum "$t")
    [ "$cur" = "$f" ] && { echo "$t IMAGE_PATCHED"; continue; }
    [ "$cur" = "$o" ] || { echo "$t SKIPPED image $cur"; continue; }
    if [ "$(sum "$c")" != "$f" ]; then
      [ "$(sum "$u")" = "$f" ] || { rm -f "$R$u"; echo "$t NEED_UPLOAD"; need=1; continue; }
      rm -f "$R$c"; cp "$R$u" "$R$c"; rm -f "$R$u"
    fi
    got=$(sum "$c")
    [ "$got" = "$f" ] || { rm -f "$R$c"; echo "$t SKIPPED patched $got"; continue; }
    ready="$ready $t $c $f"
  done
  if [ $need = 1 ]; then
    set -- $ready
    while [ $# -ge 3 ]; do echo "$1 READY"; shift 3; done
    finish 0
  fi
  changed=0
  set -- $ready
  while [ $# -ge 3 ]; do
    t=$1 c=$2 f=$3
    shift 3
    l=$(label "$t")
    chmod 644 "$R$c"
    chcon "$l" "$R$c"
    now=$(label "$c")
    [ -n "$l" ] && [ "$now" = "$l" ] || { echo "$t FAILED chcon $l -> $now"; continue; }
    mount --bind "$R$c" "$R$t"
    got=$(sum "$t")
    if [ "$got" != "$f" ]; then
      mounted "$t" && umount "$R$t"
      echo "$t FAILED mount $got"
      continue
    fi
    echo "$t MOUNTED $got"
    changed=1
  done
  finish $changed
  ;;
revert)
  shift
  [ $# -gt 0 ] && [ $(($# % 2)) -eq 0 ] || { echo "usage: libfix.sh revert (<target> <fix>)..."; exit 2; }
  changed=0
  while [ $# -ge 2 ]; do
    t=$1 f=$2
    shift 2
    mounted "$t" || { echo "$t NOT_MOUNTED"; continue; }
    cur=$(sum "$t")
    [ "$cur" = "$f" ] || { echo "$t OTHER $cur"; continue; }
    umount -l "$R$t"
    if mounted "$t"; then echo "$t FAILED umount"; continue; fi
    echo "$t REVERTED $(sum "$t")"
    changed=1
  done
  finish $changed
  ;;
*)
  echo "usage: libfix.sh mount|revert ..."
  exit 2
  ;;
esac
