package main

// PowerShell scripts (ASCII only) and runner. Used only on Windows.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const psHead = "[Console]::OutputEncoding=[Text.Encoding]::UTF8\r\n$ProgressPreference='SilentlyContinue'\r\n$ErrorActionPreference='Continue'\r\n"

const psGuard = `
param([int]$n, [string]$fs = '', [string]$label = '', [int]$full = 0, [int]$on = 0)
$d = Get-Disk -Number $n -ErrorAction SilentlyContinue
if (-not $d) { Write-Output 'FAIL|Disk not found'; Write-Output 'RESULT|failed'; exit 2 }
if ($d.IsSystem -or $d.IsBoot) { Write-Output 'FAIL|Refusing to touch the system disk'; Write-Output 'RESULT|failed'; exit 2 }
`

const psList = psHead + `
$out = @()
foreach ($d in (Get-Disk | Where-Object { -not $_.IsSystem -and -not $_.IsBoot })) {
  $parts = @(Get-Partition -DiskNumber $d.Number -ErrorAction SilentlyContinue)
  $vols = @()
  foreach ($p in $parts) {
    if ($p.DriveLetter) {
      $v = Get-Volume -DriveLetter $p.DriveLetter -ErrorAction SilentlyContinue
      $vols += [pscustomobject]@{ letter=[string]$p.DriveLetter; fs=[string]$v.FileSystem; label=[string]$v.FileSystemLabel; size=[int64]$v.Size; free=[int64]$v.SizeRemaining; ro=[bool]$p.IsReadOnly }
    }
  }
  $out += [pscustomobject]@{ number=[int]$d.Number; name=[string]$d.FriendlyName; size=[int64]$d.Size; bus=[string]$d.BusType; style=[string]$d.PartitionStyle; readonly=[bool]$d.IsReadOnly; offline=[bool]$d.IsOffline; partitions=[int]$parts.Count; volumes=$vols; health=[string]$d.HealthStatus; opstatus=[string]$d.OperationalStatus }
}
ConvertTo-Json -InputObject @($out) -Depth 5 -Compress
`

const psUnlock = psHead + psGuard + `
Write-Output 'STEP|disk'
Write-Output ('INFO|IsReadOnly=' + $d.IsReadOnly + '  IsOffline=' + $d.IsOffline)
try { Set-Disk -Number $n -IsOffline $false -ErrorAction Stop } catch {}
try { Set-Disk -Number $n -IsReadOnly $false -ErrorAction Stop; Write-Output 'OK|Disk read-only flag cleared' }
catch { Write-Output ('WARN|Set-Disk: ' + $_.Exception.Message) }
Write-Output 'STEP|partitions'
$letters = @()
foreach ($p in @(Get-Partition -DiskNumber $n -ErrorAction SilentlyContinue)) {
  try { Set-Partition -DiskNumber $n -PartitionNumber $p.PartitionNumber -IsReadOnly $false -ErrorAction Stop; Write-Output ('OK|Partition ' + $p.PartitionNumber + ' unlocked') }
  catch { Write-Output ('WARN|Partition ' + $p.PartitionNumber + ': ' + $_.Exception.Message) }
  if ($p.DriveLetter) { $letters += [string]$p.DriveLetter }
}
Write-Output 'STEP|diskpart'
$lines = @("select disk $n", 'attributes disk clear readonly noerr')
foreach ($l in $letters) { $lines += "select volume $l"; $lines += 'attributes volume clear readonly noerr' }
$tmp = Join-Path $env:TEMP 'cardwake_dp.txt'
$lines | Set-Content -Path $tmp -Encoding ASCII
diskpart /s $tmp | ForEach-Object { if ($_.Trim()) { Write-Output ('INFO|' + $_.Trim()) } }
Remove-Item $tmp -ErrorAction SilentlyContinue
Write-Output 'STEP|policy'
$k1 = 'HKLM:\SYSTEM\CurrentControlSet\Control\StorageDevicePolicies'
if ((Test-Path $k1) -and ((Get-ItemProperty $k1).WriteProtect -eq 1)) {
  Set-ItemProperty -Path $k1 -Name WriteProtect -Value 0
  Write-Output 'OK|Cleared the Windows WriteProtect policy'
} else { Write-Output 'OK|WriteProtect policy: not set' }
$k2 = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\RemovableStorageDevices\{53f5630d-b6bf-11d0-94f2-00a0c91efb8b}'
if ((Test-Path $k2) -and ((Get-ItemProperty $k2).Deny_Write -eq 1)) {
  Set-ItemProperty -Path $k2 -Name Deny_Write -Value 0
  Write-Output 'OK|Cleared the deny-write group policy'
} else { Write-Output 'OK|Deny-write group policy: not set' }
Write-Output 'STEP|verify'
Start-Sleep -Seconds 2
$d = Get-Disk -Number $n
Write-Output ('INFO|IsReadOnly now = ' + $d.IsReadOnly)
$tested = 0; $passed = 0
foreach ($p in @(Get-Partition -DiskNumber $n -ErrorAction SilentlyContinue | Where-Object DriveLetter)) {
  $tested++
  $t = ([string]$p.DriveLetter) + ':\__cardwake_test.tmp'
  try { Set-Content -Path $t -Value 'x' -ErrorAction Stop; if (Test-Path $t) { Remove-Item $t -ErrorAction Stop }; $passed++; Write-Output ('OK|Write test passed on ' + $p.DriveLetter + ': (file created and deleted)') }
  catch { Write-Output ('FAIL|Write test failed on ' + $p.DriveLetter + ':') }
}
if ($tested -eq 0) { if ($d.IsReadOnly) { Write-Output 'RESULT|locked' } else { Write-Output 'RESULT|noletter' } }
elseif ($passed -gt 0) { Write-Output 'RESULT|ok' }
else { Write-Output 'RESULT|locked' }
`

