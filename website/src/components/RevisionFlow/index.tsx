import React, { useCallback, useEffect, useRef, useState } from 'react';
import styles from './styles.module.css';

type Pt = { x: number; y: number };

interface Edge {
  from: Pt;
  to: Pt;
  color: string;
  dash?: boolean;
  dim?: boolean;
}

function edgesFromRefs(
  graphEl: HTMLDivElement,
  refs: Record<string, HTMLDivElement | null>,
): Edge[] {
  const gr = graphEl.getBoundingClientRect();
  const w = gr.width;
  const h = gr.height;

  function right(key: string): Pt {
    const r = refs[key]?.getBoundingClientRect();
    if (!r) return { x: 0, y: 0 };
    return { x: ((r.right - gr.left) / w) * 900, y: ((r.top + r.height / 2 - gr.top) / h) * 340 };
  }

  function left(key: string): Pt {
    const r = refs[key]?.getBoundingClientRect();
    if (!r) return { x: 0, y: 0 };
    return { x: ((r.left - gr.left) / w) * 900, y: ((r.top + r.height / 2 - gr.top) / h) * 340 };
  }

  const blue = 'var(--ifm-color-primary)';
  const green = '#34a853';

  return [
    { from: right('rgd'), to: left('gr1'), color: blue, dim: true },
    { from: right('rgd'), to: left('gr2'), color: blue, dim: true },
    { from: right('rgd'), to: left('gr3'), color: blue },
    { from: right('gr1'), to: left('c1'), color: green, dash: true, dim: true },
    { from: right('gr2'), to: left('c2'), color: green, dash: true, dim: true },
    { from: right('gr3'), to: left('c3'), color: green, dash: true },
    { from: left('i1'), to: right('c1'), color: blue },
    { from: left('i2'), to: right('c3'), color: blue },
  ];
}

function VerticalArrow({ label, color }: { label: string; color?: string }) {
  return (
    <div className={styles.vArrow}>
      <svg width="20" height="28" viewBox="0 0 20 28">
        <line x1="10" y1="0" x2="10" y2="20" stroke={color || 'var(--ifm-color-emphasis-400)'} strokeWidth="1.5" strokeDasharray="4 3" />
        <polygon points="6,20 14,20 10,28" fill={color || 'var(--ifm-color-emphasis-400)'} opacity="0.7" />
      </svg>
      <span className={styles.vArrowLabel} style={color ? { color } : undefined}>{label}</span>
    </div>
  );
}

