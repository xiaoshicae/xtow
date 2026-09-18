package xlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiaoshicae/xtow/xerror"
)

// 日志文件名的时间后缀格式，按轮转周期选择合适的粒度
// 周期小于一天时需要更细的粒度，否则不同周期会生成同名文件而无法真正轮转
const (
	rotateLayoutDay    = "20060102"
	rotateLayoutHour   = "2006010215"
	rotateLayoutMinute = "200601021504"
)

// defaultLogFilePerm 日志文件默认权限
const defaultLogFilePerm = 0o644

// rotateErrLogInterval 轮转失败告警的最小间隔
const rotateErrLogInterval = time.Minute

// rotateWriter 按时间轮转的日志文件写入器
//
// 文件名形如 {base}.20260912，并维护一个指向当前文件的符号链接 {base}，
// 便于 tail 等工具始终跟随最新日志。
//
// 写入方为 asyncWriter 的单个消费协程，但 Close 可能来自其他协程，故仍加锁保护。
type rotateWriter struct {
	base     string        // 文件名前缀，如 /var/log/app.log
	linkName string        // 符号链接路径，为空表示不创建
	layout   string        // 时间后缀格式
	maxAge   time.Duration // 超过该时长的历史文件会被清理，<=0 表示不清理
	rotate   time.Duration // 轮转周期

	// clock 可注入的时间源，便于测试轮转与清理
	clock func() time.Time

	// perm 日志文件权限
	perm os.FileMode

	mu          sync.Mutex
	file        *os.File
	currentName string
	closed      bool

	// lastRotateErrAt 上次记录轮转失败的时刻，用于告警降噪
	lastRotateErrAt time.Time
}

// newRotateWriter 创建轮转写入器并立即打开当前文件
// 提前打开可将权限、路径等问题暴露在初始化阶段，而非首次写日志时才失败
func newRotateWriter(base string, maxAge, rotate time.Duration, perm os.FileMode) (*rotateWriter, error) {
	if perm == 0 {
		perm = defaultLogFilePerm
	}
	w := &rotateWriter{
		base:     base,
		linkName: base,
		layout:   rotateLayoutFor(rotate),
		maxAge:   maxAge,
		rotate:   rotate,
		clock:    time.Now,
		perm:     perm,
	}
	if err := w.rotateTo(w.filenameFor(w.clock())); err != nil {
		return nil, err
	}
	return w, nil
}

// rotateLayoutFor 依据轮转周期选择时间后缀粒度
func rotateLayoutFor(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return rotateLayoutDay
	case d >= time.Hour:
		return rotateLayoutHour
	default:
		return rotateLayoutMinute
	}
}

// Write 写入日志，跨越轮转周期时先切换文件
func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, os.ErrClosed
	}

	if name := w.filenameFor(w.clock()); name != w.currentName {
		if err := w.rotateTo(name); err != nil {
			// 轮转失败不能丢日志：旧文件句柄仍然可用，降级继续写它。
			// 直接返回错误的话，从第一次轮转失败起每条日志都走这条路，
			// 而 asyncWriter 只保留第一个错误且只在 Close 时返回——
			// 现象就是某天日志突然断了，没有任何报错
			w.reportRotateFailure(err)
		} else {
			// 清理放到后台执行，避免阻塞日志写入；当前文件名以参数传入，避免再次取锁
			go w.purge(name)
		}
	}

	if w.file == nil {
		return 0, os.ErrClosed
	}
	return w.file.Write(p)
}

// reportRotateFailure 记录轮转失败
// 按间隔降噪：轮转周期到了之后每条日志都会重试，不限流会把告警刷爆
func (w *rotateWriter) reportRotateFailure(err error) {
	now := w.clock()
	if w.lastRotateErrAt.IsZero() || now.Sub(w.lastRotateErrAt) >= rotateErrLogInterval {
		w.lastRotateErrAt = now
		warnf("rotate: failed, keep writing current file=[%s], err=[%v]", w.currentName, err)
	}
}

