{{/* vim: set filetype=mustache: */}}

{{/*
Chart name (always "tunnelport"; truncated for safety).
*/}}
{{- define "tunnelport.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified resource name. Stem for ServiceAccount / Deployment /
ClusterRole etc. Operator-only resources live in `installNamespace`;
the rendered tbot resources live in the RemoteApp's namespace and are
named after the CR — those are out of this chart's hands.
*/}}
{{- define "tunnelport.fullname" -}}
{{- printf "%s" (include "tunnelport.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Chart label (helm.sh/chart). Sanitised per Helm convention.
*/}}
{{- define "tunnelport.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels stamped on every operator-owned object the chart renders.
These describe the *operator*; rendered tbot pods get a different label
set (see CONTEXT.md / README.md "Pod labels for NetworkPolicy"), which
is the operator code's responsibility, not the chart's.
*/}}
{{- define "tunnelport.labels" -}}
helm.sh/chart: {{ include "tunnelport.chart" . }}
{{ include "tunnelport.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: bumblebee
{{- end -}}

{{/*
Selector labels — must be a strict subset of common labels, and must
match between Deployment.spec.selector and the manager pod template.
*/}}
{{- define "tunnelport.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tunnelport.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Manager image reference. `image.tag` falls back to .Chart.AppVersion so
release-time ABS substitution flows through unchanged.
*/}}
{{- define "tunnelport.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.name $tag -}}
{{- end -}}