// Clears partitions so that raw writes to the physical drive are allowed.
const psClear = psHead + psGuard + `
Write-Output 'STEP|clean'
try {
  Set-Disk -Number $n -IsOffline $false -ErrorAction SilentlyContinue
  Set-Disk -Number $n -IsReadOnly $false -ErrorAction SilentlyContinue
  Clear-Disk -Number $n -RemoveData -RemoveOEM -Confirm:$false -ErrorAction Stop
  Write-Output 'OK|Disk wiped (partitions removed)'
  Write-Output 'RESULT|ok'
} catch { Write-Output ('FAIL|' + $_.Exception.Message); Write-Output 'RESULT|failed'; exit 1 }
`

// After raw writes: make Windows re-read the disk.
const psRescan = psHead + `
param([int]$n)
try { Set-Disk -Number $n -IsOffline $true -ErrorAction SilentlyContinue; Start-Sleep -Milliseconds 600; Set-Disk -Number $n -IsOffline $false -ErrorAction SilentlyContinue } catch {}
try { Update-Disk -Number $n -ErrorAction SilentlyContinue } catch {}
try { Update-HostStorageCache } catch {}
Start-Sleep -Seconds 2
$l = (Get-Partition -DiskNumber $n -ErrorAction SilentlyContinue | Where-Object DriveLetter | ForEach-Object { $_.DriveLetter }) -join ','
Write-Output ('INFO|Windows sees drive letter(s): ' + $l)
`

const psFormat = psHead + psGuard + `
Write-Output 'STEP|clean'
try {
  Set-Disk -Number $n -IsOffline $false -ErrorAction SilentlyContinue
  Set-Disk -Number $n -IsReadOnly $false -ErrorAction SilentlyContinue
  Clear-Disk -Number $n -RemoveData -RemoveOEM -Confirm:$false -ErrorAction Stop
  Write-Output 'OK|Disk wiped'
} catch { Write-Output ('FAIL|' + $_.Exception.Message); Write-Output 'RESULT|failed'; exit 1 }
Write-Output 'STEP|partition'
try {
  $d = Get-Disk -Number $n
  if ($d.PartitionStyle -eq 'RAW') {
    $style = 'MBR'; if ($d.Size -gt 2TB) { $style = 'GPT' }
    Initialize-Disk -Number $n -PartitionStyle $style -ErrorAction Stop
  }
  $p = New-Partition -DiskNumber $n -UseMaximumSize -AssignDriveLetter -ErrorAction Stop
  Write-Output ('OK|Partition created, drive ' + $p.DriveLetter + ':')
} catch { Write-Output ('FAIL|' + $_.Exception.Message); Write-Output 'RESULT|failed'; exit 1 }
Write-Output 'STEP|format'
try {
  if ($full -eq 1) {
    Write-Output 'INFO|Full format: every sector is overwritten with zeros. This can take a long time.'
    Format-Volume -DriveLetter $p.DriveLetter -FileSystem $fs -NewFileSystemLabel $label -Full -Confirm:$false -ErrorAction Stop | Out-Null
  } else {
    Format-Volume -DriveLetter $p.DriveLetter -FileSystem $fs -NewFileSystemLabel $label -Confirm:$false -ErrorAction Stop | Out-Null
  }
  Write-Output ('OK|Formatted ' + $p.DriveLetter + ': as ' + $fs)
  Write-Output 'RESULT|ok'
} catch { Write-Output ('FAIL|' + $_.Exception.Message); Write-Output 'RESULT|failed'; exit 1 }
`

