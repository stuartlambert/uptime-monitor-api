interface Props {
  up: boolean | null
  enabled?: boolean
}

/** Encodes site state as shape and colour, not just text, so a dashboard row
 *  reads at a glance. `null` is "no checks yet", which is distinct from down. */
export function StatusPill({ up, enabled = true }: Props) {
  if (!enabled) {
    return <span className="pill paused"><span className="dot" />Paused</span>
  }
  if (up === null || up === undefined) {
    return <span className="pill unknown"><span className="dot" />No data</span>
  }
  return up
    ? <span className="pill up"><span className="dot" />Up</span>
    : <span className="pill down"><span className="dot" />Down</span>
}
