{{/*
Template to generate additional resources configuration
*/}}
{{- define "application-sets.additionalResources" -}}
{{- $chartName := .chartName -}}
{{- $chartConfig := .chartConfig -}}
{{- $valueFiles := .valueFiles -}}
{{- $additionalResourcesType := .additionalResourcesType -}}
{{- $additionalResourcesPath := .path -}}
{{- $values := .values -}}
{{- if $chartConfig.additionalResources.path }}
- repoURL: {{ $values.repoURLGit | squote }}
  targetRevision: {{ $values.repoURLGitRevision | squote }}
  path: {{- if eq $additionalResourcesType "manifests" }}
    '{{ $values.repoURLGitBasePath }}{{ if $values.useValuesFilePrefix }}{{ $values.valuesFilePrefix }}{{ end }}clusters/{{`{{.nameNormalized}}`}}/{{ $chartConfig.additionalResources.manifestPath }}'
  {{- else }}
    {{ $chartConfig.additionalResources.path | squote }}
  {{- end}}
{{- end }}
{{- if $chartConfig.additionalResources.chart }}
- repoURL: '{{$chartConfig.additionalResources.repoURL}}'
  chart: '{{$chartConfig.additionalResources.chart}}'
  targetRevision: '{{$chartConfig.additionalResources.chartVersion }}'
{{- end }}
{{- if $chartConfig.additionalResources.helm }}
  helm:
    releaseName: '{{`{{ .name }}`}}-{{ $chartConfig.additionalResources.helm.releaseName }}'
    {{- if $chartConfig.additionalResources.helm.valuesObject }}
    valuesObject:
    {{- $chartConfig.additionalResources.helm.valuesObject | toYaml | nindent 6 }}
    {{- end }}
    ignoreMissingValueFiles: true
    valueFiles:
    {{- include "application-sets.valueFiles" (dict 
      "nameNormalize" $chartName 
      "valueFiles" $valueFiles 
      "values" $values 
      "chartType" $additionalResourcesType) | nindent 6 }}
{{- end }}
{{- end }}


{{/*
Define the values path for reusability
*/}}
{{- define "application-sets.valueFiles" -}}
{{- $nameNormalize := .nameNormalize -}}
{{- $chartConfig := .chartConfig -}}
{{- $valueFiles := .valueFiles -}}
{{- $chartType := .chartType -}}
{{- $values := .values -}}
{{- with .valueFiles }}
{{- range . }}
- $values/{{ $values.repoURLGitBasePath }}{{ . }}/{{ $nameNormalize }}{{ if $chartType }}/{{ $chartType }}{{ end }}/{{ if $chartConfig.valuesFileName }}{{ $chartConfig.valuesFileName }}{{ else }}values.yaml{{ end }}
- $values/{{ $values.repoURLGitBasePath }}{{ if $values.useValuesFilePrefix }}{{ $values.valuesFilePrefix }}{{ end }}{{ . }}/{{ $nameNormalize }}{{ if $chartType }}/{{ $chartType }}{{ end }}/{{ if $chartConfig.valuesFileName }}{{ $chartConfig.valuesFileName }}{{ else }}values.yaml{{ end }}
{{- end }}
{{- end }}
{{- end }}
