package installertest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsPathPreservesExpandableEntries(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires native Windows registry")
	}
	shell := os.Getenv("TORANA_TEST_POWERSHELL")
	if shell == "" {
		shell = "pwsh"
	}
	data, err := os.ReadFile(filepath.Join("..", "install.ps1"))
	must(t, err)
	script := string(data)
	start := strings.Index(script, "$environmentKey =")
	end := strings.Index(script, "if (($env:PATH -split ';') -inotcontains")
	if start < 0 || end <= start {
		t.Fatal("missing shipped registry PATH update")
	}
	// Execute the shipped code against an isolated key, never the user's PATH.
	block := strings.Replace(script[start:end], "CreateSubKey('Environment')", "CreateSubKey($testKey)", 1)
	program := `$ErrorActionPreference = 'Stop'
$testKey = 'Software\ToranaInstallerTest\' + [Guid]::NewGuid().ToString('N')
$InstallDir = 'C:\ToranaTest\bin'
$original = '%USERPROFILE%\custom-bin;%LOCALAPPDATA%\other-bin'
$key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($testKey)
try {
  $key.SetValue('Path', $original, [Microsoft.Win32.RegistryValueKind]::ExpandString)
  $update = {
` + block + `
  }
  & $update
  & $update
  $raw = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
  if ($raw -cne "$original;$InstallDir") { throw 'Existing variables changed or install entry duplicated' }
  if ($key.GetValueKind('Path') -ne [Microsoft.Win32.RegistryValueKind]::ExpandString) { throw 'Expandable registry type lost' }
} finally {
  $key.Dispose()
  [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($testKey)
}
`
	file := filepath.Join(t.TempDir(), "check-path.ps1")
	must(t, os.WriteFile(file, []byte(program), 0o600))
	output, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-File", file).CombinedOutput()
	if err != nil {
		t.Fatalf("native PATH regression: %v\n%s", err, output)
	}
}
