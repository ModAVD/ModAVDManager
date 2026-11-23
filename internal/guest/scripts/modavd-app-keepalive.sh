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
    # The host may repeat unfinished boot tuning after the user saves new settings.
    # A non-persistent property records the first choice until Android reboots.
    prop=debug.modavd.app_keepalive
    mode=$(getprop "$prop") || exit 1
    if [ -z "$mode" ]; then
        mode=$2
        case "$mode" in on|off) ;; *) exit 2 ;; esac
        setprop "$prop" "$mode" || exit 1
    fi
    case "$mode" in on) exit 0 ;; off) ;; *) exit 1 ;; esac
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
    typeset pkg=$1 user packages homes ime activity resumed component
    case "$pkg" in
        android|com.android.vending|com.google.android.gms|com.google.android.gms.*|com.google.android.gsf|com.google.android.gsf.*|com.google.android.partnersetup) return 1 ;;
    esac
    # 1 means exempt, 2 means Android could not confirm the state yet.
    user=$(am get-current-user) || return 2
    case "$user" in 0) ;; ''|*[!0-9]*) return 2 ;; *) return 1 ;; esac
    packages=$(pm list packages -3 --user 0 "$pkg") || return 2
    printf '%s\n' "$packages" | grep -Fxq "package:$pkg" || return 1
    homes=$(cmd package query-activities --components --user 0 -a android.intent.action.MAIN -c android.intent.category.HOME) || return 2
    case "$homes" in */*) ;; *) return 2 ;; esac
    for component in $homes; do [ "${component%%/*}" != "$pkg" ] || return 1; done
    ime=$(settings --user 0 get secure default_input_method) || return 2
    case "$ime" in */*) ;; *) return 2 ;; esac
    [ "${ime%%/*}" != "$pkg" ] || return 1
    # Re-read foreground state just before stopping. Treat an unrecognised dump as unknown.
    activity=$(dumpsys activity activities) || return 2
    resumed=$(printf '%s\n' "$activity" | sed -nE '/topResumedActivity=|Resumed: ActivityRecord|ResumedActivity: ActivityRecord/s/.* u0 ([A-Za-z0-9_.]+)\/.*/\1/p')
    [ -n "$resumed" ] || return 2
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
    # Android mksh drops its special coprocess input after a timed-out read -p.
    # Retain a normal descriptor across timeouts so later switches still reach us.
    exec 3<&p
    while kill -0 "$logger" 2>/dev/null; do
        now
        wait_for=86400
        due=''
        for entry in $pending; do
            left=$((${entry#*@} - seconds))
            if [ "$left" -le 0 ]; then due=$entry; wait_for=0.05; break; fi
            [ "$left" -ge "$wait_for" ] || wait_for=$left
        done
        if IFS= read -r -t "$wait_for" line <&3; then transition "$line"; continue; fi
        [ -n "$due" ] || continue
        pkg=${due%@*}
        eligible "$pkg"
        result=$?
        if [ "$result" = 0 ]; then
            if IFS= read -r -t 0.05 line <&3; then transition "$line"; continue; fi
            kill -0 "$logger" 2>/dev/null || break
            am force-stop --user 0 "$pkg" || result=2
        fi
        pending=${pending/" $due"/}
        if [ "$result" = 2 ]; then
            now
            pending="$pending $pkg@$((seconds + 10))"
        fi
    done
    exec 3<&-
    wait "$logger"
    # Reconnect with fresh timers: switches during a logd restart are unknown.
    sleep 1
done
