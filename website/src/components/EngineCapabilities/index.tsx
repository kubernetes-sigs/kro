import React, { useEffect, useRef, useState } from 'react';
import Link from '@docusaurus/Link';
import { useActiveVersion } from '@docusaurus/plugin-content-docs/client';
import styles from './styles.module.css';

/**
 * Stable keys for each capability card. The page that renders this component
 * supplies the doc path for each key via the `links` prop, so a docs
 * reorganization only needs to update the MDX that imports the component and
 * never this file. Paths are relative to the docs version root (for example
 * `/docs/concepts/rgd/schema`) and are prefixed with the active version path
 * at render time.
 */
export type CapabilityKey =
  | 'schema'
  | 'celExpressions'
  | 'dependencyOrdering'
  | 'conditionalResources'
  | 'collections'
  | 'typeChecking';

export type CapabilityLinks = Partial<Record<CapabilityKey, string>>;

interface Capability {
  key: CapabilityKey;
  title: string;
  description: string;
  viz: React.ReactNode;
  linkLabel?: string;
}

const capabilities: Capability[] = [
  {
    key: 'schema',
    title: 'SimpleSchema',
    description:
      'Define your API schema inline — types, defaults, constraints, and validation in a single readable line. No OpenAPI boilerplate.',
    viz: <SimpleSchemaViz />,
    linkLabel: 'Schema docs',
  },
  {
    key: 'celExpressions',
    title: 'Wires data that doesn\'t exist yet',
    description:
      'Reference status fields from resources that haven\'t been created. kro waits for the data to exist, then wires it into dependent resources.',
    viz: <FutureWireViz />,
    linkLabel: 'CEL expressions',
  },
  {
    key: 'dependencyOrdering',
    title: 'Infers ordering from expressions',
    description:
      'You never declare resource order. kro reads your CEL expressions and builds the dependency graph automatically.',
    viz: <AutoOrderViz />,
    linkLabel: 'Dependency ordering',
  },
  {
    key: 'conditionalResources',
    title: 'Conditional resources',
    description:
      'Include or exclude entire subgraphs based on any CEL expression. When a condition is false, the resource and everything that depends on it are skipped.',
    viz: <ConditionalViz />,
    linkLabel: 'Conditional resources',
  },
  {
    key: 'collections',
    title: 'One template, many resources',
    description:
      'forEach expands a single resource template into multiple resources from a list or range. Define once, create N.',
    viz: <FanOutViz />,
    linkLabel: 'Collections',
  },
  {
    key: 'typeChecking',
    title: 'Non-Turing complete by design',
    description:
      'CEL always terminates, has no side effects, and is type-checked at apply time. You can prove what your definitions do.',
    viz: <SafetyViz />,
    linkLabel: 'Type checking',
  },
];

interface EngineCapabilitiesProps {
  /** Doc paths per capability, relative to the docs version root. */
  links: CapabilityLinks;
}