const psWBlock = psHead + psGuard + `
Write-Output 'STEP|apply'
$ro = ($on -eq 1)
try { Set-Disk -Number $n -IsReadOnly $ro -ErrorAction Stop; Write-Output ('OK|Disk write protection ' + $(if ($ro) {'ON'} else {'OFF'})) } catch { Write-Output ('WARN|Set-Disk: ' + $_.Exception.Message) }
foreach ($p in @(Get-Partition -DiskNumber $n -ErrorAction SilentlyContinue)) {
  try { Set-Partition -DiskNumber $n -PartitionNumber $p.PartitionNumber -IsReadOnly $ro -ErrorAction Stop } catch {}
}
Write-Output 'STEP|verify'
Start-Sleep -Milliseconds 800
$d = Get-Disk -Number $n
if ($d.IsReadOnly -eq $ro) { Write-Output 'RESULT|ok' } else { Write-Output 'RESULT|failed' }
`

const psDevInfo = psHead + `
param([int]$n)
$dd = Get-CimInstance Win32_DiskDrive | Where-Object { $_.Index -eq $n } | Select-Object -First 1
$g = Get-Disk -Number $n -ErrorAction SilentlyContinue
$vid = ''; $vpid = ''; $chain = @()
if ($dd) {
  $id = $dd.PNPDeviceID
  for ($i = 0; $i -lt 6 -and $id; $i++) {
    $chain += $id
    if ($id -match 'VID_([0-9A-Fa-f]{4})&PID_([0-9A-Fa-f]{4})') { $vid = $Matches[1]; $vpid = $Matches[2]; break }
    try { $id = (Get-PnpDeviceProperty -InstanceId $id -KeyName 'DEVPKEY_Device_Parent' -ErrorAction Stop).Data } catch { $id = $null }
  }
}
$o = [pscustomobject]@{
  model = [string]$dd.Model; caption = [string]$dd.Caption; manufacturer = [string]$dd.Manufacturer
  interface = [string]$dd.InterfaceType; firmware = [string]$dd.FirmwareRevision; serial = ([string]$dd.SerialNumber).Trim()
  mediaType = [string]$dd.MediaType; size = [int64]$dd.Size; bytesPerSector = [int]$dd.BytesPerSector; sectors = [int64]$dd.TotalSectors
  partitions = [int]$dd.Partitions; signature = [string]$dd.Signature; status = [string]$dd.Status; capabilities = ($dd.CapabilityDescriptions -join ', ')
  vid = $vid.ToUpper(); pid = $vpid.ToUpper(); pnp = [string]$dd.PNPDeviceID
  physSector = [int]$g.PhysicalSectorSize; logSector = [int]$g.LogicalSectorSize; uniqueId = [string]$g.UniqueId; location = [string]$g.Location
  partStyle = [string]$g.PartitionStyle; health = [string]$g.HealthStatus; opStatus = [string]$g.OperationalStatus; readonly = [bool]$g.IsReadOnly
}
ConvertTo-Json -InputObject $o -Compress
`

