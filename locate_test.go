package lorca

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestLocate(t *testing.T) {
	if exe := LocateChrome(); exe == "" {
		t.Fatal()
	} else {
		t.Log(exe)
		args := []string{"--version", "--headless"}
		if runtime.GOOS == "windows" {
			args = []string{
				"-command",
				"&{(Get-Item " + exe + ").VersionInfo.ProductVersion}",
			}
			// https://stackoverflow.com/a/27912928/8608146
			// https://stackoverflow.com/a/57618035/8608146
			// executable now needs to powershell.exe
			exe = "powershell.exe"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		var b bytes.Buffer
		cmd.Stdout = &b
		if err := cmd.Run(); err == nil {
			t.Log(b.String())
			cmd.Process.Kill()
		} else {
			t.Fatal(err)
		}
	}
}
