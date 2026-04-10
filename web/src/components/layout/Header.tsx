'use client'

import { useEffect, useRef } from 'react'
import { Network, Search } from 'lucide-react'
import { useStore } from '@/lib/store'
import { Input } from '@/components/ui/input'

export function Header() {
  const connected = useStore((s) => s.connected)
  const searchQuery = useStore((s) => s.searchQuery)
  const setSearchQuery = useStore((s) => s.setSearchQuery)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault()
        inputRef.current?.focus()
      }
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [])

  return (
    <header className="flex h-12 shrink-0 items-center gap-4 border-b border-zinc-800 bg-zinc-950 px-4">
      <div className="flex items-center gap-2 text-zinc-100">
        <Network className="h-5 w-5 text-blue-400" />
        <span className="text-sm font-semibold tracking-tight">Gortex</span>
      </div>

      <div className="flex-1 flex justify-center max-w-md mx-auto">
        <div className="relative w-full">
          <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-zinc-500" />
          <Input
            ref={inputRef}
            type="text"
            placeholder="Search symbols... (Cmd+K)"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="h-8 w-full rounded-md border-zinc-800 bg-zinc-900 pl-8 pr-3 text-xs text-zinc-300 placeholder:text-zinc-600 focus-visible:border-zinc-700 focus-visible:ring-zinc-700/50"
          />
        </div>
      </div>

      <div className="flex items-center gap-2">
        <span
          className={`h-2 w-2 rounded-full ${connected ? 'bg-emerald-500' : 'bg-red-500'}`}
        />
        <span className="text-xs text-zinc-500">
          {connected ? 'Connected' : 'Disconnected'}
        </span>
      </div>
    </header>
  )
}
