import {
  tokenize,
  getTokensByType,
  tokensToText,
  CEL_RE,
  MARKER_KEYWORDS,
} from '../theme/CodeBlock/highlighter';

describe('CEL Expression Regex', () => {
  it('matches simple CEL expressions', () => {
    const matches = [...'${schema.spec.name}'.matchAll(CEL_RE)];
    expect(matches).toHaveLength(1);
    expect(matches[0][0]).toBe('${schema.spec.name}');
  });

  it('matches CEL with nested braces', () => {
    const matches = [...'${map(x, {a: 1})}'.matchAll(CEL_RE)];
    expect(matches).toHaveLength(1);
    expect(matches[0][0]).toBe('${map(x, {a: 1})}');
  });

  it('matches multiple CEL expressions', () => {
    const code = 'name: ${schema.spec.name}-${schema.spec.suffix}';
    const matches = [...code.matchAll(CEL_RE)];
    expect(matches).toHaveLength(2);
    expect(matches[0][0]).toBe('${schema.spec.name}');
    expect(matches[1][0]).toBe('${schema.spec.suffix}');
  });

  it('matches multi-line CEL expressions', () => {
    const code = `\${
      deployment.status.availableReplicas >= deployment.spec.replicas ?
      "healthy" :
      "degraded"
    }`;
    const matches = [...code.matchAll(CEL_RE)];
    expect(matches).toHaveLength(1);
    expect(matches[0][0]).toContain('deployment.status.availableReplicas');
  });

  it('does not match incomplete expressions', () => {
    const matches = [...'${schema.spec.name'.matchAll(CEL_RE)];
    expect(matches).toHaveLength(0);
  });
});

describe('tokenize - CEL expressions', () => {
  it('tokenizes simple CEL expression', () => {
    const tokens = tokenize('name: ${schema.spec.name}', true);
    expect(getTokensByType(tokens, 'cel')).toEqual(['${schema.spec.name}']);
  });

  it('tokenizes multiple CEL expressions in one line', () => {
    const tokens = tokenize('full: ${schema.spec.first}-${schema.spec.last}', true);
    expect(getTokensByType(tokens, 'cel')).toEqual([
      '${schema.spec.first}',
      '${schema.spec.last}',
    ]);
  });

  it('tokenizes multi-line CEL expressions', () => {
    const code = `status: \${
  deployment.status.replicas > 0 ?
  "running" : "stopped"
}`;
    const tokens = tokenize(code, true);
    const celTokens = getTokensByType(tokens, 'cel');
    expect(celTokens).toHaveLength(1);
    expect(celTokens[0]).toContain('deployment.status.replicas');
  });

  it('preserves text before and after CEL', () => {
    const code = 'name: prefix-${schema.spec.name}-suffix';
    const tokens = tokenize(code, true);
    expect(tokensToText(tokens)).toBe(code);
  });
});

describe('tokenize - Comments', () => {
  it('tokenizes full line comments', () => {
    const code = '# This is a comment';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'comment')).toEqual(['# This is a comment']);
  });

  it('tokenizes inline comments', () => {
    const code = 'replicas: 3 # number of replicas';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'comment')).toEqual(['# number of replicas']);
  });

  it('preserves indentation before comments', () => {
    const code = '  # indented comment';
    const tokens = tokenize(code, true);
    expect(tokensToText(tokens)).toBe(code);
  });

  it('does not treat # in strings as comments', () => {
    const code = 'value: "contains#hash"';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'comment')).toEqual([]);
  });
});

describe('tokenize - YAML keys', () => {
  it('tokenizes simple keys', () => {
    const code = 'apiVersion: apps/v1';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'yaml-key')).toEqual(['apiVersion']);
  });

  it('tokenizes nested keys', () => {
    const code = `metadata:
  name: my-app
  namespace: default`;
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'yaml-key')).toEqual([
      'metadata',
      'name',
      'namespace',
    ]);
  });

  it('tokenizes keys with list items', () => {
    const code = '  - id: deployment';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual(['id']);
  });

  it('does not treat colons in values as key separators', () => {
    const code = 'image: nginx:latest';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'yaml-key')).toEqual(['image']);
  });

  it('handles arn-style values correctly', () => {
    const code = 'policy: arn:aws:iam::123456789:policy/MyPolicy';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'yaml-key')).toEqual(['policy']);
  });
});

describe('tokenize - kro keywords', () => {
  it('highlights id as kro keyword', () => {
    const code = '  - id: deployment';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual(['id']);
  });

  it('highlights template as kro keyword', () => {
    const code = '    template:';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual(['template']);
  });

  it('highlights readyWhen as kro keyword', () => {
    const code = '    readyWhen:';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual(['readyWhen']);
  });

  it('highlights includeWhen as kro keyword', () => {
    const code = '    includeWhen:';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual(['includeWhen']);
  });

  it('highlights externalRef as kro keyword', () => {
    const code = '    externalRef: true';
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual(['externalRef']);
  });

  it('does not highlight kro keywords when isKro is false', () => {
    const code = '  - id: deployment';
    const tokens = tokenize(code, false);
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual([]);
    expect(getTokensByType(tokens, 'yaml-key')).toEqual(['id']);
  });
});

