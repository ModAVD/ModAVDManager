#!/system/bin/sh
# modavd-vkmode.sh - listed games start in their Vulkan display mode. Run as root
# (adb shell su 0 sh ...).
#
#   daemon  one pass now (waits up to 20 s for the shared storage folder), then
#           start "watch" in the background unless it is already running
#   watch   loop: one pass every TICK seconds
#   once    one pass
#   status  print current state
#
# Why (ModAVD/ModAVD#222): Honkai: Star Rail picks its display mode at its first
# start. It picks OpenGL ES (files/vulkancaps.bin = 03), and gfxstream's GLES
# lacks what the game needs there: it closes about 15 s after the start. In
# Vulkan mode ("Normal Mode" under Settings > Graphics > Display Settings) it
# runs. 11-16: with files/customvulkancaps.bin and files/vulkancaps.bin both
# 0x01 before the first start, the game starts in Vulkan and keeps both at 01
# (dozens of starts). vulkancaps.bin alone: Vulkan once, then the game rewrites
# it to 03; customvulkancaps.bin alone: GLES. The game writes both when the user
# picks a mode in its settings, so customvulkancaps.bin being there means a
# mode was chosen (by the user, or by an earlier pass).
#
# What a pass does for each package of LIST, never while a process of the
# package runs:
#   - not installed: nothing (no folder is made);
#   - files/customvulkancaps.bin there: nothing;
#   - otherwise: the missing folders are made the way the system makes them for
#     the app (owner, ext_data_rw, 2770); both files are written as 01 (owner,
#     ext_data_rw, 660) under a temporary name and moved into place,
#     customvulkancaps.bin last. A pass that fails half way leaves that one
#     missing, so the next pass does it again.
# The watcher repeats this, so a game installed after boot is set before its
# first start.

R=${MODAVD_VKMODE_ROOT:-}
TAG=modavd-vkmode
BASE=$R/data/local/tmp/$TAG
MEDIA=$R/data/media/0/Android/data
LIST="com.miHoYo.hkrpg com.miHoYo.hkrpg.bilibili"
TICK=3

say() { echo "$TAG: $*"; /system/bin/log -t $TAG "$*" 2>/dev/null; }

# note <package> <text>: say it when it changes; status prints the latest
note() {
    f=$BASE/.$1.note
    [ "$(cat "$f" 2>/dev/null)" = "$2" ] && return 0
    mkdir -p "$BASE" && echo "$2" > "$f"
    say "$1: $2"
}

uid_of() { stat -c %u "$R/data/data/$1" 2>/dev/null; }
running() { [ -n "$(pgrep -u "$1" 2>/dev/null | head -1)" ]; }
label_of() { ls -Zd "$1" 2>/dev/null | cut -d' ' -f1; }

# mkpath <package> <uid>: <package>/files under MEDIA; a folder that cannot be given its owner is not left behind
mkpath() {
    c=$(label_of "$MEDIA")
    [ -n "$c" ] || return 1
    p=$MEDIA
    for seg in $1 files; do
        p=$p/$seg
        [ -d "$p" ] && continue
        mkdir "$p" || return 1
        chown "$2:1078" "$p" && chmod 2770 "$p" && chcon "$c" "$p" || { rmdir "$p" 2>/dev/null; return 1; }
    done
}

# put <uid> <folder> <name>: the file holds the byte 01 and belongs to the app
put() {
    t=$2/.$3.$TAG
    rm -f "$t"
    printf '\001' > "$t" && chown "$1:1078" "$t" && chmod 660 "$t" && mv -f "$t" "$2/$3" || { rm -f "$t"; return 1; }
}

setup() {
    pkg=$1
    uid=$(uid_of "$pkg")
    if [ -z "$uid" ]; then
        rm -f "$BASE/.$pkg.note"
        return 0
    fi
    dir=$MEDIA/$pkg/files
    if [ -e "$dir/customvulkancaps.bin" ]; then
        [ "$(cat "$BASE/.$pkg.note" 2>/dev/null)" = "set to Vulkan" ] || note "$pkg" "display mode chosen already"
        return 0
    fi
    if running "$uid"; then
        note "$pkg" "running, set once it exits"
        return 0
    fi
    [ -d "$MEDIA" ] || return 0
    mkpath "$pkg" "$uid" || { note "$pkg" "cannot make ${dir#$R}"; return 0; }
    if put "$uid" "$dir" vulkancaps.bin && put "$uid" "$dir" customvulkancaps.bin; then
        note "$pkg" "set to Vulkan"
    else
        note "$pkg" "cannot write the display mode files"
    fi
}

pass() {
    mkdir -p "$BASE" || return 1
    # one pass at a time (daemon and watch can meet); a lock older than a minute is a leftover
    if ! mkdir "$BASE/.lock" 2>/dev/null; then
        [ -n "$(find "$BASE/.lock" -maxdepth 0 -mmin +1 2>/dev/null)" ] && rmdir "$BASE/.lock" 2>/dev/null
        return 0
    fi
    for pkg in $LIST; do setup "$pkg"; done
    rmdir "$BASE/.lock"
}

watchers() { pgrep -f "$TAG.sh watch"; }

case "$1" in
daemon)
    if [ -z "$(watchers)" ]; then
        # first call of this boot: nothing of the last boot is running any more
        rmdir "$BASE/.lock" 2>/dev/null
        rm -f "$BASE"/.*.note 2>/dev/null
    fi
    i=0
    while [ ! -d "$MEDIA" ] && [ $i -lt 20 ]; do sleep 1; i=$((i + 1)); done
    pass
    [ -n "$(watchers)" ] || { nohup setsid sh "$0" watch > /dev/null 2>&1 & }
    i=0
    while [ $i -lt 10 ] && [ -z "$(watchers)" ]; do sleep 0.1; i=$((i + 1)); done
    ;;
watch)
    for p in $(watchers); do
        [ "$p" -lt $$ ] && exit 0
    done
    while true; do
        sleep $TICK
        pass
    done
    ;;
once)
    pass
    ;;
status)
    echo "watchers: $(watchers | tr '\n' ' ')"
    for pkg in $LIST; do
        if [ -z "$(uid_of "$pkg")" ]; then echo "$pkg: not installed"; continue; fi
        echo "$pkg: $(cat "$BASE/.$pkg.note" 2>/dev/null || echo "nothing done yet")"
    done
    ;;
*)
    echo "usage: $0 daemon|watch|once|status"
    exit 2
    ;;
esac
