import React, { useEffect, useId, useRef, useState } from 'react';
import { tokenize, Token } from '../../theme/CodeBlock/highlighter';
import codeStyles from '../../theme/CodeBlock/styles.module.css';
import styles from './styles.module.css';

const SCHEMA_CODE = `spec:
  schema:
    kind: WebApp
    spec:
      image: string | default=nginx
      replicas: integer | default=1
      bucketName: string | required=true
    status:
      bucketArn: \${bucket.status.arn}
      endpoint: \${service.status.endpoint}`;

const RESOURCES_CODE = `spec:
  resources:
    - id: config
      template:
        kind: ConfigMap
        # ...
        name: \${schema.metadata.name}-config
    - id: bucket
      template:
        kind: Bucket
        # ...
        name: \${schema.spec.bucketName}
    - id: deployment
      template:
        kind: Deployment
        # ...
        image: \${schema.spec.image}
        env: \${bucket.status.arn}
    - id: service
      template:
        kind: Service
        # ...
        name: \${deployment.metadata.name}`;

const INSTANCE_CODE = `apiVersion: kro.run/v1alpha1
kind: WebApp
metadata:
  name: my-app
spec:
  image: nginx
  bucketName: my-app-assets`;

const INSTANCE_STATUS_CODE = `apiVersion: kro.run/v1alpha1
kind: WebApp
metadata:
  name: my-app
spec:
  image: nginx
  bucketName: my-app-assets
status:
  bucketArn: arn:aws:s3:::my-app-assets
  endpoint: my-app.example.com`;

const SCHEMA_TOKENS = tokenize(SCHEMA_CODE, true, true);
const RESOURCES_TOKENS = tokenize(RESOURCES_CODE, true, false);
const INSTANCE_TOKENS = tokenize(INSTANCE_CODE, false, false);
const INSTANCE_STATUS_TOKENS = tokenize(INSTANCE_STATUS_CODE, false, false);

const styleMap: Record<string, string> = {
  'cel': codeStyles.celExpression,
  'comment': codeStyles.comment,
  'yaml-key': codeStyles.yamlKey,
  'kro-keyword': codeStyles.kroKeyword,
  'schema-type': codeStyles.schemaType,
  'schema-pipe': codeStyles.schemaPipe,
  'schema-keyword': codeStyles.schemaKeyword,
  'schema-value': codeStyles.schemaValue,
};

function KroCode({
  code = '',
  tokens,
  isKro = true,
  alwaysSchema = false,
}: {
  code?: string;
  tokens?: Token[];
  isKro?: boolean;
  alwaysSchema?: boolean;
}) {
  const resolvedTokens = tokens ?? tokenize(code, isKro, alwaysSchema);
  return (
    <div className={codeStyles.codeBlockContainer}>
      <div className={codeStyles.codeBlockContent}>
        <pre className={codeStyles.kroCodeBlock}>
          <code>
            {resolvedTokens.map((token, idx) => {
              if (token.type === 'text') return token.value;
              const cls = styleMap[token.type];
              return <span key={idx} className={cls}>{token.value}</span>;
            })}
          </code>
        </pre>
      </div>
    </div>
  );
}

function ArrowDef({ id }: { id: string }) {
  return (
    <defs>
      <marker
        id={id}
        viewBox="0 0 10 8"
        refX="10"
        refY="4"
        markerWidth="10"
        markerHeight="8"
        markerUnits="userSpaceOnUse"
        orient="auto"
      >
        <path d="M0,0 L10,4 L0,8Z" fill="currentColor" />
      </marker>
    </defs>
  );
}

export default function IntroFlow(): JSX.Element {
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
      <div className={styles.mainRow}>
        {/* Left: what users create */}
        <FlipCard
          label="Users create"
          flipLabel="How it's defined"
          front={<InstanceFront />}
          back={<KroCode tokens={SCHEMA_TOKENS} />}
          delayClass={styles.delay1}
          panelClass={styles.panelNarrow}
        />

        {/* Connector */}
        <div className={styles.connector}>
          <div className={styles.connectorDot} />
        </div>

        {/* Right: what kro creates */}
        <FlipCard
          label="kro creates"
          flipLabel="How it's defined"
          front={<DagFront />}
          back={<KroCode tokens={RESOURCES_TOKENS} />}
          delayClass={styles.delay2}
          panelClass={styles.panelWide}
        />
      </div>
    </div>
  );
}

