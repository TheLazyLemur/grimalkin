package clipboard

import (
	"os/exec"
	"runtime"
	"strings"
)

type Clipboard struct{}

func (Clipboard) Copy(text string) {
	if runtime.GOOS != "darwin" {
		return
	}
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}
