package ec2

import (
	"os"
	"testing"
)

// TestMain runs every PowerShell this package's tests start without .NET's
// ReadyToRun code, which is where pwsh 7.6.9 on .NET 10.0.14 crashes: a segfault,
// or an abort on an assembly name it read corrupted. Measured on 2026-11-10
// (#791): 7 crashes in 8000 runs with it and none in 8000 without, on billet's
// runners, and GitHub's own runner crashes the same way; pwsh then starts in
// about 3.5 times the CPU. Every test here that starts pwsh hands it this
// process's environment.
func TestMain(m *testing.M) {
	if err := os.Setenv("DOTNET_ReadyToRun", "0"); err != nil {
		panic(err)
	}

	m.Run()
}
