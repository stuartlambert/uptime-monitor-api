import { WINDOWS, type Window } from '../api/types'

interface Props {
  value: Window
  onChange: (w: Window) => void
}

export function WindowPicker({ value, onChange }: Props) {
  return (
    <div className="seg" role="group" aria-label="Time window">
      {WINDOWS.map((w) => (
        <button key={w} type="button" className={w === value ? 'on' : ''}
                aria-pressed={w === value} onClick={() => onChange(w)}>
          {w}
        </button>
      ))}
    </div>
  )
}
