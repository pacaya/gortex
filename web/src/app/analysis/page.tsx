'use client'

import { useState, useCallback } from 'react'
import Link from 'next/link'
import {
  Trash2,
  Flame,
  RefreshCw,
  HeartPulse,
  CheckCircle,
  XCircle,
} from 'lucide-react'
import { api } from '@/lib/api'
import type { IndexHealth } from '@/lib/types'
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
} from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import {
  Table,
  TableHeader,
  TableBody,
  TableHead,
  TableRow,
  TableCell,
} from '@/components/ui/table'

// --- Dead Code types ---
interface DeadCodeEntry {
  id: string
  name: string
  kind: string
  file_path: string
  start_line: number
}

// --- Hotspot types ---
interface HotspotEntry {
  id: string
  name: string
  fan_in: number
  fan_out: number
  community_crossings: number
  complexity_score?: number
}

// --- Cycle types ---
interface CycleGroup {
  path: string[]
  kind: string
  severity: number | string
}

interface CycleResult {
  cycles: CycleGroup[]
  total: number
}

type TabKey = 'dead-code' | 'hotspots' | 'cycles' | 'health'

function severityLabel(severity: number | string): string {
  if (typeof severity === 'string') return severity
  if (severity >= 3) return 'critical'
  if (severity >= 2) return 'warning'
  return 'info'
}

function severityColor(severity: number | string) {
  const label = typeof severity === 'string' ? severity : severityLabel(severity)
  switch (label) {
    case 'critical':
      return 'bg-red-500/10 text-red-400 border-red-500/20'
    case 'warning':
      return 'bg-yellow-500/10 text-yellow-400 border-yellow-500/20'
    default:
      return 'bg-blue-500/10 text-blue-400 border-blue-500/20'
  }
}

function healthScoreColor(score: number) {
  if (score >= 80) return 'text-emerald-400'
  if (score >= 50) return 'text-yellow-400'
  return 'text-red-400'
}

