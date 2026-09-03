import type { SeriesPoint } from '../api/types'

/** Builds an SVG path from values scaled into a viewBox. */
function linePath(values: number[], w: number, h: number, max: number): string {
  if (values.length === 0) return ''
  const step = values.length === 1 ? 0 : w / (values.length - 1)
  const y = (v: number) => h - (max > 0 ? (v / max) * h : 0)
  return values
    .map((v, i) => `${i === 0 ? 'M' : 'L'}${(i * step).toFixed(2)},${y(v).toFixed(2)}`)
    .join(' ')
}

interface SparklineProps {
  points: SeriesPoint[]
  field: 'avg_ms' | 'p95_ms' | 'uptime_percent'
}

/** Small inline trend, sized by CSS rather than props so it adapts in a table. */
export function Sparkline({ points, field }: SparklineProps) {
  const withData = points.filter((p) => p.checks > 0)
  if (withData.length < 2) {
    return <span className="subtle" style={{ fontSize: 12 }}>—</span>
  }
  const values = withData.map((p) => p[field])
  const max = Math.max(...values, 1)
  const W = 100
  const H = 26
  const d = linePath(values, W, H, max)
  const lastX = W
  const lastY = H - (values[values.length - 1] / max) * H

  return (
    <svg className="chart sparkline" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none"
         role="img" aria-label={`${field} trend`}>
      <path className="series-area" d={`${d} L${W},${H} L0,${H} Z`} />
      <path className="series-line" d={d} vectorEffect="non-scaling-stroke" />
      <circle className="endpoint" cx={lastX} cy={lastY} r={2.5} vectorEffect="non-scaling-stroke" />
    </svg>
  )
}

interface UptimeBarsProps {
  points: SeriesPoint[]
}

/** One bar per bucket. Empty buckets render grey rather than being skipped, so a
 *  monitoring gap is visible instead of being closed up by the neighbours. */
export function UptimeBars({ points }: UptimeBarsProps) {
  return (
    <div className="bars" role="img" aria-label="Uptime by period">
      {points.map((p, i) => {
        let cls = 'none'
        let height = '30%'
        if (p.checks > 0) {
          height = '100%'
          cls = p.uptime_percent >= 99.5 ? '' : p.uptime_percent >= 90 ? 'partial' : 'bad'
        }
        const title = p.checks === 0
          ? 'No checks in this period'
          : `${p.uptime_percent}% over ${p.checks} check${p.checks === 1 ? '' : 's'}`
        return (
          <div key={i} className={`bar ${cls}`} style={{ height }}>
            <title>{title}</title>
          </div>
        )
      })}
    </div>
  )
}

interface LineChartProps {
  points: SeriesPoint[]
  field: 'avg_ms' | 'p95_ms'
  label: string
  unit?: string
  height?: number
}

/** Latency over time, with a faint grid and a labelled y-axis. */
export function LineChart({ points, field, label, unit = 'ms', height = 160 }: LineChartProps) {
  const withData = points.filter((p) => p.checks > 0 && p[field] > 0)
  if (withData.length < 2) {
    return <div className="empty" style={{ padding: '28px 0' }}>Not enough data yet for {label.toLowerCase()}.</div>
  }
  const values = withData.map((p) => p[field])
  const max = Math.max(...values)
  const niceMax = Math.ceil(max / 10) * 10 || 10
  const W = 600
  const H = height
  const padL = 46
  const padB = 18
  const plotW = W - padL - 8
  const plotH = H - padB - 8

  const d = linePath(values, plotW, plotH, niceMax)
  const ticks = [0, 0.5, 1].map((f) => ({ f, v: Math.round(niceMax * f) }))
  const first = withData[0].ts
  const last = withData[withData.length - 1].ts

  return (
    <svg className="chart" viewBox={`0 0 ${W} ${H}`} height={H} role="img"
         aria-label={`${label} over time, peak ${niceMax}${unit}`}>
      <g transform={`translate(${padL},8)`}>
        {ticks.map(({ f, v }) => (
          <g key={f}>
            <line className="grid-line" x1={0} y1={plotH - f * plotH} x2={plotW} y2={plotH - f * plotH} />
            <text className="axis-label" x={-8} y={plotH - f * plotH + 3} textAnchor="end">
              {v}{unit}
            </text>
          </g>
        ))}
        <path className="series-area" d={`${d} L${plotW},${plotH} L0,${plotH} Z`} />
        <path className="series-line" d={d} vectorEffect="non-scaling-stroke" />
        <text className="axis-label" x={0} y={plotH + 14}>
          {new Date(first * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit' })}
        </text>
        <text className="axis-label" x={plotW} y={plotH + 14} textAnchor="end">
          {new Date(last * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit' })}
        </text>
      </g>
    </svg>
  )
}
