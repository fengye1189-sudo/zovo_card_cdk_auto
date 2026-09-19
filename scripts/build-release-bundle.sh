#!/usr/bin/env bash
# 构建管理后台一键更新用的预编译包 cdk-bundle-linux-amd64.tgz
# 用法：./scripts/build-release-bundle.sh [version]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VER="${1:-}"
if [[ -z "$VER" ]]; then
  VER="$(tr -d ' \n' < "$ROOT/VERSION" 2>/dev/null || echo 0.0.0)"
fi
VER="${VER#v}"
echo "$VER" > "$ROOT/VERSION"
mkdir -p "$ROOT/dist/web"
echo "==> go build v$VER (linux/amd64)"
cd "$ROOT/backend"
# ★必须显式交叉编译★：产物名字叫 cdk-bundle-linux-amd64.tgz，但这里原来不设
# GOOS/GOARCH——在 macOS 上跑就会把一个 Mach-O/arm64 二进制塞进这个包。
# 而且全程没有任何报错：打包成功、上传成功、一键更新也报"成功"，
# 直到生产机起服务时 Exec format error，站点已经下线了。
# CGO_ENABLED=0 产出静态二进制，不吃目标机的 glibc 版本。
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w -X github.com/tuzi/cdk-recharge-system/internal/handler.BuildVersion=${VER}" \
  -o "$ROOT/dist/cdk-recharge" ./cmd/server
# 打包前自证架构：与其让错误在生产机上以「服务起不来」的形式暴露，不如在这里失败。
if command -v file >/dev/null 2>&1; then
  if ! file "$ROOT/dist/cdk-recharge" | grep -q "ELF 64-bit.*x86-64"; then
    echo "产物不是 linux/amd64 ELF，拒绝打包：$(file "$ROOT/dist/cdk-recharge")"
    exit 1
  fi
fi
echo "==> frontend build"
cd "$ROOT/frontend"
if [[ -f package-lock.json ]]; then npm ci; else npm install; fi
npm run build
rm -rf "$ROOT/dist/web"
mkdir -p "$ROOT/dist/web"
cp -a dist/. "$ROOT/dist/web/"
cp "$ROOT/VERSION" "$ROOT/dist/VERSION"
cd "$ROOT/dist"
tar -czf "$ROOT/cdk-bundle-linux-amd64.tgz" cdk-recharge web VERSION
sha256sum "$ROOT/cdk-bundle-linux-amd64.tgz" | tee "$ROOT/cdk-bundle-linux-amd64.tgz.sha256"
ls -lh "$ROOT/cdk-bundle-linux-amd64.tgz"*
echo "==> done. 上传到 GitHub Release 资产："
echo "    gh release upload v${VER} cdk-bundle-linux-amd64.tgz cdk-bundle-linux-amd64.tgz.sha256 --clobber"
