# SPDX-License-Identifier: AGPL-3.0-or-later
# Windows integration test on a Windows runner with process isolation.
# Usage: run-windows.ps1 IMAGE COVERDIR LTSC MSI HOSTEXE FULLMSI
param(
    [Parameter(Mandatory)] [string] $Image,
    [Parameter(Mandatory)] [string] $Cover,
    [Parameter(Mandatory)] [string] $Ltsc,
    [Parameter(Mandatory)] [string] $Msi,
    [Parameter(Mandatory)] [string] $HostExe,
    [Parameter(Mandatory)] [string] $FullMsi
)
$ErrorActionPreference = 'Stop'

New-Item -ItemType Directory -Force $Cover | Out-Null
$Cover = (Resolve-Path $Cover).Path
$data = Join-Path (Split-Path $Cover) 'hl-data'
New-Item -ItemType Directory -Force $data | Out-Null
$dir = 'C:\hl-dir'
New-Item -ItemType Directory -Force $dir | Out-Null
# A volume mounted at a folder, and a directory name Win32 normalizes away.
$mount = 'C:\hl-mount'
New-Item -ItemType Directory -Force $mount | Out-Null
@"
create vdisk file="$data\hl-mount.vhdx" maximum=64 type=expandable
attach vdisk
create partition primary
format fs=ntfs quick
assign mount="$mount"
"@ | Set-Content -Encoding ascii (Join-Path $data 'diskpart.txt')
diskpart /s (Join-Path $data 'diskpart.txt') | Out-Null
if ($LASTEXITCODE -ne 0) { Fail 'diskpart' }
fsutil usn createjournal m=1000000 a=100000 $mount | Out-Null
if ($LASTEXITCODE -ne 0) {
    fsutil usn createjournal m=1000000 a=100000 (Get-Volume -FilePath $mount).UniqueId | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail 'usn journal' }
}
New-Item -ItemType Directory -Force (Join-Path $mount 'data') | Out-Null
[IO.File]::WriteAllBytes((Join-Path $mount 'data\a'), (New-Object byte[] 250000))
Add-Type -Namespace HL -Name Kernel -MemberDefinition '[DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)] public static extern bool CreateDirectoryW(string n, IntPtr sa);'
if (-not [HL.Kernel]::CreateDirectoryW("\\?\$dir\dot.", [IntPtr]::Zero)) { Fail 'dot directory' }
[IO.File]::WriteAllBytes("\\?\$dir\dot.\b", (New-Object byte[] 400000))
$script:url = $null
$nano = "mcr.microsoft.com/windows/nanoserver:$Ltsc"

function Step($text) { Write-Host "== $text" }
function Metrics {
    try { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 "$script:url/metrics").Content } catch { '' }
}
function Fail($text) {
    Write-Host "FAIL: $text"
    Write-Host '--- metrics'; Write-Host (Metrics)
    Write-Host '--- log'; docker logs hatchlens 2>&1 | Select-Object -Last 80 | Write-Host
    foreach ($name in 'hatchlens-host.log', 'hatchlens.log') {
        $log = Join-Path $env:ProgramData "HatchLens\$name"
        if (Test-Path $log) { Write-Host "--- $name"; Get-Content $log -Tail 40 | Write-Host }
    }
    exit 1
}
function Invoke-Docker {
    & docker @args
    if ($LASTEXITCODE -ne 0) { Fail "docker $args" }
}
function WaitFor($seconds, $what, [scriptblock] $check) {
    for ($i = 0; $i -lt $seconds; $i++) {
        try { if (& $check) { return } } catch { }
        Start-Sleep -Seconds 1
    }
    Fail "timeout: $what"
}
function Value($pattern) {
    foreach ($l in ((Metrics) -split "`n")) {
        if ($l -match $pattern) { return [double]($l.Trim() -split ' ')[-1] }
    }
    return -1
}
function Start-HatchLens([string[]] $extra) {
    docker rm -f hatchlens 2>$null | Out-Null
    $run = @('run', '-d', '--name', 'hatchlens', '-e', 'GOCOVERDIR=C:\cover', '-e', 'HATCHLENS_CONFIG_FILE=C:\data\config.yml',
        '-v', "${Cover}:C:\cover", '-v', "${data}:C:\data", '-v', '\\.\pipe\docker_engine:\\.\pipe\docker_engine') + $extra + @($Image)
    Invoke-Docker @run | Out-Null
    $ip = docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' hatchlens
    $script:url = "http://${ip}:8080"
    WaitFor 120 'ready' { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 "$script:url/-/ready").StatusCode -eq 200 }
}

