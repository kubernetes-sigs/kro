import React, { useState } from 'react';
import yaml from 'js-yaml';
import styles from './styles.module.css';

interface Field {
  name: string;
  type: string;
  description?: string;
  required?: boolean;
  immutable?: boolean;
  properties?: Field[];
}

interface OpenAPIProperty {
  type?: string;
  description?: string;
  properties?: Record<string, OpenAPIProperty>;
  items?: OpenAPIProperty;
  required?: string[];
  'x-kubernetes-validations'?: Array<{ message?: string; rule?: string }>;
}

function parseOpenAPIProperty(
  name: string,
  prop: OpenAPIProperty,
  required: string[] = []
): Field {
  const field: Field = {
    name,
    type: prop.type || 'unknown',
    description: prop.description,
    required: required.includes(name),
  };

  // Check for immutability in validation rules
  if (prop['x-kubernetes-validations']) {
    const hasImmutableValidation = prop['x-kubernetes-validations'].some(
      (validation) => validation.rule?.includes('== oldSelf')
    );
    if (hasImmutableValidation) {
      field.immutable = true;
    }
  }

  // Handle array types
  if (prop.type === 'array' && prop.items) {
    field.type = `[]${prop.items.type || 'object'}`;
    if (prop.items.properties) {
      field.properties = Object.entries(prop.items.properties).map(([subName, subProp]) =>
        parseOpenAPIProperty(subName, subProp, prop.items?.required || [])
      );
    }
  }

  // Handle object types
  if (prop.type === 'object' && prop.properties) {
    field.properties = Object.entries(prop.properties).map(([subName, subProp]) =>
      parseOpenAPIProperty(subName, subProp, prop.required || [])
    );
  }

  return field;
}

function parseFieldsFromCRD(crdData: any): { specFields: Field[]; statusFields: Field[] } {
  const schema = crdData?.spec?.versions?.[0]?.schema?.openAPIV3Schema;

  const specProps = schema?.properties?.spec?.properties || {};
  const specRequired = schema?.properties?.spec?.required || [];
  const statusProps = schema?.properties?.status?.properties || {};

  const specFields = Object.entries(specProps).map(([name, prop]) =>
    parseOpenAPIProperty(name, prop as OpenAPIProperty, specRequired)
  );

  const statusFields = Object.entries(statusProps).map(([name, prop]) =>
    parseOpenAPIProperty(name, prop as OpenAPIProperty, [])
  );

  return { specFields, statusFields };
}

function FieldRow({ field, depth = 0 }: { field: Field; depth?: number }) {
  const [expanded, setExpanded] = useState(depth < 1);
  const hasNested = field.properties && field.properties.length > 0;

  return (
    <>
      <tr className={styles.fieldRow}>
        <td
          className={styles.fieldName}
          style={{
            paddingLeft: `${depth * 48 + 16}px`,
            borderLeft: depth > 0 ? `2px solid var(--ifm-color-emphasis-200)` : 'none'
          }}
        >
          <div className={styles.fieldNameContent}>
            <span className={styles.expandIconSpace}>
              {hasNested && (
                <button
                  className={styles.expandIcon}
                  onClick={() => setExpanded(!expanded)}
                  aria-label={expanded ? 'Collapse' : 'Expand'}
                >
                  {expanded ? '−' : '+'}
                </button>
              )}
            </span>
            <code className={styles.fieldNameCode}>{field.name}</code>
            {field.required && <span className={styles.requiredBadge}>required</span>}
            {field.immutable && <span className={styles.immutableBadge}>immutable</span>}
          </div>
        </td>
        <td className={styles.fieldType}>
          <span className={`${styles.typeBadge} ${getTypeClass(field.type)}`}>
            {field.type}
          </span>
        </td>
        <td className={styles.fieldDescription}>
          {field.description ? (
            <span dangerouslySetInnerHTML={{ __html: field.description }} />
          ) : (
            <span className={styles.noDescription}>No description</span>
          )}
        </td>
      </tr>
      {expanded && hasNested && field.properties?.map((subField) => (
        <FieldRow key={subField.name} field={subField} depth={depth + 1} />
      ))}
    </>
  );
}

function getTypeClass(type: string): string {
  const baseType = type.replace(/\[\]/, '').toLowerCase();
  if (baseType === 'string') return styles.typeString;
  if (baseType === 'integer' || baseType === 'number') return styles.typeNumber;
  if (baseType === 'boolean') return styles.typeBoolean;
  if (baseType === 'object' || baseType === 'simpleschema') return styles.typeObject;
  if (type.includes('[]')) return styles.typeArray;
  return styles.typeObject;
}

function FieldTable({ fields, title }: { fields: Field[]; title: string }) {
  if (!fields || fields.length === 0) {
    return null;
  }

  return (
    <div className={styles.section}>
      <h3>{title}</h3>
      <div className={styles.tableWrapper}>
        <table className={styles.fieldTable}>
          <thead>
            <tr>
              <th style={{ width: '40%' }}>Field</th>
              <th style={{ width: '15%' }}>Type</th>
              <th style={{ width: '45%' }}>Description</th>
            </tr>
          </thead>
          <tbody>
            {fields.map((field) => (
              <FieldRow key={field.name} field={field} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export default function RGDReference({ crdYaml }: { crdYaml: string }): JSX.Element {
  const crdData = yaml.load(crdYaml);
  const { specFields, statusFields } = parseFieldsFromCRD(crdData);

  return (
    <div className={styles.rgdReference}>
      <FieldTable fields={specFields} title="Spec" />
      <FieldTable fields={statusFields} title="Status" />
    </div>
  );
}
