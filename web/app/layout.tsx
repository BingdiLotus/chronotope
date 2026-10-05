import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Chronotope Console",
  description: "时空可组合持久运行时 · 控制台（W4）",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="zh-CN">
      <body>{children}</body>
    </html>
  );
}
