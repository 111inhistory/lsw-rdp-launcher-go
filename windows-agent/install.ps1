<# Copy a build and provision/verify the shared agent token without replacing
   running agents or changing login/service policy. #>
[CmdletBinding()]
param(
    [string]$InstallDir = 'C:\Program Files\RdpLauncher',
    [string]$SourceExe,
    [string]$AuthTokenFile = 'C:\ProgramData\rdp-launcher\agent-token',
    [string]$InteractiveUser = $env:USERNAME,
    [string]$TokenSource,
    [switch]$SkipTokenProvision,
    [switch]$VerifyOnly
)
$ErrorActionPreference = 'Stop'

function Get-AllowedTokenSids([string]$User) {
    if ([string]::IsNullOrWhiteSpace($User)) { throw 'InteractiveUser is required.' }
    try {
        $account = New-Object -TypeName System.Security.Principal.NTAccount -ArgumentList $User
        $userSid = $account.Translate([System.Security.Principal.SecurityIdentifier])
    } catch {
        throw "Cannot resolve interactive user '$User': $_"
    }
    return [pscustomobject]@{
        System         = New-Object -TypeName System.Security.Principal.SecurityIdentifier -ArgumentList 'S-1-5-18'
        Administrators = New-Object -TypeName System.Security.Principal.SecurityIdentifier -ArgumentList 'S-1-5-32-544'
        User           = $userSid
    }
}

# Return one token line. One final LF/CRLF is accepted, matching the Rust
# loader. Embedded CR/LF and invalid UTF-8 are rejected; other whitespace
# (spaces, tabs) is preserved as part of the token, not trimmed.
function Get-TokenBytes([byte[]]$Bytes, [string]$Description) {
    if ($null -eq $Bytes) { throw "$Description is empty." }
    if ($Bytes.Length -ge 3 -and $Bytes[0] -eq 0xEF -and $Bytes[1] -eq 0xBB -and $Bytes[2] -eq 0xBF) {
        throw "$Description must be UTF-8 without a BOM."
    }
    $length = $Bytes.Length
    if ($length -gt 0 -and $Bytes[$length - 1] -eq 10) {
        $length--
        if ($length -gt 0 -and $Bytes[$length - 1] -eq 13) { $length-- }
    }
    if ($length -eq 0) { throw "$Description is empty." }
    $token = [byte[]]::new($length)
    for ($i = 0; $i -lt $length; $i++) {
        if ($Bytes[$i] -eq 13 -or $Bytes[$i] -eq 10) {
            throw "$Description must contain one token without CR/LF."
        }
        $token[$i] = $Bytes[$i]
    }
    try {
        $null = [System.Text.UTF8Encoding]::new($false, $true).GetString($token)
    } catch {
        throw "$Description must be valid UTF-8."
    }
    return ,$token
}

function Read-TokenFromPrompt {
    $secure = Read-Host -AsSecureString 'Agent token (input is hidden)'
    if ($null -eq $secure) { throw 'No token was entered.' }
    $bstr = [IntPtr]::Zero
    try {
        $bstr = [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
        $plain = [System.Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
    } finally {
        if ($bstr -ne [IntPtr]::Zero) {
            [System.Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
        }
    }
    $bytes = [System.Text.UTF8Encoding]::new($false).GetBytes($plain)
    return ,(Get-TokenBytes $bytes 'Entered token')
}

function Set-TokenDirectoryAcl([string]$Path, $Sids) {
    $acl = New-Object -TypeName System.Security.AccessControl.DirectorySecurity
    $acl.SetAccessRuleProtection($true, $false)
    $inherit = [System.Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [System.Security.AccessControl.InheritanceFlags]::ObjectInherit
    $none = [System.Security.AccessControl.PropagationFlags]::None
    foreach ($sid in @($Sids.System, $Sids.Administrators)) {
        $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
            $sid, [System.Security.AccessControl.FileSystemRights]::FullControl,
            $inherit, $none, [System.Security.AccessControl.AccessControlType]::Allow)))
    }
    $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
        $Sids.User, [System.Security.AccessControl.FileSystemRights]::ReadAndExecute,
        $inherit, $none, [System.Security.AccessControl.AccessControlType]::Allow)))
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function Set-TokenFileAcl([string]$Path, $Sids) {
    $acl = New-Object -TypeName System.Security.AccessControl.FileSecurity
    $acl.SetAccessRuleProtection($true, $false)
    $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
        $Sids.System, [System.Security.AccessControl.FileSystemRights]::FullControl,
        [System.Security.AccessControl.AccessControlType]::Allow)))
    $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
        $Sids.Administrators, [System.Security.AccessControl.FileSystemRights]::FullControl,
        [System.Security.AccessControl.AccessControlType]::Allow)))
    $acl.AddAccessRule((New-Object -TypeName System.Security.AccessControl.FileSystemAccessRule -ArgumentList @(
        $Sids.User, [System.Security.AccessControl.FileSystemRights]::Read,
        [System.Security.AccessControl.AccessControlType]::Allow)))
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function Assert-OnlyAllowedTrustees([string]$Path, $Sids) {
    $acl = Get-Acl -LiteralPath $Path
    if (-not $acl.AreAccessRulesProtected) {
        throw "$Path inherits ACL entries. Use a dedicated directory and re-run install.ps1."
    }
    $allowed = @{}
    foreach ($sid in @($Sids.System, $Sids.Administrators, $Sids.User)) {
        $allowed[$sid.Value] = $false
    }
    foreach ($rule in $acl.Access) {
        if ($rule.AccessControlType -eq [System.Security.AccessControl.AccessControlType]::Deny) {
            throw "$Path contains an unsupported deny ACE for $($rule.IdentityReference); re-provision the token ACL."
        }
        if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) {
            continue
        }
        $trustee = $rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
        if (-not $allowed.ContainsKey($trustee)) {
            throw "$Path grants access to unexpected trustee $($rule.IdentityReference) ($trustee); reject broad-readable permissions."
        }
        if (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::Read) -eq [System.Security.AccessControl.FileSystemRights]::Read) {
            $allowed[$trustee] = $true
        }
    }
    if (-not $allowed[$Sids.System.Value]) {
        throw "$Path does not grant read access to SYSTEM."
    }
    if (-not $allowed[$Sids.User.Value]) {
        throw "$Path does not grant read access to interactive user $($Sids.User.Value)."
    }
}

