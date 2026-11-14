@echo off
rem Double-click this to remove the Cheflink print agent from this PC.
setlocal
if not exist "%~dp0install.ps1" goto :notextracted
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" -Uninstall %*
set result=%errorlevel%
if not "%result%"=="0" pause
exit /b %result%

:notextracted
echo The installer's files are not together.
echo Extract the whole zip first (right-click it, "Extract All"), then run
echo uninstall.cmd from the extracted folder.
pause
exit /b 1
