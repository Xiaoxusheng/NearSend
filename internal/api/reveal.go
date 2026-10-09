package api

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// revealInFileManager 在系统文件管理器中定位文件。
//
// 仅在确认请求来自本机回环地址后调用。explorer 即使成功也常返回非零退出码，
// 因此这里不把退出码当作失败依据，只要能启动即视为成功。
func revealInFileManager(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", "/select,"+abs)
	case "darwin":
		cmd = exec.Command("open", "-R", abs)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(abs))
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不等待进程结束：文件管理器是长期驻留进程。
	go func() { _ = cmd.Wait() }()
	return nil
}
