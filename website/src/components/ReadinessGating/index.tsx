import React, { useEffect, useRef, useState } from 'react';
import styles from './styles.module.css';

/**
 * Two timelines showing when a dependent resource is applied relative to its
 * dependency's readyWhen: gated in an RGD, not gated in a Graph.
 */

// Horizontal time axis (x in a 600-wide viewBox)
const T0 = 150;      // database applied
const T_FIELD = 300; // database.status.endpoint published
const T_READY = 440; // database readyWhen becomes true
const T_END = 575;

const LABEL_X = 12;
const ROW_H = 34;

interface Row {
  label: string;
  start: number;         // x where the bar starts
  waitUntil?: number;    // x until which the row is "waiting" (dashed)
  marks?: { x: number; text: string; above?: boolean }[];
}

interface Panel {
  title: string;
  rows: Row[];
  readyAt: number;
  note: string;
}

const panels: Panel[] = [
  {
    title: 'ResourceGraphDefinition',
    rows: [
      {
        label: 'database',
        start: T0,
        marks: [
          { x: T_FIELD, text: 'status.endpoint set' },
          { x: T_READY, text: 'readyWhen true', above: true },
        ],
      },
      { label: 'app', start: T_READY, waitUntil: T_READY },
    ],
    readyAt: T_READY,
    note: 'app waits for database to be ready',
  },
  {
    title: 'Graph',
    rows: [
      {
        label: 'database',
        start: T0,
        marks: [
          { x: T_FIELD, text: 'status.endpoint set' },
          { x: T_READY, text: 'readyWhen true', above: true },
        ],
      },
      { label: 'app', start: T_FIELD, waitUntil: T_FIELD },
    ],
    readyAt: T_READY,
    note: 'app is applied as soon as status.endpoint exists',
  },
];

const ROW0 = 44;  // y of the first row's center, below the panel title
const PANEL_H = ROW0 + ROW_H + 7 + 36;
const TOTAL_H = panels.length * (PANEL_H + 18) + 6;

export default function ReadinessGating(): JSX.Element {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setVisible(true);
          observer.disconnect();
        }
      },
      { threshold: 0.1 },
    );
    if (ref.current) observer.observe(ref.current);
    return () => observer.disconnect();
  }, []);

  return (
    <div ref={ref} className={`${styles.container} ${visible ? styles.visible : ''}`}>
      <svg className={styles.svg} viewBox={`0 0 600 ${TOTAL_H}`} preserveAspectRatio="xMidYMid meet">
        {panels.map((panel, pi) => {
          const top = 6 + pi * (PANEL_H + 18);
          const rowY = (i: number) => top + ROW0 + i * ROW_H;
          return (
            <g key={panel.title} className={`${styles.panel} ${styles[`d${pi}`]}`}>
              <rect x="2" y={top} width="596" height={PANEL_H} rx="8" className={styles.panelBox} />
              <text x={LABEL_X} y={top + 16} className={styles.panelTitle}>{panel.title}</text>

              {/* time axis guide lines */}
              {[T_FIELD, T_READY].map((x) => (
                <line key={x} x1={x} y1={rowY(0) - 8} x2={x} y2={top + PANEL_H - 24} className={styles.guide} />
              ))}

              {panel.rows.map((row, ri) => {
                const y = rowY(ri);
                return (
                  <g key={row.label}>
                    <text x={LABEL_X} y={y + 4} className={styles.rowLabel}>{row.label}</text>

                    {/* waiting segment */}
                    {row.waitUntil && row.waitUntil > T0 && (
                      <line x1={T0} y1={y} x2={row.waitUntil - 4} y2={y} className={styles.waitLine} />
                    )}

                    {/* applied bar */}
                    <rect x={row.start} y={y - 7} width={T_END - row.start} height="14" rx="4" className={styles.bar} />
                    <text x={row.start + 6} y={y + 4} className={styles.barLabel}>applied</text>

                    {/* marks */}
                    {row.marks?.map((m) => (
                      <g key={m.text}>
                        <circle cx={m.x} cy={y} r="4.5" className={styles.mark} />
                        <text
                          x={m.x}
                          y={m.above ? y - 13 : y + 19}
                          textAnchor="middle"
                          className={styles.markLabel}
                        >
                          {m.text}
                        </text>
                      </g>
                    ))}
                  </g>
                );
              })}

              {/* Ready marker + note */}
              <g>
                <circle cx={panel.readyAt} cy={top + PANEL_H - 14} r="4.5" className={styles.readyMark} />
                <text x={panel.readyAt + 9} y={top + PANEL_H - 10} className={styles.readyLabel}>Ready</text>
                <text x={LABEL_X} y={top + PANEL_H - 10} className={styles.note}>{panel.note}</text>
              </g>
            </g>
          );
        })}
      </svg>
    </div>
  );
}