const psParts = psHead + `
param([int]$n)
$out = @()
foreach ($p in @(Get-Partition -DiskNumber $n -ErrorAction SilentlyContinue)) {
  $v = $null; if ($p.DriveLetter) { $v = Get-Volume -DriveLetter $p.DriveLetter -ErrorAction SilentlyContinue }
  $out += [pscustomobject]@{ number=[int]$p.PartitionNumber; letter=[string]$p.DriveLetter; size=[int64]$p.Size; offset=[int64]$p.Offset; type=[string]$p.Type; active=[bool]$p.IsActive; fs=[string]$v.FileSystem; label=[string]$v.FileSystemLabel; free=[int64]$v.SizeRemaining }
}
$max = 0; try { $max = [int64](Get-Disk -Number $n).LargestFreeExtent } catch {}
ConvertTo-Json -InputObject ([pscustomobject]@{ parts = @($out); freeExtent = $max }) -Depth 4 -Compress
`

const psPartOp = psHead + `
param([int]$n, [string]$op, [int]$part = 0, [string]$letter = '', [string]$newletter = '', [string]$label = '', [string]$fs = 'exFAT', [int64]$size = 0)
$d = Get-Disk -Number $n -ErrorAction SilentlyContinue
if (-not $d -or $d.IsSystem -or $d.IsBoot) { Write-Output 'FAIL|Refusing: disk not found or system disk'; exit 2 }
try {
  switch ($op) {
    'label'  { Set-Volume -DriveLetter $letter -NewFileSystemLabel $label -ErrorAction Stop; Write-Output ('OK|Label set to ' + $label) }
    'letter' { Set-Partition -DiskNumber $n -PartitionNumber $part -NewDriveLetter $newletter -ErrorAction Stop; Write-Output ('OK|Drive letter changed to ' + $newletter + ':') }
    'delete' { Remove-Partition -DiskNumber $n -PartitionNumber $part -Confirm:$false -ErrorAction Stop; Write-Output 'OK|Partition deleted' }
    'resize' { Resize-Partition -DiskNumber $n -PartitionNumber $part -Size $size -ErrorAction Stop; Write-Output 'OK|Partition resized' }
    'create' {
      if ($d.PartitionStyle -eq 'RAW') { Initialize-Disk -Number $n -PartitionStyle MBR -ErrorAction Stop }
      if ($size -gt 0) { $p = New-Partition -DiskNumber $n -Size $size -AssignDriveLetter -ErrorAction Stop } else { $p = New-Partition -DiskNumber $n -UseMaximumSize -AssignDriveLetter -ErrorAction Stop }
      Format-Volume -DriveLetter $p.DriveLetter -FileSystem $fs -NewFileSystemLabel $label -Confirm:$false -ErrorAction Stop | Out-Null
      Write-Output ('OK|Partition created and formatted: ' + $p.DriveLetter + ':')
    }
    default { Write-Output 'FAIL|Unknown operation' }
  }
} catch { Write-Output ('FAIL|' + $_.Exception.Message) }
`

const psEject = psHead + `
param([string]$letter, [int]$kill = 0, [int]$n = -1)
$me = $PID
if ($letter) {
  $root = $letter + ':\'
  Write-Output 'STEP|flush'
  try { Write-VolumeCache -DriveLetter $letter -ErrorAction Stop; Write-Output 'OK|Write cache flushed' } catch { Write-Output 'WARN|Could not flush cache (older Windows?)' }
  Write-Output 'STEP|release'
  $busy = @(Get-Process -ErrorAction SilentlyContinue | Where-Object { try { $_.Path -like ($root + '*') } catch { $false } })
  foreach ($b in $busy) { Write-Output ('WARN|In use by: ' + $b.ProcessName + ' (PID ' + $b.Id + ')') }
  if ($kill -eq 1) {
    foreach ($b in $busy) { if ($b.ProcessName -notin @('explorer','powershell','Cardwake') -and $b.Id -ne $me) { try { Stop-Process -Id $b.Id -Force -ErrorAction Stop; Write-Output ('OK|Closed ' + $b.ProcessName) } catch {} } }
  }
  Write-Output 'STEP|eject'
  try {
    $sh = New-Object -ComObject Shell.Application
    $sh.Namespace(17).ParseName($letter + ':').InvokeVerb('Eject')
  } catch { Write-Output ('WARN|' + $_.Exception.Message) }
  Start-Sleep -Seconds 3
  if (Test-Path $root) { Write-Output 'FAIL|The drive is still in use. Close programs that use it (or use "close blocking programs") and try again.'; Write-Output 'RESULT|busy' }
  else { Write-Output 'OK|It is now safe to remove the card'; Write-Output 'RESULT|ok' }
} else {
  Write-Output 'STEP|flush'
  Write-Output 'STEP|release'
  Write-Output 'STEP|eject'
  try { Set-Disk -Number $n -IsOffline $true -ErrorAction Stop; Write-Output 'OK|Disk taken offline - safe to remove'; Write-Output 'RESULT|ok' } catch { Write-Output ('FAIL|' + $_.Exception.Message); Write-Output 'RESULT|busy' }
}
`

