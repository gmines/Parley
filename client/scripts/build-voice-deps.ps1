# Windows counterpart of build-voice-deps.sh: builds Opus (CMake) and
# pjproject (MSBuild, Release-Dynamic = static libraries against the DLL C
# runtime, matching Rust's default) into client\.deps\windows-x86_64.
# Needs Visual Studio 2022 build tools, CMake and msbuild on PATH.
#
#   pwsh client/scripts/build-voice-deps.ps1
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$OpusVersion = "1.6.1"
$OpusSha256  = "6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1"
$PjVersion   = "2.17"
$PjSha256    = "065fe06c06788d97c35f563796d59f00ce52fe9558a52d7b490a042a966facce"

$Here   = Resolve-Path (Join-Path $PSScriptRoot "..")
$Prefix = if ($env:VOICE_DEPS_PREFIX) { $env:VOICE_DEPS_PREFIX } else { Join-Path $Here ".deps\windows-x86_64" }
$Work   = Join-Path $Prefix "src"
$Stamp  = Join-Path $Prefix ".stamp"
$ScriptHash = (Get-FileHash $PSCommandPath -Algorithm SHA256).Hash.Substring(0, 16).ToLower()
$Want = "opus=$OpusVersion pjproject=$PjVersion script=$ScriptHash sound=native"

if ((Test-Path $Stamp) -and ((Get-Content $Stamp -Raw).Trim() -eq $Want)) {
    Write-Host "voice deps up to date in $Prefix"
    exit 0
}

function Fetch($url, $sha, $dest) {
    Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing
    $got = (Get-FileHash $dest -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $sha) { throw "checksum mismatch for ${url}: got $got" }
}

if (Test-Path $Prefix) { Remove-Item -Recurse -Force $Prefix }
New-Item -ItemType Directory -Force -Path $Work, "$Prefix\lib", "$Prefix\include" | Out-Null

Write-Host "==> opus $OpusVersion"
Fetch "https://downloads.xiph.org/releases/opus/opus-$OpusVersion.tar.gz" $OpusSha256 "$Work\opus.tgz"
New-Item -ItemType Directory -Force "$Work\opus" | Out-Null
tar -xzf "$Work\opus.tgz" -C "$Work\opus" --strip-components=1
cmake -S "$Work\opus" -B "$Work\opus\build" -G "Visual Studio 17 2022" -A x64 `
    -DBUILD_SHARED_LIBS=OFF -DOPUS_BUILD_PROGRAMS=OFF -DOPUS_BUILD_TESTING=OFF `
    -DCMAKE_POLICY_DEFAULT_CMP0091=NEW -DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreadedDLL | Out-Null
if ($LASTEXITCODE) { throw "opus cmake configure failed" }
cmake --build "$Work\opus\build" --config Release | Out-Null
if ($LASTEXITCODE) { throw "opus build failed" }
Copy-Item "$Work\opus\build\Release\opus.lib" "$Prefix\lib\opus.lib"
New-Item -ItemType Directory -Force "$Prefix\include\opus" | Out-Null
Copy-Item "$Work\opus\include\*.h" "$Prefix\include\opus\"