/* ── Flip card ── */

function FlipCard({
  label,
  flipLabel,
  front,
  back,
  delayClass,
  panelClass,
}: {
  label: string;
  flipLabel: string;
  front: React.ReactNode;
  back: React.ReactNode;
  delayClass: string;
  panelClass?: string;
}) {
  const [flipped, setFlipped] = useState(false);

  return (
    <div className={`${styles.panel} ${delayClass} ${panelClass ?? ''}`}>
      <div className={styles.panelLabel}>{flipped ? flipLabel : label}</div>
      <button
        className={`${styles.flipBtn} ${flipped ? styles.flipBtnActive : ''}`}
        onClick={() => setFlipped(!flipped)}
        aria-label={flipped ? 'Show result' : 'Show definition'}
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
          {flipped ? (
            /* back arrow */
            <>
              <path d="M9 14L4 9l5-5" />
              <path d="M4 9h11a4 4 0 0 1 0 8h-1" />
            </>
          ) : (
            /* horizontal flip arrows */
            <>
              <path d="M17 1l4 4-4 4" />
              <path d="M3 11V9a4 4 0 0 1 4-4h14" />
              <path d="M7 23l-4-4 4-4" />
              <path d="M21 13v2a4 4 0 0 1-4 4H3" />
            </>
          )}
        </svg>
      </button>
      <div className={`${styles.flipContainer} ${flipped ? styles.flipped : ''}`}>
        <div className={styles.flipFront}>{front}</div>
        <div className={styles.flipBack}>{back}</div>
      </div>
    </div>
  );
}

/* ── Front: Instance YAML ── */

function InstanceFront() {
  return (
    <div className={styles.instanceStack}>
      <KroCode tokens={INSTANCE_TOKENS} />
      <div className={styles.instanceLabel}>User sees</div>
      <KroCode tokens={INSTANCE_STATUS_TOKENS} />
    </div>
  );
}

/* ── Front: DAG ── */

function DagFront() {
  const markerBaseId = useId().replace(/:/g, '');
  const topId = `${markerBaseId}-top`;
  const downId = `${markerBaseId}-down`;

  return (
    <div className={styles.dagGraph}>
      <div className={styles.miniDag}>
        <DagNode kind="ConfigMap" api="v1" />
        <DagNode kind="Bucket" api="s3.services.k8s.aws/v1alpha1" />

        <div className={styles.miniEdgeRow}>
          <svg viewBox="0 0 380 58" className={styles.miniEdgeSvg}>
            <ArrowDef id={topId} />
            <path
              d="M95,7 V26 H154 V51"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
              markerEnd={`url(#${topId})`}
            />
            <path
              d="M285,7 V26 H226 V51"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
              markerEnd={`url(#${topId})`}
            />
          </svg>
        </div>

        <DagNode kind="Deployment" api="apps/v1" span />

        <div className={styles.miniEdgeCenter}>
          <svg viewBox="0 0 20 38" className={styles.miniEdgeDownSvg}>
            <ArrowDef id={downId} />
            <path
              d="M10,7 V31"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              markerEnd={`url(#${downId})`}
            />
          </svg>
        </div>

        <DagNode kind="Service" api="v1" span />
      </div>
    </div>
  );
}

function DagNode({
  kind,
  api,
  span = false,
  col1 = false,
}: {
  kind: string;
  api: string;
  span?: boolean;
  col1?: boolean;
}) {
  return (
    <div
      className={[
        styles.miniNode,
        span ? styles.miniSpan : '',
        col1 ? styles.miniCol1 : '',
      ].filter(Boolean).join(' ')}
    >
      <span className={styles.miniKind}>{kind}</span>
      <span className={styles.miniApi}>{api}</span>
    </div>
  );
}
