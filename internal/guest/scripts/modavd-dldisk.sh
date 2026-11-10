#!/system/bin/sh
# modavd-dldisk.sh - a checkpoint disk under the download folder of games that
# fsync after every few KB. Run as root (adb shell su 0 sh ...).
#
#   daemon  one pass now (waits up to 20 s for the shared storage), then start
#           "watch" in the background unless it is already running
#   watch   loop: one pass every TICK seconds
#   once    one pass
#   off     stop the watcher; copy what the disks hold back into the plain
#           folders, unmount and delete them (the disk of a running game stays
#           until the next boot)
#   status  print current state
#
# Why: Arknights writes its resource packages 7 KB at a time and calls fsync
# after each piece. On /data every fsync is three write requests in a row, each
# a round trip to the host: about 8 MiB/s (ModAVD/ModAVD#35), 11 with the
# shorter round trip of modavd's QEMU arguments. With the folder on this disk:
# 73-95 MiB/s (11-10, as fast as the network and the unpacking allow).
#
# The disk: an ext4 image on /data, mounted through a loop device with
# data=journal and a commit every 3 s, and over it an overlayfs in "volatile"
# mode, mounted on the game's folder as apps see it. In that mode fsync
# returns at once and nothing is passed down; the ext4 below commits by
# itself every 3 s, and that commit does flush the image file into /data.
# So an fsync'ed byte reaches the host up to 3 s later than on /data. After
# a hard kill the journal is replayed and the whole folder is what it was at
# the last commit: file sizes, renames and the game's own index files all
# from the same moment, and the game resumes its downloads by file size as
# it always does. 11-10: three hard kills in the middle of a download, the
# game carried on each time and finished all 15 GB without an unpack error.
# A plain "do not wait for fsync" on /data cannot give that: there a file's
# size can be committed before its data (ModAVD/ModAVD#96, error 302).
#
# discard hands the blocks of deleted packages back to /data: without it the
# image grew to 13.5 GB for 8 MB of files and filled a 32 GB data disk.
#
# What a pass does for each entry of LIST (package:folder below the package's
# Android/data directory), never while a process of the package is running:
#   - package not installed: remove its disk, if any;
#   - folder missing (never started, or app data cleared): what an old image
#     holds belongs to an earlier life of the app, so it goes; the folder is
#     made the way the system makes it for the app (owner, ext_data_rw, 2770);
#   - folder holds a download that began without the disk: leave the game
#     alone, remove a stale disk; the disk comes once that download is done;
#   - otherwise: make the image if missing, mount both layers.
# The watcher repeats this, so a game installed after boot gets its disk
# before its first start, and a mount lost with the shared storage (framework
# restart) comes back.

R=${MODAVD_DLDISK_ROOT:-}
TAG=modavd-dldisk
BASE=$R/data/local/tmp/$TAG
MEDIA=$R/data/media/0/Android/data
VIEW=$R/mnt/user/0/emulated/0/Android/data
MI=$R/proc/self/mountinfo
LIST="com.hypergryph.arknights:files/HGGameUpdate/HGDownload com.hypergryph.arknights.bilibili:files/HGGameUpdate/HGDownload"
SIZE=16G
MIN_FREE_KB=2097152
TICK=3

say() { echo "$TAG: $*"; /system/bin/log -t $TAG "$*" 2>/dev/null; }

# note <package> <text>: say it when it changes; status prints the latest
note() {
    f=$BASE/.$1.note
    [ "$(cat "$f" 2>/dev/null)" = "$2" ] && return 0
    mkdir -p "$BASE" && echo "$2" > "$f"
    say "$1: $2"
}

mounted() { grep -q " ${1#$R} " "$MI" 2>/dev/null; }
read_only() { grep " ${1#$R} " "$MI" 2>/dev/null | grep -q " ro[, ]"; }
uid_of() { stat -c %u "$R/data/data/$1" 2>/dev/null; }
running() { [ -n "$(pgrep -u "$1" 2>/dev/null | head -1)" ]; }
loop_of() { losetup -a 2>/dev/null | grep -F "${1#$R}" | head -1 | cut -d: -f1; }
label_of() { ls -Zd "$1" 2>/dev/null | cut -d' ' -f1; }
# files other than the index the game keeps at the top of the folder
busy() { [ -n "$(find "$1" -type f ! -path "$1/index.json" 2>/dev/null | head -1)" ]; }

# mkpath <package> <folder> <uid>
mkpath() {
    c=$(label_of "$MEDIA")
    [ -n "$c" ] || return 1
    p=$MEDIA
    for seg in $1 $(echo "$2" | tr / ' '); do
        p=$p/$seg
        [ -d "$p" ] && continue
        mkdir "$p" && chown "$3:1078" "$p" && chmod 2770 "$p" && chcon "$c" "$p" || return 1
    done
}

# drop <package>: unmount and delete its disk; fails while something still uses it
drop() {
    dd=$BASE/$1
    [ -d "$dd" ] || return 0
    v=$(cat "$dd/view" 2>/dev/null)
    if [ -n "$v" ] && mounted "$v"; then umount "$v" || return 1; fi
    if mounted "$dd/inner"; then umount "$dd/inner" || return 1; fi
    l=$(loop_of "$dd/dl.img")
    [ -n "$l" ] && losetup -d "$l" 2>/dev/null
    rm -rf "$dd"
}

# open_image <package>: the image mounted on its inner directory (journal replay included)
open_image() {
    dd=$BASE/$1
    mounted "$dd/inner" && return 0
    l=$(loop_of "$dd/dl.img")
    [ -n "$l" ] || l=$(losetup -f -s "$dd/dl.img")
    [ -n "$l" ] || return 1
    mount -t ext4 -o noatime,data=journal,commit=3,discard "$l" "$dd/inner"
}

