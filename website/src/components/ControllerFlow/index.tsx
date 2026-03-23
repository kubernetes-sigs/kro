import React, { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { tokenize, Token } from '../../theme/CodeBlock/highlighter';
import codeStyles from '../../theme/CodeBlock/styles.module.css';
import styles from './styles.module.css';

type ModalKind = 'rgd' | 'instance';

const INSTANCE_YAML = `apiVersion: kro.run/v1alpha1
kind: WebApp
metadata:
  name: my-app
spec:
  image: nginx
  replicas: 1
  bucketName: my-app-assets`;

const RGD_YAML = `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: webapp
spec:
  schema:
    apiVersion: v1alpha1
    kind: WebApp
    spec:
      image: string | default=nginx
      replicas: integer | default=1
      bucketName: string
    status:
      endpoint: \${ingress.status.address}

  resources:
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: \${schema.metadata.name}-config

    - id: bucket
      template:
        apiVersion: s3.services.k8s.aws/v1alpha1
        kind: Bucket
        metadata:
          name: \${schema.spec.bucketName}

    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: \${schema.metadata.name}
        spec:
          replicas: \${schema.spec.replicas}
          selector:
            matchLabels:
              app: \${schema.metadata.name}
          template:
            metadata:
              labels:
                app: \${schema.metadata.name}
            spec:
              containers:
                - name: app
                  image: \${schema.spec.image}
                  envFrom:
                    - configMapRef:
                        name: \${config.metadata.name}
                  env:
                    - name: BUCKET_NAME
                      value: \${bucket.metadata.name}

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: \${schema.metadata.name}
        spec:
          selector: \${deployment.spec.selector.matchLabels}

    - id: hpa
      template:
        apiVersion: autoscaling/v2
        kind: HorizontalPodAutoscaler
        metadata:
          name: \${deployment.metadata.name}
        spec:
          scaleTargetRef:
            apiVersion: apps/v1
            kind: Deployment
            name: \${deployment.metadata.name}

    - id: ingress
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: \${service.metadata.name}`;

const RGD_TOKENS = tokenize(RGD_YAML, true, false);
const INSTANCE_TOKENS = tokenize(INSTANCE_YAML, false, false);

const tokenClassMap: Record<string, string> = {
  'cel': codeStyles.celExpression,
  'comment': codeStyles.comment,
  'yaml-key': codeStyles.yamlKey,
  'kro-keyword': codeStyles.kroKeyword,
  'schema-type': codeStyles.schemaType,
  'schema-pipe': codeStyles.schemaPipe,
  'schema-keyword': codeStyles.schemaKeyword,
  'schema-value': codeStyles.schemaValue,
};

function renderTokens(tokens: Token[]): React.ReactNode[] {
  return tokens.map((token, idx) => {
    if (token.type === 'text') {
      return token.value;
    }

    return (
      <span key={idx} className={tokenClassMap[token.type]}>
        {token.value}
      </span>
    );
  });
}

function ModalCodeBlock({
  title,
  tokens,
}: {
  title: string;
  tokens: Token[];
}) {
  return (
    <div className={`${codeStyles.codeBlockContainer} ${styles.modalCode}`}>
      <div className={codeStyles.codeBlockTitle}>{title}</div>
      <div className={codeStyles.codeBlockContent}>
        <pre className={codeStyles.kroCodeBlock}>
          <code>{renderTokens(tokens)}</code>
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

export default function ControllerFlow(): JSX.Element {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);
  const [activeModal, setActiveModal] = useState<ModalKind | null>(null);
  const [portalRoot, setPortalRoot] = useState<HTMLElement | null>(null);
  const markerBaseId = useId().replace(/:/g, '');
  const ids = [`${markerBaseId}-merge`, `${markerBaseId}-split`, `${markerBaseId}-down`, `${markerBaseId}-input`];

  useEffect(() => {
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setVisible(true);
          observer.disconnect();
        }
      },
      { threshold: 0.15 },
    );
    if (ref.current) observer.observe(ref.current);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (!activeModal) {
      return;
    }

    const previousOverflow = document.body.style.overflow;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setActiveModal(null);
      }
    };

    document.body.style.overflow = 'hidden';
    window.addEventListener('keydown', onKeyDown);

    return () => {
      document.body.style.overflow = previousOverflow;
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [activeModal]);

  useEffect(() => {
    setPortalRoot(document.body);
  }, []);

  const modal =
    activeModal && portalRoot
      ? createPortal(
          <div
            className={styles.modalOverlay}
            onMouseDown={(event) => {
              if (event.target === event.currentTarget) {
                setActiveModal(null);
              }
            }}
            role="presentation"
          >
            <div
              className={styles.modal}
              role="dialog"
              aria-modal="true"
            aria-labelledby={`${markerBaseId}-${activeModal}-title`}
            onMouseDown={(event) => event.stopPropagation()}
          >
            <div className={styles.modalHeader}>
              <h3
                id={`${markerBaseId}-${activeModal}-title`}
                className={styles.modalTitle}
              >
                {activeModal === 'rgd' ? 'WebApp ResourceGraphDefinition' : 'WebApp instance'}
              </h3>
                <button
                  type="button"
                  className={styles.modalClose}
                  onClick={() => setActiveModal(null)}
                  aria-label="Close dialog"
                >
                  <svg
                    viewBox="0 0 16 16"
                    className={styles.modalCloseIcon}
                    aria-hidden="true"
                  >
                    <path
                      d="M4 4l8 8M12 4l-8 8"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="1.8"
                      strokeLinecap="round"
                    />
                  </svg>
                </button>
              </div>

              <div className={styles.modalBody}>
                <ModalCodeBlock
                  title={activeModal === 'rgd' ? 'webapp-rgd.kro' : 'my-app.yaml'}
                  tokens={activeModal === 'rgd' ? RGD_TOKENS : INSTANCE_TOKENS}
                />
              </div>
            </div>
          </div>,
          portalRoot,
        )
      : null;

  return (
    <div ref={ref} className={`${styles.container} ${visible ? styles.visible : ''}`}>
      <div className={styles.flow}>
        {/* Left: RGD prototype + Instance */}
        <div className={styles.inputs}>
          <button
            type="button"
            className={`${styles.card} ${styles.rgdCard} ${styles.cardButton}`}
            onClick={() => setActiveModal('rgd')}
            aria-label="Open the ResourceGraphDefinition example"
          >
            <div className={styles.cardHeader}>
              <span className={styles.cardBadge}>RGD</span>
              <span className={styles.cardKind}>WebApp</span>
            </div>
            <div className={styles.cardFields}>
              <div className={styles.field}>
                <span className={styles.fieldKey}>image</span>
                <span className={styles.fieldType}>string</span>
              </div>
              <div className={styles.field}>
                <span className={styles.fieldKey}>replicas</span>
                <span className={styles.fieldType}>integer</span>
              </div>
              <div className={styles.field}>
                <span className={styles.fieldKey}>bucketName</span>
                <span className={styles.fieldType}>string</span>
              </div>
            </div>
          </button>

          <div className={styles.instantiateArrow}>
            <svg width="2" height="14" viewBox="0 0 2 14">
              <line x1="1" y1="0" x2="1" y2="14" stroke="currentColor" strokeWidth="1.5" strokeDasharray="3 2" />
            </svg>
            <svg width="8" height="5" viewBox="0 0 8 5">
              <polygon points="0,0 8,0 4,5" fill="currentColor" />
            </svg>
          </div>

          <button
            type="button"
            className={`${styles.card} ${styles.instanceCard} ${styles.cardButton}`}
            onClick={() => setActiveModal('instance')}
            aria-label="Open the instance example"
          >
            <div className={styles.cardHeader}>
              <span className={styles.cardBadge}>Instance</span>
              <span className={styles.cardKind}>my-app</span>
            </div>
            <div className={styles.cardFields}>
              <div className={styles.field}>
                <span className={styles.fieldKey}>image</span>
                <span className={styles.fieldVal}>nginx</span>
              </div>
              <div className={styles.field}>
                <span className={styles.fieldKey}>replicas</span>
                <span className={styles.fieldVal}>1</span>
              </div>
              <div className={styles.field}>
                <span className={styles.fieldKey}>bucketName</span>
                <span className={styles.fieldVal}>my-app-assets</span>
              </div>
            </div>
          </button>
        </div>

        {/* Inputs -> controller */}
        <div className={`${styles.inputWire} ${styles.stage2}`}>
          <svg viewBox="0 0 84 176" className={styles.inputWireSvg} preserveAspectRatio="none">
            <ArrowDef id={ids[3]} />
            <path
              d="M0,36 H46 V66 H82"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              className={styles.wireLine}
              markerEnd={`url(#${ids[3]})`}
            />
            <path
              d="M0,140 H46 V110 H82"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              className={styles.wireLine}
              markerEnd={`url(#${ids[3]})`}
            />
          </svg>
        </div>

        <div className={`${styles.stackWire} ${styles.stage2}`}>
          <svg viewBox="0 0 24 48" className={styles.wireSvgV} preserveAspectRatio="none">
            <line x1="12" y1="0" x2="12" y2="38" stroke="currentColor" strokeWidth="1.5" className={styles.wireLine} />
            <polygon points="8,38 16,38 12,48" fill="currentColor" className={styles.wireHead} />
          </svg>
        </div>

        {/* Controller */}
        <div className={`${styles.controller} ${styles.stage2}`}>
          <div className={styles.controllerInner}>
            <div className={styles.controllerIcon}>
              <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                <circle cx="12" cy="12" r="3" />
                <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
              </svg>
            </div>
            <div className={styles.controllerLabel}>kro</div>
          </div>
        </div>

        {/* Wire: controller → outputs */}
        <div className={`${styles.wire} ${styles.stage3} ${styles.desktopOutputWire}`}>
          <svg viewBox="0 0 60 24" className={styles.wireSvgH} preserveAspectRatio="none">
            <line x1="0" y1="12" x2="48" y2="12" stroke="currentColor" strokeWidth="1.5" className={styles.wireLine} />
            <polygon points="48,8 58,12 48,16" fill="currentColor" className={styles.wireHead} />
          </svg>
        </div>

        <div className={`${styles.stackWire} ${styles.stage3}`}>
          <svg viewBox="0 0 24 48" className={styles.wireSvgV} preserveAspectRatio="none">
            <line x1="12" y1="0" x2="12" y2="38" stroke="currentColor" strokeWidth="1.5" className={styles.wireLine} />
            <polygon points="8,38 16,38 12,48" fill="currentColor" className={styles.wireHead} />
          </svg>
        </div>

        {/* Output: resolved resource graph */}
        <div className={`${styles.outputs} ${styles.stage3}`}>
          <div className={styles.graphCard}>
            <div className={styles.dag}>
              <div className={styles.resourceNode}>
                <span className={styles.resKind}>ConfigMap</span>
                <span className={styles.resApi}>v1</span>
              </div>
              <div className={styles.resourceNode}>
                <span className={styles.resKind}>Bucket</span>
                <span className={styles.resApi}>s3.services.k8s.aws/v1alpha1</span>
              </div>

              <div className={styles.edgeRow}>
                <svg viewBox="0 0 380 58" className={styles.edgeSvg}>
                  <ArrowDef id={ids[0]} />
                  <path
                    d="M95,7 V26 H154 V51"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    markerEnd={`url(#${ids[0]})`}
                  />
                  <path
                    d="M285,7 V26 H226 V51"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    markerEnd={`url(#${ids[0]})`}
                  />
                </svg>
              </div>

              <div className={`${styles.resourceNode} ${styles.dagSpan}`}>
                <span className={styles.resKind}>Deployment</span>
                <span className={styles.resApi}>apps/v1</span>
              </div>

              <div className={styles.edgeRow}>
                <svg viewBox="0 0 380 58" className={styles.edgeSvg}>
                  <ArrowDef id={ids[1]} />
                  <path
                    d="M154,7 V26 H95 V51"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    markerEnd={`url(#${ids[1]})`}
                  />
                  <path
                    d="M226,7 V26 H285 V51"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    markerEnd={`url(#${ids[1]})`}
                  />
                </svg>
              </div>

              <div className={styles.resourceNode}>
                <span className={styles.resKind}>Service</span>
                <span className={styles.resApi}>v1</span>
              </div>
              <div className={styles.resourceNode}>
                <span className={styles.resKind}>Autoscaler</span>
                <span className={styles.resApi}>autoscaling/v2</span>
              </div>

              <div className={styles.edgeDown}>
                <svg viewBox="0 0 20 38" className={styles.edgeDownSvg}>
                  <ArrowDef id={ids[2]} />
                  <path
                    d="M10,7 V31"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    markerEnd={`url(#${ids[2]})`}
                  />
                </svg>
              </div>

              <div className={`${styles.resourceNode} ${styles.dagCol1}`}>
                <span className={styles.resKind}>Ingress</span>
                <span className={styles.resApi}>networking.k8s.io/v1</span>
              </div>
            </div>
          </div>
        </div>
      </div>
      {modal}
    </div>
  );
}
