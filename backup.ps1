# One-click backup: Trae memory + chat history db + NAS data snapshot + project zip + git push
# Usage: tell the AI "update backup", or: powershell -File <this file>
$ErrorActionPreference = "Stop"
$proj = Split-Path -Parent $PSCommandPath          # project root (script lives there)
$mem  = "c:\Users\web\.trae-cn\memory\projects\-d-crps------p2-0e1e6b334b9462401340"
$chat = "C:\Users\web\AppData\Roaming\Trae CN\ModularData\ai-agent"
$git  = "C:\Program Files\Git\cmd\git.exe"
$key  = "$env:USERPROFILE\.ssh\fnos_key"

# 1) Trae project memory (mirror, incl. date subdirs)
robocopy $mem "$proj\backup\trae-memory" /MIR /NJH /NJS /NDL /NFL | Out-Null
if ($LASTEXITCODE -ge 8) { throw "robocopy memory sync failed: $LASTEXITCODE" }
"[1/5] memory synced"

# 2) Chat history database (snapshot while Trae runs; closing Trae first gives a cleaner copy)
New-Item -ItemType Directory -Force -Path "$proj\backup\trae-chatdb" | Out-Null
Copy-Item "$chat\database.db","$chat\database.db-shm","$chat\database.db-wal" "$proj\backup\trae-chatdb\" -Force
"[2/5] chat db synced"

# 3) NAS data snapshot (energy/crps.json; skipped if key missing or not yet authorized on NAS)
$ErrorActionPreference = "Continue"   # PS5.1: sudo writes "[sudo] password:" to stderr; would abort under Stop
if (Test-Path $key) {
  $ErrorActionPreference = "Continue"   # sudo writes prompts to stderr; PS5.1 would abort on them
  ssh -i $key -o BatchMode=yes -o ConnectTimeout=8 yufeise@192.168.0.2 "true" 2>$null
  if ($LASTEXITCODE -eq 0) {
    ssh -i $key -o BatchMode=yes yufeise@192.168.0.2 "echo Sseasea123 | sudo -S sh -c 'cp /usr/local/apps/@appconf/tad-module/energy.json /usr/local/apps/@appconf/tad-module/crps.json /tmp/ && chmod 644 /tmp/energy.json /tmp/crps.json'" 2>$null
    scp -i $key -o BatchMode=yes -q "yufeise@192.168.0.2:/tmp/energy.json" "yufeise@192.168.0.2:/tmp/crps.json" "$proj\backup\"
    "[3/5] NAS snapshot done"
  } else { "[3/5] NAS snapshot SKIPPED (ssh public key not installed on NAS yet)" }
  $ErrorActionPreference = "Stop"
} else { "[3/5] NAS snapshot SKIPPED (no ssh key: $key)" }

# 4) Project zip (incl. .git history) to D:\ root; remove older ones
$zip = "d:\" + (Split-Path -Leaf $proj) + "-backup-" + (Get-Date -Format yyyy-MM-dd) + ".zip"
Get-ChildItem "d:\" -Filter "*-backup-*.zip" | Where-Object { $_.FullName -ne $zip } | Remove-Item -Force
if (Test-Path $zip) { Remove-Item $zip -Force }
Compress-Archive -Path $proj -DestinationPath $zip -CompressionLevel Optimal
$mb = [math]::Round((Get-Item $zip).Length / 1MB, 1)
"[4/5] zip done: $zip ($mb MB)"

# 5) Commit & push (only tracked files; chatdb is gitignored)
$ErrorActionPreference = "Continue"   # git writes LF/CRLF warnings to stderr; PS5.1 would abort on them
Set-Location $proj
& $git add backup/trae-memory backup/energy.json backup/crps.json PROGRESS.md 2>&1 | Out-Null
$st = & $git status --porcelain
if ($st) {
  & $git -c user.name="yufeise" -c user.email="yufeise@nas.local" commit -m ("chore: backup update " + (Get-Date -Format yyyy-MM-dd)) 2>&1 | Out-Null
  foreach ($i in 1..3) { & $git push origin main *> "$env:TEMP\push-backup.log"; if ($LASTEXITCODE -eq 0) { "push ok"; break }; Start-Sleep 5 }
} else { "[5/5] git: no changes" }
$ErrorActionPreference = "Stop"
"== BACKUP COMPLETE =="
