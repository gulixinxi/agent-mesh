// Package filelog 把进程的标准输出接管到日志文件，并按大小轮转。
//
// 为什么必须做这件事：Windows 服务没有交互桌面，stdout/stderr 无处可去，
// 所有 fmt.Println 直接丢进黑洞——出问题时一点线索都没有。
// 而服务端/客户端里已经有几十处打印，逐个改成结构化日志成本太高且不必要，
// 所以这里用 os.Pipe 接管 os.Stdout / os.Stderr：业务代码一行都不用改。
package filelog

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Options 是日志落盘的参数。
type Options struct {
	// Dir 是日志目录，必须可写。
	Dir string
	// Name 是日志文件基名，如 "agent-mesh-server"。
	Name string
	// MaxBytes 是单文件上限，超过即轮转。默认 5 MiB。
	MaxBytes int64
	// Keep 是保留的历史文件份数（不含当前文件）。默认 3。
	Keep int
}

// Handle 是一次接管的句柄。
type Handle struct {
	file     *os.File
	pipeW    *os.File
	pipeR    *os.File
	done     chan struct{}
	mu       sync.Mutex
	dir      string
	name     string
	maxBytes int64
	keep     int
	size     int64
}

// Redirect 接管 os.Stdout 与 os.Stderr，返回句柄；调用方应在退出前 Close。
//
// 注意：本函数会替换包级变量 os.Stdout / os.Stderr，
// 凡是在此之前已经把 os.Stdout 快照下来的东西（gin.DefaultWriter、log 包的默认 logger）
// 都不会自动跟随，需要调用方显式重设。
func Redirect(opts Options) (*Handle, error) {
	if opts.Name == "" {
		opts.Name = "agent-mesh"
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 5 << 20
	}
	if opts.Keep <= 0 {
		opts.Keep = 3
	}
	if opts.Dir == "" {
		return nil, fmt.Errorf("日志目录不能为空")
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}

	h := &Handle{
		done:     make(chan struct{}),
		dir:      opts.Dir,
		name:     opts.Name,
		maxBytes: opts.MaxBytes,
		keep:     opts.Keep,
	}

	f, err := os.OpenFile(h.current(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件失败: %w", err)
	}
	if stat, err := f.Stat(); err == nil {
		h.size = stat.Size()
	}
	h.file = f

	r, w, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("创建日志管道失败: %w", err)
	}
	h.pipeR, h.pipeW = r, w

	os.Stdout = w
	os.Stderr = w

	go h.pump()
	return h, nil
}

// Path 返回当前日志文件的绝对路径。
func (h *Handle) Path() string { return h.current() }

func (h *Handle) current() string {
	return filepath.Join(h.dir, h.name+".log")
}

// pump 持续把管道里的行加时间戳写进文件，并在超过上限时轮转。
func (h *Handle) pump() {
	defer close(h.done)

	scanner := bufio.NewScanner(h.pipeR)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 单行最长 1 MiB
	for scanner.Scan() {
		h.writeLine(scanner.Text())
	}
}

func (h *Handle) writeLine(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	stamp := time.Now().Format("2006-01-02 15:04:05")
	n, err := fmt.Fprintf(h.file, "%s %s\n", stamp, line)
	if err != nil {
		return
	}
	h.size += int64(n)

	if h.size >= h.maxBytes {
		_ = h.rotate()
	}
}

// rotate 把当前日志改名为 .1.log，更旧的依次后移，超出 Keep 的删除。
func (h *Handle) rotate() error {
	if err := h.file.Close(); err != nil {
		return err
	}

	// 从最旧的一份开始往后挪：.N -> .N+1，超出保留份数的丢弃。
	for i := h.keep; i >= 1; i-- {
		src := filepath.Join(h.dir, fmt.Sprintf("%s.%d.log", h.name, i))
		if i >= h.keep {
			_ = os.Remove(src)
			continue
		}
		_ = os.Rename(src, filepath.Join(h.dir, fmt.Sprintf("%s.%d.log", h.name, i+1)))
	}
	if err := os.Rename(h.current(), filepath.Join(h.dir, h.name+".1.log")); err != nil {
		// 改名失败也要把文件重新打开，否则后续日志全丢。
		f, err2 := os.OpenFile(h.current(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err2 != nil {
			return err2
		}
		h.file = f
		h.size = 0
		return err
	}

	f, err := os.OpenFile(h.current(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	h.file = f
	h.size = 0
	_ = h.trimOld()
	return nil
}

// trimOld 清理超出保留份数的历史文件。
func (h *Handle) trimOld() error {
	pattern := filepath.Join(h.dir, h.name+".*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	if len(matches) <= h.keep {
		return nil
	}
	sort.Strings(matches)
	for _, m := range matches[:len(matches)-h.keep] {
		_ = os.Remove(m)
	}
	return nil
}

// Close 结束接管：关闭管道写端、等泵排空、关闭文件。
func (h *Handle) Close() error {
	_ = h.pipeW.Close()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		err := h.file.Close()
		h.file = nil
		return err
	}
	return nil
}
