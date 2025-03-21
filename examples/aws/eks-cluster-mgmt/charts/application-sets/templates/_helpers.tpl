{{/*
Expand the name of the chart. Defaults to `.Chart.Name` or `nameOverride`.
*/}}
{{- define "application-sets.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Generate a fully qualified app name.
If `fullnameOverride` is defined, it uses that; otherwise, it constructs the name based on `Release.Name` and chart name.
*/}}
{{- define "application-sets.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (default .Chart.Name .Values.nameOverride) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version, useful for labels.
*/}}
{{- define "application-sets.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels for the ApplicationSet, including version and managed-by labels.
*/}}
{{- define "application-sets.labels" -}}
helm.sh/chart: {{ include "application-sets.chart" . }}
app.kubernetes.io/name: {{ include "application-sets.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Common Helm and Kubernetes Annotations
*/}}
{{- define "application-sets.annotations" -}}
helm.sh/chart: {{ include "application-sets.chart" . }}
{{- if .Values.annotations }}
{{ toYaml .Values.annotations }}
{{- end }}
{{- end }}
