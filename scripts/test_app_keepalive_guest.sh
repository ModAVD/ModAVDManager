#!/system/bin/sh
# Copyright 2026 jinghero
# On an Android 14 guest: sh test_app_keepalive_guest.sh /path/to/modavd-app-keepalive.sh
# Exercise the real mksh timers and logcat reader. No installed apps or settings are used.
set -eu
script=${1:?pass the guest watcher script}
test -r "$script"
scratch=$(mktemp -d /data/local/tmp/app-keepalive-test.XXXXXX)
export scratch
watcher=''
cleanup() {
    if [ -n "$watcher" ]; then kill "$watcher" 2>/dev/null || :; wait "$watcher" 2>/dev/null || :; fi
    if [ -s "$scratch/logger" ]; then kill "$(cat "$scratch/logger")" 2>/dev/null || :; fi
    rm -rf "$scratch"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

mkdir "$scratch/bin"
cat >"$scratch/command" <<'STUB'
#!/system/bin/sh
case "${0##*/}" in
    logcat)
        [ ! -s "$scratch/logger" ] || exit 1
        echo "$$" >"$scratch/logger"
        exec tail -n +1 -f "$scratch/events" ;;
    am)
        case "$1" in
            get-current-user) echo 0 ;;
            force-stop) echo "$4" >>"$scratch/stopped" ;;
            *) exit 1 ;;
        esac ;;
    pm)
        for pkg do :; done
        echo "$pkg" >"$scratch/query"
        echo "package:$pkg" ;;
    cmd) echo org.example.home/.Home ;;
    settings) echo org.example.ime/.Service ;;
    dumpsys)
        if [ ! -f "$scratch/ready" ]; then
            touch "$scratch/checked.$(cat "$scratch/query")"
            exit 1
        fi
        echo 'topResumedActivity=ActivityRecord{123 u0 org.example.home/.Home t1}' ;;
esac
STUB
chmod 700 "$scratch/command"
for cmd in logcat am pm cmd settings dumpsys; do ln -s "$scratch/command" "$scratch/bin/$cmd"; done
: >"$scratch/events"
PATH="$scratch/bin:$PATH" sh "$script" watch &
watcher=$!

# Pause both apps. Make the foreground query fail when each original deadline expires.
for pkg in org.example.game org.example.login; do
    echo "I/wm_pause_activity: [0,123,$pkg/.Main,userLeaving=true,pauseBackTasks]" >>"$scratch/events"
done
limit=$((SECONDS + 80))
while [ ! -e "$scratch/checked.org.example.game" ] || [ ! -e "$scratch/checked.org.example.login" ]; do
    [ "$SECONDS" -lt "$limit" ] || { echo 'did not check both expired timers' >&2; exit 1; }
    sleep 0.2
done
test ! -e "$scratch/stopped"

# Login briefly resumes and leaves again: only the game is due on the retry.
echo 'I/wm_set_resumed_activity: [0,org.example.login/.Main,bringingFoundTaskToFront]' >>"$scratch/events"
echo 'I/wm_pause_activity: [0,123,org.example.login/.Main,userLeaving=true,pauseBackTasks]' >>"$scratch/events"
touch "$scratch/ready"
limit=$((SECONDS + 20))
while [ ! -s "$scratch/stopped" ]; do
    [ "$SECONDS" -lt "$limit" ] || { echo 'lost pending app after a query failure' >&2; exit 1; }
    sleep 0.2
done
sleep 1
test "$(cat "$scratch/stopped")" = org.example.game
echo 'PASS: retried the background app and cancelled the old login timer'
