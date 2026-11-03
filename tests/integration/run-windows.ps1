# SPDX-License-Identifier: AGPL-3.0-or-later
# Windows integration test on a Windows runner with process isolation.
# Usage: run-windows.ps1 IMAGE COVERDIR LTSC
param(
    [Parameter(Mandatory)] [string] $Image,
    [Parameter(Mandatory)] [string] $Cover,
    [Parameter(Mandatory)] [string] $Ltsc
)
$ErrorActionPreference = 'Stop'

New-Item -ItemType Directory -Force $Cover | Out-Null
$Cover = (Resolve-Path $Cover).Path
$data = Join-Path (Split-Path $Cover) 'hl-data'
New-Item -ItemType Directory -Force $data | Out-Null
$script:url = $null

function Step($text) { Write-Host "== $text" }
function Metrics {
    try { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 "$script:url/metrics").Content } catch { '' }
}
function Fail($text) {
    Write-Host "FAIL: $text"
    Write-Host '--- metrics'; Write-Host (Metrics)
    Write-Host '--- log'; docker logs hatchlens 2>&1 | Select-Object -Last 80 | Write-Host
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

Step 'start'
@'
reconcile_interval: 5s
exclude:
  containers: ["hatchlens"]
'@ | Set-Content -Encoding ascii (Join-Path $data 'config.yml')
Invoke-Docker create --name target "mcr.microsoft.com/windows/nanoserver:$Ltsc" | Out-Null
Invoke-Docker run -d --name hatchlens -e GOCOVERDIR=C:\cover -e HATCHLENS_CONFIG_FILE=C:\data\config.yml `
    -v "${Cover}:C:\cover" -v "${data}:C:\data" `
    -v '\\.\pipe\docker_engine:\\.\pipe\docker_engine' $Image | Out-Null
$ip = docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' hatchlens
$script:url = "http://${ip}:8080"
WaitFor 120 'ready' { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 "$script:url/-/ready").StatusCode -eq 200 }

Step 'container from mcr.microsoft.com checked'
WaitFor 120 'target checked' { (Metrics) -match 'hatchlens_container_image_info\{container="target",mode="checked",' }
if ((Metrics) -notmatch 'hatchlens_container_image_update_available\{container="target",') { Fail 'update_available' }
if ((Metrics) -match 'hatchlens_directory_') { Fail 'directory metrics on Windows' }

Step 'container CPU, memory, disk and network'
Invoke-Docker run -d --name busy --cpus 1 --memory 512m "mcr.microsoft.com/windows/nanoserver:$Ltsc" ping -t 127.0.0.1 | Out-Null
WaitFor 120 'CPU of busy' { (Metrics) -match 'hatchlens_container_cpu_usage_seconds_total\{container="busy"\} ' }
$m = Metrics
($m -split "`n") | Where-Object { $_ -match '^hatchlens_container_(network_receive_bytes|memory_|cpu_limit|disk_writes)' } | Write-Host
foreach ($want in 'hatchlens_container_memory_working_set_bytes\{container="busy"\}',
                  'hatchlens_container_memory_limit_bytes\{container="busy"\} 5\.36870912e\+08',
                  'hatchlens_container_cpu_limit_cores\{container="busy"\} 1$',
                  'hatchlens_container_disk_writes_total\{container="busy"\}',
                  'hatchlens_container_network_receive_bytes_total\{container="busy"\} ') {
    if (-not (($m -split "`n") -match $want)) { Fail "missing $want" }
}
docker rm -f busy | Out-Null

docker stop -t 10 hatchlens | Out-Null
Step 'passed'
