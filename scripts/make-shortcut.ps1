# 在桌面创建 LocalMap 快捷方式（自动定位同仓库 app/ 目录，任意机器可用）
$app = Resolve-Path (Join-Path $PSScriptRoot '..\app')
$exe = Join-Path $app 'localmap.exe'
if (-not (Test-Path $exe)) { Write-Error "未找到 $exe"; exit 1 }
$ws = New-Object -ComObject WScript.Shell
$sc = $ws.CreateShortcut([IO.Path]::Combine($env:USERPROFILE, 'Desktop', 'LocalMap.lnk'))
$sc.TargetPath = $exe
$sc.WorkingDirectory = $app.Path
$sc.IconLocation = "$exe,0"
$sc.Save()
Write-Output "shortcut created -> $exe"