Step 'host service binary'
$env:GOCOVERDIR = $Cover
& $HostExe version
if ($LASTEXITCODE -ne 0) { Fail 'hatchlens-host version' }
Remove-Item Env:GOCOVERDIR

Step 'start without the host service'
@"
reconcile_interval: 5s
exclude:
  containers: ["hatchlens"]
directories:
  - path: '$dir'
  - path: '$mount\data'
"@ | Set-Content -Encoding ascii (Join-Path $data 'config.yml')
[IO.File]::WriteAllBytes((Join-Path $dir 'a'), (New-Object byte[] 200000))
Invoke-Docker volume create vol1 | Out-Null
$volDir = docker volume inspect -f '{{.Mountpoint}}' vol1
[IO.File]::WriteAllBytes((Join-Path $volDir 'a'), (New-Object byte[] 300000))
Invoke-Docker create --name target -v 'vol1:C:\v' $nano | Out-Null
Start-HatchLens @()
WaitFor 120 'install_host_service' { (Metrics) -match 'hatchlens_admin_action_needed\{action="install_host_service"' }
if ((Metrics) -match 'hatchlens_directory_bytes') { Fail 'sizes without the host service' }

Step 'container from mcr.microsoft.com checked'
WaitFor 120 'target checked' { (Metrics) -match 'hatchlens_container_image_info\{container="target",mode="checked",' }
if ((Metrics) -notmatch 'hatchlens_container_image_update_available\{container="target",') { Fail 'update_available' }

Step 'container CPU, memory, disk and network sum without the host service'
Invoke-Docker run -d --name busy --cpus 1 --memory 512m $nano ping -t 127.0.0.1 | Out-Null
WaitFor 120 'CPU of busy' { (Metrics) -match 'hatchlens_container_cpu_usage_seconds_total\{container="busy"\} ' }
$m = Metrics
foreach ($want in 'hatchlens_container_memory_working_set_bytes\{container="busy"\}',
                  'hatchlens_container_memory_limit_bytes\{container="busy"\} 5\.36870912e\+08',
                  'hatchlens_container_cpu_limit_cores\{container="busy"\} 1$',
                  'hatchlens_container_disk_writes_total\{container="busy"\}',
                  'hatchlens_container_network_receive_bytes_total\{container="busy"\} ') {
    if (-not (($m -split "`n") -match $want)) { Fail "missing $want" }
}
docker rm -f busy | Out-Null
docker stop -t 10 hatchlens | Out-Null

Step 'install the host service'
$p = Start-Process msiexec -ArgumentList '/i', "`"$Msi`"", '/qn', 'TCP=127.0.0.1:8091', '/l*v', "`"$(Join-Path $data 'msi-install.log')`"" -Wait -PassThru
if ($p.ExitCode -ne 0) { Get-Content (Join-Path $data 'msi-install.log') -Tail 40 | Write-Host; Fail "msiexec /i: $($p.ExitCode)" }
WaitFor 30 'service running' { (Get-Service HatchLensHost).Status -eq 'Running' }
$acl = Get-Acl (Join-Path $env:ProgramData 'HatchLens')
$sids = $acl.Access | ForEach-Object { $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value }
if ($sids -match '^S-1-(1-0|3-0|5-32-545)$') { Fail "ProgramData\HatchLens open to users: $sids" }

Step 'host service over TCP with the token'
$tokenFile = Join-Path $env:ProgramData 'HatchLens\host-token'
WaitFor 30 'token file' { Test-Path $tokenFile }
$token = (Get-Content $tokenFile -Raw).Trim()
$denied = try { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 'http://127.0.0.1:8091/v1/version').StatusCode } catch { [int]$_.Exception.Response.StatusCode }
if ($denied -ne 401) { Fail "TCP without token: $denied" }
$v = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Headers @{ Authorization = "Bearer $token" } 'http://127.0.0.1:8091/v1/version'
if ($v.StatusCode -ne 200) { Fail "TCP with token: $($v.StatusCode)" }

