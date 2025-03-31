{{/*
Template to generate pod-identity configuration
*/}}
{{- define "application-sets.pod-identity" -}}
{{- $chartName := .chartName -}}
{{- $chartConfig := .chartConfig -}}
{{- $valueFiles := .valueFiles -}}
{{- $values := .values -}}
- repoURL: '{{ $values.repoURLGit }}'
  targetRevision: '{{ $values.repoURLGitRevision }}'
  path: 'charts/pod-identity'
  helm:
    releaseName: '{{`{{ .name }}`}}-{{ $chartConfig.chartName | default $chartName }}'
    valuesObject:
      create: '{{`{{default "`}}{{ $chartConfig.enableACK }}{{`" (index .metadata.annotations "ack_create")}}`}}'
      region: '{{`{{ .metadata.annotations.aws_region }}`}}'
      accountId: '{{`{{ .metadata.annotations.aws_account_id}}`}}'
      podIdentityAssociation:
        clusterName: '{{`{{ .name }}`}}'
        namespace: '{{ default $chartConfig.namespace .namespace }}'
    ignoreMissingValueFiles: true
    valueFiles:
     {{- include "application-sets.valueFiles" (dict 
     "nameNormalize" $chartName 
     "valueFiles" $valueFiles 
     "values" $values "chartType" "pod-identity") | nindent 6 }}
{{- end }}
