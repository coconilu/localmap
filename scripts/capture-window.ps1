Add-Type -AssemblyName System.Drawing
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public class Win32Cap {
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int c);
  [DllImport("user32.dll")] public static extern bool MoveWindow(IntPtr h, int x, int y, int w, int hh, bool repaint);
  public struct RECT { public int Left; public int Top; public int Right; public int Bottom; }
}
"@
$p = Get-Process localmap -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowHandle -ne 0 } | Select-Object -First 1
if (-not $p) { Write-Error "localmap window not found"; exit 1 }
$h = $p.MainWindowHandle
[Win32Cap]::ShowWindow($h, 9) | Out-Null    # SW_RESTORE
[Win32Cap]::MoveWindow($h, 0, 0, 980, 760, $true) | Out-Null
[Win32Cap]::SetForegroundWindow($h) | Out-Null
Start-Sleep -Milliseconds 1500
$r = New-Object Win32Cap+RECT
[Win32Cap]::GetWindowRect($h, [ref]$r) | Out-Null
$w = $r.Right - $r.Left
$hh = $r.Bottom - $r.Top
# 进程 DPI 不感知时坐标被虚拟化，截屏需按缩放比例放大到物理像素
$scale = 1.5
$pw = [int]($w * $scale)
$ph = [int]($hh * $scale)
$px = [int]($r.Left * $scale)
$py = [int]($r.Top * $scale)
$bmp = New-Object System.Drawing.Bitmap $pw, $ph
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($px, $py, 0, 0, $bmp.Size)
$out = 'E:\dev\kimi_workbench\localmap\docs\screenshot.png'
New-Item -ItemType Directory -Force (Split-Path $out) | Out-Null
$bmp.Save($out, [System.Drawing.Imaging.ImageFormat]::Png)
$g.Dispose(); $bmp.Dispose()
Write-Output "saved $out ${w}x${hh}"
