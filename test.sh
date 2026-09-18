#!/bin/sh
# 跑全仓库的测试。go test ./... 不跨模块边界，所以按模块逐个跑。
set -e
for m in . $(find . -mindepth 2 -name go.mod -not -path './.git/*' | xargs -r -n1 dirname | sort); do
  echo "── $m"
  (cd "$m" && GOWORK=off go test -race "$@" ./...)
done
