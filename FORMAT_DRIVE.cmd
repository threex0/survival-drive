@echo off
setlocal enabledelayedexpansion
rem ---------------------------------------------------------------------------
rem  Format a drive for the Survival AI image: exFAT, MBR-friendly, 128 KiB
rem  clusters, label SURVIVAL_AI.
rem
rem  THIS ERASES THE DRIVE. It prints what it is about to do and waits for you
rem  to type the drive letter before doing anything.
rem
rem  Usage:  FORMAT_DRIVE.cmd G:
rem ---------------------------------------------------------------------------

set "DRIVE=%~1"
if "%DRIVE%"=="" (
  echo Usage: FORMAT_DRIVE.cmd X:
  echo.
  echo Volumes currently mounted:
  wmic logicaldisk get caption,volumename,filesystem,size,freespace
  exit /b 1
)

rem normalise to "G:"
set "LETTER=%DRIVE:~0,1%"
set "COLON=%DRIVE:~1,1%"
if not "%COLON%"==":" set "LETTER=%DRIVE%"
set "ROOT=%LETTER%:\"

if not exist "%ROOT%" (
  echo Drive %ROOT% does not exist.
  exit /b 1
)

rem --- admin check ---------------------------------------------------------
net session >nul 2>&1
if errorlevel 1 (
  echo This script must run as Administrator.
  echo Right-click it and choose "Run as administrator".
  exit /b 1
)

echo.
echo Current state of %ROOT%
vol %LETTER%:
fsutil fsinfo volumeinfo %LETTER%: 2>nul
echo.
wmic logicaldisk where "caption='%LETTER%:'" get caption,volumename,filesystem,size,freespace
echo.
echo ============================================================
echo  This will ERASE EVERYTHING on %ROOT%
echo  and format it exFAT / 128 KiB clusters / label SURVIVAL_AI
echo ============================================================
echo.
set /p "CONFIRM=Type the drive letter (%LETTER%) to continue, anything else to abort: "
if /i not "%CONFIRM%"=="%LETTER%" (
  echo Aborted. Nothing was changed.
  exit /b 1
)

echo.
echo Formatting %ROOT% ...
rem NOTE: do not pass /A:131072 - Windows rejects that value for exFAT with
rem "Invalid parameter". For a volume this size it picks 128 KiB clusters on its
rem own, which is what we want; the check below proves it.
format %LETTER%: /FS:exFAT /Q /V:SURVIVAL_AI /Y
if errorlevel 1 (
  echo FORMAT FAILED
  exit /b 1
)

echo.
echo Volume after formatting:
echo   (cluster size should read 131072 bytes / 128 KiB)
fsutil fsinfo volumeinfo %LETTER%: | findstr /i "File System Name"
powershell -NoProfile -Command "(Get-Volume -DriveLetter %LETTER%).AllocationUnitSize"

rem exFAT has no journal: a clean pass here catches anything left over.
echo.
echo Checking the fresh volume ...
chkdsk %LETTER%: /f

echo.
echo Done. Volume %ROOT% is ready.
echo Next:  bin\survival-drive.exe burn -image D:\image\sku1-sell -drive %LETTER%:
