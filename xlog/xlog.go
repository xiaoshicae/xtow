package xlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xerror"
)

// New 按配置造一个 logger。
//
// 纯构造器：不碰全局、不读配置文件、不依赖框架运行。测试直接调它。
//
// 返回的 io.Closer 用于收尾（关闭日志文件），即便没有文件输出也不会是 nil，
// 调用方不必判空。
func New(cfg Config) (*slog.Logger, io.Closer, error) {
	// 配置项全部先校验完，再动文件。反过来的话，Format 写错时
	// 日志文件已经建好、fd 也开着，而 New 返回了错误——调用方手上
	// 没有 Closer 可关，那个 fd 和它的符号链接就留在那里了
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}
	newHandler, err := handlerFor(cfg.Format)
	if err != nil {
		return nil, nil, err
	}

	writers := make([]io.Writer, 0, 2)
	closers := make([]io.Closer, 0, 1)

	if cfg.Console {
		writers = append(writers, os.Stdout)
	}
	if cfg.File.Enable {
		w, err := newFileWriter(cfg.File)
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, w)
		closers = append(closers, w)
	}

	// 一个输出都没开时写到 io.Discard 而不是报错：
	// 「我就是不要日志」是个合理的选择，不该让服务起不来
	var out io.Writer = io.Discard
	switch {
	case len(writers) == 1:
		out = writers[0]
	case len(writers) > 1:
		out = io.MultiWriter(writers...)
	}

	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.AddSource}
	return slog.New(newCtxHandler(newHandler(out, opts))), multiCloser(closers), nil
}

// handlerFor 按格式选出 handler 的构造函数。
// 只认格式、不碰输出，好让格式写错这件事在打开日志文件之前就暴露。
func handlerFor(format string) (func(io.Writer, *slog.HandlerOptions) slog.Handler, error) {
	switch strings.ToLower(format) {
	case FormatText:
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewTextHandler(w, o) }, nil
	case FormatJSON, "":
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewJSONHandler(w, o) }, nil
	default:
		return nil, xerror.Newf("xlog", "new",
			"unknown log format Format=[%s], expected %s or %s", format, FormatJSON, FormatText)
	}
}

// newFileWriter 造文件写入器，目录不存在时创建
func newFileWriter(c FileConfig) (io.WriteCloser, error) {
	if c.Path != "" {
		if err := os.MkdirAll(c.Path, 0o755); err != nil {
			return nil, xerror.Newf("xlog", "new", "create log dir failed Path=[%s], err=[%v]", c.Path, err)
		}
	}
	perm, err := parsePerm(c.Perm)
	if err != nil {
		return nil, err
	}
	return newRotateWriter(filepath.Join(c.Path, c.Name), c.MaxAge, c.RotateTime, perm)
}

// parseLevel 解析日志级别。不认识的值直接报错而不是退回默认值——
// 配置写错了应该在启动时知道，而不是上线后发现日志级别不对。
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, xerror.Newf("xlog", "new",
			"unknown log level Level=[%s], expected debug / info / warn / error", s)
	}
}

// parsePerm 解析八进制权限字符串
func parsePerm(s string) (os.FileMode, error) {
	if s == "" {
		return defaultLogFilePerm, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, xerror.Newf("xlog", "new",
			"malformed log file permission Perm=[%s], expected an octal string such as \"0644\"", s)
	}
	return os.FileMode(v), nil
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var first error
	for _, c := range m {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---- 登记 ----

var cfg = DefaultConfig()

// init 只登记，不初始化。真正的初始化由框架在 StageLog 执行。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Stage:  registry.StageLog,
		Config: &cfg,
		Init: func(context.Context) (io.Closer, error) {
			l, c, err := New(cfg)
			if err != nil {
				return nil, err
			}
			// 装进标准库的全局默认 logger：业务代码直接用 slog.Info / slog.InfoContext，
			// 不需要认识本包。这与 slog.SetDefault 是同一个模式。
			slog.SetDefault(l)
			return c, nil
		},
	})
}
