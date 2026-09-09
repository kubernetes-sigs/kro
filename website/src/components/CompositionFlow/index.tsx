import React, { useEffect, useRef, useState } from 'react';
import styles from './styles.module.css';

/**
 * Shows the two authoring APIs (ResourceGraphDefinition and Graph) converging
 * on the single composition engine that applies resources to the cluster.
 */
export default function CompositionFlow(): JSX.Element {
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

  const W = 600;
  const L = 150; // left column center (RGD)
  const R = 450; // right column center (Graph)
  const C = 300; // center

  return (
    <div ref={ref} className={`${styles.container} ${visible ? styles.visible : ''}`}>
      <svg className={styles.svg} viewBox={`0 0 ${W} 330`} preserveAspectRatio="xMidYMid meet">
        <defs>
          <marker id="comp-arrow" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="8" markerHeight="6" markerUnits="userSpaceOnUse" orient="auto">
            <path d="M0,0.5 L9,4 L0,7.5Z" fill="var(--ifm-color-emphasis-500)" />
          </marker>
          <marker id="comp-arrow-kro" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="8" markerHeight="6" markerUnits="userSpaceOnUse" orient="auto">
            <path d="M0,0.5 L9,4 L0,7.5Z" fill="var(--ifm-color-primary)" />
          </marker>
        </defs>

        {/* ── Row 1: authoring APIs ── */}
        <g className={`${styles.step} ${styles.d0}`}>
          <rect x={L - 105} y="10" width="210" height="46" rx="8" className={styles.apiBox} />
          <text x={L} y="30" textAnchor="middle" className={styles.apiTitle}>ResourceGraphDefinition</text>
          <text x={L} y="46" textAnchor="middle" className={styles.apiSub}>schema + resources</text>
        </g>
        <g className={`${styles.step} ${styles.d0}`}>
          <rect x={R - 105} y="10" width="210" height="46" rx="8" className={styles.apiBox} />
          <text x={R} y="30" textAnchor="middle" className={styles.apiTitle}>Graph</text>
          <text x={R} y="46" textAnchor="middle" className={styles.apiSub}>nodes</text>
        </g>

        {/* ── Row 2 (RGD only): generated CRD + instances ── */}
        <g className={`${styles.step} ${styles.d1}`}>
          <line x1={L} y1="56" x2={L} y2="78" stroke="var(--ifm-color-emphasis-500)" strokeWidth="1.5" markerEnd="url(#comp-arrow)" />
          <text x={L + 8} y="72" className={styles.edgeLabel}>generates</text>
          <rect x={L - 70} y="80" width="140" height="30" rx="6" className={styles.objBox} />
          <text x={L} y="99" textAnchor="middle" className={styles.objLabel}>CRD</text>

          <line x1={L} y1="110" x2={L} y2="130" stroke="var(--ifm-color-emphasis-500)" strokeWidth="1.5" markerEnd="url(#comp-arrow)" />
          {/* stacked instance chips */}
          <rect x={L - 62} y="140" width="140" height="30" rx="6" className={styles.objBoxDim} />
          <rect x={L - 66} y="136" width="140" height="30" rx="6" className={styles.objBoxDim} />
          <rect x={L - 70} y="132" width="140" height="30" rx="6" className={styles.objBox} />
          <text x={L} y="151" textAnchor="middle" className={styles.objLabel}>instance</text>
          <text x={L + 88} y="151" className={styles.edgeLabel}>× N</text>
        </g>

        {/* ── Row 3: both inputs converge on the engine ── */}
        {/* A Graph has no intermediate objects; it feeds the engine directly */}
        <g className={`${styles.step} ${styles.d1}`}>
          <path d={`M${R},56 V186 Q${R},196 ${R - 10},196 H${C}`} fill="none" stroke="var(--ifm-color-primary)" strokeWidth="1.5" />
          <text x={R + 8} y="128" className={styles.edgeLabel}>reconciled directly</text>
        </g>
        <g className={`${styles.step} ${styles.d2}`}>
          <path d={`M${L},170 V186 Q${L},196 ${L + 10},196 H${C}`} fill="none" stroke="var(--ifm-color-primary)" strokeWidth="1.5" />
          {/* merge point */}
          <circle cx={C} cy="196" r="3" fill="var(--ifm-color-primary)" />
          <line x1={C} y1="196" x2={C} y2="214" stroke="var(--ifm-color-primary)" strokeWidth="1.5" markerEnd="url(#comp-arrow-kro)" />
        </g>

        <g className={`${styles.step} ${styles.d3}`}>
          <rect x={C - 210} y="216" width="420" height="60" rx="10" className={styles.engineBox} />
          <text x={C} y="238" textAnchor="middle" className={styles.engineTitle}>kro composition engine</text>
          <text x={C} y="257" textAnchor="middle" className={styles.engineSub}>
            CEL · dependency order · includeWhen · readyWhen · forEach
          </text>
        </g>

        {/* ── Row 4: cluster ── */}
        <g className={`${styles.step} ${styles.d4}`}>
          <line x1={C} y1="276" x2={C} y2="294" stroke="var(--ifm-color-primary)" strokeWidth="1.5" markerEnd="url(#comp-arrow-kro)" />
          <rect x={C - 120} y="296" width="240" height="30" rx="6" className={styles.objBox} />
          <text x={C} y="315" textAnchor="middle" className={styles.objLabel}>Kubernetes resources</text>
        </g>
      </svg>
    </div>
  );
}
