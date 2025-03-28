#  {{/*
# Template creating git matrix generator
# */}}
# {{- define "application-sets.git-matrix" -}}
# {{- $chartName := .chartName -}}
# {{- $chartConfig := .chartConfig -}}
# {{- $repoURLGit := .repoURLGit -}}
# {{- $repoURLGitRevision := .repoURLGitRevision -}}
# {{- $selectors := .selectors -}}
# {{- $useSelectors := .useSelectors -}}
# generators:
# - matrix:
#     generators:
#       - clusters:
#           selector:
#               matchLabels:
#                 argocd.argoproj.io/secret-type: cluster
#                 {{- if $selectors }}
#                 {{- toYaml $selectors | nindent 16 }}
#                                 - key: fleet_member
#                                   operator: NotIn
#                                   values: ['control-plane']
#                 {{- end }}
#                 {{- if $chartConfig.selectorMatchLabels }}
#                 {{- toYaml $chartConfig.selectorMatchLabels | nindent 18 }}            
#                 {{- end }}
#               {{- if and $chartConfig.selector $useSelectors }}
#                 {{- toYaml $chartConfig.selector | nindent 16 }}
#               {{- end }}
#           values:
#             chart:  {{ $chartConfig.chartName | default $chartName | quote }}
#       - git:
#           repoURL: {{ $repoURLGit | squote }}
#           revision: {{ $repoURLGitRevision | squote }}
#           files:
#             - path: {{ $chartConfig.matrixPath | squote }}
# {{- end }}