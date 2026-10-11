# Registers the .gocpy file type and icon for the current user (no admin needed).
# Usage (from packaging\windows):  powershell -ExecutionPolicy Bypass -File install-filetype.ps1
$ErrorActionPreference = "Stop"
$dir = Join-Path $env:LOCALAPPDATA "Gocpy"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Copy-Item -Force (Join-Path $PSScriptRoot "gocpy.ico") (Join-Path $dir "gocpy.ico")
$classes = "HKCU:\Software\Classes"
New-Item -Force -Path "$classes\.gocpy" | Out-Null
Set-ItemProperty -Path "$classes\.gocpy" -Name "(default)" -Value "Gocpy.Source"
Set-ItemProperty -Path "$classes\.gocpy" -Name "Content Type" -Value "text/x-gocpy"
Set-ItemProperty -Path "$classes\.gocpy" -Name "PerceivedType" -Value "text"
New-Item -Force -Path "$classes\Gocpy.Source\DefaultIcon" | Out-Null
Set-ItemProperty -Path "$classes\Gocpy.Source" -Name "(default)" -Value "Gocpy source file"
Set-ItemProperty -Path "$classes\Gocpy.Source\DefaultIcon" -Name "(default)" -Value "`"$dir\gocpy.ico`",0"
# Ask Explorer to refresh icons
Add-Type -Namespace Win32 -Name Shell -MemberDefinition '[DllImport("shell32.dll")] public static extern void SHChangeNotify(int e, int f, System.IntPtr a, System.IntPtr b);'
[Win32.Shell]::SHChangeNotify(0x08000000, 0, [IntPtr]::Zero, [IntPtr]::Zero)
Write-Host "done: .gocpy -> Gocpy.Source"
