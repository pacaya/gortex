import type { Metadata } from 'next'
import { Inter, JetBrains_Mono } from 'next/font/google'
import './globals.css'
import { Sidebar } from '@/components/layout/Sidebar'
import { Header } from '@/components/layout/Header'
import { StatusBar } from '@/components/layout/StatusBar'

const inter = Inter({
  variable: '--font-inter',
  subsets: ['latin'],
})

const jetbrainsMono = JetBrains_Mono({
  variable: '--font-jetbrains-mono',
  subsets: ['latin'],
})

export const metadata: Metadata = {
  title: 'Gortex',
  description: 'Code intelligence engine — graph explorer and analysis dashboard',
}

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode
}>) {
  return (
    <html
      lang="en"
      className={`${inter.variable} ${jetbrainsMono.variable} dark h-full antialiased`}
    >
      <body className="h-full bg-zinc-950 text-zinc-100">
        <div className="flex h-full flex-col">
          <Header />
          <div className="flex flex-1 overflow-hidden">
            <Sidebar />
            <main className="flex-1 overflow-y-auto bg-zinc-950 p-6">
              {children}
            </main>
          </div>
          <StatusBar />
        </div>
      </body>
    </html>
  )
}
