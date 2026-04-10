'use client'

import { useStore } from '@/lib/store'

export function StatusBar() {
  const connected = useStore((s) => s.connected)
  const health = useStore((s) => s.health)
  const stats = useStore((s) => s.stats)

  return (
    <footer className="flex h-6 shrink-0 items-center justify-between border-t border-zinc-800 bg-zinc-950 px-4 text-[11px] text-zinc-500">
      <div className="flex items-center gap-2">
        <span
          className={`h-1.5 w-1.5 rounded-full ${connected ? 'bg-emerald-500' : 'bg-red-500'}`}
        />
        <span>{connected ? 'Connected' : 'Disconnected'}</span>
      </div>

      <div className="flex items-center gap-4">
        <span>Nodes: {stats?.total_nodes ?? '---'}</span>
        <span>Edges: {stats?.total_edges ?? '---'}</span>
        {health?.version && <span>v{health.version}</span>}
      </div>
    </footer>
  )
}
