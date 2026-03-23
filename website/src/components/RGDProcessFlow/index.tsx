import React, { useEffect, useRef, useState } from 'react';
import styles from './styles.module.css';

// Lane x-positions in the 600-wide viewBox
const USER = 100;
const API  = 300;
const KRO  = 500;

// Vertical spacing
const HEADER_BOTTOM = 60;
const STEP_START = 90;
const STEP_GAP = 45;

interface Step {
  num: number;
  label: string;
  from: number;  // x of source lane
  to: number;    // x of target lane
  kro?: boolean;
  self?: boolean;
}

const steps: Step[] = [
  { num: 1, label: 'Apply RGD',          from: USER, to: API },
  { num: 2, label: 'Watch RGDs',         from: KRO,  to: API,  kro: true },
  { num: 3, label: 'Validate',           from: KRO,  to: KRO,  kro: true, self: true },
  { num: 4, label: 'Create CRD',         from: KRO,  to: API,  kro: true },
  { num: 5, label: 'Create instance',    from: USER, to: API },
  { num: 6, label: 'Watch instances',    from: KRO,  to: API,  kro: true },
  { num: 7, label: 'Reconcile',          from: KRO,  to: KRO,  kro: true, self: true },
  { num: 8, label: 'Create resources',   from: KRO,  to: API,  kro: true },
];

const TOTAL_HEIGHT = STEP_START + steps.length * STEP_GAP + 20;

export default function RGDProcessFlow(): JSX.Element {
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
      <svg
        className={styles.svg}
        viewBox={`0 0 600 ${TOTAL_HEIGHT}`}
        preserveAspectRatio="xMidYMid meet"
      >
        <defs>
          <marker id="seq-arrow" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="8" markerHeight="6" markerUnits="userSpaceOnUse" orient="auto">
            <path d="M0,0.5 L9,4 L0,7.5Z" fill="currentColor" />
          </marker>
          <marker id="seq-arrow-kro" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="8" markerHeight="6" markerUnits="userSpaceOnUse" orient="auto">
            <path d="M0,0.5 L9,4 L0,7.5Z" fill="var(--ifm-color-primary)" />
          </marker>
        </defs>

        {/* Column headers */}
        <g className={styles.header}>
          {/* User */}
          <rect x={USER - 50} y="8" width="100" height="42" rx="8" fill="var(--ifm-background-color)" stroke="var(--ifm-color-emphasis-300)" strokeWidth="1.5" />
          <text x={USER} y="35" textAnchor="middle" className={styles.headerLabel}>User</text>

          {/* API Server */}
          <rect x={API - 60} y="8" width="120" height="42" rx="8" fill="var(--ifm-background-color)" stroke="var(--ifm-color-emphasis-300)" strokeWidth="1.5" />
          <text x={API} y="35" textAnchor="middle" className={styles.headerLabel}>API Server</text>

          {/* kro */}
          <rect x={KRO - 40} y="8" width="80" height="42" rx="8" fill="var(--ifm-background-color)" stroke="var(--ifm-color-primary)" strokeWidth="1.5" />
          <text x={KRO} y="35" textAnchor="middle" className={styles.headerLabelKro}>kro</text>
        </g>

        {/* Vertical lane lines */}
        <line x1={USER} y1={HEADER_BOTTOM} x2={USER} y2={TOTAL_HEIGHT} stroke="var(--ifm-color-emphasis-200)" strokeWidth="1.5" strokeDasharray="4 4" />
        <line x1={API}  y1={HEADER_BOTTOM} x2={API}  y2={TOTAL_HEIGHT} stroke="var(--ifm-color-emphasis-200)" strokeWidth="1.5" strokeDasharray="4 4" />
        <line x1={KRO}  y1={HEADER_BOTTOM} x2={KRO}  y2={TOTAL_HEIGHT} stroke="rgba(91, 127, 201, 0.25)" strokeWidth="1.5" strokeDasharray="4 4" />

        {/* Steps */}
        {steps.map((step, i) => {
          const y = STEP_START + i * STEP_GAP;
          const color = step.kro ? 'var(--ifm-color-primary)' : 'var(--ifm-color-emphasis-600)';
          const markerUrl = step.kro ? 'url(#seq-arrow-kro)' : 'url(#seq-arrow)';

          if (step.self) {
            // Self-loop: small loop to the right of the kro lane
            return (
              <g key={step.num} className={`${styles.step} ${styles[`delay${i}`]}`}>
                <path
                  d={`M${KRO},${y} h30 v20 h-30`}
                  fill="none"
                  stroke={color}
                  strokeWidth="1.5"
                  markerEnd={markerUrl}
                />
                <text x={KRO + 38} y={y + 6} className={step.kro ? styles.stepLabelKro : styles.stepLabel}>
                  {step.label}
                </text>
                {/* Number on top of the lane line */}
                <circle cx={KRO} cy={y} r="10" className={step.kro ? styles.numBgKro : styles.numBg} />
                <text x={KRO} y={y + 4} textAnchor="middle" className={styles.numText}>{step.num}</text>
              </g>
            );
          }

          const fromX = step.from;
          const toX = step.to;
          const dir = fromX < toX ? 1 : -1;
          // Arrow line with gap for number circle
          const lineFromX = fromX + dir * 2;
          const lineToX = toX - dir * 2;
          const midX = (fromX + toX) / 2;

          return (
            <g key={step.num} className={`${styles.step} ${styles[`delay${i}`]}`}>
              {/* Arrow line */}
              <line
                x1={lineFromX} y1={y}
                x2={lineToX} y2={y}
                stroke={color}
                strokeWidth="1.5"
                markerEnd={markerUrl}
              />
              {/* Label above arrow */}
              <text x={midX} y={y - 8} textAnchor="middle" className={step.kro ? styles.stepLabelKro : styles.stepLabel}>
                {step.label}
              </text>
              {/* Number circle at source */}
              <circle cx={fromX} cy={y} r="10" className={step.kro ? styles.numBgKro : styles.numBg} />
              <text x={fromX} y={y + 4} textAnchor="middle" className={styles.numText}>{step.num}</text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}
