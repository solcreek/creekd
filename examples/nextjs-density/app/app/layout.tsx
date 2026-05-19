export const metadata = {
  title: "creekd density bench fixture",
  description: "Tiny Next.js app for the creekd vs docker density benchmark.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
