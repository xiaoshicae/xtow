// Package xerror 提供统一的错误类型：带模块名和操作名，并正确支持 errors.Is / As
package xerror

import (
	"errors"
	"fmt"
	"strings"
)

// Error 统一错误类型，包含模块名、操作名和原始错误
type Error struct {
	Module string // 模块名，如 "xconfig", "xgorm"
	Op     string // 操作名，如 "init", "close"
	Err    error  // 原始错误
}

// Error 渲染为 "xtwo {module} {op} failed, err=[...]"
func (e *Error) Error() string {
	var b strings.Builder
	b.Grow(32 + len(e.Module) + len(e.Op))
	b.WriteString("xtwo ")
	b.WriteString(e.Module)
	b.WriteByte(' ')
	b.WriteString(e.Op)
	b.WriteString(" failed")
	if e.Err != nil {
		b.WriteString(", err=[")
		b.WriteString(e.Err.Error())
		b.WriteByte(']')
	}
	return b.String()
}

// Unwrap 支持 errors.Is / errors.As 链式判断
func (e *Error) Unwrap() error {
	return e.Err
}

// New 创建一个 Error
func New(module, op string, err error) *Error {
	return &Error{Module: module, Op: op, Err: err}
}

// Newf 创建一个带格式化消息的 Error
func Newf(module, op, format string, args ...any) *Error {
	return &Error{Module: module, Op: op, Err: fmt.Errorf(format, args...)}
}

// Is 判断 err 链中是否包含指定模块产生的错误
//
// 遍历整条链而不是只看最外层：模块之间会互相包装错误
// （xgorm 初始化失败里裹着 xconfig 的错误），只比对第一个 Error
// 会让 Is(err, "xconfig") 在这种链上返回 false，与本函数的语义不符
func Is(err error, module string) bool {
	for err != nil {
		var xe *Error
		if !errors.As(err, &xe) {
			return false
		}
		if xe.Module == module {
			return true
		}
		err = xe.Err // 从当前 Error 的内层继续找
	}
	return false
}

// Module 提取最外层错误的模块名，若不是本包的错误则返回空字符串
//
// 取最外层而非遍历整条链：错误一路向上包装，最外层代表「谁最终报出了这个错误」，
// 这正是调用方要分流处理的依据。要判断链中是否涉及某个模块，用 Is
func Module(err error) string {
	var xe *Error
	if errors.As(err, &xe) {
		return xe.Module
	}
	return ""
}