Step 'start with the host service'
Start-HatchLens @('-v', '\\.\pipe\hatchlens-host:\\.\pipe\hatchlens-host')
WaitFor 120 'volume measured' { (Value 'hatchlens_directory_bytes\{.*source="volume".*volume="vol1"') -gt 0 }
WaitFor 60 'directory measured' { (Value 'hatchlens_directory_bytes\{path="C:\\\\hl-dir",source="config"') -ge 600000 }
WaitFor 60 'mounted volume measured' { (Value 'hatchlens_directory_bytes\{path="C:\\\\hl-mount\\\\data",source="config"') -ge 250000 }
if ((Metrics) -match 'install_host_service') { Fail 'admin action with the host service' }
$before = Value 'hatchlens_directory_bytes\{.*source="volume".*volume="vol1"'
[IO.File]::WriteAllBytes((Join-Path $volDir 'b'), (New-Object byte[] 5000000))
WaitFor 60 'volume growth' { (Value 'hatchlens_directory_bytes\{.*source="volume".*volume="vol1"') -ge $before + 5000000 }

Step 'network per Docker network'
Invoke-Docker network create -d nat net2 | Out-Null
Invoke-Docker create --name busy2 --network nat $nano ping -t 127.0.0.1 | Out-Null
Invoke-Docker network connect net2 busy2
Invoke-Docker start busy2 | Out-Null
WaitFor 120 'nat series' { (Metrics) -match 'hatchlens_container_network_receive_bytes_total\{container="busy2",network="nat"\}' }
WaitFor 60 'net2 series' { (Metrics) -match 'hatchlens_container_network_receive_bytes_total\{container="busy2",network="net2"\}' }
docker rm -f busy2 | Out-Null
docker stop -t 10 hatchlens | Out-Null

Step 'uninstall the host service'
$p = Start-Process msiexec -ArgumentList '/x', "`"$Msi`"", '/qn' -Wait -PassThru
if ($p.ExitCode -ne 0) { Fail "msiexec /x: $($p.ExitCode)" }
if (Get-Service HatchLensHost -ErrorAction SilentlyContinue) { Fail 'service still installed' }

Step 'HatchLens as one Windows service on the host'
$pd = Join-Path $env:ProgramData 'HatchLens'
New-Item -ItemType Directory -Force $pd | Out-Null
@"
reconcile_interval: 5s
directories:
  - path: '$dir'
"@ | Set-Content -Encoding ascii (Join-Path $pd 'config.yml')
$p = Start-Process msiexec -ArgumentList '/i', "`"$FullMsi`"", '/qn', '/l*v', "`"$(Join-Path $data 'msi-full.log')`"" -Wait -PassThru
if ($p.ExitCode -ne 0) { Get-Content (Join-Path $data 'msi-full.log') -Tail 40 | Write-Host; Fail "msiexec /i full: $($p.ExitCode)" }
WaitFor 30 'HatchLens service running' { (Get-Service HatchLens).Status -eq 'Running' }
$script:url = 'http://127.0.0.1:8080'
WaitFor 120 'ready' { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 "$script:url/-/ready").StatusCode -eq 200 }
WaitFor 120 'volume measured' { (Value 'hatchlens_directory_bytes\{.*source="volume".*volume="vol1"') -gt 0 }
WaitFor 60 'directory measured' { (Value 'hatchlens_directory_bytes\{path="C:\\\\hl-dir",source="config"') -gt 0 }
WaitFor 120 'target checked' { (Metrics) -match 'hatchlens_container_image_info\{container="target",mode="checked",' }
if ((Metrics) -match 'install_host_service') { Fail 'admin action in the service' }
if (-not (Test-Path (Join-Path $pd 'state.json'))) { Fail 'state not in ProgramData' }
Invoke-Docker create --name busy3 --network nat $nano ping -t 127.0.0.1 | Out-Null
Invoke-Docker network connect net2 busy3
Invoke-Docker start busy3 | Out-Null
WaitFor 120 'nat series' { (Metrics) -match 'hatchlens_container_network_receive_bytes_total\{container="busy3",network="nat"\}' }
WaitFor 60 'net2 series' { (Metrics) -match 'hatchlens_container_network_receive_bytes_total\{container="busy3",network="net2"\}' }
docker rm -f busy3 | Out-Null
$p = Start-Process msiexec -ArgumentList '/x', "`"$FullMsi`"", '/qn' -Wait -PassThru
if ($p.ExitCode -ne 0) { Fail "msiexec /x full: $($p.ExitCode)" }
if (Get-Service HatchLens -ErrorAction SilentlyContinue) { Fail 'HatchLens service still installed' }
Step 'passed'
