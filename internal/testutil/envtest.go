//go:build integration

package testutil

import (
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// StopEnv stops env. On Windows, env.Stop() cannot signal its children and
// leaves etcd and kube-apiserver running, so they are killed by parent PID -
// never by image name, which would also kill another suite's apiserver on
// the same machine and look like a flake over there.
func StopEnv(env *envtest.Environment) {
	_ = env.Stop()
	if goruntime.GOOS != "windows" {
		return
	}
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process -Filter "ParentProcessId=%d" | `+
			`Where-Object { $_.Name -in 'etcd.exe','kube-apiserver.exe' } | `+
			`ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		os.Getpid())
	_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}
