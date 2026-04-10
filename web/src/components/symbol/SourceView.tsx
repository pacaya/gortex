'use client'

interface SourceViewProps {
  source: string
  startLine: number
  language: string
}

export function SourceView({ source, startLine, language }: SourceViewProps) {
  const lines = source.split('\n')
  // Remove trailing empty line if present
  if (lines.length > 0 && lines[lines.length - 1] === '') {
    lines.pop()
  }

  const maxLineNum = startLine + lines.length - 1
  const gutterWidth = String(maxLineNum).length

  return (
    <div className="overflow-x-auto rounded-lg border border-zinc-800 bg-zinc-900">
      <div className="flex items-center justify-between border-b border-zinc-800 px-4 py-2">
        <span className="text-xs text-zinc-500">{language}</span>
        <span className="text-xs text-zinc-600">
          {lines.length} line{lines.length !== 1 ? 's' : ''}
        </span>
      </div>
      <pre className="overflow-x-auto p-0 text-sm leading-relaxed">
        <code>
          {lines.map((line, i) => {
            const lineNum = startLine + i
            return (
              <div
                key={lineNum}
                className="flex hover:bg-zinc-800/50"
              >
                <span
                  className="sticky left-0 shrink-0 select-none bg-zinc-900 px-4 py-0 text-right text-zinc-600"
                  style={{ minWidth: `${gutterWidth + 3}ch` }}
                >
                  {lineNum}
                </span>
                <span className="flex-1 whitespace-pre px-4 py-0 font-mono text-zinc-300">
                  {line}
                </span>
              </div>
            )
          })}
        </code>
      </pre>
    </div>
  )
}
