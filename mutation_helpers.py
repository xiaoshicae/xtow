# 这段由 mutate.sh 自动拼在每条变异前面，变异本身只写 swap() / cut()
import sys

_p = sys.argv[1]
_s = open(_p, encoding='utf-8').read()


def swap(old, new, count=1):
    """把 old 换成 new，并断言它恰好出现 count 次。

    断言是重点，不是顺手加的：重构挪走一段代码之后，模式会悄悄匹配不到，
    这条承诺从此再也没被检查过，而整轮照报绿——这个仓库里真发生过两次。
    匹配到的处数不对同样要拦：多匹配一处就是改坏了两个地方，
    测试红了也说明不了是哪一条承诺在起作用。
    """
    global _s
    n = _s.count(old)
    if n != count:
        raise SystemExit(f"模式匹配到 {n} 处、期望 {count} 处，这条变异要跟着代码改：\n{old}")
    _s = _s.replace(old, new)


def cut(start, end_after):
    """删掉从 start 开始、到 end_after 那一段结束为止的整块代码"""
    global _s
    if _s.count(start) != 1:
        raise SystemExit(f"起点匹配到 {_s.count(start)} 处、期望 1 处：\n{start}")
    i = _s.index(start)
    j = _s.index(end_after, i)
    if j < 0:
        raise SystemExit(f"找不到终点：\n{end_after}")
    _s = _s[:i] + _s[j + len(end_after):]