export default function RevisionFlow(): JSX.Element {
  const containerRef = useRef<HTMLDivElement>(null);
  const graphRef = useRef<HTMLDivElement>(null);
  const nodeRefs = useRef<Record<string, HTMLDivElement | null>>({});
  const [visible, setVisible] = useState(false);
  const [edges, setEdges] = useState<Edge[]>([]);
  const [wide, setWide] = useState(typeof window !== 'undefined' ? window.innerWidth > 1500 : true);

  const setNodeRef = useCallback((key: string) => (el: HTMLDivElement | null) => {
    nodeRefs.current[key] = el;
  }, []);

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
    if (containerRef.current) observer.observe(containerRef.current);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    function measure() {
      const isWide = window.innerWidth > 1500;
      setWide(isWide);
      if (!isWide || !graphRef.current) {
        setEdges([]);
        return;
      }
      setEdges(edgesFromRefs(graphRef.current, nodeRefs.current));
    }

    measure();
    const timer = setTimeout(measure, 200);
    window.addEventListener('resize', measure);
    return () => {
      clearTimeout(timer);
      window.removeEventListener('resize', measure);
    };
  }, [visible]);

  // Shared node markup
  const rgdNode = (ref?: (el: HTMLDivElement | null) => void, style?: React.CSSProperties) => (
    <div ref={ref} className={`${styles.node} ${styles.nodeK8s} ${styles.nodeRgd}`} style={style}>
      <div className={styles.nodeHeader}>
        <span className={`${styles.badge} ${styles.badgeRgd}`}>RGD</span>
      </div>
      <div className={styles.nodeName}>my-webapp</div>
      <div className={styles.nodeDetail}>schema + 3 resources</div>
    </div>
  );

  const grNode = (key: string, name: string, rev: string, old: boolean, latest: boolean, ref?: (el: HTMLDivElement | null) => void, style?: React.CSSProperties) => (
    <div ref={ref} className={`${styles.node} ${styles.nodeK8s} ${old ? styles.nodeOld : ''} ${latest ? styles.nodeLatest : ''}`} style={style}>
      <div className={styles.nodeHeader}>
        <span className={`${styles.badge} ${styles.badgeGr}`}>GraphRevision</span>
      </div>
      <div className={styles.nodeName}>{name}</div>
      <div className={styles.nodeDetail}>{rev}</div>
    </div>
  );

  const compiledNode = (key: string, detail: string, old: boolean, active: boolean, ref?: (el: HTMLDivElement | null) => void, style?: React.CSSProperties) => (
    <div ref={ref} className={`${styles.node} ${styles.nodeMem} ${old ? styles.nodeOld : ''} ${active ? styles.nodeActive : ''}`} style={style}>
      <div className={styles.nodeHeader}>
        <span className={styles.memIcon}>&#9881;</span>
      </div>
      <div className={styles.nodeName}>Compiled</div>
      <div className={styles.nodeDetail}>{detail}</div>
    </div>
  );

  const instanceNode = (key: string, name: string, rev: string, ref?: (el: HTMLDivElement | null) => void, style?: React.CSSProperties) => (
    <div ref={ref} className={`${styles.node} ${styles.nodeK8s}`} style={style}>
      <div className={styles.nodeHeader}>
        <span className={`${styles.badge} ${styles.badgeInstance}`}>Instance</span>
      </div>
      <div className={styles.nodeName}>{name}</div>
      <div className={styles.nodeRevRef}>{rev}</div>
    </div>
  );

  // ── Wide layout (graph with SVG edges) ──
  if (wide) {
    return (
      <div ref={containerRef} className={`${styles.container} ${visible ? styles.visible : ''}`}>
        <div ref={graphRef} className={styles.graph}>
          {edges.length > 0 && (
            <svg className={styles.edges} viewBox="0 0 900 340" preserveAspectRatio="xMidYMid meet">
              <defs>
                <marker id="rf-a-blue" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="9" markerHeight="7" markerUnits="userSpaceOnUse" orient="auto">
                  <path d="M0,0.5 L9,4 L0,7.5Z" fill="var(--ifm-color-primary)" />
                </marker>
                <marker id="rf-a-green" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="9" markerHeight="7" markerUnits="userSpaceOnUse" orient="auto">
                  <path d="M0,0.5 L9,4 L0,7.5Z" fill="#34a853" />
                </marker>
              </defs>

              {edges.map((e, i) => (
                <line
                  key={i}
                  x1={e.from.x} y1={e.from.y}
                  x2={e.to.x} y2={e.to.y}
                  stroke={e.color}
                  strokeWidth="1.5"
                  strokeDasharray={e.dash ? '6 3' : undefined}
                  markerEnd={e.color.includes('34a853') ? 'url(#rf-a-green)' : 'url(#rf-a-blue)'}
                  opacity={e.dim ? 0.45 : 0.8}
                  className={styles.edge}
                />
              ))}

              <text x="215" y="148" className={styles.edgeLabel} fill="var(--ifm-color-primary)">issues</text>
              <text x="448" y="220" className={styles.edgeLabel} fill="#34a853">compiles</text>
              {edges.length >= 8 && (
                <text
                  x={edges[6].from.x + 30}
                  y={(edges[6].from.y + edges[7].from.y) / 2}
                  className={styles.edgeLabel}
                  fill="var(--ifm-color-primary)"
                >resolves</text>
              )}
            </svg>
          )}

          <div className={styles.nodes}>
            {rgdNode(setNodeRef('rgd'), { left: '2%', top: '48%', transform: 'translateY(-50%)' })}
            {grNode('gr1', 'r00001', 'rev 1', true, false, setNodeRef('gr1'), { left: '33%', top: '5%' })}
            {grNode('gr2', 'r00002', 'rev 2', true, false, setNodeRef('gr2'), { left: '33%', top: '39%' })}
            {grNode('gr3', 'r00003', 'rev 3', false, true, setNodeRef('gr3'), { left: '33%', top: '73%' })}
            {compiledNode('c1', 'rev 1', true, false, setNodeRef('c1'), { left: '58%', top: '5%' })}
            {compiledNode('c2', 'rev 2', true, false, setNodeRef('c2'), { left: '58%', top: '39%' })}
            {compiledNode('c3', 'rev 3 \u00b7 Active', false, true, setNodeRef('c3'), { left: '58%', top: '73%' })}
            {instanceNode('i1', 'my-webapp-abc', 'using rev 1', setNodeRef('i1'), { left: '83%', top: '17%' })}
            {instanceNode('i2', 'my-webapp-xyz', 'using rev 3', setNodeRef('i2'), { left: '83%', top: '61%' })}
          </div>
        </div>

        <div className={styles.legend}>
          <div className={styles.legendItem}>
            <span className={`${styles.legendSwatch} ${styles.legendK8s}`} />
            <span className={styles.legendText}>Kubernetes object</span>
          </div>
          <div className={styles.legendItem}>
            <span className={`${styles.legendSwatch} ${styles.legendMem}`} />
            <span className={styles.legendText}>In-memory</span>
          </div>
        </div>
      </div>
    );
  }

  // ── Narrow layout (vertical flow) ──
  return (
    <div ref={containerRef} className={`${styles.container} ${visible ? styles.visible : ''}`}>
      <div className={styles.vertical}>
        {/* RGD */}
        <div className={styles.vSection}>
          <div className={styles.vSectionLabel}>ResourceGraphDefinition</div>
          {rgdNode()}
        </div>

        <VerticalArrow label="issues" color="var(--ifm-color-primary)" />

        {/* GraphRevisions */}
        <div className={styles.vSection}>
          <div className={styles.vSectionLabel}>GraphRevisions</div>
          <div className={styles.vGroup}>
            {grNode('gr1', 'r00001', 'rev 1', true, false)}
            {grNode('gr2', 'r00002', 'rev 2', true, false)}
            {grNode('gr3', 'r00003', 'rev 3', false, true)}
          </div>
        </div>

        <VerticalArrow label="compiles into" color="#34a853" />

        {/* Compiled Graphs */}
        <div className={styles.vSection}>
          <div className={styles.vSectionLabel}>Compiled Graphs (in-memory)</div>
          <div className={styles.vGroup}>
            {compiledNode('c1', 'rev 1', true, false)}
            {compiledNode('c2', 'rev 2', true, false)}
            {compiledNode('c3', 'rev 3 \u00b7 Active', false, true)}
          </div>
        </div>

        <VerticalArrow label="resolves from" color="var(--ifm-color-primary)" />

        {/* Instances */}
        <div className={styles.vSection}>
          <div className={styles.vSectionLabel}>Instances</div>
          <div className={styles.vGroup}>
            {instanceNode('i1', 'my-webapp-abc', 'using rev 1')}
            {instanceNode('i2', 'my-webapp-xyz', 'using rev 3')}
          </div>
        </div>
      </div>

      <div className={styles.legend}>
        <div className={styles.legendItem}>
          <span className={`${styles.legendSwatch} ${styles.legendK8s}`} />
          <span className={styles.legendText}>Kubernetes object</span>
        </div>
        <div className={styles.legendItem}>
          <span className={`${styles.legendSwatch} ${styles.legendMem}`} />
          <span className={styles.legendText}>In-memory</span>
        </div>
      </div>
    </div>
  );
}