// --- Dead Code Tab ---
function DeadCodeTab() {
  const [data, setData] = useState<DeadCodeEntry[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (data !== null) return
    setLoading(true)
    try {
      const result = await api.findDeadCode()
      // The API may return various shapes; normalize to array
      if (Array.isArray(result)) {
        setData(result as DeadCodeEntry[])
      } else if (
        result &&
        typeof result === 'object' &&
        'dead_code' in (result as Record<string, unknown>)
      ) {
        setData(
          ((result as Record<string, unknown>).dead_code as DeadCodeEntry[]) ?? []
        )
      } else if (
        result &&
        typeof result === 'object' &&
        'symbols' in (result as Record<string, unknown>)
      ) {
        setData(
          ((result as Record<string, unknown>).symbols as DeadCodeEntry[]) ?? []
        )
      } else {
        setData([])
      }
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load dead code')
    } finally {
      setLoading(false)
    }
  }, [data])

  // Load on first render of this tab
  if (data === null && !loading && !error) {
    load()
  }

  if (loading) {
    return <p className="py-8 text-center text-sm text-zinc-500">Analyzing dead code...</p>
  }
  if (error) {
    return <p className="py-8 text-center text-sm text-red-400">{error}</p>
  }
  if (!data || data.length === 0) {
    return <p className="py-8 text-center text-sm text-zinc-600">No dead code found</p>
  }

  return (
    <Card className="border-zinc-800 bg-zinc-900">
      <Table>
        <TableHeader>
          <TableRow className="border-zinc-800 hover:bg-transparent">
            <TableHead className="text-zinc-400">Name</TableHead>
            <TableHead className="text-zinc-400">Kind</TableHead>
            <TableHead className="text-zinc-400">File</TableHead>
            <TableHead className="text-zinc-400">Line</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.map((entry) => (
            <TableRow key={entry.id} className="border-zinc-800">
              <TableCell>
                <Link
                  href={`/symbol/${encodeURIComponent(entry.id)}`}
                  className="font-mono text-xs text-blue-400 hover:text-blue-300 hover:underline"
                >
                  {entry.name}
                </Link>
              </TableCell>
              <TableCell>
                <Badge variant="outline" className="border-zinc-700 text-zinc-300">
                  {entry.kind}
                </Badge>
              </TableCell>
              <TableCell className="max-w-xs truncate font-mono text-xs text-zinc-400">
                {entry.file_path}
              </TableCell>
              <TableCell className="text-zinc-400">{entry.start_line}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Card>
  )
}

// --- Hotspots Tab ---
function HotspotsTab() {
  const [data, setData] = useState<HotspotEntry[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (data !== null) return
    setLoading(true)
    try {
      const result = await api.findHotspots()
      if (Array.isArray(result)) {
        setData(result as HotspotEntry[])
      } else if (
        result &&
        typeof result === 'object' &&
        'hotspots' in (result as Record<string, unknown>)
      ) {
        setData(
          ((result as Record<string, unknown>).hotspots as HotspotEntry[]) ?? []
        )
      } else {
        setData([])
      }
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load hotspots')
    } finally {
      setLoading(false)
    }
  }, [data])

  if (data === null && !loading && !error) {
    load()
  }

  if (loading) {
    return <p className="py-8 text-center text-sm text-zinc-500">Analyzing hotspots...</p>
  }
  if (error) {
    return <p className="py-8 text-center text-sm text-red-400">{error}</p>
  }
  if (!data || data.length === 0) {
    return <p className="py-8 text-center text-sm text-zinc-600">No hotspots found</p>
  }

  return (
    <div className="grid gap-3 lg:grid-cols-2">
      {data.map((entry, idx) => (
        <Card key={entry.id} className="border-zinc-800 bg-zinc-900">
          <CardHeader>
            <div className="flex items-start justify-between">
              <div>
                <CardTitle className="text-zinc-100">
                  <span className="mr-2 text-sm text-zinc-500">#{idx + 1}</span>
                  <Link
                    href={`/symbol/${encodeURIComponent(entry.id)}`}
                    className="font-mono text-sm text-blue-400 hover:text-blue-300 hover:underline"
                  >
                    {entry.name}
                  </Link>
                </CardTitle>
              </div>
              {entry.complexity_score !== undefined && (
                <Badge
                  variant="secondary"
                  className="bg-orange-500/10 text-orange-400 border-orange-500/20"
                >
                  {entry.complexity_score} risk
                </Badge>
              )}
            </div>
          </CardHeader>
          <CardContent>
            <div className="flex gap-4 text-xs text-zinc-400">
              <div>
                <span className="text-zinc-500">Fan-in: </span>
                <span className="text-zinc-200">{entry.fan_in}</span>
              </div>
              <div>
                <span className="text-zinc-500">Fan-out: </span>
                <span className="text-zinc-200">{entry.fan_out}</span>
              </div>
              <div>
                <span className="text-zinc-500">Crossings: </span>
                <span className="text-zinc-200">{entry.community_crossings}</span>
              </div>
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

// --- Cycles Tab ---
function CyclesTab() {
  const [data, setData] = useState<CycleResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (data !== null) return
    setLoading(true)
    try {
      const result = await api.findCycles()
      if (
        result &&
        typeof result === 'object' &&
        'cycles' in (result as Record<string, unknown>)
      ) {
        setData(result as CycleResult)
      } else if (Array.isArray(result)) {
        setData({ cycles: result as CycleGroup[], total: (result as CycleGroup[]).length })
      } else {
        setData({ cycles: [], total: 0 })
      }
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load cycles')
    } finally {
      setLoading(false)
    }
  }, [data])

  if (data === null && !loading && !error) {
    load()
  }

  if (loading) {
    return <p className="py-8 text-center text-sm text-zinc-500">Detecting cycles...</p>
  }
  if (error) {
    return <p className="py-8 text-center text-sm text-red-400">{error}</p>
  }
  if (!data || data.cycles.length === 0) {
    return <p className="py-8 text-center text-sm text-zinc-600">No cycles detected</p>
  }

  return (
    <div className="space-y-3">
      <p className="text-sm text-zinc-400">{data.total} cycle(s) detected</p>
      {data.cycles.map((cycle, idx) => (
        <Card key={idx} className="border-zinc-800 bg-zinc-900">
          <CardHeader>
            <div className="flex items-center justify-between">
              <CardTitle className="text-sm text-zinc-100">
                Cycle #{idx + 1} ({cycle.path?.length ?? 0} nodes)
                {cycle.kind && (
                  <span className="ml-2 text-xs font-normal text-zinc-500">{cycle.kind}</span>
                )}
              </CardTitle>
              <Badge variant="secondary" className={severityColor(cycle.severity)}>
                {severityLabel(cycle.severity)}
              </Badge>
            </div>
          </CardHeader>
          <CardContent>
            <div className="flex flex-wrap gap-1.5">
              {cycle.path?.map((node) => (
                <Badge
                  key={node}
                  variant="outline"
                  className="border-zinc-700 font-mono text-xs text-zinc-300"
                >
                  {node}
                </Badge>
              ))}
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

// --- Health Tab ---
function HealthTab() {
  const [data, setData] = useState<IndexHealth | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (data !== null) return
    setLoading(true)
    try {
      const result = await api.indexHealth()
      setData(result)
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load health')
    } finally {
      setLoading(false)
    }
  }, [data])

  if (data === null && !loading && !error) {
    load()
  }

  if (loading) {
    return <p className="py-8 text-center text-sm text-zinc-500">Checking index health...</p>
  }
  if (error) {
    return <p className="py-8 text-center text-sm text-red-400">{error}</p>
  }
  if (!data) {
    return <p className="py-8 text-center text-sm text-zinc-600">No health data</p>
  }

  const staleFiles =
    data.total_detected - data.successfully_indexed

  return (
    <div className="space-y-4">
      {/* Big score card */}
      <Card className="border-zinc-800 bg-zinc-900">
        <CardContent className="flex items-center gap-6 py-6">
          <div className="flex flex-col items-center">
            <p className="text-xs uppercase tracking-wider text-zinc-500">
              Health Score
            </p>
            <p className={`text-5xl font-bold ${healthScoreColor(data.health_score)}`}>
              {data.health_score}
            </p>
          </div>
          <div className="grid gap-2 text-sm text-zinc-400">
            <div>
              Nodes: <span className="text-zinc-200">{data.node_count.toLocaleString()}</span>
            </div>
            <div>
              Edges: <span className="text-zinc-200">{data.edge_count.toLocaleString()}</span>
            </div>
            <div>
              Indexed:{' '}
              <span className="text-zinc-200">
                {data.successfully_indexed} / {data.total_detected} files
              </span>
            </div>
            {staleFiles > 0 && (
              <div className="text-yellow-400">
                {staleFiles} stale/failed file(s)
              </div>
            )}
            {data.last_index_time && (
              <div>
                Last indexed:{' '}
                <span className="text-zinc-300">{data.last_index_time}</span>
              </div>
            )}
          </div>
        </CardContent>
      </Card>

      {/* Language coverage */}
      {data.language_coverage &&
        Object.keys(data.language_coverage).length > 0 && (
          <Card className="border-zinc-800 bg-zinc-900">
            <CardHeader>
              <CardTitle className="text-zinc-100">Language Coverage</CardTitle>
              <CardDescription className="text-zinc-500">
                Parser availability per language
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-4">
                {Object.entries(data.language_coverage).map(([lang, covered]) => (
                  <div
                    key={lang}
                    className="flex items-center gap-2 text-sm"
                  >
                    {covered ? (
                      <CheckCircle className="h-4 w-4 text-emerald-400" />
                    ) : (
                      <XCircle className="h-4 w-4 text-red-400" />
                    )}
                    <span className={covered ? 'text-zinc-200' : 'text-zinc-500'}>
                      {lang}
                    </span>
                  </div>
                ))}
              </div>
            </CardContent>
          </Card>
        )}
    </div>
  )
}

// --- Main Analysis Page ---
export default function AnalysisPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-zinc-100">Analysis</h1>
        <p className="text-sm text-zinc-500">
          Code quality analysis and index health
        </p>
      </div>

      <Tabs defaultValue="dead-code">
        <TabsList variant="line">
          <TabsTrigger value="dead-code">
            <Trash2 className="h-4 w-4" />
            Dead Code
          </TabsTrigger>
          <TabsTrigger value="hotspots">
            <Flame className="h-4 w-4" />
            Hotspots
          </TabsTrigger>
          <TabsTrigger value="cycles">
            <RefreshCw className="h-4 w-4" />
            Cycles
          </TabsTrigger>
          <TabsTrigger value="health">
            <HeartPulse className="h-4 w-4" />
            Health
          </TabsTrigger>
        </TabsList>

        <TabsContent value="dead-code">
          <DeadCodeTab />
        </TabsContent>
        <TabsContent value="hotspots">
          <HotspotsTab />
        </TabsContent>
        <TabsContent value="cycles">
          <CyclesTab />
        </TabsContent>
        <TabsContent value="health">
          <HealthTab />
        </TabsContent>
      </Tabs>
    </div>
  )
}
