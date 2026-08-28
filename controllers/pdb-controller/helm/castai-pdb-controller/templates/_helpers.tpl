{{/*
Expand the name of the chart.
*/}}
{{- define "castai-pdb-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "castai-pdb-controller.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- printf "%s" $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "castai-pdb-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "castai-pdb-controller.labels" -}}
helm.sh/chart: {{ include "castai-pdb-controller.chart" . }}
app.kubernetes.io/name: {{ include "castai-pdb-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Render the controller ConfigMap content as a string for checksum calculation.
*/}}
{{- define "castai-pdb-controller.config" -}}
{{- $minAvailable := toString .Values.config.defaultMinAvailable }}
{{- $maxUnavailable := toString .Values.config.defaultMaxUnavailable }}
{{- $minAvailableValid := and (ne $minAvailable "") (ne $minAvailable "null") (ne $minAvailable "<nil>") }}
{{- $maxUnavailableValid := and (ne $maxUnavailable "") (ne $maxUnavailable "null") (ne $maxUnavailable "<nil>") }}
{{- if $maxUnavailableValid }}defaultMaxUnavailable: {{ .Values.config.defaultMaxUnavailable | quote }}
{{- else if $minAvailableValid }}defaultMinAvailable: {{ .Values.config.defaultMinAvailable | quote }}
{{- end }}
FixPoorPDBs: {{ .Values.config.FixPoorPDBs | quote }}
logInterval: {{ .Values.config.logInterval | quote }}
pdbScanInterval: {{ .Values.config.pdbScanInterval | quote }}
garbageCollectInterval: {{ .Values.config.garbageCollectInterval | quote }}
pdbDumpInterval: {{ .Values.config.pdbDumpInterval | quote }}
logLevel: {{ .Values.config.logLevel | quote }}
{{- $uep := toString .Values.config.defaultUnhealthyPodEvictionPolicy }}
{{- if and $uep (ne $uep "") (ne $uep "null") (ne $uep "<nil>") }}
defaultUnhealthyPodEvictionPolicy: {{ .Values.config.defaultUnhealthyPodEvictionPolicy | quote }}
{{- end }}
{{- if .Values.config.exclusions }}
exclusions: |
{{ toYaml .Values.config.exclusions | indent 4 }}
{{- end }}
{{- end }} 