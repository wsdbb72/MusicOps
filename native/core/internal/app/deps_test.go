package app

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestApplicationModuleDoesNotImportPython(t *testing.T) {
	root := filepath.Join("..", "..")
	command := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/wsdbb72/MusicOps/native/core/cmd/musicops")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	for _, line := range strings.Split(string(output), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "python") || strings.Contains(lower, "pyqt") || strings.Contains(lower, "cgo") && strings.Contains(lower, "python") {
			t.Fatal(line)
		}
	}
	if runtime.Compiler != "gc" {
		t.Fatal(runtime.Compiler)
	}
}
