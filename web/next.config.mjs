/** @type {import('next').NextConfig} */
const nextConfig = {
  // standalone 输出配合 deploy/Dockerfile.web 的多阶段构建
  output: "standalone",
  // 控制台经同源 /api/* 访问平台（开发时指向本机 api）
  async rewrites() {
    return [
      {
        source: "/api/:path*",
        destination: `${process.env.API_URL || "http://localhost:8080"}/:path*`,
      },
    ];
  },
};

export default nextConfig;