setup() {
    pkg=$1; rel=$2
    d=$BASE/$pkg; img=$d/dl.img; in=$d/inner
    lower=$MEDIA/$pkg/$rel; view=$VIEW/$pkg/$rel
    uid=$(uid_of "$pkg")
    if [ -z "$uid" ]; then
        [ -d "$d" ] || return 0
        drop "$pkg" && rm -f "$BASE/.$pkg.note" && say "$pkg: not installed any more, disk removed"
        return 0
    fi
    if mounted "$view"; then
        # still good unless the app's data was cleared under it or the image went read-only on an error
        if [ -d "$lower" ] && ! read_only "$in"; then note "$pkg" "on its disk"; return 0; fi
        running "$uid" && return 0
        drop "$pkg" || return 0
    fi
    running "$uid" && return 0
    [ -d "$VIEW" ] || return 0
    if [ ! -d "$lower" ]; then
        drop "$pkg" || return 0
        mkpath "$pkg" "$rel" "$uid" || { note "$pkg" "cannot make ${lower#$R}"; return 0; }
    elif busy "$lower"; then
        drop "$pkg" || return 0
        note "$pkg" "a download is under way in the plain folder, the disk comes when it is done"
        return 0
    fi
    kept=kept
    if [ -f "$img" ] && tune2fs -l "$img" 2>/dev/null | grep -q "^Filesystem state:.*error"; then
        say "$pkg: the image has file system errors, starting a new one"
        drop "$pkg" || return 0
    fi
    if [ ! -f "$img" ]; then
        free=$(df -k "$R/data" 2>/dev/null | awk 'NR == 2 { print $4 }')
        if [ "${free:-0}" -lt $MIN_FREE_KB ]; then note "$pkg" "less than 2 GB free on /data, no disk"; return 0; fi
        mkdir -p "$in" && truncate -s $SIZE "$img" && chmod 600 "$img" &&
            mke2fs -F -q -t ext4 -b 4096 -m 0 -J size=512 "$img" > /dev/null 2>&1 || {
            rm -rf "$d"; note "$pkg" "cannot make the image"; return 0; }
        kept=new
    fi
    if ! open_image "$pkg"; then
        # the game downloads again what was in flight
        say "$pkg: the image does not mount, starting a new one"
        drop "$pkg"
        return 0
    fi
    mkdir -p "$in/lower" "$in/upper" "$in/work" || return 0
    o=$(stat -c %u:%g "$lower"); m=$(stat -c %a "$lower"); c=$(label_of "$lower")
    for x in "$in/lower" "$in/upper"; do
        chown "$o" "$x" && chmod "$m" "$x" && chcon "$c" "$x" || { note "$pkg" "cannot label the disk"; umount "$in"; return 0; }
    done
    # a volatile overlay refuses to mount again while this marker of its last mount is there; what is under it is
    # the journalled ext4, consistent after any stop
    rm -rf "$in/work/work/incompat"
    echo "$view" > "$d/view"
    if mount -t overlay overlay -o "lowerdir=$in/lower,upperdir=$in/upper,workdir=$in/work,volatile" "$view"; then
        note "$pkg" "on its disk"
        say "$pkg: disk mounted ($kept image)"
    else
        umount "$in"
        note "$pkg" "cannot mount the disk on ${view#$R}"
    fi
}

pass() {
    mkdir -p "$BASE" || return 1
    # one pass at a time (daemon and watch can meet); a lock older than a minute is a leftover
    if ! mkdir "$BASE/.lock" 2>/dev/null; then
        [ -n "$(find "$BASE/.lock" -maxdepth 0 -mmin +1 2>/dev/null)" ] && rmdir "$BASE/.lock" 2>/dev/null
        return 0
    fi
    for e in $LIST; do setup "${e%%:*}" "${e#*:}"; done
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
    while [ ! -d "$VIEW" ] && [ $i -lt 20 ]; do sleep 1; i=$((i + 1)); done
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
off)
    for p in $(watchers); do [ "$p" != "$$" ] && kill "$p"; done
    rmdir "$BASE/.lock" 2>/dev/null
    for e in $LIST; do
        pkg=${e%%:*}; rel=${e#*:}
        d=$BASE/$pkg; lower=$MEDIA/$pkg/$rel
        [ -d "$d" ] || continue
        uid=$(uid_of "$pkg")
        if [ -n "$uid" ] && running "$uid"; then
            say "$pkg: running, its disk stays until the next boot"
            continue
        fi
        # hand the downloads in flight back to the plain folder
        if [ -n "$uid" ] && [ -d "$lower" ] && [ -f "$d/dl.img" ] && open_image "$pkg" &&
            [ -n "$(find "$d/inner/upper" -mindepth 1 2>/dev/null | head -1)" ]; then
            v=$(cat "$d/view" 2>/dev/null)
            if [ -n "$v" ] && mounted "$v"; then umount "$v" || { say "$pkg: disk in use, kept"; continue; }; fi
            cp -a "$d/inner/upper/." "$lower/" || { say "$pkg: could not copy the downloads back, disk kept"; continue; }
        fi
        drop "$pkg" && rm -f "$BASE/.$pkg.note" && say "$pkg: disk removed"
    done
    ;;
status)
    echo "watchers: $(watchers | tr '\n' ' ')"
    for e in $LIST; do
        pkg=${e%%:*}
        if [ -z "$(uid_of "$pkg")" ]; then echo "$pkg: not installed"; continue; fi
        echo "$pkg: $(cat "$BASE/.$pkg.note" 2>/dev/null || echo "nothing done yet")"
    done
    ;;
*)
    echo "usage: $0 daemon|watch|once|off|status"
    exit 2
    ;;
esac
