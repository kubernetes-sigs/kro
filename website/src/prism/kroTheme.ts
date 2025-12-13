import type { PrismTheme } from 'prism-react-renderer';

// Subtle theme with muted colors
// Keeps things readable without being too colorful
export const kroLight: PrismTheme = {
  plain: {
    color: '#1f2937',
    backgroundColor: '#fafafa',
  },
  styles: [
    {
      types: ['comment', 'prolog', 'doctype', 'cdata'],
      style: { color: '#9ca3af' },
    },
    {
      types: ['punctuation'],
      style: { color: '#9ca3af' },
    },
    {
      types: ['key', 'atrule', 'attr-name', 'property', 'tag'],
      style: { color: '#6b7280' },
    },
    {
      types: ['constant', 'symbol', 'deleted'],
      style: { color: '#6b7280' },
    },
    {
      types: ['boolean', 'number', 'string', 'char', 'inserted', 'scalar'],
      style: { color: '#1f2937' },
    },
    {
      types: ['selector', 'attr-value', 'builtin', 'important'],
      style: { color: '#1f2937' },
    },
    {
      types: ['operator', 'entity', 'url'],
      style: { color: '#9ca3af' },
    },
    {
      types: ['keyword', 'function', 'class-name'],
      style: { color: '#1f2937' },
    },
    {
      types: ['regex', 'variable'],
      style: { color: '#6b7280' },
    },
  ],
};

export const kroDark: PrismTheme = {
  plain: {
    color: '#f5f5f4',
    backgroundColor: '#161616',
  },
  styles: [
    {
      types: ['comment', 'prolog', 'doctype', 'cdata'],
      style: { color: '#6b6b6b' },
    },
    {
      types: ['punctuation'],
      style: { color: '#78716c' },
    },
    {
      types: ['key', 'atrule', 'attr-name', 'property', 'tag'],
      style: { color: '#a8a29e' },
    },
    {
      types: ['constant', 'symbol', 'deleted'],
      style: { color: '#a8a29e' },
    },
    {
      types: ['boolean', 'number', 'string', 'char', 'inserted', 'scalar'],
      style: { color: '#f5f5f4' },
    },
    {
      types: ['selector', 'attr-value', 'builtin', 'important'],
      style: { color: '#f5f5f4' },
    },
    {
      types: ['operator', 'entity', 'url'],
      style: { color: '#78716c' },
    },
    {
      types: ['keyword', 'function', 'class-name'],
      style: { color: '#f5f5f4' },
    },
    {
      types: ['regex', 'variable'],
      style: { color: '#a8a29e' },
    },
  ],
};
