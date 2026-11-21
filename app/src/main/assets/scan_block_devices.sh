for d in /sys/block/sd* /sys/block/mmcblk*; do
  [ -e "$d" ] || continue
  b=$(basename "$d")
  [ "$b" = "mmcblk0" ] && continue
  rem=$(cat "$d/removable" 2>/dev/null || echo 0)
  ro=$(cat "$d/ro" 2>/dev/null || echo 0)
  sec=$(cat "$d/size" 2>/dev/null || echo 0)
  bytes=$((sec * 512))
  model=$(cat "$d/device/model" "$d/device/name" 2>/dev/null | tr '\n' ' ')
  cid=$(cat "$d/device/cid" 2>/dev/null || echo "")
  csd=$(cat "$d/device/csd" 2>/dev/null || echo "")
  serial=$(cat "$d/device/serial" 2>/dev/null || echo "")
  echo "$b|$bytes|$ro|$rem|$model|$cid|$csd|$serial"
done
