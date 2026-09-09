import React, { useEffect, useRef, useState } from 'react';
import styles from './styles.module.css';

/**
 * Lexical frames in a nested Graph: a parent frame containing a `graph` node
 * whose child nodes can capture parent nodes, while the parent addresses the
 * child's nodes through the `graph` node's id.
 */

type Box = { id: string; x: number; y: number; w: number; label: string };

const boxes: Record<string, Box> = {
  shared:     { id: 'shared',     x: 50,  y: 70,  w: 110, label: 'shared' },
  ingress:    { id: 'ingress',    x: 50,  y: 205, w: 110, label: 'ingress' },
  deployment: { id: 'deployment', x: 285, y: 105, w: 120, label: 'deployment' },
  service:    { id: 'service',    x: 425, y: 105, w: 110, label: 'service' },
};
const H = 32;

const right = (b: Box) => ({ x: b.x + b.w, y: b.y + H / 2 });
const left = (b: Box) => ({ x: b.x, y: b.y + H / 2 });
const bottom = (b: Box) => ({ x: b.x + b.w / 2, y: b.y + H });
const top = (b: Box) => ({ x: b.x + b.w / 2, y: b.y });

export default function ScopeFrames(): JSX.Element {
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

  const b = boxes;

  return (
    <div ref={ref} className={`${styles.container} ${visible ? styles.visible : ''}`}>
      <svg className={styles.svg} viewBox="0 0 600 300" preserveAspectRatio="xMidYMid meet">
        <defs>
          <marker id="scope-arrow" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="8" markerHeight="6" markerUnits="userSpaceOnUse" orient="auto">
            <path d="M0,0.5 L9,4 L0,7.5Z" fill="var(--ifm-color-primary)" />
          </marker>
          <marker id="scope-arrow-dim" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="8" markerHeight="6" markerUnits="userSpaceOnUse" orient="auto">
            <path d="M0,0.5 L9,4 L0,7.5Z" fill="var(--ifm-color-emphasis-500)" />
          </marker>
        </defs>

        {/* Parent frame */}
        <g className={`${styles.step} ${styles.d0}`}>
          <rect x="15" y="15" width="570" height="270" rx="10" className={styles.parentFrame} />
          <text x="30" y="38" className={styles.frameTitle}>Graph</text>
          <text x="30" y="52" className={styles.frameSub}>spec.nodes</text>
        </g>

        {/* Child frame */}
        <g className={`${styles.step} ${styles.d1}`}>
          <rect x="255" y="60" width="300" height="150" rx="10" className={styles.childFrame} />
          <text x="270" y="82" className={styles.frameTitleChild}>backend</text>
          <text x="330" y="82" className={styles.frameSub}>graph: nodes</text>
        </g>

        {/* Edges (drawn before boxes so boxes sit on top) */}
        <g className={`${styles.step} ${styles.d2}`}>
          {/* deployment -> shared : capture */}
          <path
            d={`M${left(b.deployment).x},${left(b.deployment).y} C 220,${left(b.deployment).y} 210,${right(b.shared).y} ${right(b.shared).x + 2},${right(b.shared).y}`}
            className={styles.edge}
            markerEnd="url(#scope-arrow)"
          />
          <text x="212" y="140" textAnchor="middle" className={styles.edgeLabel}>${'{'}shared.app{'}'}</text>
          <text x="212" y="152" textAnchor="middle" className={styles.edgeNote}>capture</text>

          {/* service -> deployment : sibling */}
          <line
            x1={left(b.service).x} y1={left(b.service).y}
            x2={right(b.deployment).x + 2} y2={right(b.deployment).y}
            className={styles.edge}
            markerEnd="url(#scope-arrow)"
          />
          <text x="415" y="100" textAnchor="middle" className={styles.edgeLabel}>${'{'}deployment.metadata.name{'}'}</text>

          {/* ingress -> service : addressed through backend */}
          <path
            d={`M${right(b.ingress).x},${right(b.ingress).y} C 300,${right(b.ingress).y} ${bottom(b.service).x},200 ${bottom(b.service).x},${bottom(b.service).y + 2}`}
            className={styles.edge}
            markerEnd="url(#scope-arrow)"
          />
          <text x="300" y="248" textAnchor="middle" className={styles.edgeLabel}>${'{'}backend.service.metadata.name{'}'}</text>
          <text x="300" y="260" textAnchor="middle" className={styles.edgeNote}>addressed through the graph node's id</text>

          {/* ingress -> shared : ordinary same-frame reference */}
          <line
            x1={top(b.ingress).x} y1={top(b.ingress).y}
            x2={bottom(b.shared).x} y2={bottom(b.shared).y + 2}
            className={styles.edgeDim}
            markerEnd="url(#scope-arrow-dim)"
          />
          <text x="97" y="168" textAnchor="end" className={styles.edgeLabelDim}>${'{'}shared.app{'}'}</text>
          <text x="97" y="180" textAnchor="end" className={styles.edgeNote}>same frame</text>
        </g>

        {/* Boxes */}
        <g className={`${styles.step} ${styles.d1}`}>
          {Object.values(b).map((box) => (
            <g key={box.id}>
              <rect x={box.x} y={box.y} width={box.w} height={H} rx="6" className={styles.node} />
              <text x={box.x + box.w / 2} y={box.y + 20} textAnchor="middle" className={styles.nodeLabel}>{box.label}</text>
            </g>
          ))}
        </g>
      </svg>
    </div>
  );
}
