{{/*
Expand the name of the chart.
*/}}
{{- define "pod-identity.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "pod-identity.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "pod-identity.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "pod-identity.labels" -}}
helm.sh/chart: {{ include "pod-identity.chart" . }}
{{ include "pod-identity.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "pod-identity.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pod-identity.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "pod-identity.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pod-identity.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
{{/*
Construct a dynamic ARN based on the values passed from ArgoCD or values.yaml.
*/}}
{{- define "pod-identity.resourceArn" -}}
arn:aws:{{ .resourceType }}:{{ .region }}:{{ .accountId }}:{{ .resourceName }}
{{- end }}
{{- define "pod-identity.accountID" -}}
{{ .accountId }}
{{- end }}
{{- define "pod-identity.region" -}}
{{ .region }}
{{- end }}