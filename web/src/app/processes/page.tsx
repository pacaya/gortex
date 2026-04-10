'use client'

import { useEffect, useState } from 'react'
import Link from 'next/link'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { api } from '@/lib/api'
import type { Process } from '@/lib/types'
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
} from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import {
  Table,
  TableHeader,
  TableBody,
  TableHead,
  TableRow,
  TableCell,
} from '@/components/ui/table'

function ProcessRow({ process }: { process: Process }) {
  const [expanded, setExpanded] = useState(false)
  const [detail, setDetail] = useState<Process | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)

  const handleToggle = async () => {
    const willExpand = !expanded
    setExpanded(willExpand)
    if (willExpand && !detail && !detailLoading) {
      setDetailLoading(true)
      try {
        const full = await api.getProcess(process.id)
        setDetail(full)
      } catch {
        // Fall back to summary data
      } finally {
        setDetailLoading(false)
      }
    }
  }

  const steps = detail?.steps ?? process.steps
  const files = detail?.files ?? process.files

  return (
    <>
      <TableRow
        className="cursor-pointer"
        onClick={handleToggle}
      >
        <TableCell className="text-zinc-400">
          {expanded ? (
            <ChevronDown className="h-4 w-4" />
          ) : (
            <ChevronRight className="h-4 w-4" />
          )}
        </TableCell>
        <TableCell className="font-medium text-zinc-100">
          {process.name}
        </TableCell>
        <TableCell className="font-mono text-xs text-zinc-400">
          {process.entry_point}
        </TableCell>
        <TableCell className="text-zinc-300">{process.step_count}</TableCell>
        <TableCell className="text-zinc-300">
          {process.file_count ?? process.files?.length ?? 0}
        </TableCell>
        <TableCell>
          <Badge
            variant="secondary"
            className="bg-zinc-800 text-zinc-300"
          >
            {process.score}
          </Badge>
        </TableCell>
      </TableRow>
      {expanded && (
        <TableRow>
          <TableCell />
          <TableCell colSpan={5}>
            {detailLoading ? (
              <div className="flex items-center gap-2 py-3 text-sm text-zinc-500">
                <div className="h-4 w-4 animate-spin rounded-full border-2 border-zinc-700 border-t-zinc-400" />
                Loading steps...
              </div>
            ) : (
              <div className="space-y-2 py-2">
                <p className="text-xs font-medium uppercase tracking-wider text-zinc-500">
                  Steps ({steps?.length ?? process.step_count ?? 0})
                </p>
                {steps && steps.length > 0 ? (
                  <ol className="space-y-1">
                    {steps.map((stepId, idx) => {
                      const isUnresolved = stepId.startsWith('unresolved::')
                      return (
                        <li key={stepId} className="flex items-center gap-2">
                          <span className="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-zinc-800 text-[10px] font-medium text-zinc-400">
                            {idx + 1}
                          </span>
                          {isUnresolved ? (
                            <span className="truncate font-mono text-xs text-zinc-600">
                              {stepId.replace('unresolved::', '')}
                              <span className="ml-1 text-zinc-700">(external)</span>
                            </span>
                          ) : (
                            <Link
                              href={`/symbol/${encodeURIComponent(stepId)}`}
                              className="truncate font-mono text-xs text-blue-400 hover:text-blue-300 hover:underline"
                              onClick={(e) => e.stopPropagation()}
                            >
                              {stepId}
                            </Link>
                          )}
                        </li>
                      )
                    })}
                  </ol>
                ) : (
                  <p className="text-xs text-zinc-500">
                    {process.step_count ?? 0} steps
                  </p>
                )}
                {files && files.length > 0 && (
                  <div className="mt-2">
                    <p className="text-xs font-medium uppercase tracking-wider text-zinc-500">
                      Files ({files.length})
                    </p>
                    <div className="mt-1 space-y-0.5">
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
              </div>
            )}
          </TableCell>
        </TableRow>
      )}
    </>
  )
}

export default function ProcessesPage() {
  const [processes, setProcesses] = useState<Process[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let mounted = true

    async function fetchData() {
      try {
        const result = await api.getProcesses()
        if (!mounted) return
        setProcesses(result.processes ?? [])
        setError(null)
      } catch (err) {
        if (!mounted) return
        setError(err instanceof Error ? err.message : 'Failed to load processes')
      } finally {
        if (mounted) setLoading(false)
      }
    }

    fetchData()
    return () => {
      mounted = false
    }
  }, [])

  const sorted = [...processes].sort((a, b) => b.score - a.score)

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-sm text-zinc-500">Loading processes...</p>
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
        <h1 className="text-xl font-semibold text-zinc-100">Processes</h1>
        <p className="text-sm text-zinc-500">
          Detected execution flows and business processes
        </p>
      </div>

      {sorted.length > 0 ? (
        <Card className="border-zinc-800 bg-zinc-900">
          <Table>
            <TableHeader>
              <TableRow className="border-zinc-800 hover:bg-transparent">
                <TableHead className="w-8" />
                <TableHead className="text-zinc-400">Name</TableHead>
                <TableHead className="text-zinc-400">Entry Point</TableHead>
                <TableHead className="text-zinc-400">Steps</TableHead>
                <TableHead className="text-zinc-400">Files</TableHead>
                <TableHead className="text-zinc-400">Score</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sorted.map((p) => (
                <ProcessRow key={p.id} process={p} />
              ))}
            </TableBody>
          </Table>
        </Card>
      ) : (
        <p className="py-8 text-center text-sm text-zinc-600">
          No processes detected
        </p>
      )}
    </div>
  )
}