describe('tokenize - SimpleSchema types', () => {
  it('highlights type with markers', () => {
    const code = 'name: string | required=true';
    const tokens = tokenize(code, true, true);
    expect(getTokensByType(tokens, 'schema-type')).toEqual(['string']);
  });

  it('highlights pipe separator', () => {
    const code = 'name: string | required=true';
    const tokens = tokenize(code, true, true);
    expect(getTokensByType(tokens, 'schema-pipe')).toEqual([' | ']);
  });

  it('highlights marker keywords', () => {
    const code = 'replicas: integer | default=3 minimum=1 maximum=10';
    const tokens = tokenize(code, true, true);
    expect(getTokensByType(tokens, 'schema-keyword')).toEqual([
      'default',
      'minimum',
      'maximum',
    ]);
  });

  it('highlights marker values', () => {
    const code = 'name: string | default="my-app"';
    const tokens = tokenize(code, true, true);
    expect(getTokensByType(tokens, 'schema-value')).toEqual(['="my-app"']);
  });

  it('highlights all marker keywords', () => {
    for (const keyword of MARKER_KEYWORDS) {
      const code = `field: string | ${keyword}=value`;
      const tokens = tokenize(code, true, true);
      expect(getTokensByType(tokens, 'schema-keyword')).toEqual([keyword]);
    }
  });

  it('highlights bare types in schema section', () => {
    const code = `schema:
  spec:
    name: string
    count: integer`;
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'schema-type')).toEqual(['string', 'integer']);
  });

  it('does not highlight types outside schema section', () => {
    const code = `resources:
  - id: deployment
    template:
      spec:
        replicas: 3`;
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'schema-type')).toEqual([]);
  });
});

describe('tokenize - schema context detection', () => {
  it('enters schema context after schema:', () => {
    const code = `spec:
  schema:
    spec:
      name: string`;
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'schema-type')).toEqual(['string']);
  });

  it('exits schema context when dedenting to schema level', () => {
    const code = `spec:
  schema:
    spec:
      name: string
  resources:
    - id: test`;
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'schema-type')).toEqual(['string']);
  });

  it('handles types section', () => {
    const code = `schema:
  types:
    MyType:
      field: string`;
    const tokens = tokenize(code, true);
    expect(getTokensByType(tokens, 'schema-type')).toEqual(['string']);
  });
});

describe('tokenize - preserves original text', () => {
  it('reconstructs simple code', () => {
    const code = 'apiVersion: apps/v1';
    const tokens = tokenize(code, true);
    expect(tokensToText(tokens)).toBe(code);
  });

  it('reconstructs code with CEL', () => {
    const code = 'name: ${schema.spec.name}-suffix';
    const tokens = tokenize(code, true);
    expect(tokensToText(tokens)).toBe(code);
  });

  it('reconstructs multi-line code', () => {
    const code = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app`;
    const tokens = tokenize(code, true);
    expect(tokensToText(tokens)).toBe(code);
  });

  it('reconstructs code with comments', () => {
    const code = `# Header comment
apiVersion: apps/v1 # inline comment
kind: Deployment`;
    const tokens = tokenize(code, true);
    expect(tokensToText(tokens)).toBe(code);
  });

  it('reconstructs code with schema markers', () => {
    const code = 'name: string | required=true default="test"';
    const tokens = tokenize(code, true, true);
    expect(tokensToText(tokens)).toBe(code);
  });
});

describe('tokenize - complex examples', () => {
  it('handles full RGD example', () => {
    const code = `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: my-app
spec:
  schema:
    apiVersion: v1alpha1
    kind: Application
    spec:
      name: string | required=true
      replicas: integer | default=3
    status:
      ready: \${deployment.status.availableReplicas >= schema.spec.replicas}
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: \${schema.spec.name}
        spec:
          replicas: \${schema.spec.replicas}
      readyWhen:
        - \${deployment.status.availableReplicas == deployment.spec.replicas}`;

    const tokens = tokenize(code, true);

    // Verify CEL expressions
    expect(getTokensByType(tokens, 'cel')).toEqual([
      '${deployment.status.availableReplicas >= schema.spec.replicas}',
      '${schema.spec.name}',
      '${schema.spec.replicas}',
      '${deployment.status.availableReplicas == deployment.spec.replicas}',
    ]);

    // Verify kro keywords
    expect(getTokensByType(tokens, 'kro-keyword')).toEqual([
      'id',
      'template',
      'readyWhen',
    ]);

    // Verify schema types
    expect(getTokensByType(tokens, 'schema-type')).toEqual(['string', 'integer']);

    // Verify schema keywords
    expect(getTokensByType(tokens, 'schema-keyword')).toEqual(['required', 'default']);

    // Verify text reconstruction
    expect(tokensToText(tokens)).toBe(code);
  });

  it('handles simpleschema mode', () => {
    const code = `spec:
  name: string | required=true
  replicas: integer | default=3 minimum=1 maximum=10
types:
  Custom:
    field: boolean`;

    const tokens = tokenize(code, false, true);

    expect(getTokensByType(tokens, 'schema-type')).toEqual([
      'string',
      'integer',
      'boolean',
    ]);

    expect(getTokensByType(tokens, 'schema-keyword')).toEqual([
      'required',
      'default',
      'minimum',
      'maximum',
    ]);
  });
});
