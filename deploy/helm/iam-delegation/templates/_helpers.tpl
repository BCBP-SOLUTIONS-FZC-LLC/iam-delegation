{{/*
Expand the name of the chart.
*/}}
{{- define "iam-delegation.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "iam-delegation.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- printf "%s" $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "iam-delegation.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "iam-delegation.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels — used by Deployment, Service, HPA, PDB, ServiceMonitor,
and the four CronJobs (so NetworkPolicy egress rules cover CronJob pods
too, since they select on these same labels).
*/}}
{{- define "iam-delegation.selectorLabels" -}}
app.kubernetes.io/name: {{ include "iam-delegation.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "iam-delegation.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "iam-delegation.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image tag: prefer .Values.image.tag; fall back to chart appVersion. One
image carries both binaries (server + reconciler) — the Deployment and all
four CronJobs share this same tag.
*/}}
{{- define "iam-delegation.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Name of the Secret holding application secrets: either a pre-existing,
externally-managed Secret (.Values.existingSecret), or the one this chart
renders itself from .Values.secretValues (see templates/secret.yaml).
Shared by the server Deployment and the four reconciler CronJobs.
*/}}
{{- define "iam-delegation.secretName" -}}
{{- default (printf "%s-secrets" (include "iam-delegation.fullname" .)) .Values.existingSecret }}
{{- end }}
