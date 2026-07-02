{{/*
Expand the name of the chart.
*/}}
{{- define "castai-tsc-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "castai-tsc-controller.fullname" -}}
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
{{- define "castai-tsc-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "castai-tsc-controller.labels" -}}
helm.sh/chart: {{ include "castai-tsc-controller.chart" . }}
{{ include "castai-tsc-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "castai-tsc-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "castai-tsc-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "castai-tsc-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "castai-tsc-controller.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Generate config as YAML string
*/}}
{{- define "castai-tsc-controller.config" -}}
{{- $config := .Values.config -}}
defaultConstraints: '{{ include "castai-tsc-controller.constraintsJson" . }}'
{{- if $config.skipSingleReplica }}
skipSingleReplica: "{{ $config.skipSingleReplica }}"
{{- end }}
logInterval: "{{ $config.logInterval }}"
reconcileInterval: "{{ $config.reconcileInterval }}"
garbageCollectInterval: "{{ $config.garbageCollectInterval }}"
{{- if $config.exclusions }}
exclusions: '{{ $config.exclusions | toJson }}'
{{- end }}
{{- end }}

{{/*
Generate constraints JSON
*/}}
{{- define "castai-tsc-controller.constraintsJson" -}}
{{- $c := .Values.config.defaultConstraints -}}
{{- if eq (kindOf $c) "slice" -}}
{{- $c | toJson }}
{{- else -}}
[{"maxSkew": {{ $c.maxSkew }}, "topologyKey": "{{ $c.topologyKey }}", "whenUnsatisfiable": "{{ $c.whenUnsatisfiable }}"}]
{{- end }}
{{- end }}