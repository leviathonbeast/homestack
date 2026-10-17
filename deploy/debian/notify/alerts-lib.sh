# Shared helpers for the alert jobs. Installed as /usr/local/lib/homestack/alerts-lib.sh

# Where the storage is (Settings → Storage → Where the storage is; written by
# the agent). Unraid's paths until it has written it.
. "${LAYOUT_CONF:-/etc/default/homestack-layout}" 2>/dev/null
: "${STORAGE_CACHE:=/mnt/cache}" "${STORAGE_PARITY_PREFIX:=/mnt/parity}"

# label[sdX]: which pool or role a disk has (disk1…, cache, parity, system).
declare -gA label
_parent() { lsblk -no PKNAME "$(readlink -m "$1")" 2>/dev/null | head -1; }
load_labels() {
  local pool dev p m src pv i
  while read -r pool; do
    while read -r dev; do
      p=$(_parent "$dev"); [ -n "$p" ] && label[$p]=$pool
    done < <(zpool list -vHP "$pool" 2>/dev/null | awk '$1 ~ /^\/dev\// {print $1}')
  done < <(zpool list -Ho name 2>/dev/null)
  # The cache, and parity 1 to 6 (2 and on are added from Storage → Array devices).
  for m in "$STORAGE_CACHE:cache" "${STORAGE_PARITY_PREFIX}1:parity"; do
    src=$(findmnt -no SOURCE "${m%:*}" 2>/dev/null) && p=$(_parent "$src") && [ -n "$p" ] && label[$p]=${m##*:}
  done
  for i in 2 3 4 5 6; do
    src=$(findmnt -no SOURCE "$STORAGE_PARITY_PREFIX$i" 2>/dev/null) && p=$(_parent "$src") && [ -n "$p" ] && label[$p]=parity$i
  done
  while read -r pv; do p=$(_parent "$pv"); [ -n "$p" ] && label[$p]=system; done < <(pvs --noheadings -o pv_name 2>/dev/null)
}

# disk_desc sdX → "disk2 (/dev/sdc · ST16000NE000-2RW103 · ZL2PDPW6)"
disk_desc() {
  local d=${1#/dev/} model serial
  model=$(lsblk -dno MODEL "/dev/$d" 2>/dev/null | sed 's/ *$//')
  serial=$(lsblk -dno SERIAL "/dev/$d" 2>/dev/null)
  echo "${label[$d]:-$d} (/dev/$d${model:+ · $model}${serial:+ · $serial})"
}

# disk_name sdX → "disk2" (or the device name if it has no role)
disk_name() { local d=${1#/dev/}; echo "${label[$d]:-$d}"; }