// Close 关闭当前日志文件，多次调用安全
func (w *rotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// filenameFor 计算指定时刻所属周期的日志文件名
func (w *rotateWriter) filenameFor(t time.Time) string {
	return w.base + "." + truncateInLocation(t, w.rotate).Format(w.layout)
}

// rotateTo 切换到指定文件
// 调用方需持有锁；构造阶段实例尚未被共享，此时无需加锁
func (w *rotateWriter) rotateTo(name string) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, w.perm)
	if err != nil {
		return xerror.Newf("xlog", "rotate", "open log file failed, file=[%s], err=[%v]", name, err)
	}

	if w.file != nil {
		// 旧文件关闭失败不影响继续写入新文件，仅记录
		if cerr := w.file.Close(); cerr != nil {
			warnf("rotate: close previous file failed, err=[%v]", cerr)
		}
	}
	w.file = f
	w.currentName = name

	w.updateSymlink(name)
	return nil
}

// updateSymlink 将符号链接指向当前文件
// 符号链接在部分平台或文件系统上不被支持，失败不影响日志写入
func (w *rotateWriter) updateSymlink(target string) {
	if w.linkName == "" {
		return
	}

	// 先创建临时链接再原子替换，避免替换过程中链接短暂缺失
	tmp := w.linkName + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Base(target), tmp); err != nil {
		warnf("rotate: create symlink failed, link=[%s], err=[%v]", tmp, err)
		return
	}
	if err := os.Rename(tmp, w.linkName); err != nil {
		warnf("rotate: replace symlink failed, link=[%s], err=[%v]", w.linkName, err)
		_ = os.Remove(tmp)
	}
}

// purge 删除超过 maxAge 的历史日志文件，current 为当前正在写入的文件
func (w *rotateWriter) purge(current string) {
	if w.maxAge <= 0 {
		return
	}

	matches, err := filepath.Glob(w.base + ".*")
	if err != nil {
		warnf("rotate: glob log files failed, err=[%v]", err)
		return
	}

	// filepath.Glob 返回结果已排序，删除顺序是确定的
	cutoff := w.clock().Add(-w.maxAge)
	for _, path := range matches {
		// Lstat 而非 Stat：避免跟随符号链接把当前文件误判为历史文件
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			continue // 跳过符号链接本身
		}
		if path == current {
			continue // 不删除正在写入的文件
		}
		if !w.expired(path, fi, cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			warnf("rotate: remove expired log failed, file=[%s], err=[%v]", path, err)
		}
	}
}

// expired 判断历史日志文件是否已过保留期
//
// 优先用文件名里的时间后缀，而不是 mtime：备份恢复、rsync、容器镜像分层
// 都会重写 mtime，按它判断可能把昨天的日志当成刚写的而永远不清，
// 也可能把刚轮转出来的文件当成过期的删掉。文件名解析不了时才退回 mtime
func (w *rotateWriter) expired(path string, fi os.FileInfo, cutoff time.Time) bool {
	suffix := strings.TrimPrefix(path, w.base+".")
	if t, err := time.ParseInLocation(w.layout, suffix, w.clock().Location()); err == nil {
		// 文件名记的是所属周期的起点，整个周期结束后才算过期
		return t.Add(w.rotate).Before(cutoff) || t.Add(w.rotate).Equal(cutoff)
	}
	return !fi.ModTime().After(cutoff)
}

// truncateInLocation 按周期截断时间，且对齐到本地时区
//
// time.Truncate 以 UTC 零点为基准，直接用于按天轮转会导致切割点落在本地时间的
// 非零点时刻。这里先把本地时间的各字段原样搬到 UTC 上做截断，再搬回本地时区，
// 使按天轮转对齐本地零点（与替换前的 file-rotatelogs 行为保持一致）。
func truncateInLocation(t time.Time, d time.Duration) time.Time {
	if d <= 0 {
		return t
	}
	if t.Location() == time.UTC {
		return t.Truncate(d)
	}

	base := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	base = base.Truncate(d)
	return time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), base.Minute(), base.Second(), base.Nanosecond(), t.Location())
}

// warnf 把日志系统自身的故障直接写到 stderr
//
// 不能走日志系统：出错的正是它，再调用它就成了环。
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "xlog: "+format+"\n", args...)
}
