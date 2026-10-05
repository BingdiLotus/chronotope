/** @type {import('next').NextConfig} */
const nextConfig = {
  // standalone 输出配合 deploy/Dockerfile.web 的多阶段构建
  output: "standalone",
};

export default nextConfig;
