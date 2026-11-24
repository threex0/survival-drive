@echo off
setlocal enabledelayedexpansion
rem ---------------------------------------------------------------------------
rem  M1: download every file in a lock into the staging cache.
rem
rem  Safe to re-run: finished files are reused and abandoned downloads resume
rem  from the last completed piece. Ctrl-C is fine; nothing is placed in the
rem  cache until it has been hashed and verified.
rem
rem  Set SURVIVAL_CACHE to keep the ~90 GiB cache on a large drive.
rem  Set SURVIVAL_LOG to choose where the throughput CSV goes.
rem
rem  Usage:  FETCH_SKU1.cmd            (interactive)
rem          set SURVIVAL_NO_PAUSE=1  for unattended runs
rem ---------------------------------------------------------------------------

set "ROOT=%~dp0"
set "EXE=%ROOT%bin\survival-drive.exe"
if "%SURVIVAL_CACHE%"=="" (set "CACHE=%ROOT%cache") else (set "CACHE=%SURVIVAL_CACHE%")
if "%SURVIVAL_LOG%"=="" (set "LOGDIR=%ROOT%logs") else (set "LOGDIR=%SURVIVAL_LOG%")
set "LOCK=%CACHE%\.ledger\sku1-sell.lock.json"

if not exist "%EXE%" (
  echo Building survival-drive...
  pushd "%ROOT%"
  go build -o "%EXE%" ./cmd/survival-drive
  if errorlevel 1 (
    echo BUILD FAILED
    popd
    if not "%SURVIVAL_NO_PAUSE%"=="1" pause
    exit /b 1
  )
  popd
)

if not exist "%LOCK%" (
  echo Lock file missing: %LOCK%
  echo Run RESOLVE.cmd first.
  if not "%SURVIVAL_NO_PAUSE%"=="1" pause
  exit /b 1
)

"%EXE%" fetch -lock "%LOCK%" -cache "%CACHE%" -log "%LOGDIR%"
if errorlevel 1 (
  echo.
  echo FETCH FAILED - re-run this script to resume.
  if not "%SURVIVAL_NO_PAUSE%"=="1" pause
  exit /b 1
)

echo.
echo All files cached. Next: build the image (M2).
if not "%SURVIVAL_NO_PAUSE%"=="1" pause
exit /b 0
