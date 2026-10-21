#!/bin/bash
SOCK=/usr/local/apps/@appcenter/tad-module/tad-module.sock
echo Sseasea123 | sudo -S bash -c "
  pgrep -x tad-module >/dev/null && echo PROC-UP || echo PROC-DOWN
  curl -s --unix-socket \$SOCK http://localhost/api/crps/status | grep -o '1\.0\.49'
  echo '--- set manual 30 ---'
  curl -s --unix-socket \$SOCK -X POST -H 'Content-Type: application/json' -d '{\"mode\":\"manual\",\"pct\":30}' http://localhost/api/crps/fan; echo
  sleep 1
  curl -s --unix-socket \$SOCK http://localhost/api/crps/status | grep -o '\"fan_mode\":\"[a-z]*\"'
  echo '--- set auto ---'
  curl -s --unix-socket \$SOCK -X POST -H 'Content-Type: application/json' -d '{\"mode\":\"auto\"}' http://localhost/api/crps/fan; echo
  sleep 1
  curl -s --unix-socket \$SOCK http://localhost/api/crps/status | grep -o '\"fan_mode\":\"[a-z]*\"'
" 2>/dev/null
