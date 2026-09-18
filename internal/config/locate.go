package config

import (
	"os"
	"strings"

	"github.com/xiaoshicae/xtow/xutil"
)

const (
	// ArgKey / EnvKey 显式指定配置文件位置的两种方式
	ArgKey = "config"
	EnvKey = "XTWO_CONFIG"
)

// SearchPaths 未显式指定时，按顺序查找的约定位置
var SearchPaths = []string{
	"conf/application.yml",
	"conf/application.yaml",
	"config/application.yml",
	"config/application.yaml",
	"application.yml",
	"application.yaml",
}

// Locate 定位配置文件：启动参数 > 环境变量 > 约定路径。
//
// 都找不到时返回空串，由调用方决定是报错还是全用默认值起。
func Locate() string {
	if v := fromArgs(ArgKey); v != "" {
		return v
	}
	if v := os.Getenv(EnvKey); v != "" {
		return v
	}
	for _, p := range SearchPaths {
		if xutil.FileExist(p) {
			return p
		}
	}
	return ""
}

// fromArgs 从命令行读 --key=value 或 --key value，两种写法都支持。
//
// 不用 flag 包：flag.Parse 会接管整个命令行，而使用者的程序多半有自己的参数，
// 框架不该替他们决定怎么解析。
func fromArgs(key string) string {
	args := os.Args[1:]
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if after, ok := strings.CutPrefix(name, key+"="); ok {
			return after
		}
		if name == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
