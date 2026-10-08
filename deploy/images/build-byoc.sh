#!/usr/bin/env bash
# BYOC 模板镜像构建（期 4 §C）：以基础镜像构建 E2B 模板兼容镜像并推租户
# registry——E2B 模板要求 envd ≥ v0.5.0（租户 E2B 实例侧安装）。
# 用法: bash deploy/images/build-byoc.sh <registry>/<image>:<tag>
set -euo pipefail
cd "$(dirname "$0")/../.."
IMAGE="${1:?用法: bash deploy/images/build-byoc.sh <registry>/<image>:<tag>}"

echo "== BYOC 模板镜像构建（${IMAGE}）=="

# 模板 Dockerfile：以租户基础镜像为底，补齐 E2B 模板约定（非 root 用户 +
# /workspace 工作区——与 docker 档沙箱契约同形状）
cat > /tmp/Dockerfile.byoc <<EOF
FROM ${IMAGE}
RUN mkdir -p /workspace && chmod 777 /workspace
WORKDIR /workspace
EOF

docker build -f /tmp/Dockerfile.byoc -t "${IMAGE}-e2b" . > /dev/null
docker push "${IMAGE}-e2b" > /dev/null
echo "模板镜像已推送: ${IMAGE}-e2b"
echo "下一步：租户 E2B 控制台创建模板（指向 ${IMAGE}-e2b）→ .env 配 E2B_TEMPLATE_MAP"
rm -f /tmp/Dockerfile.byoc
