@echo off
rem Double-click this to install or update the Cheflink print agent.
rem It only starts install.ps1, which does the work.
setlocal
if not exist "%~dp0install.ps1" goto :notextracted
if not exist "%~dp0cheflink-print-agent.exe" goto :notextracted
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
set result=%errorlevel%
if not "%result%"=="0" pause
exit /b %result%

:notextracted
echo The installer's files are not together.
echo Extract the whole zip first (right-click it, "Extract All"), then run
echo install.cmd from the extracted folder.
pause
exit /b 1
