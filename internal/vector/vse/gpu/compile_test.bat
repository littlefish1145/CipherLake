@echo off
REM ============================================================
REM compile_test.bat - Compile kernels.cu and run tests
REM
REM Usage:
REM   compile_test.bat           (auto-detect arch)
REM   compile_test.bat sm_80     (specify arch)
REM
REM Common arch:
REM   sm_75  = Turing   (RTX 20xx, T4)
REM   sm_80  = Ampere   (RTX 30xx, A100)
REM   sm_86  = Ampere   (RTX 30xx refresh)
REM   sm_89  = Ada      (RTX 40xx)
REM   sm_90  = Hopper   (H100)
REM   sm_100 = Blackwell (RTX 50xx)
REM   sm_120 = Blackwell (future)
REM ============================================================

setlocal enabledelayedexpansion

set "ARCH=%~1"

REM --- Auto-detect MSVC environment ---
REM Try vswhere first (installed with VS)
set "VSWHERE=%ProgramFiles(x86)%\Microsoft Visual Studio\Installer\vswhere.exe"
if not exist "%VSWHERE%" set "VSWHERE=%ProgramFiles%\Microsoft Visual Studio\Installer\vswhere.exe"

if exist "%VSWHERE%" (
    for /f "usebackq tokens=*" %%i in (`"%VSWHERE%" -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools -property installationPath`) do set "VSDIR=%%i"
    if defined VSDIR (
        call "%VSDIR%\VC\Auxiliary\Build\vcvars64.bat" >nul 2>&1
        if errorlevel 1 (
            echo [WARN] vcvars64.bat failed, relying on current env.
        ) else (
            echo [OK] MSVC environment loaded from %VSDIR%
        )
    )
)

REM --- Check nvcc ---
where nvcc >nul 2>&1
if errorlevel 1 (
    REM Try common CUDA install paths
    for %%V in (13.2 13.1 13.0 12.6 12.5 12.4 12.3 12.2 12.1 12.0) do (
        if exist "C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v%%V\bin\nvcc.exe" (
            set "PATH=C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v%%V\bin;%PATH%"
            echo [OK] Found CUDA v%%V
            goto :nvcc_found
        )
    )
    echo [ERROR] nvcc not found in PATH or standard locations.
    echo.
    echo Please install CUDA Toolkit or add to PATH:
    echo   set PATH=%%PATH%%;"C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v13.2\bin"
    exit /b 1
)
:nvcc_found

REM --- Auto-detect GPU arch if not specified ---
if "%ARCH%"=="" (
    echo Detecting GPU architecture...
    REM Use nvidia-smi to query GPU, then map to arch
    for /f "tokens=*" %%g in ('nvidia-smi --query-gpu=compute_cap ^--format=csv,noheader 2^>nul') do set "CC=%%g"
    if defined CC (
        set "CC=!CC:.=!"
        set "ARCH=sm_!CC!"
        echo [OK] Detected compute capability !CC!, using -arch=sm_!CC!
    ) else (
        set "ARCH=sm_75"
        echo [WARN] Could not detect GPU, defaulting to sm_75
    )
)

echo ============================================================
echo  CUDA Kernel Compile and Test
echo  Architecture: %ARCH%
echo ============================================================

REM --- Common nvcc options ---
set "NVCC_OPTS=-arch=%ARCH% --expt-relaxed-constexpr -std=c++14 -Xcompiler="/Zc:__cplusplus /EHsc""

echo.
echo [1/4] Compiling kernels.cu to PTX (for Go runtime loading)...
nvcc -ptx %NVCC_OPTS% kernels.cu -o kernels.ptx
if errorlevel 1 (
    echo [FAIL] PTX compilation failed.
    exit /b 1
)
echo [OK] kernels.ptx generated.

echo.
echo [2/4] Compiling test_kernels.cu...
nvcc %NVCC_OPTS% test_kernels.cu kernels.cu -o test_kernels.exe
if errorlevel 1 (
    echo [FAIL] Test compilation failed.
    exit /b 1
)
echo [OK] test_kernels.exe generated.

echo.
echo [3/4] Running tests...
echo ------------------------------------------------------------
test_kernels.exe
set "TEST_EXIT=%errorlevel%"
echo ------------------------------------------------------------

echo.
if "%TEST_EXIT%"=="0" (
    echo [4/4] All tests PASSED!
) else (
    echo [4/4] Some tests FAILED, exit code %TEST_EXIT%
)

echo.
echo PTX file: kernels.ptx
echo Copy kernels.ptx to the Go binary working directory for runtime loading.

exit /b %TEST_EXIT%