Write-Host "==> pjproject $PjVersion"
Fetch "https://github.com/pjsip/pjproject/archive/refs/tags/$PjVersion.tar.gz" $PjSha256 "$Work\pj.tgz"
New-Item -ItemType Directory -Force "$Work\pj" | Out-Null
tar -xzf "$Work\pj.tgz" -C "$Work\pj" --strip-components=1
@"
/* Parley: audio-only SIP client (keep in sync with build-voice-deps.sh). */
#define PJMEDIA_HAS_VIDEO            0
#define PJMEDIA_HAS_SRTP             1
/* SIP over TLS with Windows' own TLS (Schannel) and certificate store. */
#define PJ_HAS_SSL_SOCK              1
#define PJ_SSL_SOCK_IMP              PJ_SSL_SOCK_IMP_SCHANNEL
#define PJMEDIA_HAS_OPUS_CODEC       1
#define PJMEDIA_HAS_G722_CODEC       1
#define PJMEDIA_HAS_SPEEX_CODEC      0
#define PJMEDIA_HAS_SPEEX_AEC        0
#define PJMEDIA_HAS_GSM_CODEC        0
#define PJMEDIA_HAS_ILBC_CODEC       0
#define PJMEDIA_HAS_G7221_CODEC      0
#define PJMEDIA_HAS_WEBRTC_AEC       1
#define PJMEDIA_HAS_WEBRTC_AEC3      1
#define PJSUA_DEFAULT_CLOCK_RATE     48000
#define PJSUA_DEFAULT_AUDIO_FRAME_PTIME 20
#define PJMEDIA_SND_DEFAULT_REC_LATENCY  60
#define PJMEDIA_SND_DEFAULT_PLAY_LATENCY 60
#define PJSUA_MAX_CALLS              4
/* Outbound-only TLS: no listening socket on the client. */
#define PJSIP_TLS_TRANSPORT_DONT_CREATE_LISTENER 1
#define PJSUA_MAX_ACC                4
"@ | Set-Content -Encoding ascii "$Work\pj\pjlib\include\pj\config_site.h"

# The Visual Studio projects compile opus.c but don't know where Opus's
# headers are; cl.exe also reads extra options from the CL variable.
$env:CL = "/I`"$Prefix\include`""
# Echo cancellation is WebRTC AEC3 (see parley_voice.c). Its library isn't
# part of the solution, and pjmedia only compiles the AEC3 wrapper as C++17
# in Debug|Win32: make that unconditional.
$pjmedia = "$Work\pj\pjmedia\build\pjmedia.vcxproj"
$proj = Get-Content -Raw $pjmedia
$fixed = $proj -replace '<LanguageStandard Condition="[^"]*">stdcpp17</LanguageStandard>', '<LanguageStandard>stdcpp17</LanguageStandard>'
if ($fixed -eq $proj) { throw "pjmedia.vcxproj: AEC3 C++17 setting not found" }
Set-Content -NoNewline -Encoding utf8 $pjmedia $fixed
msbuild "$Work\pj\pjproject-vs14.sln" /t:libpjproject /m /nologo /verbosity:minimal `
    /p:Configuration=Release-Dynamic /p:Platform=x64 `
    /p:PlatformToolset=v143 /p:WindowsTargetPlatformVersion=10.0
if ($LASTEXITCODE) { throw "pjproject build failed" }
msbuild "$Work\pj\third_party\build\webrtc_aec3\libwebrtc_aec3.vcxproj" /m /nologo /verbosity:minimal `
    /p:Configuration=Release-Dynamic /p:Platform=x64 `
    /p:PlatformToolset=v143 /p:WindowsTargetPlatformVersion=10.0
if ($LASTEXITCODE) { throw "webrtc AEC3 build failed" }
Remove-Item Env:\CL

$lib = Get-ChildItem -Path "$Work\pj\lib" -Filter "libpjproject-*Release-Dynamic.lib" | Select-Object -First 1
if (-not $lib) { throw "libpjproject library not found" }
Copy-Item $lib.FullName "$Prefix\lib\pjproject.lib"
$aec3 = Get-ChildItem -Path "$Work\pj\third_party\lib" -Filter "libwebrtc_aec3-*Release-Dynamic.lib" | Select-Object -First 1
if (-not $aec3) { throw "webrtc AEC3 library not found" }
Copy-Item $aec3.FullName "$Prefix\lib\webrtc_aec3.lib"
foreach ($dir in "pjlib", "pjlib-util", "pjnath", "pjmedia", "pjsip") {
    Copy-Item -Recurse -Force "$Work\pj\$dir\include\*" "$Prefix\include\"
}

Set-Content -Path $Stamp -Value $Want
Write-Host "voice deps installed in $Prefix"
