@echo off
setlocal enabledelayedexpansion
rem ---------------------------------------------------------------------------
rem  Serve the built image locally so its archives can be read in a browser.
rem  Nothing is copied: kiwix-serve reads the .zim files in place.
rem
rem  Usage:  BROWSE.cmd [image-dir] [port]
rem  Default image: %SURVIVAL_IMAGE%, else image\sku1-sell next to this script.
rem ---------------------------------------------------------------------------

set "ROOT=%~dp0"
set "IMG=%~1"
if "%IMG%"=="" set "IMG=%SURVIVAL_IMAGE%"
if "%IMG%"=="" set "IMG=%ROOT%image\sku1-sell"
set "PORT=%~2"
if "%PORT%"=="" set "PORT=8081"

if not exist "%IMG%\bin\kiwix\kiwix-serve.exe" (
  echo kiwix-serve.exe not found under %IMG%\bin\kiwix
  echo Build the image first:  bin\survival-drive.exe build -lock ... -out "%IMG%"
  pause
  exit /b 1
)

rem kiwix-serve takes explicit file paths; a wildcard is not expanded for it.
set "ZIMS="
for %%f in ("%IMG%\archives\*.zim") do set "ZIMS=!ZIMS! "%%~f""

if "%ZIMS%"=="" (
  echo No .zim files in %IMG%\archives
  pause
  exit /b 1
)

echo Serving %IMG%\archives on http://localhost:%PORT%
echo Ctrl-C to stop.
start "" /b cmd /c "timeout /t 2 >nul & start "" http://localhost:%PORT%"

pushd "%IMG%\bin\kiwix"
kiwix-serve.exe --port=%PORT% !ZIMS!
popd
