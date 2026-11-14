# Installs, updates or removes the Cheflink print agent on a cashier PC.
#
# Run it through install.cmd (double-click), or from a prompt:
#
#   install.cmd                                         asks for what it needs
#   install.cmd -Server https://example.com -Code 123456
#   install.cmd -Uninstall
#
# Running it again on a PC that already has the agent updates the program and
# keeps the pairing.
#
# Written for Windows PowerShell 5.1, which every Windows 10 and 11 has. Keep
# this file plain ASCII: 5.1 misreads anything else without a byte order mark.

param(
    [string]$Server = "",
    [string]$Code = "",
    [string]$Name = "",
    [switch]$Uninstall,
    # Set when the script restarts itself as administrator, so the new window
    # stays open long enough to be read.
    [switch]$Elevated
)

$ErrorActionPreference = "Stop"

$ServiceName = "CheflinkPrintAgent"
$ExeName = "cheflink-print-agent.exe"
# Program Files, because only administrators can write there: the service
# runs this program as SYSTEM, so nobody else may be able to replace it.
$InstallDir = Join-Path $env:ProgramFiles "CheflinkPrintAgent"
$InstalledExe = Join-Path $InstallDir $ExeName
$ConfigPath = Join-Path $InstallDir "config.json"

function Finish([int]$exitCode) {
    if ($Elevated) {
        Write-Host ""
        Read-Host "Press Enter to close this window" | Out-Null
    }
    exit $exitCode
}

function Fail([string]$message) {
    Write-Host ""
    Write-Host "Not finished: $message" -ForegroundColor Red
    Finish 1
}

# Runs the agent with the given arguments and returns whether it succeeded.
# Out-Host keeps what the agent prints on the screen and out of the function's
# result.
function Invoke-Agent {
    & $InstalledExe @args | Out-Host
    return ($LASTEXITCODE -eq 0)
}

function Test-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Test-ServiceInstalled {
    return $null -ne (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)
}

# Stops the service and waits until it has let go of the program. An agent in
# the middle of a chit finishes it first.
function Stop-AgentService {
    if ((Get-Service -Name $ServiceName).Status -ne "Stopped") {
        Stop-Service -Name $ServiceName -Force
        (Get-Service -Name $ServiceName).WaitForStatus("Stopped", [TimeSpan]::FromSeconds(60))
    }
}

# The service commands and Program Files need an administrator. Start again
# in a window that has the rights; Windows asks the person to allow it.
if (-not (Test-Administrator)) {
    $arguments = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "`"$PSCommandPath`"", "-Elevated")
    if ($Server) { $arguments += @("-Server", "`"$Server`"") }
    if ($Code) { $arguments += @("-Code", "`"$Code`"") }
    if ($Name) { $arguments += @("-Name", "`"$Name`"") }
    if ($Uninstall) { $arguments += "-Uninstall" }
    try {
        Start-Process -FilePath "powershell.exe" -ArgumentList $arguments -Verb RunAs
    } catch {
        Write-Host "This needs an administrator. Allow the Windows prompt, or run install.cmd from an administrator prompt." -ForegroundColor Red
        exit 1
    }
    exit 0
}

try {
    if ($Uninstall) {
        Write-Host "Removing the Cheflink print agent"
        Write-Host ""
        if (Test-ServiceInstalled) {
            Stop-AgentService
            & sc.exe delete $ServiceName | Out-Null
            if ($LASTEXITCODE -ne 0) { Fail "could not remove the service." }
            Write-Host "Removed the service."
        } else {
            Write-Host "The service is not installed."
        }
        if (Test-Path $InstallDir) {
            Remove-Item -Path $InstallDir -Recurse -Force
            Write-Host "Deleted $InstallDir, with the pairing and the log."
        }
        Write-Host ""
        Write-Host "Removed. Also revoke this PC on the venue's page in Cheflink." -ForegroundColor Green
        Finish 0
    }

    $source = Join-Path $PSScriptRoot $ExeName
    if (-not (Test-Path $source)) {
        Fail "$ExeName is not beside this script. Extract the whole zip first, then run install.cmd from the extracted folder."
    }
    $newVersion = (& $source version)
    Write-Host "Cheflink print agent $newVersion"
    Write-Host ""

    $updating = Test-ServiceInstalled
    if ($updating) {
        # A service registered by hand from another folder would keep running
        # its own copy, whatever is copied here.
        $registered = (Get-CimInstance -ClassName Win32_Service -Filter "Name='$ServiceName'").PathName
        if ($registered -notlike "*$InstalledExe*") {
            Fail "the agent is already installed somewhere else: $registered. Remove that one first (run its program with `"uninstall`" from an administrator prompt), then run this again."
        }
        Write-Host "The agent is already installed here. Updating it."
        # The program cannot be replaced while it runs.
        Stop-AgentService
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    # The folder holds the agent's token. Only SYSTEM (the service) and
    # administrators get in; the names are given as SIDs because the group
    # names change with the language of Windows.
    & icacls.exe $InstallDir /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Warning: could not restrict $InstallDir to administrators. Other users of this PC may be able to read the agent's token." -ForegroundColor Yellow
    }
    Copy-Item -Path $source -Destination $InstalledExe -Force
    # A downloaded file carries a mark that makes Windows question it again.
    Unblock-File -Path $InstalledExe -ErrorAction SilentlyContinue
    Write-Host "Copied the program to $InstallDir"

    # Pair, unless this PC is already paired and nobody asked to pair again.
    $paired = Test-Path $ConfigPath
    if ($paired -and -not $Code) {
        Write-Host "Keeping the existing pairing."
    } else {
        $asked = $false
        while ($true) {
            if (-not $Server) { $Server = (Read-Host "Cheflink address the venue signs in at (https://...)").Trim(); $asked = $true }
            if (-not $Code) { $Code = (Read-Host "Pairing code from the venue's page in Cheflink").Trim(); $asked = $true }
            if ($Server -and $Code) {
                $pairArguments = @("pair", "-server", $Server, "-code", $Code)
                if ($Name) { $pairArguments += @("-name", $Name) }
                if (Invoke-Agent @pairArguments) { break }
            }
            # Given on the command line, there is nobody to ask again.
            if (-not $asked) { Fail "pairing failed. Check the address and the code, and run this again." }
            Write-Host ""
            Write-Host "That did not work. Try again, or close this window to give up." -ForegroundColor Yellow
            $Server = ""
            $Code = ""
        }
    }

    if (-not $updating) {
        if (-not (Invoke-Agent install)) { Fail "could not register the Windows service." }
    }
    if (-not (Invoke-Agent start)) { Fail "the service is registered but did not start. Look at $InstallDir\agent.log." }

    Write-Host ""
    Write-Host "Done. The agent is running and starts with Windows." -ForegroundColor Green
    Write-Host ""
    Write-Host "Still to do by hand:"
    Write-Host "  - Switch off sleep and hibernation: the PC has to stay on while the kitchen works."
    Write-Host "  - Check this PC reaches the printer, from an administrator prompt:"
    Write-Host "      `"$InstalledExe`" test-print -printer 192.168.1.50:9100"
    Write-Host "  - The log is $InstallDir\agent.log"
    Finish 0
} catch {
    Fail $_.Exception.Message
}
