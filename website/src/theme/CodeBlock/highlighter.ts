// TODO: These keywords are duplicated from the Go codebase. We need a centralized
// place (e.g., a shared JSON/YAML file) to keep these in sync automatically between
// the Go parser and this TypeScript highlighter.
// Go locations:
//   - MARKER_KEYWORDS: pkg/simpleschema/parser/markers.go
//   - KRO_KEYWORDS: pkg/graph/parser/parser.go (resource fields like id, template, etc.)

// Marker keywords for SimpleSchema validation
export const MARKER_KEYWORDS = new Set([
  'required', 'default', 'optional', 'description', 'enum',
  'minimum', 'maximum', 'immutable', 'pattern',
  'minLength', 'maxLength', 'uniqueItems', 'minItems', 'maxItems'
]);

// kro-specific keywords
export const KRO_KEYWORDS = new Set([
  'id', 'template', 'readyWhen', 'includeWhen', 'externalRef', 'forEach'
]);

// CEL expression regex: matches ${...} with nested braces support (multi-line)
export const CEL_RE = /\$\{(?:[^{}]|\{[^}]*\})*\}/gs;

// Token types for highlighting
export type TokenType =
  | 'text'
  | 'cel'
  | 'comment'
  | 'yaml-key'
  | 'kro-keyword'
  | 'schema-type'
  | 'schema-pipe'
  | 'schema-keyword'
  | 'schema-value';

export interface Token {
  type: TokenType;
  value: string;
}

/**
 * Tokenizes code for syntax highlighting.
 * Returns an array of tokens that can be rendered with appropriate styles.
 */
