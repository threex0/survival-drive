@echo off
setlocal enabledelayedexpansion
rem ---------------------------------------------------------------------------
rem  M0: resolve every SKU/variant into a pinned lock file. Downloads nothing.
rem  Requires Go on PATH. Run from anywhere; paths are relative to this script.
rem
rem  Set SURVIVAL_CACHE to keep the cache on a large drive.
rem ---------------------------------------------------------------------------

set "ROOT=%~dp0"
set "EXE=%ROOT%bin\survival-drive.exe"
if "%SURVIVAL_CACHE%"=="" (set "CACHE=%ROOT%cache") else (set "CACHE=%SURVIVAL_CACHE%")
set "LEDGER=%CACHE%\.ledger"

if not exist "%ROOT%bin" mkdir "%ROOT%bin"

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

if not exist "%LEDGER%" mkdir "%LEDGER%"

echo.
echo === sku1 / sell ===
"%EXE%" resolve -profile "%ROOT%profiles\sku1.json" -variant sell -out "%LEDGER%\sku1-sell.lock.json"
if errorlevel 1 goto :failed

echo.
echo === sku2 / sell ===
"%EXE%" resolve -profile "%ROOT%profiles\sku2.json" -variant sell -out "%LEDGER%\sku2-sell.lock.json"
if errorlevel 1 goto :failed

echo.
echo === sku2 / full ===
"%EXE%" resolve -profile "%ROOT%profiles\sku2.json" -variant full -out "%LEDGER%\sku2-full.lock.json"
if errorlevel 1 goto :failed

echo.
echo Done. Lock files are in %LEDGER%
if not "%SURVIVAL_NO_PAUSE%"=="1" pause
exit /b 0

:failed
echo.
echo RESOLVE FAILED
if not "%SURVIVAL_NO_PAUSE%"=="1" pause
exit /b 1