const psBitlocker = psHead + `
param([string]$letter)
if (-not (Get-Command Enable-BitLocker -ErrorAction SilentlyContinue)) { Write-Output 'FAIL|BitLocker is not available on this Windows edition (Home). Use a Cardwake vault instead.'; Write-Output 'RESULT|unavailable'; exit 1 }
Write-Output 'STEP|encrypt'
try {
  $pw = ConvertTo-SecureString $env:CW_PW -AsPlainText -Force
  Enable-BitLocker -MountPoint ($letter + ':') -EncryptionMethod Aes256 -UsedSpaceOnly -PasswordProtector -Password $pw -ErrorAction Stop | Out-Null
  Write-Output ('OK|BitLocker To Go enabled on ' + $letter + ':')
  Write-Output 'RESULT|ok'
} catch { Write-Output ('FAIL|' + $_.Exception.Message); Write-Output 'RESULT|failed' }
`

const psPick = psHead + `
param([string]$kind, [string]$filter = '', [int]$multi = 0)
Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.Form -Property @{TopMost=$true; Opacity=0; ShowInTaskbar=$false}
$f.Show()
if ($kind -eq 'folder') {
  $d = New-Object System.Windows.Forms.FolderBrowserDialog
  $d.ShowNewFolderButton = $true
  if ($d.ShowDialog($f) -eq 'OK') { Write-Output $d.SelectedPath }
} else {
  $d = New-Object System.Windows.Forms.OpenFileDialog
  if ($filter) { $d.Filter = $filter } else { $d.Filter = 'All files (*.*)|*.*' }
  $d.Multiselect = ($multi -eq 1)
  if ($d.ShowDialog($f) -eq 'OK') { $d.FileNames | ForEach-Object { Write-Output $_ } }
}
$f.Close()
`

// hoistParam moves a single-line param(...) block to the top (PowerShell requires it first).
func hoistParam(script string) string {
	lines := strings.Split(script, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "param(") {
			p := lines[i]
			rest := append(append([]string{}, lines[:i]...), lines[i+1:]...)
			return p + "\n" + strings.Join(rest, "\n")
		}
	}
	return script
}

func writePS(script string) (string, error) {
	f, err := os.CreateTemp("", "cw_*.ps1")
	if err != nil {
		return "", err
	}
	f.WriteString("\xef\xbb\xbf" + hoistParam(strings.ReplaceAll(script, "\r\n", "\n")))
	f.Close()
	return f.Name(), nil
}

func psCmd(ctx context.Context, path string, args []string) *exec.Cmd {
	a := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", path}
	a = append(a, args...)
	c := exec.CommandContext(ctx, "powershell.exe", a...)
	hideCmd(c)
	return c
}

// psRun runs a script and returns its output.
func psRun(script string, timeout time.Duration, args ...string) (string, error) {
	if !isWindows {
		return "", fmt.Errorf("PowerShell is only available on Windows")
	}
	p, err := writePS(script)
	if err != nil {
		return "", err
	}
	defer os.Remove(p)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out bytes.Buffer
	c := psCmd(ctx, p, args)
	c.Stdout = &out
	c.Stderr = &out
	err = c.Run()
	return strings.TrimSpace(out.String()), err
}

// psStream runs a script and feeds TAG|text lines to onLine. env adds variables.
func psStream(j *Job, script string, env []string, args ...string) error {
	if !isWindows {
		return fmt.Errorf("PowerShell is only available on Windows")
	}
	p, err := writePS(script)
	if err != nil {
		return err
	}
	defer os.Remove(p)
	c := psCmd(context.Background(), p, args)
	c.Env = append(os.Environ(), env...)
	pipe, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = c.Stdout
	if err := c.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		j.handleLine(strings.TrimSpace(sc.Text()))
		if j.Cancel.Load() {
			c.Process.Kill()
			break
		}
	}
	return c.Wait()
}
