#!/bin/sh
# ZFS event daemon hook: sends ZFS problems and scrub results to Discord.
# Installed as /etc/zfs/zed.d/all-homestack.sh
[ -f "${ZED_ZEDLET_DIR}/zed.rc" ] && . "${ZED_ZEDLET_DIR}/zed.rc"
. "${ZED_ZEDLET_DIR}/zed-functions.sh"
N=${NOTIFY:-/usr/local/sbin/notify-discord}
[ -x "$N" ] || exit 0
pool="${ZEVENT_POOL:-?}"
nl='
'
case "$ZEVENT_SUBCLASS" in
  scrub_start)
    size=$(zpool list -Ho alloc "$pool" 2>/dev/null)
    "$N" -t "ZFS scrub started: $pool" "Checking every block of ${size:-the pool} against its checksums. Disks stay usable while it runs."
    ;;
  scrub_abort)
    "$N" -l warning -t "ZFS scrub stopped: $pool" "The scrub was cancelled before it finished. Start it again with \`zpool scrub $pool\`."
    ;;
  scrub_paused)
    "$N" -t "ZFS scrub paused: $pool" "Resume it with \`zpool scrub $pool\`."
    ;;
  resilver_start)
    "$N" -l warning -t "ZFS rebuild started: $pool" "ZFS is rebuilding data onto a disk. Avoid heavy use of $pool until it finishes."
    ;;
  scrub_finish|resilver_finish)
    what=scrub; [ "$ZEVENT_SUBCLASS" = resilver_finish ] && what=resilver
    summary=$(zpool status "$pool" 2>/dev/null | sed -n 's/^ *scan: //p; s/^errors: /errors: /p')
    if zpool status -x "$pool" 2>/dev/null | grep -q "is healthy"; then
      "$N" -l success -t "ZFS $what finished: $pool" "$summary"
    else
      "$N" -l error -t "ZFS $what finished with problems: $pool" "$summary$nl$nl$(zpool status -x "$pool" | head -30)"
    fi
    ;;
  statechange)
    case "$ZEVENT_VDEV_STATE_STR" in
      FAULTED|DEGRADED|REMOVED|UNAVAIL) ;;
      *) exit 0 ;;
    esac
    "$N" -l error -t "ZFS disk $ZEVENT_VDEV_STATE_STR in $pool" \
      "Device: ${ZEVENT_VDEV_PATH:-unknown}$nl$nl$(zpool status -x "$pool" 2>/dev/null | head -30)"
    ;;
  io|checksum|data|deadman)
    # At most one message per pool and type an hour.
    zed_rate_limit "homestack-$pool-$ZEVENT_SUBCLASS" 3600 || exit 0
    "$N" -l error -t "ZFS $ZEVENT_SUBCLASS errors in $pool" \
      "Device: ${ZEVENT_VDEV_PATH:-unknown}$nl$nl$(zpool status -x "$pool" 2>/dev/null | head -30)"
    ;;
esac
exit 0
