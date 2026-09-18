package xlog

import (
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
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}

	writers := make([]io.Writer, 0, 2)
	closers := make([]io.Closer, 0, 1)

	if cfg.Console.Enable {
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
	switch len(writers) {
	case 1:
		out = writers[0]
	default:
		if len(writers) > 1 {
			out = io.MultiWriter(writers...)
		}
	}

	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.AddSource}
	var h slog.Handler
	switch strings.ToLower(cfg.Format) {
	case FormatText:
		h = slog.NewTextHandler(out, opts)
	case FormatJSON, "":
		h = slog.NewJSONHandler(out, opts)
	default:
		return nil, nil, xerror.Newf("xlog", "new",
			"不认识的日志格式 Format=[%s]，可选 %s / %s", cfg.Format, FormatJSON, FormatText)
	}

	return slog.New(newCtxHandler(h)), multiCloser(closers), nil
}

// newFileWriter 造文件写入器，目录不存在时创建
func newFileWriter(c FileConfig) (io.WriteCloser, error) {
	if c.Path != "" {
		if err := os.MkdirAll(c.Path, 0o755); err != nil {
			return nil, xerror.Newf("xlog", "new", "创建日志目录失败 Path=[%s], err=[%v]", c.Path, err)
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
			"不认识的日志级别 Level=[%s]，可选 debug / info / warn / error", s)
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
			"日志文件权限格式不对 Perm=[%s]，应为八进制字符串如 \"0644\"", s)
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
		Init: func() (io.Closer, error) {
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