export default function EngineCapabilities({ links }: EngineCapabilitiesProps): JSX.Element {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);
  // Resolve links against the version the reader is currently viewing so a
  // card on /next/docs/overview stays on /next, and a card on an older
  // version stays on that version.
  const activeVersion = useActiveVersion(undefined);
  const versionPrefix = (activeVersion?.path ?? '').replace(/\/$/, '');

  useEffect(() => {
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setVisible(true);
          observer.disconnect();
        }
      },
      { threshold: 0.05 },
    );
    if (ref.current) observer.observe(ref.current);
    return () => observer.disconnect();
  }, []);

  return (
    <div ref={ref} className={`${styles.container} ${visible ? styles.visible : ''}`}>
      <div className={styles.grid}>
        {capabilities.map((cap, i) => (
          <div
            key={i}
            className={styles.card}
            style={{ transitionDelay: `${i * 0.1}s` }}
          >
            <div className={styles.vizArea}>{cap.viz}</div>
            <div className={styles.cardText}>
              <h3 className={styles.cardTitle}>{cap.title}</h3>
              <p className={styles.cardDesc}>{cap.description}</p>
              {links[cap.key] && (
                <Link to={`${versionPrefix}${links[cap.key]}`} className={styles.cardLink}>
                  {cap.linkLabel || 'Learn more'} &rarr;
                </Link>
              )}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

/* ---------- Visualizations ---------- */

function SimpleSchemaViz() {
  return (
    <div className={styles.vizSchema}>
      <code className={styles.schemaCode}>
        <SchemaLine field="image" type="string" constraint="default=nginx" />
        <SchemaLine field="replicas" type="integer" constraint="default=1 minimum=0" />
        <SchemaLine field="bucketName" type="string" constraint="required=true" />
        <SchemaLine field="monitoring" type="boolean" constraint="default=false" />
      </code>
    </div>
  );
}

function SchemaLine({ field, type, constraint }: { field: string; type: string; constraint: string }) {
  return (
    <div className={styles.schemaLine}>
      <span className={styles.schemaField}>{field}:</span>{' '}
      <span className={styles.schemaType}>{type}</span>
      <span className={styles.schemaPipe}> | </span>
      <span className={styles.schemaConstraint}>{constraint}</span>
    </div>
  );
}

function FutureWireViz() {
  return (
    <div className={styles.vizFutureWire}>
      <div className={styles.fwResource}>
        <div className={styles.fwKind}>Bucket</div>
        <div className={styles.fwField}>
          <span className={styles.fwKey}>status.arn</span>
          <span className={`${styles.fwVal} ${styles.fwPulse}`}>
            arn:aws:s3:::my-bucket
          </span>
        </div>
      </div>

      <div className={styles.fwWire}>
        <svg viewBox="0 0 60 16" className={styles.fwWireSvg}>
          <line
            x1="0" y1="8" x2="50" y2="8"
            stroke="currentColor" strokeWidth="1.5"
            className={styles.fwWireLine}
          />
          <polygon
            points="50,4 58,8 50,12"
            fill="currentColor"
            className={styles.fwWireHead}
          />
        </svg>
      </div>

      <div className={`${styles.fwResource} ${styles.fwTarget}`}>
        <div className={styles.fwKind}>Deployment</div>
        <div className={styles.fwField}>
          <span className={styles.fwKey}>env.BUCKET_ARN</span>
          <span className={`${styles.fwVal} ${styles.fwPulse}`}>
            arn:aws:s3:::my-bucket
          </span>
        </div>
      </div>
    </div>
  );
}

function AutoOrderViz() {
  return (
    <div className={styles.vizAutoOrder}>
      <div className={styles.aoFlat}>
        <div className={styles.aoFlatLabel}>You write (any order)</div>
        <div className={styles.aoFlatItem}>Service</div>
        <div className={styles.aoFlatItem}>Bucket</div>
        <div className={styles.aoFlatItem}>Deployment</div>
      </div>
      <div className={styles.aoArrow}>
        <svg viewBox="0 0 24 16" width="24" height="16">
          <line x1="0" y1="8" x2="16" y2="8" stroke="currentColor" strokeWidth="1.5" />
          <polygon points="16,4 24,8 16,12" fill="currentColor" />
        </svg>
      </div>
      <div className={styles.aoDag}>
        <div className={styles.aoDagLabel}>kro orders</div>
        <div className={styles.aoDagNode}>Bucket</div>
        <SmallArrow />
        <div className={styles.aoDagNode}>Deployment</div>
        <SmallArrow />
        <div className={styles.aoDagNode}>Service</div>
      </div>
    </div>
  );
}

function SmallArrow() {
  return (
    <div className={styles.smallArrow}>
      <svg width="2" height="12" viewBox="0 0 2 12">
        <line x1="1" y1="0" x2="1" y2="12" stroke="currentColor" strokeWidth="1.5" />
      </svg>
      <svg width="8" height="5" viewBox="0 0 8 5">
        <polygon points="0,0 8,0 4,5" fill="currentColor" />
      </svg>
    </div>
  );
}

function ConditionalViz() {
  return (
    <div className={styles.vizConditional}>
      <div className={styles.condNode}>Deployment</div>
      <SmallArrow />
      <div className={styles.condNode}>Service</div>
      <SmallArrow />
      <div className={styles.condGate}>
        <code className={styles.condGateExpr}>enableMonitoring</code>
      </div>
      <div className={styles.condBranch}>
        <SmallArrow />
        <div className={`${styles.condNode} ${styles.condNodeFade}`}>
          ServiceMonitor
        </div>
        <SmallArrow />
        <div className={`${styles.condNode} ${styles.condNodeFade}`}>
          AlertRule
        </div>
      </div>
    </div>
  );
}

function FanOutViz() {
  return (
    <div className={styles.vizFanOut}>
      <div className={styles.foSource}>
        <div className={styles.foTemplate}>
          <div className={styles.foKind}>Pod</div>
          <div className={styles.foExprBlock}>
            <code className={styles.foExpr}>{'forEach:'}</code>
            <code className={styles.foExprVal}>{'  ${lists.range(3)}'}</code>
          </div>
        </div>
      </div>

      <div className={styles.foArrow}>
        <svg viewBox="0 0 24 16" width="24" height="16">
          <line x1="0" y1="8" x2="16" y2="8" stroke="currentColor" strokeWidth="1.5" />
          <polygon points="16,4 24,8 16,12" fill="currentColor" />
        </svg>
      </div>

      <div className={styles.foTargets}>
        <div className={`${styles.foItem} ${styles.fo1}`}>
          <span className={styles.foKind}>Pod-0</span>
        </div>
        <div className={`${styles.foItem} ${styles.fo2}`}>
          <span className={styles.foKind}>Pod-1</span>
        </div>
        <div className={`${styles.foItem} ${styles.fo3}`}>
          <span className={styles.foKind}>Pod-2</span>
        </div>
      </div>
    </div>
  );
}

function SafetyViz() {
  return (
    <div className={styles.vizSafety}>
      <div className={styles.safetyItem}>
        <span className={styles.safetyCheck}>&#10003;</span>
        <div>
          <div className={styles.safetyLabel}>Always terminates</div>
          <div className={styles.safetySub}>No infinite loops possible</div>
        </div>
      </div>
      <div className={styles.safetyItem}>
        <span className={styles.safetyCheck}>&#10003;</span>
        <div>
          <div className={styles.safetyLabel}>No side effects</div>
          <div className={styles.safetySub}>No network calls, no file I/O</div>
        </div>
      </div>
      <div className={styles.safetyItem}>
        <span className={styles.safetyCheck}>&#10003;</span>
        <div>
          <div className={styles.safetyLabel}>Type-checked at apply</div>
          <div className={styles.safetySub}>Errors caught before anything runs</div>
        </div>
      </div>
      <div className={styles.safetyItem}>
        <span className={styles.safetyCheck}>&#10003;</span>
        <div>
          <div className={styles.safetyLabel}>Auditable</div>
          <div className={styles.safetySub}>Prove what a definition does</div>
        </div>
      </div>
    </div>
  );
}
