import React from 'react';
import CodeBlock from '@theme-original/CodeBlock';
import type CodeBlockType from '@theme/CodeBlock';
import type { WrapperProps } from '@docusaurus/types';
import styles from './styles.module.css';
import { tokenize, Token } from './highlighter';

type Props = WrapperProps<typeof CodeBlockType>;

const styleMap: Record<string, string> = {
  'cel': styles.celExpression,
  'comment': styles.comment,
  'yaml-key': styles.yamlKey,
  'kro-keyword': styles.kroKeyword,
  'schema-type': styles.schemaType,
  'schema-pipe': styles.schemaPipe,
  'schema-keyword': styles.schemaKeyword,
  'schema-value': styles.schemaValue,
};

function renderTokens(tokens: Token[]): React.ReactNode[] {
  return tokens.map((token, idx) => {
    if (token.type === 'text') {
      return token.value;
    }
    const className = styleMap[token.type];
    return <span key={idx} className={className}>{token.value}</span>;
  });
}

export default function CodeBlockWrapper(props: Props): JSX.Element {
  const { children, className, title, metastring } = props as Props & { title?: string; metastring?: string };
  const lang = className?.replace(/language-/, '') || '';
  const code = typeof children === 'string' ? children : '';

  let resolvedTitle = title;
  if (!resolvedTitle && metastring) {
    const m = metastring.match(/title=["']?([^"'\s]+)["']?/);
    if (m) resolvedTitle = m[1];
  }

  const isKro = lang === 'kro' || lang === 'rgd' || lang === 'simpleschema';

  if (isKro) {
    const trimmed = code.trim();
    const tokens = tokenize(trimmed, lang !== 'simpleschema', lang === 'simpleschema');
    return (
      <div className={styles.codeBlockContainer}>
        {resolvedTitle && <div className={styles.codeBlockTitle}>{resolvedTitle}</div>}
        <div className={styles.codeBlockContent}>
          <button className={styles.copyButton} onClick={() => navigator.clipboard.writeText(trimmed)} title="Copy">
            <svg viewBox="0 0 24 24" className={styles.copyIcon}>
              <path d="M19,21H8V7H19M19,5H8A2,2 0 0,0 6,7V21A2,2 0 0,0 8,23H19A2,2 0 0,0 21,21V7A2,2 0 0,0 19,5M16,1H4A2,2 0 0,0 2,3V17H4V3H16V1Z" />
            </svg>
          </button>
          <pre className={styles.kroCodeBlock}>
            <code>{renderTokens(tokens)}</code>
          </pre>
        </div>
      </div>
    );
  }

  return <CodeBlock {...props} />;
}
