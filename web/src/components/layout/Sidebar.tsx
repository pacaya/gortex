'use client'

import Link from 'next/link'
import { usePathname } from 'next/navigation'
import {
  LayoutDashboard,
  Network,
  Search,
  Users,
  Workflow,
  FlaskConical,
  MessageSquare,
} from 'lucide-react'
import { useStore } from '@/lib/store'
import { cn } from '@/lib/utils'
import { Separator } from '@/components/ui/separator'

const navItems = [
  { href: '/', label: 'Dashboard', icon: LayoutDashboard },
  { href: '/graph', label: 'Graph', icon: Network },
  { href: '/search', label: 'Search', icon: Search },
  { href: '/communities', label: 'Communities', icon: Users },
  { href: '/processes', label: 'Processes', icon: Workflow },
  { href: '/analysis', label: 'Analysis', icon: FlaskConical },
  { href: '/chat', label: 'Chat', icon: MessageSquare },
]

export function Sidebar() {
  const pathname = usePathname()
  const stats = useStore((s) => s.stats)

  return (
    <aside className="hidden md:flex w-60 shrink-0 flex-col border-r border-zinc-800 bg-zinc-950">
      <nav className="flex-1 px-3 py-4 space-y-1">
        {navItems.map((item) => {
          const isActive =
            item.href === '/'
              ? pathname === '/'
              : pathname.startsWith(item.href)
          return (
            <Link
              key={item.href}
              href={item.href}
              className={cn(
                'flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors',
                isActive
                  ? 'bg-zinc-800 text-zinc-100'
                  : 'text-zinc-400 hover:bg-zinc-900 hover:text-zinc-200'
              )}
            >
              <item.icon className="h-4 w-4 shrink-0" />
              {item.label}
            </Link>
          )
        })}
      </nav>

      <Separator className="bg-zinc-800" />

      <div className="px-4 py-3 text-xs text-zinc-500 space-y-1">
        <div className="flex justify-between">
          <span>Nodes</span>
          <span className="text-zinc-400">{stats?.total_nodes ?? '---'}</span>
        </div>
        <div className="flex justify-between">
          <span>Edges</span>
          <span className="text-zinc-400">{stats?.total_edges ?? '---'}</span>
        </div>
      </div>
    </aside>
  )
}
