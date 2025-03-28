{{/*
Expand the name of the chart.
*/}}
{{- define "team.name" -}}
{{- default .Chart.Name .Values.name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "team.fullname" -}}
{{- if .Values.name }}
{{- .Values.name | trunc 63 | trimSuffix "-" }}
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
{{- define "team.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common Helm and Kubernetes labels
*/}}
{{- define "team.labels" -}}
helm.sh/chart: {{ include "team.chart" . }}
app.kubernetes.io/name: {{ include "team.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Values.labels }}
{{ toYaml .Values.labels }}
{{- end }}
{{- end }}

{{/*
Common Helm and Kubernetes labels
*/}}
{{- define "team.annotations" -}}
helm.sh/chart: {{ include "team.chart" . }}
{{- if .Values.annotations }}
{{ toYaml .Values.annotations }}
{{- end }}
{{- end }}


{{/*
Create the name of the NetworkPolicy to deny all outgoing traffic
*/}}
{{- define "team.networkPolicy.egress.deny.all.name" }}
{{- printf "%s-%s" ((include "team.fullname" .) | trunc 47 | trimSuffix "-") "egress-deny-all" }}
{{- end }}

{{/*
Create the name of the NetworkPolicy to allow outgoing traffic to the Kubernetes DNS
*/}}
{{- define "team.networkPolicy.egress.allow.dns.name" }}
{{- printf "%s-%s" ((include "team.fullname" .) | trunc 47 | trimSuffix "-") "egress-allow-dns" }}
{{- end }}

{{/*
Create the name of the NetworkPolicy to deny all incoming traffic
*/}}
{{- define "team.networkPolicy.ingress.deny.all.name" }}
{{- printf "%s-%s" ((include "team.fullname" .) | trunc 46 | trimSuffix "-") "ingress-deny-all" }}
{{- end }}

