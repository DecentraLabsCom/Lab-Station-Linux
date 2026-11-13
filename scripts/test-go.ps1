[CmdletBinding()]
param(
    [string]$GoImage = 'golang:1.23',
    [string]$FmuExecutorSource = (Join-Path $PSScriptRoot '..\..\FMU-Executor')
)

$ErrorActionPreference = 'Stop'

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$fmuExecutorSourcePath = (Resolve-Path -LiteralPath $FmuExecutorSource -ErrorAction Stop).Path
$fmuExecutorCommitPath = Join-Path $repositoryRoot 'internal\agent\fmu_executor_commit.txt'
$fmuExecutorCommit = (Get-Content -LiteralPath $fmuExecutorCommitPath -Raw).Trim()
$docker = Get-Command docker -ErrorAction SilentlyContinue

if ($fmuExecutorCommit -notmatch '^[0-9a-f]{40}$') {
    throw "The pinned FMU Executor commit in $fmuExecutorCommitPath is invalid."
}

if (-not $docker) {
    throw 'Docker Desktop is required. Install it, start its Linux engine, and retry.'
}

$previousErrorActionPreference = $ErrorActionPreference
try {
    $ErrorActionPreference = 'Continue'
    $dockerInfo = & $docker.Source info --format '{{.OSType}}' 2>$null
    $dockerExitCode = $LASTEXITCODE
}
finally {
    $ErrorActionPreference = $previousErrorActionPreference
}

if ($dockerExitCode -ne 0) {
    throw 'The Docker engine is unavailable. Start Docker Desktop and retry.'
}
$dockerOs = $dockerInfo | Select-Object -First 1
if ($dockerOs -ne 'linux') {
    throw 'A Linux Docker engine is required. Switch Docker Desktop to Linux containers and retry.'
}

$repositoryMount = "type=bind,source=$repositoryRoot,target=/workspace,readonly"
$fmuExecutorMount = "type=bind,source=$fmuExecutorSourcePath,target=/fmu-executor-source,readonly"
$dockerArguments = @(
    'run',
    '--rm',
    '--pull=missing',
    '--mount', $repositoryMount,
    '--mount', $fmuExecutorMount,
    '--workdir', '/workspace',
    '--env', "FMU_EXECUTOR_COMMIT=$fmuExecutorCommit",
    $GoImage,
    'sh', '-c', 'set -eu; mkdir -p /tmp/fmu-executor-source; git -c safe.directory=/fmu-executor-source -C /fmu-executor-source archive --format=tar "$FMU_EXECUTOR_COMMIT" > /tmp/fmu-executor-source.tar; tar -xf /tmp/fmu-executor-source.tar -C /tmp/fmu-executor-source; FMU_EXECUTOR_SOURCE=/tmp/fmu-executor-source go test -race -cover ./...'
)

& $docker.Source @dockerArguments
if ($LASTEXITCODE -ne 0) {
    throw "Go tests failed in $GoImage with exit code $LASTEXITCODE."
}
