'use client'

import { useEffect, useState } from 'react'
import Link from 'next/link'
import { Users, FileCode, ChevronDown, ChevronRight } from 'lucide-react'
import { api } from '@/lib/api'
import type { Community, CommunityResult } from '@/lib/types'
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
} from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'

function CohesionBar({ value }: { value: number }) {
  const pct = Math.round(value * 100)
  const color =
    pct >= 70 ? 'bg-emerald-500' : pct >= 40 ? 'bg-yellow-500' : 'bg-red-500'

  return (
    <div className="flex items-center gap-2">
      <div className="h-2 w-24 rounded-full bg-zinc-800">
        <div
          className={`h-2 rounded-full ${color}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className="text-xs text-zinc-400">{pct}%</span>
    </div>
  )
}

function CommunityCard({ community, duplicateIndex }: { community: Community; duplicateIndex?: number }) {
  const [expanded, setExpanded] = useState(false)
  const [detail, setDetail] = useState<Community | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)

  const handleToggle = async () => {
    const willExpand = !expanded
    setExpanded(willExpand)
    if (willExpand && !detail && !detailLoading) {
      setDetailLoading(true)
      try {
        const full = await api.getCommunity(community.id)
        setDetail(full)
      } catch {
        // Fall back to summary data
      } finally {
        setDetailLoading(false)
      }
    }
  }

  const members = detail?.members ?? community.members
  const files = detail?.files ?? community.files

  // Extract short name from symbol ID (e.g. "pkg/foo.go::Bar" → "Bar")
  const shortName = (id: string) => {
    const parts = id.split('::')
    return parts.length > 1 ? parts[parts.length - 1] : id
  }

  // Build a disambiguated display name when multiple communities share the same label
  const displayName = (() => {
    const base = community.label || `Community ${community.id}`
    const communityFiles = community.files ?? []

    // Extract unique distinguishing filenames (without extension)
    const fileNames = communityFiles
      .map(f => {
        const basename = f.split('/').pop() || f
        return basename.replace(/(_test)?\.go$/, '').replace(/\.tsx?$/, '').replace(/\.py$/, '')
      })
      .filter((name, i, arr) => arr.indexOf(name) === i) // unique
      .filter(name => name !== base) // not same as label

    // If this label appears more than once, use file names as the primary label
    if (duplicateIndex !== undefined && fileNames.length > 0) {
      if (fileNames.length <= 3) return fileNames.join(', ')
      return `${fileNames.slice(0, 2).join(', ')} & ${fileNames.length - 2} more`
    }

    return base
  })()

  return (
    <Card className="border-zinc-800 bg-zinc-900">
      <CardHeader
        className="cursor-pointer"
        onClick={handleToggle}
      >
        <div className="flex items-start justify-between">
          <div>
            <CardTitle className="flex items-center gap-2 text-zinc-100">
              {expanded ? (
                <ChevronDown className="h-4 w-4 text-zinc-500" />
              ) : (
                <ChevronRight className="h-4 w-4 text-zinc-500" />
              )}
              {displayName}
              {duplicateIndex !== undefined && community.label && (
                <span className="text-xs font-normal text-zinc-600">
                  {community.label}
                </span>
              )}
            </CardTitle>
          </div>
          <div className="flex items-center gap-2">
            <Badge variant="secondary" className="bg-zinc-800 text-zinc-300">
              <Users className="mr-1 h-3 w-3" />
              {community.size} symbols
            </Badge>
            <Badge variant="secondary" className="bg-zinc-800 text-zinc-300">
              <FileCode className="mr-1 h-3 w-3" />
              {community.files?.length ?? 0} files
            </Badge>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        <div className="flex items-center gap-2">
          <span className="text-xs text-zinc-500">Cohesion:</span>
          <CohesionBar value={community.cohesion} />
        </div>

        {expanded && (
          <div className="mt-4 space-y-3 border-t border-zinc-800 pt-4">
            {detailLoading ? (
              <div className="flex items-center gap-2 py-3 text-sm text-zinc-500">
                <div className="h-4 w-4 animate-spin rounded-full border-2 border-zinc-700 border-t-zinc-400" />
                Loading details...
              </div>
            ) : (
              <>
                {members && members.length > 0 && (
                  <div>
                    <h4 className="mb-2 text-xs font-medium uppercase tracking-wider text-zinc-500">
                      Symbols ({members.length})
                    </h4>
                    <div className="space-y-1">
                      {members.map((id) => (
                        <Link
                          key={id}
                          href={`/symbol/${encodeURIComponent(id)}`}
                          className="flex items-center gap-2 rounded px-2 py-1 text-xs transition-colors hover:bg-zinc-800"
                          onClick={(e) => e.stopPropagation()}
                        >
                          <span className="font-mono font-medium text-blue-400">
                            {shortName(id)}
                          </span>
                          <span className="truncate text-zinc-600">
                            {id.split('::')[0]}
                          </span>
                        </Link>
                      ))}
                    </div>
                  </div>
                )}
                {files && files.length > 0 && (
                  <div>
                    <h4 className="mb-2 text-xs font-medium uppercase tracking-wider text-zinc-500">
                      Files ({files.length})
                    </h4>
                    <div className="space-y-0.5">
                      {files.map((f) => (
                        <p
                          key={f}
                          className="truncate font-mono text-xs text-zinc-400"
                        >
                          {f}
                        </p>
                      ))}
                    </div>
                  </div>
                )}
              </>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

export default function CommunitiesPage() {
  const [data, setData] = useState<CommunityResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let mounted = true

    async function fetchData() {
      try {
        const result = await api.getCommunities()
        if (!mounted) return
        setData(result)
        setError(null)
      } catch (err) {
        if (!mounted) return
        setError(err instanceof Error ? err.message : 'Failed to load communities')
      } finally {
        if (mounted) setLoading(false)
      }
    }

    fetchData()
    return () => {
      mounted = false
    }
  }, [])

  const [showSmall, setShowSmall] = useState(false)
  const MIN_SIZE = 5

  const all = data?.communities
    ? [...data.communities].sort((a, b) => b.size - a.size)
    : []
  const sorted = showSmall ? all : all.filter(c => c.size >= MIN_SIZE)
  const hiddenCount = all.length - sorted.length

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-sm text-zinc-500">Loading communities...</p>
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex h-64 items-center justify-center">
        <Card className="w-96 border-zinc-800 bg-zinc-900">
          <CardHeader>
            <CardTitle className="text-red-400">Error</CardTitle>
            <CardDescription>{error}</CardDescription>
          </CardHeader>
        </Card>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-zinc-100">Communities</h1>
        <p className="text-sm text-zinc-500">
          Groups of symbols that are tightly connected — high cohesion means the group forms a clear module
        </p>
      </div>

      {data && (
        <Card className="border-zinc-800 bg-zinc-900">
          <CardContent className="flex items-center gap-4 py-4">
            <div>
              <p className="text-xs uppercase tracking-wider text-zinc-500">
                Modularity Score
              </p>
              <p className="text-3xl font-bold text-zinc-100">
                {(data.modularity * 100).toFixed(1)}%
              </p>
            </div>
            <div className="text-sm text-zinc-400">
              {sorted.length} communities detected
            </div>
          </CardContent>
        </Card>
      )}

      <div className="grid gap-4 lg:grid-cols-2">
        {sorted.map((c) => {
          // Count how many communities share this label to detect duplicates
          const dupeCount = sorted.filter(o => o.label === c.label).length
          const dupeIdx = dupeCount > 1
            ? sorted.filter(o => o.label === c.label).indexOf(c)
            : undefined
          return (
            <CommunityCard
              key={c.id}
              community={c}
              duplicateIndex={dupeIdx}
            />
          )
        })}
      </div>

      {hiddenCount > 0 && (
        <button
          onClick={() => setShowSmall(!showSmall)}
          className="w-full rounded-lg border border-zinc-800 bg-zinc-900/50 py-2 text-sm text-zinc-500 transition-colors hover:border-zinc-700 hover:text-zinc-300"
        >
          {showSmall
            ? `Hide ${hiddenCount} small communities (< ${MIN_SIZE} members)`
            : `Show ${hiddenCount} more small communities (< ${MIN_SIZE} members)`}
        </button>
      )}

      {sorted.length === 0 && (
        <p className="py-8 text-center text-sm text-zinc-600">
          No communities detected
        </p>
      )}
    </div>
  )
}
