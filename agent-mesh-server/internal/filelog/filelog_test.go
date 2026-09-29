package filelog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedirectWritesToFile(t *testing.T) {
	dir := t.TempDir()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	h, err := Redirect(Options{Dir: dir, Name: "unit", MaxBytes: 4096, Keep: 2})
	if err != nil {
		t.Fatalf("接管日志失败: %v", err)
	}

	fmt.Println("第一行日志")
	fmt.Fprintln(os.Stderr, "通过 stderr 写入")
	time.Sleep(300 * time.Millisecond)

	if err := h.Close(); err != nil {
		t.Fatalf("关闭句柄失败: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "unit.log"))
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "第一行日志") {
		t.Errorf("日志缺少 stdout 内容: %q", content)
	}
	if !strings.Contains(content, "通过 stderr 写入") {
		t.Errorf("日志缺少 stderr 内容: %q", content)
	}
	// 每行都应带时间戳前缀，否则事后没法排障。
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		if len(line) < 19 || line[4] != '-' || line[7] != '-' {
			t.Errorf("日志行缺少时间戳前缀: %q", line)
		}
	}
}

func TestRotateKeepsLimitedFiles(t *testing.T) {
	dir := t.TempDir()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	// 上限压到 200 字节，逼出多次轮转。
	h, err := Redirect(Options{Dir: dir, Name: "rot", MaxBytes: 200, Keep: 2})
	if err != nil {
		t.Fatalf("接管日志失败: %v", err)
	}

	for i := 0; i < 60; i++ {
		fmt.Printf("这是第 %d 行日志，内容足够长以便快速撑爆单文件大小上限\n", i)
	}
	time.Sleep(500 * time.Millisecond)
	if err := h.Close(); err != nil {
		t.Fatalf("关闭句柄失败: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "rot.*.log"))
	if len(matches) > 2 {
		t.Errorf("历史日志保留 %d 份，超过 Keep=2 上限", len(matches))
	}
	if len(matches) == 0 {
		t.Fatal("未产生任何轮转文件，说明轮转逻辑没被触发")
	}

	// 当前文件必须存在且能继续写入，否则轮转后日志就断了。
	if _, err := os.Stat(filepath.Join(dir, "rot.log")); err != nil {
		t.Errorf("轮转后当前日志文件不存在: %v", err)
	}
}

func TestRejectsEmptyDir(t *testing.T) {
	if _, err := Redirect(Options{Dir: "", Name: "x"}); err == nil {
		t.Error("空目录应当报错，否则日志无处可写")
	}
}