function Assert-DedicatedTokenDirectory([string]$Path, [string]$TokenPath) {
    if ([string]::IsNullOrWhiteSpace($Path) -or -not [System.IO.Path]::IsPathRooted($Path)) {
        throw "AuthTokenFile must be an absolute path with a dedicated parent directory: $TokenPath"
    }
    $resolved = [System.IO.Path]::GetFullPath($Path).TrimEnd('\')
    $root = [System.IO.Path]::GetPathRoot($resolved).TrimEnd('\')
    $forbidden = @($root)
    foreach ($known in @($env:ProgramData, $env:ProgramFiles, $env:WINDIR)) {
        if ($known) { $forbidden += [System.IO.Path]::GetFullPath($known).TrimEnd('\') }
    }
    if ($forbidden -contains $resolved) {
        throw "AuthTokenFile must be inside a dedicated directory, not $resolved."
    }
    if (Test-Path -LiteralPath $resolved -PathType Container) {
        $fullToken = [System.IO.Path]::GetFullPath($TokenPath)
        $other = Get-ChildItem -LiteralPath $resolved -Force |
            Where-Object { $_.FullName -ne $fullToken }
        if ($other) {
            throw "Token directory $resolved contains other entries; use a dedicated directory."
        }
    }
}

function Assert-TokenFile([string]$Path, $Sids) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Token file not found: $Path"
    }
    $null = Get-TokenBytes ([System.IO.File]::ReadAllBytes($Path)) "Token file $Path"
    Assert-OnlyAllowedTrustees $Path $Sids
    Write-Host "Verified token file: $Path (SYSTEM + Administrators + $($Sids.User.Value))"
}

$allowed = Get-AllowedTokenSids $InteractiveUser

if ($VerifyOnly) {
    Assert-DedicatedTokenDirectory (Split-Path -Parent $AuthTokenFile) $AuthTokenFile
    Assert-TokenFile $AuthTokenFile $allowed
    Assert-OnlyAllowedTrustees (Split-Path -Parent $AuthTokenFile) $allowed
    Write-Host 'Token provisioning skipped; verification only.'
    return
}

if (-not $SourceExe) {
    Push-Location $PSScriptRoot
    try {
        cargo build --release
        if ($LASTEXITCODE -ne 0) { throw 'cargo build failed' }
    } finally { Pop-Location }
    $SourceExe = Join-Path $PSScriptRoot 'target\release\remoteapp-launcher.exe'
}
$destination = Join-Path $InstallDir 'remoteapp-launcher.exe'
$running = Get-Process remoteapp-launcher -ErrorAction SilentlyContinue |
    Where-Object Path -eq $destination
if ($running) { throw "An agent is running from $destination. Stop that instance explicitly before installing." }
New-Item -ItemType Directory -Force $InstallDir | Out-Null
Copy-Item -LiteralPath $SourceExe -Destination $destination
Write-Host "Installed: $destination"

if (-not $SkipTokenProvision) {
    $tokenDirectory = Split-Path -Parent $AuthTokenFile
    Assert-DedicatedTokenDirectory $tokenDirectory $AuthTokenFile
    New-Item -ItemType Directory -Force -Path $tokenDirectory | Out-Null
    Set-TokenDirectoryAcl $tokenDirectory $allowed

    if ($TokenSource) {
        if (-not (Test-Path -LiteralPath $TokenSource -PathType Leaf)) {
            throw "Token source file not found: $TokenSource"
        }
        $token = Get-TokenBytes ([System.IO.File]::ReadAllBytes($TokenSource)) "Token source $TokenSource"
        [System.IO.File]::WriteAllBytes($AuthTokenFile, $token)
    } elseif (-not (Test-Path -LiteralPath $AuthTokenFile -PathType Leaf)) {
        $token = Read-TokenFromPrompt
        [System.IO.File]::WriteAllBytes($AuthTokenFile, $token)
    }
    Set-TokenFileAcl $AuthTokenFile $allowed
    Assert-TokenFile $AuthTokenFile $allowed
}

Write-Host "Background: remoteapp-launcher.exe backend --bind <guest-IP>:49153 --auth-token-file '$AuthTokenFile' --session-user <Windows-user> --session-host <Linux-host-IP>"
Write-Host 'RemoteApp:  remoteapp-launcher.exe remoteapp-shell (no TCP endpoint, no token argument)'
Write-Host 'Use a controlled network. Tokens authenticate host clients; they do not encrypt TCP.'
Write-Host 'Existing user token-file read permissions are unchanged; only the backend reads the token in protocol 2.'
Write-Host 'Service/startup registration and RemoteApp allow-list changes are not performed.'
