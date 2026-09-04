param([Parameter(Mandatory=$true)][string]$Workspace)
$ErrorActionPreference='Stop'
. "$PSScriptRoot\toolchain.ps1"
$env:MSBUILDDISABLENODEREUSE='1'
if(-not $Workspace.Contains('mira-openssh-windows.')){throw 'Use an isolated Windows build workspace'}
$cmake=$MiraToolchain.CMake
$deps=Join-Path $Workspace 'deps';$prefix=Join-Path $deps 'install'
function Run-CMake([string[]]$Arguments){
    $quoted=$Arguments|ForEach-Object {'"'+$_+'"'}
    # Wait for CMake itself, not compiler telemetry helpers that may outlive it.
    Invoke-MiraBuildProgram $cmake $quoted
}
function Build-Dep([string]$Name,[string[]]$Options){
    $build=Join-Path $deps ($Name+'-build')
    Run-CMake (@('-S',(Join-Path $deps $Name),'-B',$build,'-G','Visual Studio 17 2022','-A','x64',('-DCMAKE_GENERATOR_INSTANCE='+$MiraToolchain.BuildTools),('-DCMAKE_SYSTEM_VERSION='+$MiraToolchain.SdkVersion),('-DCMAKE_INSTALL_PREFIX='+$prefix),'-DCMAKE_POLICY_DEFAULT_CMP0091=NEW','-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded','-DCMAKE_C_FLAGS=/utf-8','-DBUILD_SHARED_LIBS=OFF')+$Options)
    Run-CMake @('--build',$build,'--config','Release','--parallel','8')
    Run-CMake @('--install',$build,'--config','Release')
}
Build-Dep 'libressl-4.2.0' @('-DLIBRESSL_APPS=OFF','-DLIBRESSL_TESTS=OFF','-DUSE_STATIC_MSVC_RUNTIMES=ON')
Build-Dep 'zlib-1.3.2' @('-DZLIB_BUILD_TESTING=OFF','-DZLIB_BUILD_SHARED=OFF','-DZLIB_BUILD_STATIC=ON')
Build-Dep 'libcbor-0.14.0' @('-DWITH_EXAMPLES=OFF','-DWITH_TESTS=OFF','-DSANITIZE=OFF','-DCMAKE_INTERPROCEDURAL_OPTIMIZATION_RELEASE=OFF')
$include=Join-Path $prefix 'include';$lib=Join-Path $prefix 'lib'
Build-Dep 'libfido2-1.16.0' @('-DBUILD_TESTS=OFF','-DBUILD_EXAMPLES=OFF','-DBUILD_TOOLS=OFF','-DBUILD_MANPAGES=OFF','-DBUILD_STATIC_LIBS=ON',('-DCBOR_INCLUDE_DIRS='+$include),('-DCBOR_LIBRARY_DIRS='+$lib),('-DCRYPTO_INCLUDE_DIRS='+$include),('-DCRYPTO_LIBRARY_DIRS='+$lib),('-DZLIB_INCLUDE_DIRS='+$include),('-DZLIB_LIBRARY_DIRS='+$lib),'-DZLIB_LIBRARIES=zs','-DCRYPTO_LIBRARIES=crypto','-DCBOR_LIBRARIES=cbor')
