package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// launchExec runs an external launcher command and returns once it has
// started; it does not wait for the installer to finish. Tests replace it
// to assert the constructed command without opening a real installer; the
// download and verification themselves are never stubbed.
var launchExec = func(ctx context.Context, name string, args []string, dir string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	applyLaunchAttr(cmd)
	return cmd.Start()
}

// handoff is the payload the Windows helper script reads: which process to
// wait for, which installer to verify and run, and where to install.
type handoff struct {
	PID       int    `json:"pid"`
	Installer string `json:"installer"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Directory string `json:"directory"`
}

// Launch verifies the staged installer and hands it to the platform
// installer. Windows writes the PowerShell handoff (a port of Python's
// WINDOWS_HANDOFF) plus its JSON payload and starts it hidden; macOS opens
// the .pkg detached. Linux has no auto-update installer: Docker images
// update instead. installDir defaults to the directory holding the current
// executable; a waitPID of 0 means the current process.
func (u *Updater) Launch(s *Staged, waitPID int, installDir string) error {
	if err := Verify(s); err != nil {
		return err
	}
	system := u.system()
	switch system {
	case "win32":
		return u.launchWindows(s, waitPID, installDir)
	case "darwin":
		return launchExec(context.Background(), "/usr/bin/open", []string{s.Path}, "")
	default:
		return errors.New("当前系统没有自动更新安装器")
	}
}

func (u *Updater) launchWindows(s *Staged, waitPID int, installDir string) error {
	if waitPID <= 0 {
		waitPID = os.Getpid()
	}
	if installDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(exe)
		if err == nil {
			exe = resolved
		}
		installDir = filepath.Dir(exe)
	}
	dir := filepath.Dir(s.Path)
	helper := filepath.Join(dir, "apply-update.ps1")
	payloadPath := filepath.Join(dir, "handoff-"+randomSuffix()+".json")
	if err := rejectReparseChain(helper, dir); err != nil {
		return err
	}
	if err := rejectReparseChain(payloadPath, dir); err != nil {
		return err
	}
	if err := os.WriteFile(helper, []byte(windowsHandoff), 0600); err != nil {
		return err
	}
	payload, err := json.Marshal(handoff{
		PID:       waitPID,
		Installer: s.Path,
		SHA256:    s.SHA256,
		Size:      s.Size,
		Directory: installDir,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(payloadPath, payload, 0600); err != nil {
		return err
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	powershell := filepath.Join(systemRoot, `System32\WindowsPowerShell\v1.0\powershell.exe`)
	// Hidden window, no profile, bypass policy for this one script file:
	// the same invocation Python's launch_installer used.
	return launchExec(context.Background(), powershell, []string{
		"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden",
		"-ExecutionPolicy", "Bypass", "-File", helper, "-Payload", payloadPath,
	}, dir)
}

func randomSuffix() string {
	// 8 random bytes as hex; crypto/rand, no new dependency.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return strconv.FormatInt(int64(os.Getpid()), 16)
	}
	return hex.EncodeToString(nonce)
}

// windowsHandoff is a verbatim port of Python's WINDOWS_HANDOFF: wait for
// the running app to exit (≤600s), confirm MusicOps.exe and
// MusicOps-worker.exe are not locked (≤60s), re-verify the installer size
// and SHA-256, then run it with /DIR="<install directory>". Any failure
// writes handoff-error.txt next to the payload and exits 1.
const windowsHandoff = `param([Parameter(Mandatory=$true)][string]$Payload)
$ErrorActionPreference = 'Stop'
try {
    $updateInfo = Get-Content -LiteralPath $Payload -Raw -Encoding UTF8 | ConvertFrom-Json
    $exitDeadline = [DateTime]::UtcNow.AddSeconds(600)
    while (Get-Process -Id $updateInfo.pid -ErrorAction SilentlyContinue) {
        if ([DateTime]::UtcNow -gt $exitDeadline) { throw 'MusicOps did not exit' }
        Start-Sleep -Milliseconds 200
    }
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $free = $true
        foreach ($name in @('MusicOps.exe','MusicOps-worker.exe')) {
            $candidate = Join-Path $updateInfo.directory $name
            if (Test-Path -LiteralPath $candidate) {
                try { $stream = [IO.File]::Open($candidate,'Open','Read','None'); $stream.Dispose() }
                catch { $free = $false }
            }
        }
        if (-not $free) { Start-Sleep -Milliseconds 300 }
    } until ($free -or [DateTime]::UtcNow -gt $deadline)
    if (-not $free) { throw 'MusicOps is still in use' }
    $installerInfo = Get-Item -LiteralPath $updateInfo.installer
    if (($installerInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Invalid installer path' }
    $hashAlgorithm = [Security.Cryptography.SHA256]::Create()
    $installerStream = [IO.File]::OpenRead($updateInfo.installer)
    try { $actualHash = [BitConverter]::ToString($hashAlgorithm.ComputeHash($installerStream)).Replace('-','').ToLowerInvariant() }
    finally { $installerStream.Dispose(); $hashAlgorithm.Dispose() }
    if ($installerInfo.Length -ne $updateInfo.size -or $actualHash -ne $updateInfo.sha256) { throw 'Update checksum mismatch' }
    Start-Process -FilePath $updateInfo.installer -ArgumentList @('/DIR="' + $updateInfo.directory + '"')
} catch {
    ('Update handoff failed at line ' + $_.InvocationInfo.ScriptLineNumber + ': ' + $_.Exception.Message) | Set-Content -LiteralPath (Join-Path (Split-Path -LiteralPath $Payload) 'handoff-error.txt') -Encoding UTF8
    exit 1
}
`
