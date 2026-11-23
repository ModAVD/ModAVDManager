#!/system/bin/sh
# Copyright 2026 jinghero
# Android 14, owner user. Only ActivityTaskManager transitions start or cancel timers.
# The watcher and its timers disappear with the guest. It runs as adb shell.
set -f
lock=/data/local/tmp/modavd-app-keepalive.lock

if [ "$1" = daemon ]; then
    # Pass the locked file description to the worker on stdin. A repeated boot setup
    # leaves the existing worker and its pending timers alone.
    exec 9>"$lock" || exit 1
    flock -n 0 <&9 || exit 0
    nohup setsid sh "$0" watch <&9 >/dev/null 2>&1 &
    worker=$!
    sleep 0.1
    kill -0 "$worker" 2>/dev/null
    exit $?
fi
[ "$1" = watch ] || exit 2

now() {
    read -r uptime rest < /proc/uptime
    seconds=${uptime%%.*}
}

eligible() {
    typeset pkg=$1 homes ime activity resumed component
    case "$pkg" in
        android|com.android.vending|com.google.android.gms|com.google.android.gms.*|com.google.android.gsf|com.google.android.gsf.*|com.google.android.partnersetup) return 1 ;;
    esac
    [ "$(am get-current-user)" = 0 ] || return 1
    pm list packages -3 --user 0 "$pkg" | grep -Fxq "package:$pkg" || return 1
    homes=$(cmd package query-activities --components --user 0 -a android.intent.action.MAIN -c android.intent.category.HOME) || return 1
    case "$homes" in */*) ;; *) return 1 ;; esac
    for component in $homes; do [ "${component%%/*}" != "$pkg" ] || return 1; done
    ime=$(settings --user 0 get secure default_input_method) || return 1
    case "$ime" in */*) ;; *) return 1 ;; esac
    [ "${ime%%/*}" != "$pkg" ] || return 1
    # Re-read foreground state just before stopping. Treat an unrecognised dump as unknown.
    activity=$(dumpsys activity activities) || return 1
    resumed=$(printf '%s\n' "$activity" | sed -nE '/topResumedActivity=|Resumed: ActivityRecord|ResumedActivity: ActivityRecord/s/.* u0 ([A-Za-z0-9_.]+)\/.*/\1/p')
    [ -n "$resumed" ] || return 1
    for component in $resumed; do [ "$component" != "$pkg" ] || return 1; done
}

transition() {
    typeset payload component pkg entry keep=''
    payload=${1#*\[0,}
    [ "$payload" != "$1" ] || return
    case "$1" in
        I/wm_set_resumed_activity:*) component=${payload%%,*} ;;
        I/wm_resume_activity:*) component=${payload##*,}; component=${component%]} ;;
        I/wm_pause_activity:*) payload=${payload#*,}; component=${payload%%,*} ;;
        *) return ;;
    esac
    case "$component" in */*) pkg=${component%%/*} ;; *) return ;; esac
    case "$pkg" in ''|*[!a-zA-Z0-9_.]*) return ;; esac
    for entry in $pending; do
        [ "${entry%@*}" = "$pkg" ] || keep="$keep $entry"
    done
    pending=$keep
    case "$1" in I/wm_pause_activity:*)
        now
        # uptime is rounded down here. Adding 61 never shortens the 60-second grace period.
        pending="$pending $pkg@$((seconds + 61))"
    esac
}

# Block in logcat while idle. A timeout wakes us for the earliest deadline. Drain queued
# transitions first, including any that arrived while querying package and activity state.
while true; do
    pending=''
    logcat -b events --uid=1000 -v tag -T "$(date +%s.%N)" wm_pause_activity:I wm_resume_activity:I wm_set_resumed_activity:I '*:S' </dev/null |&
    logger=$!
    while kill -0 "$logger" 2>/dev/null; do
        now
        wait_for=86400
        due=''
        for entry in $pending; do
            left=$((${entry#*@} - seconds))
            if [ "$left" -le 0 ]; then due=$entry; wait_for=0.05; break; fi
            [ "$left" -ge "$wait_for" ] || wait_for=$left
        done
        if IFS= read -r -p -t "$wait_for" line; then transition "$line"; continue; fi
        [ -n "$due" ] || continue
        pkg=${due%@*}
        if eligible "$pkg"; then
            if IFS= read -r -p -t 0.05 line; then transition "$line"; continue; fi
            kill -0 "$logger" 2>/dev/null || break
            am force-stop --user 0 "$pkg"
        fi
        pending=${pending/" $due"/}
    done
    wait "$logger"
    # Reconnect with fresh timers: switches during a logd restart are unknown.
    sleep 1
done
