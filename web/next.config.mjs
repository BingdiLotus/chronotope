/** @type {import('next').NextConfig} */
const nextConfig = {
  // standalone 输出配合 deploy/Dockerfile.web 的多阶段构建
  output: "standalone",
  // 控制台经 NEXT_PUBLIC_API_URL 直连平台 api（无同源代理）
};

export default nextConfig;
