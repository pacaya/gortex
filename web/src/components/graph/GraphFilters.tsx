'use client'

import { NODE_COLORS } from '@/lib/colors'
import { useStore } from '@/lib/store'
import type { NodeKind } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { RotateCcw, Maximize2 } from 'lucide-react'

interface GraphFiltersProps {
  nodeCount: number
  edgeCount: number
  onFitCamera: () => void
  onRelayout: () => void
}

export default function GraphFilters({ nodeCount, edgeCount, onFitCamera, onRelayout }: GraphFiltersProps) {
  const { visibleKinds, toggleKind, hideTestFiles, setHideTestFiles, hideImports, setHideImports } = useStore()

  const kinds = Object.entries(NODE_COLORS) as [NodeKind, string][]

  return (
    <div className="flex h-full w-[240px] shrink-0 flex-col border-r border-zinc-800 bg-zinc-900/50 p-4">
      <h2 className="mb-4 text-sm font-semibold text-zinc-300">Filters</h2>

      {/* Stats */}
      <div className="mb-4 flex gap-4 text-xs text-zinc-500">
        <span>{nodeCount.toLocaleString()} nodes</span>
        <span>{edgeCount.toLocaleString()} edges</span>
      </div>

      {/* Node kind checkboxes */}
      <div className="mb-4 space-y-1.5">
        <p className="mb-2 text-xs font-medium uppercase tracking-wider text-zinc-500">Node Kinds</p>
        {kinds.map(([kind, color]) => (
          <label
            key={kind}
            className="flex cursor-pointer items-center gap-2 rounded px-1.5 py-1 text-sm hover:bg-zinc-800/50"
          >
            <input
              type="checkbox"
              checked={visibleKinds.has(kind)}
              onChange={() => toggleKind(kind)}
              className="sr-only"
            />
            <span
              className="flex h-4 w-4 shrink-0 items-center justify-center rounded border border-zinc-700"
              style={{
                backgroundColor: visibleKinds.has(kind) ? color + '33' : 'transparent',
                borderColor: visibleKinds.has(kind) ? color : undefined,
              }}
            >
              {visibleKinds.has(kind) && (
                <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke={color} strokeWidth={3}>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M5 13l4 4L19 7" />
                </svg>
              )}
            </span>
            <span
              className="h-2.5 w-2.5 shrink-0 rounded-full"
              style={{ backgroundColor: color }}
            />
            <span className="text-zinc-300">{kind}</span>
          </label>
        ))}
      </div>

      {/* Toggles */}
      <div className="mb-4 space-y-2">
        <p className="mb-2 text-xs font-medium uppercase tracking-wider text-zinc-500">Visibility</p>
        <label className="flex cursor-pointer items-center gap-2 rounded px-1.5 py-1 text-sm hover:bg-zinc-800/50">
          <input
            type="checkbox"
            checked={hideTestFiles}
            onChange={(e) => setHideTestFiles(e.target.checked)}
            className="h-3.5 w-3.5 rounded border-zinc-700 bg-zinc-800 accent-blue-500"
          />
          <span className="text-zinc-300">Hide test files</span>
        </label>
        <label className="flex cursor-pointer items-center gap-2 rounded px-1.5 py-1 text-sm hover:bg-zinc-800/50">
          <input
            type="checkbox"
            checked={hideImports}
            onChange={(e) => setHideImports(e.target.checked)}
            className="h-3.5 w-3.5 rounded border-zinc-700 bg-zinc-800 accent-blue-500"
          />
          <span className="text-zinc-300">Hide imports</span>
        </label>
      </div>

      {/* Actions */}
      <div className="mt-auto space-y-2">
        <Button variant="outline" size="sm" className="w-full justify-start gap-2" onClick={onFitCamera}>
          <Maximize2 className="h-3.5 w-3.5" />
          Fit to screen
        </Button>
        <Button variant="outline" size="sm" className="w-full justify-start gap-2" onClick={onRelayout}>
          <RotateCcw className="h-3.5 w-3.5" />
          Re-layout
        </Button>
      </div>
    </div>
  )
}