export function tokenize(code: string, isKro: boolean, alwaysSchema = false): Token[] {
  const tokens: Token[] = [];

  // Track schema context
  let schemaIndent = -1;
  let inSchemaSection = alwaysSchema;
  let sectionIndent = -1;

  // First pass: extract CEL expressions (handles multi-line)
  let pos = 0;
  for (const m of code.matchAll(CEL_RE)) {
    if (m.index! > pos) {
      tokens.push(...processYAML(code.slice(pos, m.index!)));
    }
    tokens.push({ type: 'cel', value: m[0] });
    pos = m.index! + m[0].length;
  }
  if (pos < code.length) {
    tokens.push(...processYAML(code.slice(pos)));
  }

  function processYAML(yaml: string): Token[] {
    const out: Token[] = [];
    const lines = yaml.split('\n');

    lines.forEach((line, idx) => {
      if (idx > 0) out.push({ type: 'text', value: '\n' });
      const trimmed = line.trim();
      const indent = line.search(/\S/);

      // Update schema context
      if (trimmed.startsWith('schema:')) {
        schemaIndent = indent;
        inSchemaSection = false;
      } else if (trimmed.startsWith('spec:') || trimmed.startsWith('types:')) {
        if (schemaIndent >= 0 && indent > schemaIndent) {
          inSchemaSection = true;
          sectionIndent = indent;
        } else if (alwaysSchema && schemaIndent < 0) {
          inSchemaSection = true;
          sectionIndent = indent;
        }
      } else if (schemaIndent >= 0 && indent >= 0 && indent <= schemaIndent && trimmed && !trimmed.startsWith('#')) {
        schemaIndent = -1;
        inSchemaSection = false;
      } else if (inSchemaSection && indent <= sectionIndent && trimmed && !trimmed.startsWith('#')) {
        inSchemaSection = false;
      }

      // Full comment line
      if (trimmed.startsWith('#')) {
        const ws = line.match(/^(\s*)/)?.[1] || '';
        if (ws) out.push({ type: 'text', value: ws });
        out.push({ type: 'comment', value: line.slice(ws.length) });
        return;
      }

      // Check for inline comment
      let commentIdx = -1;
      let inQ = false;
      for (let i = 0; i < line.length; i++) {
        if (line[i] === '"' && line[i - 1] !== '\\') inQ = !inQ;
        if (!inQ && line[i] === '#') {
          commentIdx = i;
          break;
        }
      }

      const codePart = commentIdx >= 0 ? line.slice(0, commentIdx) : line;
      const commentPart = commentIdx >= 0 ? line.slice(commentIdx) : '';

      out.push(...processLine(codePart));
      if (commentPart) {
        out.push({ type: 'comment', value: commentPart });
      }
    });

    return out;
  }

  function processLine(line: string): Token[] {
    const out: Token[] = [];
    const trimmed = line.trim();
    if (!trimmed) {
      out.push({ type: 'text', value: line });
      return out;
    }

    // Key: value pattern
    const colonSpaceIdx = line.indexOf(': ');
    const trailingColon = trimmed.endsWith(':') && !trimmed.includes(': ');
    const colonIdx = colonSpaceIdx > 0 ? colonSpaceIdx : (trailingColon ? line.lastIndexOf(':') : -1);

    if (colonIdx > 0 && !trimmed.startsWith('"') && !trimmed.startsWith('{') && !trimmed.startsWith('}')) {
      const keyPart = line.slice(0, colonIdx);
      const valPart = line.slice(colonIdx + (colonSpaceIdx > 0 ? 2 : 1));
      const km = keyPart.match(/^(\s*)(-\s+)?(\S+)$/);

      if (km) {
        if (km[1]) out.push({ type: 'text', value: km[1] });
        if (km[2]) out.push({ type: 'text', value: km[2] });
        const keyName = km[3];
        const keyType: TokenType = isKro && KRO_KEYWORDS.has(keyName) ? 'kro-keyword' : 'yaml-key';
        out.push({ type: keyType, value: keyName });
      } else {
        out.push({ type: 'text', value: keyPart });
      }
      out.push({ type: 'text', value: colonSpaceIdx > 0 ? ': ' : ':' });

      if (valPart) out.push(...highlightValue(valPart));
    } else {
      out.push(...highlightValue(line));
    }

    return out;
  }

  function highlightValue(val: string): Token[] {
    const out: Token[] = [];
    let v = val.trim();
    const leadingWs = val.match(/^(\s*)/)?.[1] || '';

    // Strip quotes
    let pre = '', suf = '';
    if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) {
      pre = v[0];
      suf = v[v.length - 1];
      v = v.slice(1, -1);
    }

    // Type | markers pattern
    const schemaMatch = v.match(/^(\S+)(\s+\|\s+)(.+)$/);
    if (schemaMatch) {
      const [, type, pipe, markers] = schemaMatch;
      if (leadingWs) out.push({ type: 'text', value: leadingWs });
      if (pre) out.push({ type: 'text', value: pre });
      out.push({ type: 'schema-type', value: type });
      out.push({ type: 'schema-pipe', value: pipe });

      const markerRe = /(\w+)(=)("[^"]*"|\{[^}]*\}|\[[^\]]*\]|\S+)/g;
      let last = 0;
      let m: RegExpExecArray | null;
      while ((m = markerRe.exec(markers)) !== null) {
        if (m.index > last) out.push({ type: 'text', value: markers.slice(last, m.index) });
        const [, kw, eq, mval] = m;
        if (MARKER_KEYWORDS.has(kw)) {
          out.push({ type: 'schema-keyword', value: kw });
          out.push({ type: 'schema-value', value: eq + mval });
        } else {
          out.push({ type: 'text', value: m[0] });
        }
        last = markerRe.lastIndex;
      }
      if (last < markers.length) out.push({ type: 'text', value: markers.slice(last) });
      if (suf) out.push({ type: 'text', value: suf });
    } else if (inSchemaSection && v) {
      if (leadingWs) out.push({ type: 'text', value: leadingWs });
      if (pre) out.push({ type: 'text', value: pre });
      out.push({ type: 'schema-type', value: v });
      if (suf) out.push({ type: 'text', value: suf });
    } else {
      out.push({ type: 'text', value: val });
    }

    return out;
  }

  return tokens;
}

/**
 * Helper to get all tokens of a specific type
 */
export function getTokensByType(tokens: Token[], type: TokenType): string[] {
  return tokens.filter(t => t.type === type).map(t => t.value);
}

/**
 * Helper to reconstruct the original text from tokens
 */
export function tokensToText(tokens: Token[]): string {
  return tokens.map(t => t.value).join('');
}
